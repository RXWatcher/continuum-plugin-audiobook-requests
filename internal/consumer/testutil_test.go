package consumer_test

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ContinuumApp/continuum-plugin-audiobook-requests/internal/migrate"
	"github.com/ContinuumApp/continuum-plugin-audiobook-requests/internal/store"
)

var (
	testSchemaOnce sync.Once
	testSchema     string
)

func schemaName() string {
	testSchemaOnce.Do(func() {
		testSchema = fmt.Sprintf("audiobookbay_consumer_test_%d", os.Getpid())
	})
	return testSchema
}

func testDSN() string {
	if v := os.Getenv("TEST_DATABASE_URL"); v != "" {
		u, err := url.Parse(v)
		if err == nil {
			q := u.Query()
			q.Set("search_path", schemaName())
			u.RawQuery = q.Encode()
			return u.String()
		}
		return v
	}
	return fmt.Sprintf(
		"postgres://continuum:continuum@localhost:5432/continuum?search_path=%s&sslmode=disable",
		schemaName(),
	)
}

func stripSearchPath(dsn string) string {
	u, err := url.Parse(dsn)
	if err != nil {
		return dsn
	}
	q := u.Query()
	q.Del("search_path")
	u.RawQuery = q.Encode()
	return u.String()
}

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	dsn := testDSN()
	ctx := context.Background()
	adminDSN := stripSearchPath(dsn)
	admin, err := pgxpool.New(ctx, adminDSN)
	if err != nil {
		t.Skipf("postgres unreachable: %v", err)
	}
	if err := admin.Ping(ctx); err != nil {
		admin.Close()
		t.Skipf("postgres unreachable: %v", err)
	}
	defer admin.Close()
	_, _ = admin.Exec(ctx, fmt.Sprintf("DROP SCHEMA IF EXISTS %s CASCADE", schemaName()))
	if _, err := admin.Exec(ctx, fmt.Sprintf("CREATE SCHEMA %s", schemaName())); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	if err := migrate.Run(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(func() { pool.Close() })
	return store.New(pool)
}

func TestMain(m *testing.M) {
	code := m.Run()
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, stripSearchPath(testDSN()))
	if err == nil {
		_, _ = admin.Exec(ctx, fmt.Sprintf("DROP SCHEMA IF EXISTS %s CASCADE", schemaName()))
		admin.Close()
	}
	os.Exit(code)
}

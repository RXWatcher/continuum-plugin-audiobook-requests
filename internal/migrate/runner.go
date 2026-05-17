// Package migrate runs the plugin's embedded SQL migrations against the
// caller-provided database.
package migrate

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"strings"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
)

//go:embed files/*.sql
var migrations embed.FS

func Run(_ context.Context, dsn string) error {
	src, err := iofs.New(migrations, "files")
	if err != nil {
		return fmt.Errorf("load migrations: %w", err)
	}
	driverDSN := dsn
	for _, p := range []string{"postgres://", "postgresql://"} {
		if strings.HasPrefix(driverDSN, p) {
			driverDSN = "pgx5://" + driverDSN[len(p):]
			break
		}
	}
	m, err := migrate.NewWithSourceInstance("iofs", src, driverDSN)
	if err != nil {
		return fmt.Errorf("init migrate: %w", err)
	}
	defer func() {
		_, _ = m.Close()
	}()
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("apply migrations: %w", err)
	}
	return nil
}

package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ForwardedRequest tracks a request the portal sent us that we forwarded
// to AudiobookBay. No AutoMonitor — AudiobookBay has no monitoring feature.
type ForwardedRequest struct {
	RequestID  string
	ExternalID string
	Status     string
	LastPolled time.Time
	ErrorText  string
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

type RequestStats struct {
	Total       int `json:"total"`
	Active      int `json:"active"`
	Failed      int `json:"failed"`
	Imported    int `json:"imported"`
	WithErrors  int `json:"with_errors"`
	Unsubmitted int `json:"unsubmitted"`
}

var ErrNotFound = errors.New("not found")

func (s *Store) UpsertForwardedRequest(ctx context.Context, r ForwardedRequest) error {
	if r.RequestID == "" {
		return fmt.Errorf("request_id required")
	}
	if r.UpdatedAt.IsZero() {
		r.UpdatedAt = time.Now()
	}
	var (
		extPtr     *string
		errPtr     *string
		lastPolled *time.Time
	)
	if r.ExternalID != "" {
		v := r.ExternalID
		extPtr = &v
	}
	if r.ErrorText != "" {
		v := r.ErrorText
		errPtr = &v
	}
	if !r.LastPolled.IsZero() {
		v := r.LastPolled
		lastPolled = &v
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO forwarded_request (request_id, external_id, status, last_polled, error_text, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (request_id) DO UPDATE SET
			external_id = COALESCE(EXCLUDED.external_id, forwarded_request.external_id),
			status      = EXCLUDED.status,
			last_polled = COALESCE(EXCLUDED.last_polled, forwarded_request.last_polled),
			error_text  = COALESCE(EXCLUDED.error_text, forwarded_request.error_text),
			updated_at  = EXCLUDED.updated_at
	`, r.RequestID, extPtr, r.Status, lastPolled, errPtr, r.UpdatedAt)
	if err != nil {
		return fmt.Errorf("upsert forwarded_request: %w", err)
	}
	return nil
}

func (s *Store) GetForwardedRequest(ctx context.Context, requestID string) (ForwardedRequest, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT request_id, COALESCE(external_id,''), status,
		       COALESCE(last_polled, '0001-01-01 00:00:00'::timestamptz),
		       COALESCE(error_text,''), created_at, updated_at
		FROM forwarded_request WHERE request_id = $1
	`, requestID)
	var r ForwardedRequest
	if err := row.Scan(&r.RequestID, &r.ExternalID, &r.Status, &r.LastPolled,
		&r.ErrorText, &r.CreatedAt, &r.UpdatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ForwardedRequest{}, ErrNotFound
		}
		return ForwardedRequest{}, fmt.Errorf("get forwarded_request: %w", err)
	}
	return r, nil
}

func (s *Store) ListNonTerminal(ctx context.Context, limit int) ([]ForwardedRequest, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx, `
		SELECT request_id, COALESCE(external_id,''), status,
		       COALESCE(last_polled, '0001-01-01 00:00:00'::timestamptz),
		       COALESCE(error_text,''), created_at, updated_at
		FROM forwarded_request
		WHERE status NOT IN ('imported','failed')
		ORDER BY COALESCE(last_polled, '0001-01-01 00:00:00'::timestamptz) ASC
		LIMIT $1
	`, limit)
	if err != nil {
		return nil, fmt.Errorf("list non-terminal: %w", err)
	}
	defer rows.Close()

	var out []ForwardedRequest
	for rows.Next() {
		var r ForwardedRequest
		if err := rows.Scan(&r.RequestID, &r.ExternalID, &r.Status, &r.LastPolled,
			&r.ErrorText, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		out = append(out, r)
	}
	return out, nil
}

func (s *Store) RequestStats(ctx context.Context) (RequestStats, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT
			COUNT(*)::int,
			COUNT(*) FILTER (WHERE status NOT IN ('imported','failed'))::int,
			COUNT(*) FILTER (WHERE status = 'failed')::int,
			COUNT(*) FILTER (WHERE status = 'imported')::int,
			COUNT(*) FILTER (WHERE COALESCE(error_text,'') <> '')::int,
			COUNT(*) FILTER (WHERE COALESCE(external_id,'') = '')::int
		FROM forwarded_request
	`)
	var stats RequestStats
	if err := row.Scan(&stats.Total, &stats.Active, &stats.Failed, &stats.Imported, &stats.WithErrors, &stats.Unsubmitted); err != nil {
		return RequestStats{}, fmt.Errorf("request stats: %w", err)
	}
	return stats, nil
}

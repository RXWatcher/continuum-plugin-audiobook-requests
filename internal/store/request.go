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
	RequestID           string
	ExternalID          string
	Status              string
	SourceID            string
	SearchQuery         string
	SelectedTitle       string
	DetailURL           string
	InfoHash            string
	MagnetURI           string
	SelectedScore       int
	SelectedScoreReason string
	LastPolled          time.Time
	ErrorText           string
	CreatedAt           time.Time
	UpdatedAt           time.Time
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
		extPtr     = stringPtr(r.ExternalID)
		sourcePtr  = stringPtr(r.SourceID)
		queryPtr   = stringPtr(r.SearchQuery)
		titlePtr   = stringPtr(r.SelectedTitle)
		detailPtr  = stringPtr(r.DetailURL)
		hashPtr    = stringPtr(r.InfoHash)
		magnetPtr  = stringPtr(r.MagnetURI)
		reasonPtr  = stringPtr(r.SelectedScoreReason)
		errPtr     = stringPtr(r.ErrorText)
		lastPolled *time.Time
	)
	if !r.LastPolled.IsZero() {
		v := r.LastPolled
		lastPolled = &v
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO forwarded_request (
			request_id, external_id, status, source_id, search_query,
			selected_title, detail_url, info_hash, magnet_uri,
			selected_score, selected_score_reason, last_polled, error_text, updated_at
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
		ON CONFLICT (request_id) DO UPDATE SET
			external_id = COALESCE(EXCLUDED.external_id, forwarded_request.external_id),
			status      = EXCLUDED.status,
			source_id   = COALESCE(EXCLUDED.source_id, forwarded_request.source_id),
			search_query = COALESCE(EXCLUDED.search_query, forwarded_request.search_query),
			selected_title = COALESCE(EXCLUDED.selected_title, forwarded_request.selected_title),
			detail_url = COALESCE(EXCLUDED.detail_url, forwarded_request.detail_url),
			info_hash = COALESCE(EXCLUDED.info_hash, forwarded_request.info_hash),
			magnet_uri = COALESCE(EXCLUDED.magnet_uri, forwarded_request.magnet_uri),
			selected_score = CASE
				WHEN EXCLUDED.selected_score <> 0 THEN EXCLUDED.selected_score
				ELSE forwarded_request.selected_score
			END,
			selected_score_reason = COALESCE(EXCLUDED.selected_score_reason, forwarded_request.selected_score_reason),
			last_polled = COALESCE(EXCLUDED.last_polled, forwarded_request.last_polled),
			error_text  = COALESCE(EXCLUDED.error_text, forwarded_request.error_text),
			updated_at  = EXCLUDED.updated_at
	`, r.RequestID, extPtr, r.Status, sourcePtr, queryPtr, titlePtr, detailPtr,
		hashPtr, magnetPtr, r.SelectedScore, reasonPtr, lastPolled, errPtr, r.UpdatedAt)
	if err != nil {
		return fmt.Errorf("upsert forwarded_request: %w", err)
	}
	return nil
}

func (s *Store) GetForwardedRequest(ctx context.Context, requestID string) (ForwardedRequest, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT request_id, COALESCE(external_id,''), status,
		       COALESCE(source_id,''), COALESCE(search_query,''), COALESCE(selected_title,''),
		       COALESCE(detail_url,''), COALESCE(info_hash,''), COALESCE(magnet_uri,''),
		       selected_score, COALESCE(selected_score_reason,''),
		       COALESCE(last_polled, '0001-01-01 00:00:00'::timestamptz),
		       COALESCE(error_text,''), created_at, updated_at
		FROM forwarded_request WHERE request_id = $1
	`, requestID)
	var r ForwardedRequest
	if err := row.Scan(&r.RequestID, &r.ExternalID, &r.Status,
		&r.SourceID, &r.SearchQuery, &r.SelectedTitle, &r.DetailURL, &r.InfoHash,
		&r.MagnetURI, &r.SelectedScore, &r.SelectedScoreReason, &r.LastPolled,
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
		       COALESCE(source_id,''), COALESCE(search_query,''), COALESCE(selected_title,''),
		       COALESCE(detail_url,''), COALESCE(info_hash,''), COALESCE(magnet_uri,''),
		       selected_score, COALESCE(selected_score_reason,''),
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
		if err := rows.Scan(&r.RequestID, &r.ExternalID, &r.Status,
			&r.SourceID, &r.SearchQuery, &r.SelectedTitle, &r.DetailURL, &r.InfoHash,
			&r.MagnetURI, &r.SelectedScore, &r.SelectedScoreReason, &r.LastPolled,
			&r.ErrorText, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		out = append(out, r)
	}
	return out, nil
}

func (s *Store) ListRecent(ctx context.Context, limit int) ([]ForwardedRequest, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := s.pool.Query(ctx, `
		SELECT request_id, COALESCE(external_id,''), status,
		       COALESCE(source_id,''), COALESCE(search_query,''), COALESCE(selected_title,''),
		       COALESCE(detail_url,''), COALESCE(info_hash,''), COALESCE(magnet_uri,''),
		       selected_score, COALESCE(selected_score_reason,''),
		       COALESCE(last_polled, '0001-01-01 00:00:00'::timestamptz),
		       COALESCE(error_text,''), created_at, updated_at
		FROM forwarded_request
		ORDER BY updated_at DESC
		LIMIT $1
	`, limit)
	if err != nil {
		return nil, fmt.Errorf("list recent: %w", err)
	}
	defer rows.Close()
	var out []ForwardedRequest
	for rows.Next() {
		var r ForwardedRequest
		if err := rows.Scan(&r.RequestID, &r.ExternalID, &r.Status,
			&r.SourceID, &r.SearchQuery, &r.SelectedTitle, &r.DetailURL, &r.InfoHash,
			&r.MagnetURI, &r.SelectedScore, &r.SelectedScoreReason, &r.LastPolled,
			&r.ErrorText, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan recent: %w", err)
		}
		out = append(out, r)
	}
	return out, nil
}

func stringPtr(v string) *string {
	if v == "" {
		return nil
	}
	return &v
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

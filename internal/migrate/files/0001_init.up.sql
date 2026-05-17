CREATE TABLE IF NOT EXISTS forwarded_request (
  request_id    TEXT PRIMARY KEY,
  external_id   TEXT,
  status        TEXT NOT NULL,
  last_polled   TIMESTAMPTZ,
  error_text    TEXT,
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS forwarded_request_status_polled_idx
  ON forwarded_request (status, last_polled);

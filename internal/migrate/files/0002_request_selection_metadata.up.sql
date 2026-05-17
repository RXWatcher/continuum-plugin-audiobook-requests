ALTER TABLE forwarded_request
  ADD COLUMN IF NOT EXISTS source_id TEXT,
  ADD COLUMN IF NOT EXISTS search_query TEXT,
  ADD COLUMN IF NOT EXISTS selected_title TEXT,
  ADD COLUMN IF NOT EXISTS detail_url TEXT,
  ADD COLUMN IF NOT EXISTS info_hash TEXT,
  ADD COLUMN IF NOT EXISTS magnet_uri TEXT,
  ADD COLUMN IF NOT EXISTS selected_score INT NOT NULL DEFAULT 0,
  ADD COLUMN IF NOT EXISTS selected_score_reason TEXT;

CREATE INDEX IF NOT EXISTS forwarded_request_info_hash_idx
  ON forwarded_request (info_hash)
  WHERE info_hash IS NOT NULL AND info_hash <> '';

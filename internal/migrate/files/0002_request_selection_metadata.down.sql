ALTER TABLE forwarded_request
  DROP COLUMN IF EXISTS selected_score_reason,
  DROP COLUMN IF EXISTS selected_score,
  DROP COLUMN IF EXISTS magnet_uri,
  DROP COLUMN IF EXISTS info_hash,
  DROP COLUMN IF EXISTS detail_url,
  DROP COLUMN IF EXISTS selected_title,
  DROP COLUMN IF EXISTS search_query,
  DROP COLUMN IF EXISTS source_id;

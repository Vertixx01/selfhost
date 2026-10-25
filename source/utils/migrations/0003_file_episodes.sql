-- Episode numbers that identify this episode's file inside a multi-file
-- (batch) torrent, comma-separated: the season number and any absolute
-- numbers. Empty = take the largest video file.
ALTER TABLE episodes ADD COLUMN file_episodes TEXT NOT NULL DEFAULT '';

-- The file chosen inside the torrent (path within it). With infohash it
-- identifies the source exactly, for dedup across anime/episode IDs.
ALTER TABLE episodes ADD COLUMN file_path TEXT NOT NULL DEFAULT '';
CREATE INDEX idx_episodes_source ON episodes(infohash, file_path);

-- Episode time of the first fingerprint point (outro windows start near the end).
ALTER TABLE episode_fingerprints ADD COLUMN start_offset REAL NOT NULL DEFAULT 0;

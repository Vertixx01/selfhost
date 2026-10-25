-- Initial schema. Keep in sync with PROJECT.MD §3 "SQLite schema".

-- Job-level aggregate (one row per POST /job or append call)
CREATE TABLE jobs (
    job_id        TEXT PRIMARY KEY,         -- client-generated idempotency key
    anime_id      TEXT NOT NULL,            -- upstream anime ID (MAL, AniDB, etc.)
    title         TEXT NOT NULL DEFAULT '',
    status        TEXT NOT NULL DEFAULT 'pending'
                  CHECK(status IN ('pending','processing','completed','failed')),
    callback_url  TEXT NOT NULL DEFAULT '',
    episode_count INTEGER NOT NULL DEFAULT 0,
    created_at    TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    updated_at    TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);

-- Per-episode pipeline state (one row per episode in the job)
CREATE TABLE episodes (
    job_id        TEXT NOT NULL REFERENCES jobs(job_id),
    ep_id         TEXT NOT NULL,            -- upstream episode ID
    title         TEXT NOT NULL DEFAULT '',
    magnet_url    TEXT NOT NULL,
    infohash      TEXT NOT NULL DEFAULT '', -- resolved from magnet
    priority      INTEGER NOT NULL DEFAULT 0,
    needs_poster  INTEGER NOT NULL DEFAULT 0,
    provider      TEXT NOT NULL DEFAULT '', -- 'animetosho', 'nyaa', etc.
    source_type   TEXT NOT NULL DEFAULT '', -- 'bd' | 'web' | '' (set by analyzer)

    status        TEXT NOT NULL DEFAULT 'queued'
                  CHECK(status IN (
                      'queued','resolving','downloading','analyzing',
                      'extracting','transcoding','packaging',
                      'done','failed'
                  )),
    progress      REAL NOT NULL DEFAULT 0.0,
    error         TEXT NOT NULL DEFAULT '',
    size_bytes    INTEGER NOT NULL DEFAULT 0, -- output package size, set at packaging

    output_path   TEXT NOT NULL DEFAULT '', -- output/{anime_id}/{ep_id}/

    created_at    TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    updated_at    TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),

    PRIMARY KEY (job_id, ep_id)
);

-- Font deduplication cache (shared across all jobs)
CREATE TABLE font_cache (
    hash          TEXT PRIMARY KEY,         -- sha256 hex of font content (first 16 chars)
    filename      TEXT NOT NULL,            -- canonical filename (first seen)
    size_bytes    INTEGER NOT NULL,
    ref_count     INTEGER NOT NULL DEFAULT 1,
    created_at    TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);

-- Which cached fonts each episode uses, under the filename its torrent gave them
CREATE TABLE episode_fonts (
    job_id        TEXT NOT NULL,
    ep_id         TEXT NOT NULL,
    hash          TEXT NOT NULL REFERENCES font_cache(hash),
    filename      TEXT NOT NULL,
    PRIMARY KEY (job_id, ep_id, hash, filename),
    FOREIGN KEY (job_id, ep_id) REFERENCES episodes(job_id, ep_id)
);

-- Chromaprint fingerprints for series-level intro/outro detection
CREATE TABLE episode_fingerprints (
    anime_id      TEXT NOT NULL,
    ep_id         TEXT NOT NULL,
    part          TEXT NOT NULL CHECK(part IN ('intro','outro')),
    fingerprint   BLOB NOT NULL,
    created_at    TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    PRIMARY KEY (anime_id, ep_id, part)
);

CREATE INDEX idx_jobs_anime ON jobs(anime_id);
CREATE INDEX idx_episodes_job ON episodes(job_id);
CREATE INDEX idx_episodes_status ON episodes(status);
CREATE INDEX idx_episodes_priority ON episodes(priority, created_at);
CREATE INDEX idx_episodes_infohash ON episodes(infohash);
CREATE INDEX idx_font_cache_refcount ON font_cache(ref_count);

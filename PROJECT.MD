# torClient — Anime Self-Host Pipeline Plan

## Table of Contents
1. [Architecture Overview](#1-architecture-overview)
2. [Directory & Output Structure](#2-directory--output-structure)
3. [Job Specification](#3-job-specification)
4. [Pipeline Stages](#4-pipeline-stages)
5. [Component Design](#5-component-design)
6. [Configuration](#6-configuration)
7. [Edge Cases & Error Handling](#7-edge-cases--error-handling)
8. [Dependencies](#8-dependencies)
9. [Implementation Order (Phases)](#9-implementation-order-phases)
10. [Releasing Anime Support](#10-releasing-anime-support-ongoing-series)

---

## Current State

**Status: Phases 1–6 + AniList discovery + GPU/hybrid encoding + web UI done; tuned for a CPU-only VPS. Remaining: Phase 7 (Docker, auth, metrics, infohash dedup, retry, batch torrents).**

| Component | State |
|---|---|
| `go.mod` | Module `torclient`, Go 1.25. `godotenv`, `modernc.org/sqlite` v1.50 (newer needs Go 1.26), `anacrolix/torrent` pinned to commit `4bd78e745c` (after v1.61.0): v1.61.0 panics in its web-seed scheduler when a torrent is dropped with web-seed requests in flight (upstream #1112). Move to the next tagged release once there is one. |
| Build | `CGO_ENABLED=0` (as in the Dockerfile). With cgo on, this Windows machine's gcc fails to build `runtime/cgo`. |
| `.env` | Full planned config (§6), including torrent timeouts. |
| `config/` | Typed config loader; reports every bad variable at once; refuses `KEY_DIR` inside `OUTPUT_DIR`. |
| `source/utils/` | SQLite (WAL, FKs, `BEGIN IMMEDIATE`) + embedded migrations, job store, atomic `PopNext`, status rollup, progress/infohash updates, path layout, logger. |
| `source/models/` | Job/Episode structs, status enums, request payloads. |
| `source/api/` | `POST /job`, `POST /api/anime/{anime_id}/episodes`, `GET /job/{job_id}`, `GET /jobs`, `GET /health`, `GET /ready`, `GET /keys/{anime_id}/{ep_id}.key`, `GET /api/anilist/{id}/search`, `POST /api/anilist/{id}/episodes`; with `SERVE_MEDIA=true` also `GET /media/{path}` (output tree, HLS types, range requests, no dot-dirs) and `GET /play`, `/play/{anime_id}/{ep_id}` (hls.js player with quality/audio/subtitle pickers and intro skip). |
| `source/torrent/` | anacrolix engine, per-episode download with watchdog, classic-file-IO re-launch. |
| `source/resolvers/` | Magnet → metadata (retries) → main file selection. |
| `source/intro/` | OP/ED detection: chapters, Chromaprint fingerprints + series correlation, silence/black heuristics, tier merge, `timestamps.json`, boundary-aligned keyframe list. |
| `source/transcode/` | One-run HLS encode (1080p/720p + audio), fast path, CRF auto-adjust, AES-128, ASS→WebVTT with timestamp map, master playlist, output validation. |
| `source/pipeline/` | Stage orchestration with checkpoints/resume, packager (staging + atomic publish), series correlation, callbacks, worker pool, cleanup daemon. |
| `source/discovery/` | AniList-ID torrent discovery ported from anitorrent (GPL-3.0): AnimeTosho, SeaDex, SubsPlease, Nyaa, Yameii, ToonsHub; matching, ranking, auto-pick. |
| `source/media/` | ffprobe → stream map (BD/WEB, fast path, keyframes), subtitle extraction, font extraction, audio mapping. Font cache in `source/utils/fontcache.go`. |
| Tests | `go test ./...` — store, handlers, config, file selection, end-to-end torrent download from a local seeder, media analysis on a generated MKV with real ffmpeg, font cache, intro detection (synthetic + real ffmpeg), transcoding (real ffmpeg, incl. playback of the encrypted package), full pipeline end to end (seeder + ffmpeg + callback server), discovery (rules, fake sites, endpoints; live test opt-in). |
| `Dockerfile`, `docker-compose.yml`, `README.md` | Not written yet (Phase 7). |
| License | GPL-3.0 (`LICENSE`), because `source/discovery/` is ported from anitorrent — see `NOTICE`. |
| Git | Private GitHub repo `Vertixx01/torClient`. `.env` is ignored; `.env.example` is the template. |

**Next:** Phase 7 — Dockerfile + compose, auth in front of `/keys` and the UI, infohash dedup, retry/cancel endpoints, batch-torrent support, metrics.

---

## 1. Architecture Overview

```
                      External Cron / Caller
                            │
                    POST /api/anime/{anime_id}/episodes
                    (single episode, same anime_id)
                            │
                            ▼
                   Job In (HTTP/Queue)
                         |
                    ┌─────┴──────┐
                    │ Scheduler  │  ← priority queue, concurrency control
                    └─────┬──────┘
                          │
               ┌──────────┴──────────┐
               │     Resolver        │  ← magnet → torrent metadata
               └──────────┬──────────┘
                          │
               ┌──────────┴──────────┐
               │    Downloader       │  ← anacrolix/torrent
               └──────────┬──────────┘
                          │
               ┌──────────┴──────────┐
               │  Media Analyzer     │  ← jellyfin-ffprobe → stream map
               └──────────┬──────────┘
                          │
          ┌───────────────┼───────────────┐
          │               │               │
   ┌──────┴──────┐  ┌─────┴──────┐  ┌────┴─────┐
   │ Sub Extract │  │Intro Detect│  │Audio Map │
   └──────┬──────┘  └─────┬──────┘  └────┬─────┘
          │               │               │
          └───────────────┼───────────────┘
                          │
               ┌──────────┴──────────┐
               │   Transcode Engine  │  ← jellyfin-ffmpeg HLS packaging
               └──────────┬──────────┘
                          │
               ┌──────────┴──────────┐
               │    Output Packager  │  ← master.m3u8, rename, organize
               └──────────┬──────────┘
                          │
               ┌──────────┴──────────┐
               │  Callback Notifier  │  ← webhook POST to external cron
               └──────────┬──────────┘
                          │
               ┌──────────┴──────────┐
               │    Cleanup Daemon   │  ← remove work/{job_id}/ after TTL
               └─────────────────────┘
```

**Releasing Anime flow:**
- Initial batch: a complete season (12 eps) submitted as one job → processed → `output/{anime_id}/{ep1..ep12}/`
- Incremental update: weeks later, ep13 releases. An **external cron** (separate service, not part of torClient) POSTs to `POST /api/anime/{anime_id}/episodes` with the new single-episode payload
- torClient processes ep13 and places it at `output/{anime_id}/ep13/` — same series directory, no re-processing of old episodes
- On completion, torClient fires a callback to the external cron so it can update its tracking state
- **Two ways in:** the caller sends magnets (`POST /job`, append endpoint), or sends AniList IDs and lets torClient find the torrents itself (`POST /api/anilist/{id}/episodes`, §3a). The external cron only needs to know which AniList ID / episode to ask for.

**Key principles:**
- Each stage is a discrete step with its own error handling
- Pipeline is sequential per-job, but multiple jobs run concurrently
- **Hybrid state storage:** SQLite for job registry/queue (ACID, queryable, atomic), JSON files in work dirs for per-job analysis data (write-once, human-readable)
- Crash-safe — SQLite WAL mode + atomic writes on JSON
- Decoupled stages allow manual re-run of any failed step
- Concurrency limited by semaphore (MAX_CONCURRENT_TORRENTS, MAX_FFMPEG_WORKERS)

---

## 2. Directory & Output Structure

### Font cache (encode-server local — avoids redundant MKV extraction only)
```
fonts/
  a1b2c3d4e5f67890/              ← sha256[:16] of font file content
    SourceHanSans-Bold.otf        ← one canonical copy; filename from first-seen torrent
  9988776655443322/
    NotoSansCJKjp-Regular.otf
```

Fonts are stored once by content hash on the **encode server only**. During packaging, fonts are **copied** (not symlinked) into the output dir so each `output/{anime_id}/{ep_id}/subs/fonts/` is self-contained. This works naturally with lsyncd — no symlinks to break across storage servers. See [Stage 4: Subtitle Extractor](#stage-4-subtitle-extractor) for the full flow.

### Working directory (during processing — per-episode under job)
```
work/
  {job_id}/
    {ep_id}/
      raw/                          ← torrent download destination
        episode.mkv
      meta/
        media_info.json             ← ffprobe full output
        stream_map.json             ← parsed stream indices (video/audio/sub)
        timestamps.json             ← intro/outro boundaries
        enc.key / enc.keyinfo       ← AES key for ffmpeg (moved to KEY_DIR at packaging)
      subs/                         ← Stage 4 extraction (eng.ass, spa.vtt, ...)
      work/                         ← Stage 7 output, same layout as output/{anime_id}/{ep_id}/
        master.m3u8
        1080p/  720p/
        audio/{rendition}/
        subs/                       ← .vtt + .m3u8 per track, .ass copies
```

### Output directory (final artifact — anime_id/ep_id/ layout)
```
output/
  {anime_id}/
    {ep_id}/
      master.m3u8                ← HLS master playlist (all renditions)
      meta/
        media_info.json
        timestamps.json           ← intro/outro for the frontend skipper
        job.json                  ← original job params
                                  (enc.key is NOT here — see Key store below)
      1080p/
        stream.m3u8               ← contains #EXT-X-KEY referencing key URI
        seg0000.ts                ← AES-128 encrypted MPEG-TS segments (%04d: films run past 999)
        seg0001.ts
        ...
      720p/
        stream.m3u8
        seg0000.ts
        ...
      audio/
        jpn/                      ← rendition name: {lang}, {lang}_2 for a second track
          stream.m3u8
          seg0000.ts              ← MPEG-TS, not raw .aac: TS carries the PTS players
          ...                       need to sync alternate audio with video
        eng/
          stream.m3u8
          seg0000.ts
          ...
      subs/
        eng.ass                   ← original ASS for libass players
        eng.vtt                   ← WebVTT for HLS, with X-TIMESTAMP-MAP
        eng.m3u8                  ← one-segment playlist (HLS renditions must be playlists)
        eng_signs.ass / .vtt / .m3u8
        fonts/                    ← real files copied from global cache
          SourceHanSans-Bold.otf  ← copied from fonts/a1b2c3d4e5f67890/SourceHanSans-Bold.otf
          NotoSansCJKjp-Regular.otf
```

### Key store (outside OUTPUT_DIR — never served with the segments)
```
keys/
  {anime_id}/
    {ep_id}.key                   ← AES-128 key (16 bytes), served only by the key endpoint
```
Keys live under `KEY_DIR`, separate from `OUTPUT_DIR`, so publishing or replicating `output/` never exposes them. See [7f](#7f-hls-encryption-aes-128).

### Master playlist (master.m3u8)
```
#EXTM3U
#EXT-X-VERSION:6
#EXT-X-INDEPENDENT-SEGMENTS
#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="audio",LANGUAGE="ja",NAME="Japanese",DEFAULT=YES,AUTOSELECT=YES,CHANNELS="2",URI="audio/jpn/stream.m3u8"
#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="audio",LANGUAGE="en",NAME="English",DEFAULT=NO,AUTOSELECT=YES,CHANNELS="2",URI="audio/eng/stream.m3u8"
#EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID="subs",LANGUAGE="en",NAME="English",DEFAULT=YES,AUTOSELECT=YES,FORCED=NO,URI="subs/eng.m3u8"
#EXT-X-STREAM-INF:BANDWIDTH=3128000,AVERAGE-BANDWIDTH=2228000,RESOLUTION=1920x1080,FRAME-RATE=23.976,CODECS="avc1.640029,mp4a.40.2",AUDIO="audio",SUBTITLES="subs"
1080p/stream.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=1628000,AVERAGE-BANDWIDTH=1178000,RESOLUTION=1280x720,FRAME-RATE=23.976,CODECS="avc1.640028,mp4a.40.2",AUDIO="audio",SUBTITLES="subs"
720p/stream.m3u8
```
- `LANGUAGE` is BCP 47 (`ja`, not `jpn`); NAME comes from the audio/subtitle map.
- `CODECS`: `avc1.6400{level}` — High profile at 4.1 is `640029`, at 4.0 `640028` (the earlier `64001e` was level 3.0). A fast-path copy uses the source's own profile and level. Audio codecs are listed once each (`mp4a.40.2` LC; `mp4a.40.5` for copied HE-AAC).
- `BANDWIDTH` = peak segment bitrate of the video + the most demanding audio rendition; `AVERAGE-BANDWIDTH` likewise with averages — both measured from the written segments.
- Subtitle renditions point at a playlist (`subs/eng.m3u8`), not at the `.vtt` — HLS requires a media playlist there. Signs tracks are named "English (Signs)" and `FORCED=YES` when the source flagged them forced; default subtitle is English dialogue, else the first dialogue track.

### Encrypted variant playlist (1080p/stream.m3u8)
```
#EXTM3U
#EXT-X-VERSION:6
#EXT-X-TARGETDURATION:6
#EXT-X-ALLOW-CACHE:YES
#EXT-X-PLAYLIST-TYPE:VOD
#EXT-X-KEY:METHOD=AES-128,URI="https://keys.example.com/keys/{anime_id}/{ep_id}.key",IV=0xABCDEF1234567890ABCDEF1234567890
#EXT-X-MEDIA-SEQUENCE:0
#EXTINF:6.000,
seg000.ts
#EXTINF:6.000,
seg001.ts
...
#EXT-X-ENDLIST
```

### Rationale
- `anime_id/ep_id/` flat layout — frontend/media-server can serve directly without a DB lookup for path
- Audio separated by language — HLS alternate renditions, player auto-switches
- Subs provide both ASS (rich format) and WEBVTT (HLS-compatible) so the HLS manifest can reference WEBVTT while the ASS file is available for direct download or libass-based players
- Fonts extracted to a **content-addressed cache** (`fonts/{sha256[:16]}/`) on the encode server to avoid redundant `ffmpeg -dump_attachment` calls per episode. Each font is extracted once from MKV, period. During packaging, fonts are **copied** (real files, not symlinks) into the output dir so `output/{anime_id}/{ep_id}/` is fully self-contained and survives lsyncd replication to storage servers. Reference counting in SQLite enables safe cache cleanup. The dedup win is CPU time (no re-extraction), not storage space — font files are small enough that copying is fine.
- Meta dir keeps sidecar metadata without cluttering media paths

---

## 3. Job Specification

### Design Rationale

A **job** = one anime submission containing N episodes, each with its own magnet URL. Episodes are fully independent (different torrents, different files) and process concurrently. The job is the aggregate — it tracks overall status, while each episode has its own pipeline lifecycle. This replaces the old batch model (one torrent, multiple files) which was a poor fit for modern releases where each episode has its own magnet on Nyaa/animeTosho.

**Why this model wins:**
- Each episode downloads independently → no shared-torrent coordination, no O_EXCL races
- Episodes process in parallel up to MAX_CONCURRENT_TORRENTS — a 12-episode series finishes in ~1× the time of a single episode (not 12×)
- Per-episode failure is isolated — one bad torrent doesn't block the rest
- The caller sends the complete anime payload once; we fan out internally
- The info endpoint returns per-episode progress and status, enabling live UI updates

### Input (received via HTTP POST)

```json
{
  "job_id": "someid",
  "anime_id": "ourId",
  "title": "animeTitle",
  "status": "pending",
  "callback_url": "https://next-service/webhook/complete",
  "episodes": [
    {
      "ep_id": "episodeId",
      "title": "Episode Title",
      "magnet_url": "magnet:?xt=urn:btih:...",
      "priority": 1,
      "needs_poster": true,
      "provider": "animetosho"
    }
  ]
}
```

| Field | Description |
|---|---|
| `job_id` | Client-generated unique ID (e.g. uuid-v7). Serves as idempotency key — re-POST with same job_id silently succeeds (no-op). |
| `anime_id` | The upstream system's anime identifier (MyAnimeList, AniDB, etc.). Used to group episodes for series-level intro detection. |
| `title` | Human-readable anime title. |
| `status` | Always `"pending"` on input (reserved for future use). |
| `callback_url` | Optional webhook called when **all** episodes finish. |
| `episodes[]` | Array of episodes to process. Each has its own magnet_url, priority, metadata. |

### 3a. AniList discovery (torClient finds the torrents)

Ported from [anitorrent](https://github.com/anh9000/anitorrent) (Hayase extension pack, GPL-3.0 — see `NOTICE`). Code: `source/discovery/`, endpoints in `source/api/anilist.go`.

**`GET /api/anilist/{anilist_id}/search?episode=N[&dub=1]`** — ranked candidates + the pick, no job created. `episode` is omitted for movies.

**`POST /api/anilist/{anilist_id}/episodes`** — discover, then submit as one job (same path as `POST /job`):
```json
{ "job_id": "uuid", "episodes": [1, 2, 3], "anime_id": "optional (default: the AniList ID)",
  "callback_url": "https://...", "priority": 0, "dub": false }
```
- `episodes` omitted (or `[1]`) for a movie / single-episode show; `ep_id` is `ep{N}` (or `movie`).
- Episodes with no suitable release are left out and reported (`discovery[]` in the response, with the reason); if none is found → 404 and no job.
- Re-POST with the same `job_id` → 200 with the existing job, no new search. In-flight duplicates → 409 as for `POST /job`.

**How a search works**
1. AniList (`graphql.anilist.co`): titles (romaji, english, synonyms, native), episode count, format, start year; plus the **prequel chain** (cached 6h) which yields absolute-number candidates (S2E05 of a 28-episode show may be released as `- 33`), "not aired yet", and the chain season number.
2. All sources in parallel (45s each):

| Source | How | Notes |
|---|---|---|
| `animetosho` | AniList→AniDB mapping (anitorrent's `anilist-to-anidb.json`, cached 24h) → `?aid=`; text search fallback | Tries `feed.animetosho.org/json`, then `animetosho.org/feed/json` (some ISPs block the `feed.` host) |
| `seadex` | `releases.moe` by AniList ID | Curated best/alt; mostly batches |
| `subsplease` | `subsplease.org/api` search, `latest` fallback | Airing shows; no seeder counts |
| `nyaa` | nyaa.si RSS text search, numbered queries if needed | Browser UA (DDoS-Guard); 429 → 1.2/3/6s backoff |
| `yameii` | nyaa.si RSS, uploader `Yameii` | English dubs |
| `toonshub` | nyaa.si RSS, `[ToonsHub]` titles only | |
3. Each source classifies titles (anitorrent's rules): show tokens, season markers (S2 / 2nd Season / II / trailing digit), years, episode markers (`- 05`, `S01E05`, `EP05`, `[05]`), batches (ranges, "complete", season-only). Tier A = exact episode, B = batch covering it, C = weak. JS lookaheads became explicit checks (Go's RE2 has none) — each is covered by tests.
4. Results are **merged by infohash** across sources (max seeders, best accuracy, all source names) and **ranked**.

**Auto-pick** — only exact single-episode (or single-file movie) releases with known-alive seeders are pickable; batches are listed but not picked (the pipeline takes the largest video in a torrent, so a batch would give the wrong episode). Order: preferred resolution (`DISCOVERY_RESOLUTION`, else nearest below) → `DISCOVERY_PREFERRED_GROUPS` → dub preference (Yameii first only with `dub`) → seeders in buckets (0/1–4/5–19/20–99/100+) → accuracy (ID lookup > text) / SeaDex best → trusted, not remake → seeders → newest. `DISCOVERY_EXCLUDE` substrings are never picked.

**Verified live** (read-only searches): Frieren S1 ep 5 → SubsPlease 1080p (384 seeders, 3 sources agree); Frieren 2nd Season ep 3/5 → `S2 - 03` / `S02E05`, not S1; Suzume (movie) → movie release; One Piece 1180 → SubsPlease 1080p; One Piece 1181 (not aired) → nothing pickable.

**Limits:** batch torrents aren't auto-picked yet (needs per-episode file selection in the resolver and sharing one torrent between episodes); results depend on third-party sites and anitorrent's mapping file staying available.

### Append endpoint (POST /api/anime/{anime_id}/episodes)

For releasing/ongoing anime. Instead of submitting a full job, append one or more new episodes to an existing `anime_id`. This endpoint is called by the **external cron** (or any caller) to push new episodes as they release.

```json
{
  "job_id": "newJobId",
  "episodes": [
    {
      "ep_id": "S01E13",
      "title": "Episode 13",
      "magnet_url": "magnet:?xt=urn:btih:...",
      "priority": 1,
      "provider": "animetosho"
    }
  ],
  "callback_url": "https://external-cron/webhook/complete"
}
```

**Behavior:**
- Episodes are INSERTed into a NEW job row (with the given `job_id`), reusing the existing `anime_id`
- Output goes to `output/{existing_anime_id}/S01E13/` — same series directory, alongside previous episodes
- The new episode's audio fingerprint is added to this `anime_id`'s stored fingerprints (improves future intro/outro detection)
- If `ep_id` already exists for this `anime_id` (cache hit), the episode is immediately marked `done` — no re-download
- Response: returns job status (same shape as POST /job)
- The new job is tracked independently — the caller can poll GET /job/{newJobId} for progress, or wait for the callback

**ep_id convention:**
The external caller provides `ep_id` in whatever convention it uses. torClient stores it as-is. Common conventions:

| Type | Convention | Example |
|---|---|---|
| TV series | `S{season}E{episode}` | `S01E13` |
| TV series (absolute) | `ep{number}` | `ep13` |
| OVA / Movie | `{anime_id}-{part}` | `movie-01` |

### Info endpoint response (GET /job/{job_id})

Returns the same shape but with per-episode progress, status, and size. Used by the frontend/caller to poll live state.

```json
{
  "job_id": "someid",
  "anime_id": "ourId",
  "title": "animeTitle",
  "status": "processing",
  "episodes": [
    {
      "ep_id": "episodeId",
      "title": "Episode Title",
      "magnet_url": "magnet:?xt=urn:btih:...",
      "priority": 1,
      "needs_poster": true,
      "provider": "animetosho",
      "progress": 45.0,
      "status": "transcoding",
      "size_bytes": 524288000,
      "output_path": "/output/ourId/episodeId"
    }
  ]
}
```

| Field | Description |
|---|---|
| `job_id` | Same as input. |
| `status` | Job-level aggregate: `"pending"` → `"processing"` → `"completed"` / `"failed"`. Computed from episode statuses: all done → completed, any failed → failed, else processing. |
| `episodes[].progress` | 0.0–100.0 float. Updated after each pipeline stage. |
| `episodes[].status` | Per-episode pipeline stage: `queued`, `resolving`, `downloading`, `analyzing`, `extracting`, `transcoding`, `packaging`, `done`, `failed`. |
| `episodes[].size_bytes` | Total size of the output HLS package (all renditions + audio + subs) in bytes. |
| `episodes[].output_path` | Relative path to the finished output dir (`output/{anime_id}/{ep_id}/`). |

### SQLite schema

Two tables: `jobs` (aggregate metadata) and `episodes` (per-episode pipeline state).

```sql
-- Job-level aggregate (one row per POST /job)
CREATE TABLE jobs (
    job_id        TEXT PRIMARY KEY,         -- client-generated idempotency key
    anime_id      TEXT NOT NULL,            -- upstream anime ID (MAL, AniDB, etc.)
    title         TEXT NOT NULL DEFAULT '',
    status        TEXT NOT NULL DEFAULT 'pending'
                  CHECK(status IN ('pending','processing','completed','failed')),
    callback_url  TEXT DEFAULT '',
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
    infohash      TEXT DEFAULT '',          -- resolved from magnet
    priority      INTEGER NOT NULL DEFAULT 0,
    needs_poster  INTEGER DEFAULT 0,
    provider      TEXT DEFAULT '',          -- 'animetosho', 'nyaa', etc.
    source_type   TEXT DEFAULT '',          -- 'bd' | 'web' | '' (set by analyzer)

    status        TEXT NOT NULL DEFAULT 'queued'
                  CHECK(status IN (
                      'queued','resolving','downloading','analyzing',
                      'extracting','transcoding','packaging',
                      'done','failed'
                  )),
    progress      REAL DEFAULT 0.0,
    error         TEXT DEFAULT '',
    size_bytes    INTEGER DEFAULT 0,        -- output package size, set at packaging

    output_path   TEXT DEFAULT '',          -- output/{anime_id}/{ep_id}/

    created_at    TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    updated_at    TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),

    PRIMARY KEY (job_id, ep_id)
);

-- Font deduplication cache (shared across all jobs)
CREATE TABLE font_cache (
    hash          TEXT PRIMARY KEY,         -- sha256 hex of font content (first 16 chars)
    filename      TEXT NOT NULL,            -- original font filename
    size_bytes    INTEGER NOT NULL,
    ref_count     INTEGER NOT NULL DEFAULT 1,
    created_at    TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);

-- Which cached fonts each episode uses, under the filename its torrent gave them.
-- Written in Stage 4, read in Stage 8 (packaging), deleted by the cleanup daemon
-- (which decrements font_cache.ref_count for each row it removes).
CREATE TABLE episode_fonts (
    job_id        TEXT NOT NULL,
    ep_id         TEXT NOT NULL,
    hash          TEXT NOT NULL REFERENCES font_cache(hash),
    filename      TEXT NOT NULL,            -- original filename in this episode's MKV
    PRIMARY KEY (job_id, ep_id, hash, filename),
    FOREIGN KEY (job_id, ep_id) REFERENCES episodes(job_id, ep_id)
);

-- Chromaprint fingerprints for series-level intro/outro detection (Stage 6, tier 2).
-- Kept after work dirs are cleaned so later episodes can still be compared.
CREATE TABLE episode_fingerprints (
    anime_id      TEXT NOT NULL,
    ep_id         TEXT NOT NULL,
    part          TEXT NOT NULL CHECK(part IN ('intro','outro')),
    fingerprint   BLOB NOT NULL,            -- raw 32-bit chromaprint points
    created_at    TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    PRIMARY KEY (anime_id, ep_id, part)
);

-- Job-level status rollup is kept in sync by app logic (see below)
CREATE INDEX idx_jobs_anime ON jobs(anime_id);
CREATE INDEX idx_episodes_job ON episodes(job_id);
CREATE INDEX idx_episodes_status ON episodes(status);
CREATE INDEX idx_episodes_priority ON episodes(priority, created_at);
CREATE INDEX idx_episodes_infohash ON episodes(infohash);
CREATE INDEX idx_font_cache_refcount ON font_cache(ref_count);
```

### Per-episode state machine

```
queued → resolving → downloading → analyzing → extracting → transcoding → packaging → done
                          (any state can → failed)
```

Each stage is stored as `episodes.status`. On crash recovery, each episode resumes from its last checkpointed stage.

### Job-level status rollup

| Rule | Job status |
|---|---|
| All episodes `done` | `completed` |
| Any episode `failed` | `failed` |
| Otherwise | `processing` |

The job transitions from `pending` → `processing` when the first episode is claimed by a worker.

### Per-episode atomic claim

Workers claim a single episode atomically (no two workers can grab the same row):

```sql
UPDATE episodes SET status='resolving', updated_at=datetime('now')
WHERE (job_id, ep_id) = (
  SELECT job_id, ep_id FROM episodes
  WHERE status='queued'
  ORDER BY priority, created_at
  LIMIT 1
)
RETURNING *;
```

### Callback payload (POST to callback_url)

Sent when **all** episodes in a job reach terminal state (`done` or `failed`).

```json
{
  "status": "completed",
  "job_id": "someid",
  "anime_id": "ourId",
  "title": "animeTitle",
  "episodes": [
    {
      "ep_id": "episodeId",
      "status": "done",
      "output_path": "/output/ourId/episodeId",
      "master_playlist": "/output/ourId/episodeId/master.m3u8",
      "size_bytes": 524288000,
      "error": "",
      "timestamps": {
        "intro": { "start": 85.5, "end": 115.0, "confidence": 1.0, "source": "chapter" },
        "outro": { "start": 1250.0, "end": 1280.0, "confidence": 1.0, "source": "fingerprint" },
        "detected": true
      }
    }
  ]
}
```

---

## 4. Pipeline Stages

### Stage 0: Scheduler / Queue
- Receive job via HTTP endpoint (`POST /job`)
- **Validate:**
  - `job_id` format, `anime_id` present, `episodes` array non-empty
  - Each episode: `ep_id`, `magnet_url` required
  - Magnet URL format validation (must start with `magnet:?xt=urn:btih:`)
  - `ep_id` unique within the payload — duplicates → 400
- **Idempotency:** if `job_id` already exists in SQLite, return 200 with existing state (no-op).
- **In-flight duplicate:** if any `ep_id` is already queued or processing for the same `anime_id` under another job, return 409 with the conflicting `ep_id`s. Nothing is inserted. (Finished duplicates are not a conflict — they become cache hits, below.)
- **INSERT job + episodes atomically:** wrap job row + N episode rows in a single SQLite transaction; the idempotency and in-flight checks run inside the same transaction.
- **Cache hit check (per episode):** if `output/{anime_id}/{ep_id}/master.m3u8` exists and is valid, that episode is immediately marked `done`. Cache-hit episodes skip the pipeline entirely.
- **Infohash dedup (per episode):** after magnet resolution, if the infohash matches a previously processed episode, recreate the directory tree of `output/{old_anime_id}/{old_ep_id}/` at `output/{new_anime_id}/{new_ep_id}/` and hardlink each file into it (directories can't be hardlinked). If the paths are on different filesystems, copy instead. The encryption key is shared with the old episode (§7f). No download.
- **Per-episode atomic claim:** workers claim a single episode, not a whole job:
  ```sql
  UPDATE episodes SET status='resolving', updated_at=datetime('now')
  WHERE (job_id, ep_id) = (
    SELECT job_id, ep_id FROM episodes
    WHERE status='queued'
    ORDER BY priority, created_at
    LIMIT 1
  )
  RETURNING *;
  ```
  SQLite `RETURNING` clause (≥3.35.0) makes this atomic — no two workers can claim the same episode.
- **Stage-level checkpoint:** after each pipeline stage, `UPDATE episodes SET status=?`. On crash restart, each episode resumes from its last checkpointed stage.
- **Job status rollup:** when an episode reaches terminal state (`done` / `failed`), check all siblings. If all `done` → set `jobs.status = 'completed'`. If any `failed` → `jobs.status = 'failed'`. Otherwise leave as `'processing'`.
- Respect `MAX_CONCURRENT_TORRENTS` and `MAX_FFMPEG_WORKERS` semaphores — each episode acquires a torrent slot during download, a ffmpeg slot during transcode. Since episodes are independent, up to MAX_CONCURRENT_TORRENTS can download simultaneously.
- On shutdown: stop claiming, give in-flight episodes a grace period to reach their next checkpoint, then exit. `queued` episodes stay `queued`; in-flight ones keep their last checkpointed status.
- On startup: episodes left in a non-terminal, non-`queued` status (claimed before a crash or shutdown) are handed back to workers ahead of new claims and resume from their checkpointed stage.

### Stage 1: Resolver
- Input: magnet URL
- Add magnet to anacrolix/torrent client, fetch metadata (InfoHashes, Name, Files list)
- **Per-episode (simplified):** each episode has its own magnet_url, so no batch scanning needed. The torrent for this episode will contain one or more MKV files. The resolver:
  - Scans all files in the torrent
  - Filters to video files > 50MB (excludes samples/ extras). MKV is preferred; `.mp4`/`.m4v`/`.avi` are taken only when the torrent has no MKV (Stage 3 lets ffprobe decide if they're usable)
  - **Sample filtering:** exclude files matching: `[Ss]ample`, `sample_`, `_sample`, `< 50MB`, in `Sample/` dir
  - Picks the **largest MKV** as the main episode file (some releases include extras/omakes)
  - Logs and ignores any additional MKVs found (they're extras, not episodes — each ep has its own magnet)
- Set `infohash` on the episode row for dedup tracking
- Validate at least one valid MKV exists — fail episode if torrent has no playable content
- **Magnet without trackers:** DHT + PEX are enabled so trackerless magnets still resolve. (anacrolix/torrent has no LPD support, so LPD is not available.) `x.pe` peer hints in the magnet are honoured.
- **Metadata cache:** after metadata arrives, the `.torrent` is saved to `TORRENT_DIR/metainfo/{infohash}.torrent`; re-adding the same infohash later (restart, retry) skips the metadata exchange.
- **Same infohash in two episodes at once:** rejected (`ErrBusy`) — one anacrolix torrent can't have two storage dirs. Cross-episode infohash dedup is Phase 7.
- Output: file path within torrent, file index, torrent metadata, infohash
- **Edge case: magnet has no metadata (metadata exchange timeout)** → retry with backoff, fail after 3 attempts
- **Edge case: torrent contains multiple MKVs** → pick the largest (main episode), log the rest as skipped extras

### Stage 2: Downloader
- Input: torrent handle + file index to download
- **Per-episode (simplified):** each episode owns its magnet & downloads independently. No shared-torrent coordination needed.
  - Use anacrolix/torrent, download only the selected file index
  - Save to `work/{job_id}/{ep_id}/raw/episode.mkv`
- **Stale download watchdog:** the download loop samples progress every 2s and drops the torrent if no progress for `STALL_TIMEOUT_MINUTES` (5), no peers and no data for `NO_PEERS_TIMEOUT_SECONDS` (120), or total time exceeds `DOWNLOAD_TIMEOUT_MINUTES` (30 — raise it for big BD releases). Prevents resource leaks from hanging torrents.
- **Torrent state persistence:** each episode's raw dir has its own piece-completion db (bolt), plus the metainfo cache, so a partial download resumes after restart.
- **Completion:** done when every piece overlapping the file is hash-verified (not just written). Then the torrent is dropped, the file is checked (expected length, > 1MB), moved to `raw/episode.mkv`, and the rest of `raw/` (other files' boundary pieces, the completion db) is deleted. If `raw/episode.mkv` already has the right length, the download is skipped.
- **File IO:** anacrolix must run with `TORRENT_STORAGE_DEFAULT_FILE_IO=classic`. Its default mmap IO never unmaps files, so on Windows the episode can't be moved, and on Linux deleted episodes keep using disk until the process exits. The variable is read before `main()`, so the binary re-launches itself with it set if missing; the Docker image sets it directly.
- **Edge case: torrent has no seeders** → fail after 2-minute connect timeout, force-drop handle
- **Edge case: partial file** → check sha1 hash of completed pieces via anacrolix's built-in verification
- **Edge case: download stalls** → watchdog fires, force-drop, fail episode

### Stage 3: Media Analyzer
- Input: `work/{job_id}/{ep_id}/raw/episode.mkv`
- Run `ffprobe -v quiet -print_format json -show_streams -show_format -show_chapters episode.mkv`
- **Validate ffprobe succeeds (exit 0 + valid JSON):** if ffprobe fails, the MKV is corrupt. Fail with clear error.
- Parse output into stream_map.json:
  - Video stream: index, codec, resolution, bitrate, FPS, pixel format, bit depth
  - Audio streams: index, codec, language, channels, bitrate, layout, track title
  - Subtitle streams: index, codec, language, forced flag
  - Attachments: fonts and other embedded files
  - **Chapters:** start/end times, chapter names
- **Determine BD vs WEB definitively:**
  - BD: video codec usually AVC/high, bitrate > 4000k, lossless audio tracks present
  - WEB: video codec AVC **or HEVC**, bitrate < 4000k, audio always lossy (AAC/Opus)
  - Mixed: check source_type from resolver stage, confirm or override
- **HEVC/x265 source detection:** if video codec is hevc, note it. Transcode will decode HEVC → encode H.264. Warn about slower decode.
- **Non-MKV containers:** if not .mkv (e.g., .mp4, .avi), still process if ffprobe can read it. Rename to episode.mkv regardless.
- Check for 10-bit video (pix_fmt yuv420p10le) → warn, convert to 8-bit during transcode
- **Fast-path eligibility check:** if the video is already 1080p (≤1920 wide) H.264 8-bit yuv420p at a constant frame rate, and its largest keyframe gap is ≤ 10s, mark as fast-path eligible (stream copy video, no re-encode). Audio and subtitles don't matter here — they're handled by their own stages either way. Saves hours per episode for WEB releases. Keyframe times come from packet flags (`ffprobe -select_streams {v} -show_entries packet=pts_time,flags`, demux only, no decode) and are recorded in `stream_map.json` for segment alignment (§7c). `fast_path_reason` says why a source isn't eligible.
- **VFR detection:** `r_frame_rate` and `avg_frame_rate` more than 1% apart.
- **Video bitrate:** stream `bit_rate`, else Matroska's `BPS` statistics tag, else container bitrate minus audio. BD if any lossless audio track or video > 4000 kbit/s, otherwise WEB.
- Rename the downloaded file to `episode.mkv` so paths are deterministic
- Output: `work/{job_id}/{ep_id}/meta/media_info.json`, `work/{job_id}/{ep_id}/meta/stream_map.json`

### Stage 4: Subtitle Extractor
- Input: stream_map.json
- For each subtitle stream:
  - **PGS/HDMV PGS:** Skip the track (bitmap subs can't be repackaged for HLS). If the episode has subtitle streams and **all** of them are PGS, fail the episode. Episodes with no subtitles at all are fine.
  - **ASS/SSA:** copy to `work/{job_id}/{ep_id}/subs/{lang}.ass`
  - **WebVTT:** copy to `.../subs/{lang}.vtt`
  - **SRT and other text formats:** convert to `.../subs/{lang}.vtt`
  - All tracks come out of **one** ffmpeg pass (`-map 0:{index} -c:s copy|webvtt` per output) so a 3GB file is read once; only if that fails is each track retried alone, so one bad track doesn't lose the rest.
  - **`-copyts` is required:** without it ffmpeg shifts cues by the container's start offset (often a negative audio start, e.g. -23 ms), putting subs out of sync with the video.
  - Result list goes to `meta/subs.json` (file, lang, kind, forced, coverage per track).
  - **Encrypted subtitles:** if ffmpeg fails with "encrypted" or "decryption" error, try mkvextract. If that also fails, skip subs with warning (don't fail the job — video is fine).
- **Multi-track subs per language:** some releases split dialogue and signs. If one language has several tracks, name them `{lang}_dialogue.{ext}` / `{lang}_signs.{ext}` (`_2`, `_3` for further tracks of the same kind). A track is signs if its title says so (Signs, Songs, S&S, Forced) or, when the language has several tracks, its cues cover < 30% of the episode (the union of cue intervals, read from the extracted file). A language with one track is just `{lang}.{ext}`.

#### Font extraction with content-addressed dedup

Fonts in anime MKVs are heavily duplicated across episodes (same font embedded in every file). Naive extraction produces 12× copies of the same `SourceHanSans-Bold.otf`. We use a **global content-addressed cache** with reference counting.

**Extraction flow per episode:**
```
1. Extract all fonts to temp dir  →  work/{job_id}/{ep_id}/.fonts_temp/
2. For each font file:
   a. sha256sum → first 16 hex chars as hash key
   b. Query font_cache table: hash exists?
      │
      ├── YES → increment ref_count
      │          Delete temp file (canonical copy already in cache)
      │
      └── NO  → copy file to  fonts/{hash}/{filename}
                 INSERT into font_cache (hash, filename, size_bytes, ref_count=1)
   (rows with ref_count 0 are deleted together with their dir, so "exists" always
    means the canonical file is on disk)
3. INSERT (job_id, ep_id, hash, original_filename) into episode_fonts
   — same transaction as the ref_count change
```

**Filename tracking:** the `original_filename` preserves whatever name the torrent used for this font. Same font (same hash) with different names across releases — e.g. `SourceHanSans-Bold.otf` vs `SHS_Bold.otf` — are the same cached file but each episode's output keeps its own filename. `episode_fonts` stores `(hash, filename)` pairs per episode so packaging can look up which filename to use.

**Packaging (Stage 8):** reads the episode's font list, then for each font:
```
fonts/{hash}/{canonical_filename}  ──copy──→  output/{anime_id}/{ep_id}/subs/fonts/{original_filename}
```
Real file copy. No symlinks. Works with lsyncd across storage servers.

**Extraction methods (tried in order):**
1. `ffmpeg -dump_attachment:t "" -i episode.mkv` — preferred (fast, no external tool)
2. `mkvextract attachments episode.mkv` — fallback if ffmpeg fails
3. If both fail: log warning, proceed with system fonts only

**Font cache directory:**
```
fonts/
  a1b2c3d4e5f67890/              ← sha256 prefix
    SourceHanSans-Bold.otf        ← one canonical copy ever
  9988776655443322/
    NotoSansCJKjp-Regular.otf
```

**Reference counting cleanup:**
- When an episode's work dir is cleaned up (after TTL), decrement `ref_count` for each font it references
- When `ref_count` hits 0: delete `fonts/{hash}/` directory and row from `font_cache`
- Cleanup daemon runs this as part of its work dir sweep (see Stage 11)

**System font optimization (future):**
- Maintain a known-good hash list for fonts that ship with the Docker image (Noto, Source Han variants)
- These fonts are pre-installed — skip extraction entirely when hash matches a system font
- Reduces storage to near-zero for common releases
- **Edge case: ASS subtitles with no embedded fonts** → no problem, renderer uses system fonts
- **Edge case: forced subtitles** → tag in metadata, mark as default in HLS
- **Edge case: no subtitles at all** → skip, continue without subs
- **Edge case: no fonts found in MKV** → skip, ASS renderer falls back to system fonts

### Stage 5: Audio Mapper
- Input: stream_map.json
- For each audio stream:
  - **Determine language (robust heuristics):**
    1. Use stream metadata: `tags['language']` — but watch for common wrong values
    2. Check `tags['title']` for patterns: "Japanese", "日本語", "JPN", "English", "ENG", "Dub"
    3. Check stream index order: first audio is usually JPN in anime releases
    4. If all heuristics fail, tag as "und"
  - **Track title check for duplicates:** if two JPN tracks exist, check `tags['title']` for "Commentary", "Audio Commentary". Skip commentary tracks unless explicitly requested.
  - **Channel preference for same language:** prefer 5.1 over 2.0 for main audio (if JPN 5.1 + JPN 2.0, discard the 2.0 unless it's a different mix — check title for "Downmix", "Stereo")
  - Determine codec type:
    - **AAC:** stream copy
    - **Everything else** (FLAC, TrueHD, DTS, MLP, PCM, ALAC, and also AC3, EAC3, Opus, MP3): re-encode to AAC 128k. Audio segments are MPEG-TS (§7d); AAC is the only codec browsers reliably play there.
  - **Language order:** a language named in the track title wins over the tag (titles are written by people, tags by muxers that often default wrongly), then the tag, then "first track = jpn", else `und`.
  - **Renditions:** output dir `audio/{lang}/`, `audio/{lang}_2/` for a second track of the same language; HLS NAME from the language ("Japanese", "English 2").
  - Select default audio based on lang_priority (jpn first if available)
  - Output: `meta/audio_map.json` describing each track's disposition (kept tracks with action/rendition/default, skipped tracks with reason)
  - No audio streams, or none left after filtering → fail the episode

### Stage 6: Intro/Outro Detector
- Input: `episode.mkv` + stream_map.json + chapters + `anime_id` (for series-level detection)

This stage uses a **priority chain** — the most reliable method wins, with fallbacks for when no method produces a result.

#### Priority chain

```
1. Chapter detection (per-episode, frame-accurate)          ← 100% confidence
2. Audio fingerprint (series-level, episode correlation)     ← 95% confidence
3. Silence + black detection (per-episode, heuristic)        ← 70% confidence
```

#### Tier 1: Chapter-based detection (highest priority)
- Parse ffprobe chapter output for markers: "OP", "Opening", "オープニング", "ED", "Ending", "エンディング", "A Part", "B Part", "Intro", "Opening Theme", "Ending Theme"
- Chapter boundaries are frame-accurate — 100% confidence when found
- Also check for "Next Episode", "Preview", "次回" chapters to refine outro end
- If chapter markers found → emit timestamps immediately, skip remaining tiers
- Output source: `"chapter"`

#### Tier 2: Audio fingerprint correlation (series-level fallback)
- **Not an external tool.** The Jellyfin "Intro Skipper" plugin is a C# plugin that runs inside Jellyfin, so torClient can't call it. We implement the same technique in Go: Chromaprint fingerprints of each episode's audio, then find the longest matching run between episodes of the same series. Fingerprints come from jellyfin-ffmpeg's built-in chromaprint muxer (verify the build has it with `ffmpeg -muxers | grep chromaprint`), so no extra binary is needed:
  `ffmpeg -ss {offset} -t 600 -i episode.mkv -map 0:{audio_idx} -ac 1 -f chromaprint -fp_format raw -`
  (offset 0 for the intro; `max(0, duration-600)` for the outro, stored as `start_offset` so outro matches map back to episode time). One point = 1365.33 samples at Chromaprint's 11025 Hz = 0.1238s (measured 0.1237s).
- **Fingerprint per episode, during Stage 6:** compute it while the MKV is still on disk and save it to `work/{job_id}/{ep_id}/meta/fingerprint.bin` **and** the `episode_fingerprints` table (raw MKVs are deleted at packaging, and work dirs at TTL, so later comparisons need the stored copy).
- **Coordination:** after an episode is fingerprinted, if ≥ `FINGERPRINT_MIN_EPISODES` (default 3) episodes of the same `anime_id` have fingerprints, queue a correlation task for that `anime_id`.
- **Correlation task:**
  - Loads all stored fingerprints for this `anime_id` (across jobs, including old ones)
  - Compares pairs: candidate alignments from exactly-equal points (inverted index, top 8 shifts ±1), then the longest run where points differ by ≤ 6 of 32 bits, bridging unmatched gaps up to 3.5s; each episode keeps its longest run (15–150s intro, 15–200s outro)
  - **Precision ≈ ±2s:** each point summarizes ~2.2s of audio, so runs start ~0.5s early and end ~1.5s late (measured). Long silence/stationary noise fingerprints almost identically and can stretch a run. Snapping edges to stored silence/black gaps is the obvious refinement (not done).
  - Writes per-episode boundaries to `output/{anime_id}/{ep_id}/meta/timestamps.json` — but only for episodes that have no chapter result (tier 1 always wins)
- **Timing:** correlation often finishes after an episode is already transcoded (it needs other episodes). That's fine for the skip metadata, but those boundaries won't get `-force_key_frames` alignment. Only tier-1 (chapter) and tier-3 results, known before transcode, are keyframe-aligned.
- **Cache:** results stored in SQLite so repeated runs for the same anime_id are instant
- **Race safety:** if a new episode is fingerprinted while a correlation task runs for that anime_id, mark it pending — the next sweep picks it up
- Output source: `"fingerprint"`

#### Tier 3: Audio + Video heuristics (last resort)
- **One decode per window:** a single ffmpeg run over the first and last `min(360s, duration/2)` (windows never cross the midpoint, so a short episode's outro search can't find the OP; the outro search also starts after a found intro) with two null outputs:
  - default audio track (from the audio map) → `-ac 2 -af silencedetect=noise=-30dB:d=0.5` (d=0.5 so 1.5s gaps are caught; `d=2.0` would miss them)
  - video → `scale=160:-2,blackdetect=d=0.3:pic_th=0.98` (downscaled first; it's most of the cost)
- **Fusion:** a silence and a black stretch that overlap (±0.5s) become one strong gap. The file's start/end count as weak gaps so an OP at 0:00 or an ED to the last frame can still be bracketed.
- **Pick:** the pair of gaps whose inner span is 80–100s (TV OP/ED length), scored by evidence strength minus distance from 90s.
- **Confidence:** both ends confirmed by silence and black → 0.7; one end → 0.6; otherwise 0.5 (also the no-audio case)
- Output source: `"silence"`, `"blackdetect"` or `"silence+blackdetect"`

#### Keyframe alignment
- `intro.KeyframeTimes` builds an explicit `-force_key_frames` time list (replacing `expr:gte(t,n_forced*6)`): a 6s grid, re-anchored at each boundary. The HLS muxer cuts at the first keyframe ≥ 6s after a segment's start, so a keyframe alone isn't enough — no grid point may sit within 6s before a boundary. Result: every boundary starts a segment, the segment before it is 6–12s long, and both renditions get the same list. Boundaries < 6s after the previous one are dropped.
- Only boundaries known before transcode (chapters, heuristics) get this; fingerprint results arrive later (see tier 2 timing).

#### Output: `work/{job_id}/{ep_id}/meta/timestamps.json`
```json
{
  "intro": { "start": 85.5, "end": 115.0, "confidence": 1.0, "source": "chapter" },
  "outro": { "start": 1250.0, "end": 1280.0, "confidence": 1.0, "source": "fingerprint" },
  "detected": true
}
```

- **Edge case: OP/ED not detected** → set `detected: false`, transcode handles full episode
- **Edge case: episode starts mid-OP (recap)** → silence detection still finds transition to main
- **Edge case: no audio** → fall back to blackdetect only, lower confidence
- **Edge case: fewer than 3 fingerprinted episodes for the anime_id** → tier 2 is skipped, falls to tier 3
- Process BEFORE transcode (MKV is more reliable than segmented output for audio analysis)

### Stage 7: Transcode Engine
This is the core. It takes the raw MKV and produces HLS output.

#### 7a: Determine CRF / Bitrate
```go
// BD source (near-lossless original, much higher source bitrate). CRF 22 does the
// size reduction; the cap only stops spikes, so it sits above the WEB cap rather
// than below it (a 2000k cap would starve high-motion BD scenes).
const H264_CRF_BD = 22
const H264_MAXRATE_BD = "6000k"
const H264_BUFSIZE_BD = "12000k"

// WEB source (already lossy, compensate to avoid generation loss)
const H264_CRF_WEB = 21
const H264_MAXRATE_WEB = "5000k"
const H264_BUFSIZE_WEB = "20000k"
```

**720p caps** are half the 1080p maxrate/bufsize, computed in Go from the parsed config values (the `"…k"` strings can't be halved with shell arithmetic).

**No hard size target.** CRF targets quality, not bitrate. A 40MB source stays ~40MB, a 300MB source stays ~300MB. The encoder naturally preserves the source's compression ratio.

**Auto-adjust based on source ratio:** after first pass, compare output size to source size. If output > 1.5× source size (generation loss guard), increment CRF by 1 and retry (cap CRF 24). If output < 0.3× source size (over-compressed), decrement CRF by 1 and retry. Adapts per-episode instead of an arbitrary ceiling.

#### 7b: Single-pass multi-resolution encoding
**Key insight: Encode once, extract the other resolution without re-encoding the full video.**

```
                    ┌──────────────┐
                    │  episode.mkv │
                    └──────┬───────┘
                           │
                    ┌──────┴───────┐
                    │   Decode     │  ← single decode
                    └──────┬───────┘
                           │
                    ┌──────┴───────┐
                    │    split     │  ← filter_complex split
                    └──────┬───────┘
                           │
              ┌────────────┼────────────┐
              │            │            │
       ┌──────┴─────┐ ┌───┴─────┐ ┌───┴──────────┐
       │  1080p HLS │ │ 720p HLS│ │  Audio HLS    │
       │  (encode)  │ │(downscale│ │  (copy/encode)│
       └──────┬─────┘ │ +encode)│ └───┬──────────┘
              │       └───┬─────┘     │
              │           │           │
        work/1080p/  work/720p/   work/audio/{rend}/
```

**Implementation with ffmpeg:** example:
```bash
ffmpeg -hide_banner -y -i "$INPUT" \
  -map 0:v:0 \
  -filter_complex \
    "[0:v:0]split[vid1][vid2]; \
     [vid1]scale=1920:1080:flags=lanczos,unsharp=5:5:0.8:3:3:0.4[v1080]; \
     [vid2]scale=1280:720:flags=lanczos,unsharp=5:5:0.8:3:3:0.4[v720]" \
  -map "[v1080]" -c:v:0 libx264 \
    -preset slow -tune animation \
    -crf "$CRF" -maxrate "$MAXRATE" -bufsize "$BUFSIZE" \
    -pix_fmt yuv420p -profile:v high -level:v 4.1 \
    -x264-params "keyint=120:min-keyint=1:scenecut=60:rc-lookahead=60:ref=4" \
    -force_key_frames "expr:gte(t,n_forced*6)" \
    -f hls -hls_time 6 -hls_playlist_type vod \
    -hls_segment_filename "work/1080p/seg%03d.ts" \
    -hls_flags independent_segments "work/1080p/stream.m3u8" \
  -map "[v720]" -c:v:1 libx264 \
    -preset slow -tune animation \
    -crf "$CRF" -maxrate "$MAXRATE_720" -bufsize "$BUFSIZE_720" \
    -pix_fmt yuv420p -profile:v high -level:v 4.0 \
    -x264-params "keyint=120:min-keyint=1:scenecut=60:rc-lookahead=60:ref=4" \
    -force_key_frames "expr:gte(t,n_forced*6)" \
    -f hls -hls_time 6 -hls_playlist_type vod \
    -hls_segment_filename "work/720p/seg%03d.ts" \
    -hls_flags independent_segments "work/720p/stream.m3u8"
```

**Critical: `-force_key_frames`** (the 6s expression above, or in practice the explicit list from `intro.KeyframeTimes`, which also lands segment cuts on OP/ED boundaries — see Stage 6 "Keyframe alignment") — this ensures both renditions place keyframes at the same times. Without it, `keyint=120` gives variable keyframe positions (based on scene cuts), and switching between 1080p and 720p causes decoder reset at different times = visible glitch. This aligns segments perfectly for seamless quality switching.

**HEVC source handling:** if source is HEVC/x265, add `-hwaccel auto` for decode and warn the user. HEVC decode is slower but unavoidable. The rest of the pipeline (H.264 encode) stays the same.

**ffmpeg resource limits:**
- `-max_muxing_queue_size 1024` — prevents muxer queue overflow
- `-threads auto` (default) — let ffmpeg use all cores, but no more than available
- Cap concurrent ffmpeg processes via `MAX_FFMPEG_WORKERS` semaphore (already planned)
- Monitor RSS of each ffmpeg process; warn if > 2GB per process

**Why this is efficient:**
- Single decode of the input
- Two x264 encodes running in parallel within the same ffmpeg process
- 720p CRF kept same (quality scales down naturally with resolution)
- 720p maxrate halved to match lower resolution bitrate needs
- `-force_key_frames` aligns segments exactly — no decoder reset glitch on quality switch

**Potential issue:** ffmpeg single-process multi-output can be memory-heavy. Alternative: encode 1080p first, then downscale the 1080p encodes.

**Safer alternative (process isolation, lower memory):**
1. Encode 1080p HLS (full decode → encode)
2. Decode 1080p segments → downscale → encode 720p segments
   - Use `concat` demuxer on the 1080p segments, pipe through scale filter
   - Much faster than full re-encode since source is already compressed at 1080p

**Decision:** Use the single-process approach for efficiency. If memory issues arise, fall back to two-pass with 1080p→720p downscale.

**As implemented (`source/transcode/`), differences from the example above:**
- **x264 keyframes:** `keyint=min-keyint={2×segment+1s in frames}:scenecut=0` instead of `keyint=120:scenecut=60`. At 23.976 fps `keyint=120` is 5.0s, so x264 would add its own keyframes inside every 6s segment, and scene-cut keyframes differ per rendition; either breaks the guarantee that every rendition cuts at the same times. With scene cuts off, the forced list from `intro.KeyframeTimes` is the only source of keyframes — verified with real ffmpeg (identical EXTINF lists, a cut exactly on the OP boundary).
- **Forced keyframe times count from the output's first video frame** (as do playlist segment offsets), so boundaries are converted with `t − video start time`. The container start offset ffmpeg removes cancels out. Getting this wrong is a one-frame error that a loose test tolerance hides.
- **Audio is in the same run** (one extra HLS output per audio rendition), so the 3 GB source is read once and every output shares the same timestamp normalization; separate runs could desync alternate audio.
- **Only renditions ≤ the source height are made** — no upscaling. A 720p WEB source gets only 720p; anything smaller gets one rendition at its own height. Widths keep the source aspect ratio.
- **Level** 4.1 for 1080p, 4.0 for 720p, 4.2 above 30 fps. **VFR** sources get `fps={r_frame_rate}` before the split; 10-bit becomes 8-bit via `format=yuv420p`.
- **Crash retry:** if ffmpeg fails, the run is repeated once with `-threads 1` (outputs cleared first). RSS monitoring and `-hwaccel` for HEVC are not implemented — the target box has no GPU and decodes in software.
- **CRF auto-adjust** compares the top rendition's video bytes to the source video bytes; outside 0.3×–1.5× it moves CRF by one and re-encodes, at most twice, within [18 BD / 19 WEB, 24]. Skipped when the source bitrate is unknown and on the fast path.

#### 7c: Fast-path (stream copy when source matches)
If Media Analyzer marked the episode as fast-path eligible (video already 1080p H.264 8-bit yuv420p CFR, keyframe gap ≤ 10s — see Stage 3):
- No video re-encode. Segment the existing video with stream copy: `-c:v copy`
- **Keyframes can't be forced on a stream copy** — the HLS muxer can only cut 1080p at the source's existing keyframes, so segment boundaries follow the source GOP, not a fixed 6s grid. To keep renditions aligned:
  1. Stage 3 checks the source keyframe gap (`stream_map.json`) for eligibility.
  2. **Run 1:** stream-copy the video as 1080p (plus all audio renditions); the HLS muxer cuts at source keyframes.
  3. Read the cut times back from the 1080p playlist — what the muxer actually did, rather than a simulation of it.
  4. **Run 2:** encode 720p with `-force_key_frames` at exactly those times (and scene cuts off), so both renditions cut identically. Verified with real ffmpeg; only the last segment's reported length differs slightly (the copy keeps the MKV's ms-rounded last frame duration).
  5. If run 1 fails, fall back to the full encode.
- **Eligibility also requires** a max source keyframe gap ≤ 10s (otherwise segments get too long for smooth ABR); if not, fall back to the full transcode
- 720p rendition still needs a transcode (downscale + encode at 720p), but 1080p segments are instant
- Audio: stream copy as usual
- Subs: extract as usual
- This saves 80-90% of transcode time for WEB releases that are already well-compressed

#### 7d: Audio transcoding
Each kept audio track (Stage 5 audio map) is one more HLS output **in the same ffmpeg run** as the video:
```bash
# AAC source (stream copy)
  -map 0:{idx} -vn -sn -dn -c:a copy -f hls ... audio/{rendition}/stream.m3u8
# Any other codec (re-encode to AAC)
  -map 0:{idx} -vn -sn -dn -c:a aac -b:a 128k -ac 2 -ar 48000 -f hls ... audio/{rendition}/stream.m3u8
```
Segments are MPEG-TS (`seg%04d.ts`), encrypted with the same key as the video. AAC only, because browsers (MSE) reliably play only AAC.

#### 7e: Subtitle conversion (ASS → WEBVTT)
For each track from Stage 4 (`subs.json`):
- ASS → `ffmpeg -i eng.ass -c:s webvtt eng.vtt` (styling, positioning and override tags are dropped); the ASS is also copied next to it for libass players. WebVTT tracks are copied.
- **`X-TIMESTAMP-MAP=MPEGTS:{pts},LOCAL:00:00:00.000`** is inserted after the `WEBVTT` line. Cue times are source time (extracted with `-copyts`), and `{pts}` is the MPEG-TS PTS of source time 0: the first 1080p segment's video `start_pts` (decrypted first if encrypted) minus the video start time × 90000. Without the map, players guess and cues drift.
- A one-segment playlist `eng.m3u8` per track (TARGETDURATION = episode length) — HLS subtitle renditions must be playlists.
- A track that fails conversion is left out of the master with a warning; it doesn't fail the episode.

#### 7f: HLS Encryption (AES-128)
Encrypt all .ts segments with AES-128 to prevent direct hotlinking/leeching.

**Key reuse:** `enc.key` + `enc.keyinfo` are reused when they already exist for the same key URI, so a CRF re-encode or crash resume never leaves segments under two different keys.

**Key generation (one per episode):** keys are addressed by `{anime_id}/{ep_id}` — the same address as the output dir — so the key URI stays stable no matter which job (initial or append) produced the episode.
```go
key := make([]byte, 16)  // 128-bit
rand.Read(key)
iv := make([]byte, 16)
rand.Read(iv)
```
Write `key` to `work/{job_id}/{ep_id}/meta/enc.key` for ffmpeg; at packaging, move it to `{KEY_DIR}/{anime_id}/{ep_id}.key`.

**ffmpeg key info file** (`work/{job_id}/{ep_id}/meta/enc.keyinfo`):
```
https://keys.example.com/keys/{anime_id}/{ep_id}.key
/path/to/work/{job_id}/{ep_id}/meta/enc.key
ABCDEF1234567890ABCDEF1234567890
```
Line 1: URI the player fetches to get the key (`{HLS_KEY_URI_BASE}/{anime_id}/{ep_id}.key`).
Line 2: Path to the 16-byte key file on disk (ffmpeg reads this to encrypt).
Line 3: 32-char hex IV (optional — if omitted, ffmpeg uses segment index as IV).

**ffmpeg flags added to video/audio HLS commands:**
```bash
-hls_key_info_file "work/{job_id}/{ep_id}/meta/enc.keyinfo"
```

**Encrypted segment playlist output:**
```
#EXTM3U
#EXT-X-VERSION:6
#EXT-X-TARGETDURATION:6
#EXT-X-PLAYLIST-TYPE:VOD
#EXT-X-KEY:METHOD=AES-128,URI="https://keys.example.com/keys/{anime_id}/{ep_id}.key",IV=0xABCDEF1234567890ABCDEF1234567890
#EXT-X-MEDIA-SEQUENCE:0
#EXTINF:6.000,
seg000.ts
...
```

**Key serving strategy:**
- Keys live in `{KEY_DIR}/{anime_id}/{ep_id}.key`, **never** inside `OUTPUT_DIR` — anyone who can read the output tree could otherwise fetch the key next to the segments, making encryption pointless
- Serve via a lightweight HTTP endpoint: `GET /keys/{anime_id}/{ep_id}.key`
- Infohash-dedup copies (Stage 0) reuse the original episode's playlists, so they point at the original key URI — no new key is made
- Or delegate to a reverse proxy / CDN that controls access (e.g., nginx internal redirect after auth check)
- Origin check / token auth can be added in front of the key endpoint to restrict who can decrypt

**Why AES-128 and not SAMPLE-AES:**
- AES-128 encrypts the entire segment (simpler, universal player support)
- SAMPLE-AES is for DRM schemes (FairPlay, Widevine) — overkill for self-hosted
- Negligible CPU overhead (modern hardware has AES-NI)

**Optional toggle:**
```env
HLS_ENCRYPT=true
HLS_KEY_URI_BASE=https://keys.example.com/keys
KEY_DIR=./keys
```
### Stage 8: Output Packager
- Copy work dir contents to their final output locations (`output/{anime_id}/{ep_id}/`)
- Build master.m3u8 referencing all video + audio + subtitle renditions
- Copy meta files (timestamps.json, media_info.json, job.json)
- Move `enc.key` to `{KEY_DIR}/{anime_id}/{ep_id}.key` (not into output)
- **Font copying:** for each font hash recorded during Stage 4 extraction, copy `fonts/{hash}/{canonical_filename}` → `output/{anime_id}/{ep_id}/subs/fonts/{original_filename}`. This preserves the torrent's original filename for the font while deduplicating at extraction time. Real file copies — no symlinks — so lsyncd replication to storage servers works without issue. See [Stage 4](#stage-4-subtitle-extractor) for the full font dedup flow.
- **Set size_bytes on episode row:** after packaging, recursively sum all output files and `UPDATE episodes SET size_bytes = ?`
- **Staging + atomic publish:** everything (transcode output, fonts, meta, `job.json`) is assembled in `output/{anime_id}/.{ep_id}.staging/`, verified, and then renamed to `output/{anime_id}/{ep_id}/`. The final dir therefore either doesn't exist or is complete, so a crash never exposes a half-copied episode to the frontend or lsyncd (exclude `.*.staging` and `.trash` from replication).
- **Key before output:** the key is copied to `KEY_DIR/{anime_id}/{ep_id}.key` before the rename, so no player ever sees segments whose key isn't served yet.
- **Orphaned output guard:** if `output/{anime_id}/{ep_id}/` exists without a valid master (a leftover from older tooling or manual copying), it's moved to `output/.trash/{anime_id}/{ep_id}-{UTC timestamp}` first. A valid master.m3u8 is never overwritten — if one appeared meanwhile, it's kept and the new copy discarded.
- Verify all segments and playlists are valid (each segment exists, m3u8 files parse, keyframe intervals consistent)
- Remove `work/{job_id}/{ep_id}/raw/` (keep meta/ for retry, mark for cleanup)

### Stage 9: Callback Notifier
- POST the callback payload to callback_url
- 4 attempts with 1s, 2s, 4s between them (15s timeout each); any 2xx is success. Sent in the background once every episode of the job is terminal; shutdown waits up to 30s for pending callbacks.
- Per-episode `timestamps` in the payload are read from the packaged `meta/timestamps.json`, so they include the series fingerprint results.
- If callback fails after all retries, log error and continue (job is still "done")
- If no callback_url configured, skip

### Stage 10: Health & Readiness
- **GET /health** — lightweight ping + SQLite connectivity check
- **GET /ready** — deep check: ffmpeg/ffprobe binary exists, disk space above threshold (warn if < 10GB free), SQLite reachable, work dir writable
- Refuse new jobs if readiness fails (return 503)

### Stage 11: Cleanup Daemon
- Background goroutine that runs every 15 minutes
- Scan for episodes that reached `done` more than `WORK_TTL_HOURS` ago, or `failed` more than `FAILED_WORK_TTL_HOURS` ago (measured from `episodes.updated_at`; a failed episode that is re-run resets the clock)
- For each expired episode:
  1. Remove `work/{job_id}/{ep_id}/` entirely
  2. Decrement `font_cache.ref_count` for each font referenced by this episode
  3. If any font's `ref_count` hits 0, delete `fonts/{hash}/` and remove the row
- **Font reference tracking:** read the episode's rows from `episode_fonts` (INSERTed during Stage 4), decrement `font_cache.ref_count` once per row, then delete those rows — all in one transaction.
- **Safety:** never touch `output/`, only `work/` and the font cache

---

## 5. Component Design

### 5.1 Scheduler (scheduler.go)
```
Job struct:
  - ID, AnimeID, Title, Status, CallbackURL, EpisodeCount
  - CreatedAt, UpdatedAt

Episode struct:
  - JobID, EpID, Title, MagnetURL, Infohash, Priority
  - NeedsPoster, Provider, SourceType
  - Status, Progress, Error, SizeBytes, OutputPath
  - CreatedAt, UpdatedAt
  (font usage lives in the episode_fonts table, not on the struct)

EpisodeQueue:
  - SQLite-backed priority queue (episodes table)
  - PopNext() → claims highest-priority queued episode atomically:
      UPDATE episodes SET status='resolving' WHERE (job_id, ep_id) IN
        (SELECT job_id, ep_id FROM episodes WHERE status='queued' ORDER BY priority, created_at LIMIT 1)
      RETURNING *
  - Insert job + episodes in a single transaction

Scheduler:
  - HTTP POST /job → validate, INSERT job+episodes transactionally, return job_id
  - HTTP POST /api/anime/{anime_id}/episodes → append episodes to existing anime_id
  - HTTP GET /job/{id} → SELECT from jobs + JOIN episodes, return info response
  - HTTP GET /jobs → list with status filters
  - HTTP GET /health → queue depth by status
  - Worker goroutines (MAX_CONCURRENT_TORRENTS):
    - PopNext() → gets (job_id, ep_id) atomically
    - Read episode + parent job from DB
    - Call Pipeline.Run(episode)
    - After each stage: UPDATE episodes SET status/progress
    - After terminal: check job siblings, update jobs.status rollup
    - If all done + callback_url → POST callback
    - Handle panics/recover → set episode status='failed'
```

### 5.2 Pipeline (pipeline.go)
```
Pipeline:
  - Resolver, Downloader, Analyzer, Extractor, TranscodeEngine, Packager
  - DB handle (SQLite)
  - Run(episode):
    1. Resolve (magnet → file info, infohash)
    2. Download (torrent → work/{job_id}/{ep_id}/raw/episode.mkv)
    3. Analyze (ffprobe → media_info.json, stream_map.json)
    4. Extract subs (ASS/VTT extraction, font dedup to global cache)
    5. Map audio (language heuristics, lossless→AAC decision)
    6. Detect intro/outro (chapters → fingerprint correlation → silence/black)
    7. Transcode (multi-res HLS, audio packaging, encryption)
    8. Package (copy to output/{anime_id}/{ep_id}/, copy fonts from cache, set size)
    - Each step UPDATEs episodes.status + episodes.progress in SQLite
    - Any step can error → UPDATE status='failed', error=err.Error()
    - Per-episode analysis data written to work/{job_id}/{ep_id}/meta/*.json
    - After terminal state: Scheduler handles sibling rollup + callback
```

### 5.3 Torrent Engine (torrent/engine.go)
```
TorrentEngine:
  - anacrolix/torrent Client wrapper
  - AddMagnet(magnetURL) → TorrentHandle
  - SelectFiles(handle, fileIndices)
  - WaitForDownload(handle, timeout) → error
  - GetBytesDownloaded(handle) → int64
  - DropTorrent(handle)
  - Close()
```

### 5.4 FFmpeg Wrapper (transcode/ffmpeg.go)
```
FFmpeg:
  - Probe(path) → MediaInfo
  - ExtractSubtitles(input, streamIdx, outputPath)
  - ExtractFonts(input, outputDir)
  - DetectSilence(input, audioStreamIdx) → []SilenceSegment
  - DetectBlackFrames(input) → []BlackFrameSegment
  - TranscodeHLS(input, streams, config) → TranscodeResult
  - ValidatePlaylist(path) → bool
```

### 5.5 File Layout (source/utils/layout.go)
```
Paths:
  - WorkDir(jobID) → work/{job_id}/
  - EpWorkDir(jobID, epID) → work/{job_id}/{ep_id}/
  - RawFile(jobID, epID) → work/{job_id}/{ep_id}/raw/episode.mkv
  - MetaDir(jobID, epID) → work/{job_id}/{ep_id}/meta/
  - OutputDir(animeID, epID) → output/{anime_id}/{ep_id}/
  - AudioDir(animeID, epID, lang) → output/{anime_id}/{ep_id}/audio/{lang}/
  - SubsDir(animeID, epID) → output/{anime_id}/{ep_id}/subs/
  - FontCacheDir(hash) → fonts/{hash[:16]}/
  - FontCacheFile(hash, filename) → fonts/{hash[:16]}/{filename}
  - OutputFontsDir(animeID, epID) → output/{anime_id}/{ep_id}/subs/fonts/
  - CopyFontToOutput(hash, canonicalName, animeID, epID, originalName)
      → copies fonts/{hash}/{canonical} to output/{anime_id}/{ep_id}/subs/fonts/{original}
```

---

## 6. Configuration

### Current `.env` (actual on disk — needs expansion)
```env
WORK_DIR=./work
OUTPUT_DIR=./output
TORRENT_DIR=./torrents
MAX_CONCURRENT_TORRENTS=3
MAX_FFMPEG_WORKERS=2
LOG_LEVEL=info
```

### Planned `.env` (target)
```env
# Directories
WORK_DIR=./work
OUTPUT_DIR=./output
TORRENT_DIR=./torrents
DB_PATH=./data/torclient.db        # SQLite database (job queue + state)
FONT_CACHE_DIR=./fonts             # content-addressed font cache (encode-server only)
KEY_DIR=./keys                     # HLS AES keys — must NOT be inside OUTPUT_DIR

# Concurrency
MAX_CONCURRENT_TORRENTS=3
MAX_FFMPEG_WORKERS=2

# Network
TORRENT_PORT=42069
DHT=true
PEER_LIMIT=50

MIN_VIDEO_FILE_MB=50               # smaller videos are treated as samples/extras

# Torrent timeouts
RESOLVE_ATTEMPTS=3
RESOLVE_TIMEOUT_SECONDS=60
RESOLVE_BACKOFF_SECONDS=10
NO_PEERS_TIMEOUT_SECONDS=120
STALL_TIMEOUT_MINUTES=5
DOWNLOAD_TIMEOUT_MINUTES=30

# FFmpeg (jellyfin-ffmpeg)
FFMPEG_PATH=/usr/lib/jellyfin-ffmpeg/ffmpeg
FFPROBE_PATH=/usr/lib/jellyfin-ffmpeg/ffprobe
MKVEXTRACT_PATH=mkvextract        # optional fallback for subs/fonts

# Transcode
H264_CRF_BD=22
H264_MAXRATE_BD="6000k"
H264_BUFSIZE_BD="12000k"
H264_CRF_WEB=21
H264_MAXRATE_WEB="5000k"
H264_BUFSIZE_WEB="20000k"
H264_PRESET="slow"
H264_TUNE="animation"
AUDIO_BITRATE="128k"
AUDIO_CHANNELS=2
AUDIO_SAMPLERATE=48000
SEGMENT_DURATION=6

# Encryption (AES-128 HLS)
HLS_ENCRYPT=true
HLS_KEY_URI_BASE=https://keys.example.com/keys

# AniList discovery (§3a)
DISCOVERY_SOURCES=                 # animetosho,seadex,subsplease,nyaa,yameii,toonshub (empty = all)
DISCOVERY_RESOLUTION=1080
DISCOVERY_PREFERRED_GROUPS=        # e.g. SubsPlease,Erai-raws
DISCOVERY_EXCLUDE=                 # substrings never picked

# Cleanup (TTL counts from when the episode reached done/failed)
WORK_TTL_HOURS=3                   # done episodes — output is already packaged
FAILED_WORK_TTL_HOURS=72           # failed episodes — keep meta/ and partial work for re-runs
CLEANUP_INTERVAL_MINUTES=15

# HTTP Server
LISTEN_ADDR=:8080
SERVE_MEDIA=false                  # GET /media/{path} (OUTPUT_DIR) + /play test player
KEY_CORS_ORIGIN=*                  # Access-Control-Allow-Origin for GET /keys/... ("" = none)
MIN_FREE_DISK_GB=10                # /ready warns below this

# Logging
LOG_LEVEL=info

```

---

## 7. Edge Cases & Error Handling

| Scenario | Handling |
|---|---|
| **Magnet fails to resolve** | Retry 3x with 10s backoff. Fail episode after. Other episodes continue. |
| **Torrent has no seeders** | 2-min connect timeout. Mark episode failed. |
| **Torrent contains no MKV** | Fall back to the largest `.mp4`/`.m4v`/`.avi` (non-sample, > 50MB). If none, fail episode. |
| **Multiple MKVs found in one torrent** | Pick the largest as main episode file. Log extras as skipped. |
| **Download stalls** | No progress for 5 min → watchdog force-drop, fail episode. |
| **Disk full** | Check available space before download (need 2x torrent size). Fail if insufficient. |
| **Only PGS subtitles** | Fail the episode. No usable subtitles for HLS. |
| **No subtitles** | Skip. Continue. |
| **No audio tracks** | Fail episode (anime without audio is invalid). |
| **10-bit video** | Warn. Convert to yuv420p during transcode. |
| **Variable frame rate** | Detect via ffprobe. Use `-vsync cfr` during transcode. |
| **Non-16:9 aspect ratio** | Scale maintaining aspect ratio, add letterboxing if needed. |
| **Intro detection fails (all tiers)** | Set `detected: false`, transcode full episode. Frontend skips nothing. |
| **FFmpeg crash/segfault** | Retry transcode once with `-threads 1`. If same error, fail episode. |
| **Callback fails** | Retry 3x with exponential backoff. Log and continue. |
| **Power loss during transcode** | Work dir has partial state. Cleanup daemon picks up after restart. Episode resumes from DB checkpoint. |
| **Same ep_id twice in one payload** | Return 400. |
| **Same ep_id for the same anime_id, still in flight in another job** | Return 409 Conflict listing the ep_ids. Nothing inserted. |
| **Same ep_id for the same anime_id, already finished** | Cache hit — episode marked `done` immediately (see Stage 0). |
| **Priority inversion** | Per-episode priority queue handles correctly. Higher priority episodes processed first. |
| **OOM from FFmpeg** | Use `-max_muxing_queue_size 1024` and memory limits. Retry with `-threads 1`. |
| **HLS segment corruption** | Validate segment count. If missing segments, fail episode. |
| **Callback URL unreachable** | Retry 3x. After all failures, log and mark episode done (job proceeds). |
| **Zero-byte file after download** | Verify file size > 1MB. Fail if empty. |
| **Key file missing/corrupt** | Re-generate key & IV on transcode retry. Log fingerprint of old key. |
| **Player reports decrypt error** | Usually means key URI is unreachable or CORS. Document that `HLS_KEY_URI_BASE` must be accessible from client browsers. |
| **Key rotated mid-session** | Not applicable — VOD with static key. Key is per-episode and never changes. |
| **Cache hit (output already exists)** | Skip episode pipeline entirely. Mark as `done` with existing output path. |
| **Infohash already processed** | Recreate the old output tree at `output/{anime_id}/{ep_id}/`, hardlinking each file (copy across filesystems). No download. |
| **Source already matches target (fast-path)** | Stream copy video. Only downscale for 720p. No re-encode for 1080p. |
| **Target size > 1.5× source** | Auto-increment CRF by 1, re-encode. Max CRF 24. |
| **Target size < 0.3× source** | Auto-decrement CRF by 1, re-encode. Min CRF 18 (BD) / 19 (WEB). |
| **Chapters available for OP/ED** | Use chapter boundaries directly. 100% confidence, skip fingerprint correlation. |
| **Multi subtitle tracks same language** | Merge or name as dialogue + signs. Frontend picks based on preference. |
| **JPN 5.1 + JPN 2.0 both present** | Prefer 5.1 for main audio, discard 2.0 if it's just a downmix (track title contains "Downmix"/"Stereo"). |
| **ffmpeg/ffprobe not installed** | /ready returns 503. Episodes fail immediately with clear error. |
| **Disk space low** | /ready warns. Active episodes check before download & transcode. Fail if < 2× expected size. |
| **HEVC source** | Decode with `-hwaccel auto`, encode to H.264. Warn user of slower transcode. |
| **Two workers claim same episode** | Impossible — atomic `UPDATE episodes ... WHERE (job_id, ep_id) IN (SELECT ... LIMIT 1) RETURNING *` in single statement. |
| **ffmpeg crashes (OOM/segfault)** | Retry once with `-threads 1` (slower but stable). If same error, fail episode. Set `--max-muxing-queue-size 1024`. |
| **Torrent handle leak / stale download** | Watchdog goroutine per download: no progress for 5min → force `DropTorrent()`. |
| **Transcode fails at 85%** | Stage checkpoint in SQLite (`episodes.status`). On retry, resume from last completed stage. |
| **Partial output directory from previous failure** | Move old files to `.trash/` timestamped dir before writing new output. |
| **Encrypted subtitle streams** | Try mkvextract. If that also fails, skip subs with warning (video still fine). |
| **Audio language tag missing/wrong** | Fallback: track title heuristics → stream index order → "und". |
| **Audio commentary track mistaken for main** | Check `tags['title']` for "Commentary" / "Audio Commentary". Skip unless explicitly requested. |
| **Multiple JPN audio tracks (5.1 + 2.0)** | Prefer 5.1. Discard 2.0 only if track title contains "Downmix" or "Stereo" (indicating it's derived). |
| **Sample file in torrent** | Filter by: filename contains `sample` (case-insensitive), file < 50MB, or in `Sample/` dir. |
| **Container is MP4, not MKV** | Still process if ffprobe can read it. Rename to episode.mkv. |
| **ffprobe fails on downloaded file** | File is corrupt. Fail episode. |
| **Process restarts mid-download** | anacrolix/torrent state persisted to disk. Download resumes if torrent is still in Client. |
| **Process restarts mid-transcode** | Stage checkpoint in episodes.status. Episode resumes from failed stage. |
| **One episode in a job fails** | Other episodes continue independently. Job status = `failed`. Callback lists per-episode results. |
| **Same font hash conflict in cache** | Impossible — `font_cache` PK is `hash`. INSERT OR IGNORE + increment ref_count. |
| **Font extraction fails for one episode** | Log warning, continue without fonts. ASS renderer uses system fonts. |
| **Font ref_count never reaches 0 (orphaned)** | Cleanup daemon scans `fonts/` dir vs font_cache table monthly. Deletes orphans. |
| **ffmpeg build lacks the chromaprint muxer** | Skip tier 2; go to tier 3 (silence + black detection). No series-level detection. |
| **Fingerprint correlation running for anime_id while new episode arrives** | New episode queued for next batch sweep. No race. |
| **Fewer than 3 fingerprinted episodes for anime_id** | Skip tier 2. Falls through to tier 3 per-episode detection. |
| **Duplicate font by different filename (same content)** | Content hash dedup handles this — same hash = one canonical copy in `font_cache`. Each episode's own filename is kept in `episode_fonts.filename`. |
| **job_id already exists (idempotency)** | Return 200 with current job state. No-op. Caller responsible for dedup. |
| **Episodes array empty in POST /job** | Return 400. A job must have at least one episode. |
| **magnet_url invalid format** | Return 400. Must start with `magnet:?xt=urn:btih:`. |

---

## 8. Dependencies

### Go modules
- `github.com/anacrolix/torrent` — torrent client engine
- `github.com/joho/godotenv` — .env loading (already in go.mod)
- `modernc.org/sqlite` — pure-Go SQLite driver (no CGO, no system libs)
- `encoding/json` — standard lib, JSON serialization
- `net/http` — standard lib, HTTP server + callback

### System binaries (must be in Docker image)
- `jellyfin-ffmpeg` — custom ffmpeg build with enhanced codec support (VAAPI, NVENC, QSV, libass, more). Drops in as `ffmpeg`/`ffprobe` via symlink or alias. Use this over vanilla ffmpeg.
- `mkvextract` (optional, can use ffmpeg sub extraction) — from mkvtoolnix

**Hardware acceleration note:** The target hardware (dual Xeon E5-2680 v4, 28C/56T) lacks an iGPU and therefore has no Intel QSV support. All transcoding is software x264, which is well-suited to this CPU's 56 threads + AVX2. If HW accel is desired later, add an NVIDIA GPU for NVENC or an Intel ARC for QSV — jellyfin-ffmpeg supports both.

### Docker image base (torClient)
jellyfin-ffmpeg is published as `.deb` packages in Jellyfin's apt repo, not as a standalone Docker image, so the image installs it from that repo. (Check the package name and repo layout at https://repo.jellyfin.org before building — `jellyfin-ffmpeg7` was current at the time of writing.)
```dockerfile
FROM golang:1.25 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/torClient .

FROM ubuntu:24.04 AS runtime

RUN apt-get update && apt-get install -y --no-install-recommends         ca-certificates curl gnupg tzdata mkvtoolnix     && install -d /etc/apt/keyrings     && curl -fsSL https://repo.jellyfin.org/jellyfin_team.gpg.key         | gpg --dearmor -o /etc/apt/keyrings/jellyfin.gpg     && echo "deb [signed-by=/etc/apt/keyrings/jellyfin.gpg] https://repo.jellyfin.org/ubuntu noble main"         > /etc/apt/sources.list.d/jellyfin.list     && apt-get update && apt-get install -y --no-install-recommends jellyfin-ffmpeg7     && ln -s /usr/lib/jellyfin-ffmpeg/ffmpeg /usr/local/bin/ffmpeg     && ln -s /usr/lib/jellyfin-ffmpeg/ffprobe /usr/local/bin/ffprobe     && rm -rf /var/lib/apt/lists/*

COPY --from=build /out/torClient /bin/torClient

ENV FFMPEG_PATH=/usr/lib/jellyfin-ffmpeg/ffmpeg
ENV FFPROBE_PATH=/usr/lib/jellyfin-ffmpeg/ffprobe
# anacrolix mmap IO never releases files; see Stage 2 "File IO"
ENV TORRENT_STORAGE_DEFAULT_FILE_IO=classic
ENTRYPOINT ["/bin/torClient"]
```

**Why jellyfin-ffmpeg over vanilla ffmpeg:**
- Bundles all codecs (libx264, libx265, libvpx, libaom, etc.) without dependency hell
- Includes libass for ASS subtitle rendering
- Supports VAAPI, NVENC, QSV (HW accel ready if GPU added later)
- Regular updates with security fixes

**Why Ubuntu over Alpine:**
- jellyfin-ffmpeg is only distributed as .deb packages
- Ubuntu LTS has better compatibility with mkvtoolnix and other deps
- Slightly larger image but worth it for native jellyfin-ffmpeg support

---

## 9. Implementation Order (Phases)

### Phase 1 — Skeleton & Data Model ✅
- [x] **Go dependencies:** `modernc.org/sqlite`, `godotenv` (`anacrolix/torrent` moved to Phase 2, where it is first imported)
- [x] **Config package** (`config/config.go`): load full .env, typed config struct
- [x] **Logging setup** (`source/utils/logger.go`): slog with level from config
- [x] **SQLite init** (`source/utils/db.go`): open DB with WAL mode, run migrations (`source/utils/migrations/*.sql`, tracked by `PRAGMA user_version`)
- [x] **Job struct + schema** (`source/models/job.go`): mirror SQLite `jobs` table
- [x] **Priority queue** (`source/utils/queue.go`): `PopNext()` SQL query with atomic claim
- [x] **Job CRUD** (`source/utils/jobstore.go`): insert, get, update status (+ rollup), list
- [x] **HTTP server scaffold** (`main.go` + `source/api/server.go`): mux, logging + recover middleware, graceful shutdown
- [x] **POST /job handler:** validate input, INSERT job (idempotency + in-flight check), return job
- [x] **POST /api/anime/{anime_id}/episodes handler:** 404 if anime_id unknown, INSERT new job with same anime_id
- [x] **GET /job/{id} handler:** return job status from DB
- [x] **GET /jobs handler:** list with `status` / `anime_id` filters, `limit` / `offset`
- [x] **GET /health handler:** SQLite ping + episode counts by status
- [x] **Directory layout helpers** (`source/utils/layout.go`): `WorkDir()`, `OutputDir()`, `KeyFile()`, etc.
- **ID rule:** `job_id`, `anime_id` and `ep_id` must match `^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$` — they become path segments, so this blocks `..` and slashes.

### Phase 2 — Torrent Engine ✅
- [x] **Go dependency:** `github.com/anacrolix/torrent` v1.61.0
- [x] **Engine** (`source/torrent/engine.go`): one shared anacrolix client; per-episode storage under `raw/torrent/`
- [x] **Client init:** DHT, PEX, port, peer limit (no LPD — anacrolix doesn't implement it)
- [x] **AddMagnet / WaitInfo:** parse infohash, fetch metadata, cache `.torrent` in `TORRENT_DIR/metainfo/`
- [x] **File selection:** download only the chosen file (others set to priority none)
- [x] **Download with progress** (`source/torrent/downloader.go`): `OnProgress(done, total)` callback; store has `UpdateProgress` for the pipeline to call
- [x] **Watchdog:** stall / no-peers / total timeouts, drop torrent on failure
- [x] **Drop / Close:** releases storage and the piece-completion db
- [x] **Classic file IO** (`source/torrent/fileio.go`): re-launch with `TORRENT_STORAGE_DEFAULT_FILE_IO=classic` if unset
- [x] **Resolver** (`source/resolvers/resolver.go`): metadata with retries + backoff, then main file selection
- [x] **Main file selection:** largest non-sample video > 50MB, MKV preferred, extras logged as skipped
- [x] **Sample filtering:** "sample" in filename, `Sample/` dir, or < 50MB
- [x] **Infohash recording:** `JobStore.SetInfohash` (called by the pipeline in Phase 6)
- [x] **Stale download recovery:** per-episode piece-completion db + metainfo cache → re-adding resumes; existing `episode.mkv` short-circuits
- [x] **Tests:** selection rules; end-to-end resolve + download from a local seeder; metadata timeout; seeder leaving mid-download
- **Not yet wired:** nothing calls the resolver/downloader until the pipeline and worker pool (Phase 6). Disk-space checks before download are also Phase 6.

### Phase 3 — Media Analysis ✅
Code lives in `source/media/` (one package for Stages 3–5) plus `source/utils/fontcache.go`.
- [x] **FFprobe wrapper** (`source/media/probe.go`): run ffprobe, parse JSON; failure = corrupt file
- [x] **Stream map generation** (`source/media/streammap.go`): video/audio/sub/attachment absolute stream indices, chapters; cover art skipped
- [x] **Codec/property detection:** resolution, bitrate (stream / `BPS` tag / estimate), FPS, pix_fmt, bit depth, VFR
- [x] **BD vs WEB heuristics:** lossless audio or video > 4000 kbit/s → `bd`
- [x] **Fast-path eligibility:** video-only criteria + keyframe gap from packet flags; `fast_path_reason` when not
- [x] **Subtitle extractor** (`source/media/subs.go`): one-pass extraction with `-copyts`, per-track retry, mkvextract fallback; dialogue/signs naming by title + cue coverage
- [x] **PGS rejection:** skip bitmap tracks; `ErrOnlyBitmapSubs` only if every subtitle track is bitmap
- [x] **Font extraction** (`source/media/fonts.go`): one ffmpeg `-dump_attachment:N` pass into our own temp names (the MKV's filename never picks the write path), mkvextract fallback, warnings instead of failure
- [x] **Font cache** (`source/utils/fontcache.go`): `Register` (sha256[:16], copy once, ref per `episode_fonts` row, idempotent on retry), `EpisodeFonts` (for packaging), `Release` (for cleanup; removes the dir at ref 0). Each runs in one write transaction so register/release of the same hash can't race.
- [x] **Font filename variance handling:** canonical copy keeps the first-seen name; each episode's own name is in `episode_fonts`
- [x] **Audio mapper** (`source/media/audio.go`): title > tag > order language heuristics, AAC copy / everything else → AAC, commentary skip, downmix drop when a wider same-language track exists, renditions + default
- [x] **Write analysis JSON:** `media_info.json` (raw ffprobe), `stream_map.json`; `media.WriteJSON` (atomic) for `audio_map.json` / `subs.json`
- [x] **Config:** `MKVEXTRACT_PATH` (optional fallback)
- [x] **Tests:** unit tests for parsing/heuristics/naming/coverage/filename sanitizing; real ffmpeg run on a generated MKV (skipped if ffmpeg isn't on PATH); font cache dedup + release
- **Not yet wired:** the pipeline (Phase 6) will call `Analyze` → `ExtractSubtitles` → `ExtractFonts` + `FontCache.Register` → `MapAudio` and set `episodes.source_type`. The mkvextract fallbacks are untested here (mkvtoolnix isn't installed on the dev machine).

### Phase 4 — Intro Detection ✅
Code lives in `source/intro/`; fingerprint rows in `source/utils/fingerprints.go`.
- [x] **Chapter parser** (`chapters.go`): anchored title patterns for OP/ED/preview (English + Japanese), plausible-length checks
- [x] **Silence + black detection** (`heuristic.go`): one decode per window, log parsing, gap fusion, 80–100s pair picking, confidence by evidence
- [x] **Audio fingerprinting** (`fingerprint.go`): chromaprint via ffmpeg (`HasChromaprint` check), raw LE points, `SaveFingerprint` / `Fingerprints` / `FingerprintedEpisodes` in the store; migration `0002` adds `start_offset`
- [x] **Series correlation** (`Correlate`): per-pair longest bit-tolerant run, per-episode longest match
- [x] **Tier merge** (`Merge`): highest confidence per part (chapter 1.0 > fingerprint 0.95 > heuristic ≤ 0.7)
- [x] **Per-episode orchestration** (`Detector.DetectEpisode`): chapters → heuristics for missing parts → fingerprints; failures become warnings + `detected:false`, never episode failures
- [x] **Force keyframe calculation** (`KeyframeTimes`, `ForceKeyFramesArg`): boundary-anchored 6s grid, verified against a simulation of the HLS muxer's cutting rule
- [x] **Write timestamps.json** (`Write`)
- [x] **Tests:** chapter patterns, merge, keyframe/segment alignment, log parsing, gap picking, synthetic correlation (with bit noise and window offsets); real ffmpeg: a generated 300s episode with black+silent gaps (OP/ED found within 0.5s), three audio episodes sharing a 40s tune at non-hop-aligned offsets (found within 2s)
- **Not yet wired:** the pipeline (Phase 6) runs `DetectEpisode` before transcode, saves fingerprints, queues `Correlate` per anime_id once `FINGERPRINT_MIN_EPISODES` have them, and `Merge`s the series result into each episode's `timestamps.json`.

### Phase 5 — Transcoding ✅
Code lives in `source/transcode/`.
- [x] **FFmpeg command builder** (`ffmpeg.go`): one run — single decode, `split` + lanczos scale + unsharp + `format=yuv420p` per encoded rendition, copied video for the fast path, every audio rendition; `-max_muxing_queue_size 1024`, `independent_segments`, MPEG-TS segments
- [x] **Settings + CRF** (`settings.go`, `transcode.go`): BD/WEB rate control, 720p at half the caps, CRF auto-adjust (≤ 2 re-encodes), `SettingsFrom(config)`
- [x] **Rendition planning:** 1080p/720p without upscaling, aspect-preserving widths, H.264 levels, RFC 6381 codec strings
- [x] **Keyframe alignment:** `keyint` above the max forced gap + `scenecut=0`; forced times on the first-frame timeline
- [x] **Fast-path mode:** copy 1080p, read its real cut points, encode 720p to match; fall back to full encode on failure
- [x] **Subtitle conversion** (`subtitles.go`): ASS → WebVTT, `X-TIMESTAMP-MAP` from the real first-segment PTS, per-track playlists, ASS copies
- [x] **HLS encryption** (`keys.go`): per-episode key/IV, `enc.keyinfo`, reuse across retries; `decryptSegment` (AES-128-CBC + PKCS#7) for PTS probing and tests
- [x] **Master playlist** (`master.go`): audio + subtitle groups, BCP 47 languages, measured BANDWIDTH/AVERAGE-BANDWIDTH, FRAME-RATE, CODECS
- [x] **Validation** (`playlist.go`): every playlist parsed, every segment present and non-empty, `#EXT-X-ENDLIST` required, encryption state checked
- [x] **Crash retry:** once more with `-threads 1`
- [x] **Tests:** unit (renditions, codecs, args, playlist validation, key reuse, timestamp map, master); real ffmpeg: encrypted 1080p+720p+2 audio+subs package (identical cuts, cut on the OP boundary to half a frame, segments decrypt to TS, **ffmpeg plays it back through master.m3u8**), fast path (720p aligned to the copied 1080p)
- **Not done:** RSS monitoring of ffmpeg, `-hwaccel` for HEVC.
- **Not yet wired:** the pipeline (Phase 6) builds `transcode.Request` from the Stage 3–6 results.

### Phase 6 — Final Assembly ✅
Code lives in `source/pipeline/` (plus `/ready` and `/keys` in `source/api/`).
- [x] **Pipeline orchestrator** (`pipeline.go`): stages `resolving(+downloading)` → `analyzing` (Stage 3 + 5) → `extracting` (Stage 4 + per-episode Stage 6: chapters, heuristics, fingerprints → `timestamps.json`) → `transcoding` → `packaging` (Stage 8 + series correlation) → `done`
  - SQLite checkpoint at the start of every stage; each stage reads the previous stage's `meta/*.json`, so it can run on its own after a restart
  - Resume rules: `downloading` restarts at resolve (needs metadata; the downloaded data and piece db are reused); a stage that needs `raw/episode.mkv` falls back to the download if it's gone; `packaging` without transcode output goes back to `transcoding`
  - **Cache hit** (moved up from Phase 7): if `output/{anime_id}/{ep_id}/` already has a valid master, the episode is marked done straight away — required anyway, since a finished `ep_id` may be resubmitted (§3) and must not be reprocessed
  - Disk check after resolve: fail if the work disk has less than 2× the episode size free
  - Progress: resolve 1, download 2→40 (live), analyze 40, extract 42, transcode 50, package 95, done 100
  - Panics → `failed`; a cancelled context (shutdown) leaves the checkpoint untouched
- [x] **Packager** (`packager.go`): staging dir + atomic rename, fonts copied from the cache under each episode's own names, `meta/{timestamps,media_info,job}.json`, key published first, playlist/segment verification, orphan guard, raw MKV removed, `size_bytes`/`output_path` recorded
- [x] **Series correlation** (`series.go`): after each packaging, once `FINGERPRINT_MIN_EPISODES` episodes have fingerprints, correlate and `Merge` into every packaged episode's `timestamps.json`; serialized per anime_id with a re-run flag
- [x] **Callback Notifier** (`callback.go`): §3 payload, 4 attempts (1s/2s/4s), background with shutdown wait
- [x] **Worker pool** (`pool.go`): `MAX_CONCURRENT_TORRENTS + MAX_FFMPEG_WORKERS` workers; slots in the pipeline cap concurrent downloads (`MAX_CONCURRENT_TORRENTS`) and transcodes (`MAX_FFMPEG_WORKERS`), so new downloads start while earlier episodes transcode. In-flight episodes from a previous run are resumed first; idle workers wake on job submission or every 5s.
- [x] **Graceful shutdown** (`main.go`, moved up from Phase 7): stop claiming, 10s grace, then cancel in-flight work (it resumes from its checkpoint); `queued` stays `queued`; wait for callbacks
- [x] **Cleanup Daemon** (`cleanup.go`): sweep at start and every `CLEANUP_INTERVAL_MINUTES`; done/failed TTLs from `updated_at`; fonts released before the dir is removed (so a crash between the two is retried); never touches `output/`
- [x] **GET /ready** (`api/ready.go`): ffmpeg/ffprobe found, DB ping, work dir writable → else 503 and `POST /job` / append return 503; free disk below `MIN_FREE_DISK_GB` is a warning; cached 10s
- [x] **GET /keys/{anime_id}/{ep_id}.key**: serves from `KEY_DIR`, IDs validated (no traversal), `Access-Control-Allow-Origin: KEY_CORS_ORIGIN`, `Cache-Control: private`. Auth/tokens belong in a reverse proxy in front.
- [x] **Tests:** end-to-end with a local seeder, real ffmpeg and a callback server — full run (output tree, key only in KEY_DIR, raw removed, no staging leftovers, callback with chapter timestamps), cache hit with the seeder gone, unreachable magnet → failed job + failed callback, resume from `analyzing` after a simulated crash, cleanup sweep (work dirs + font cache gone, output untouched, second sweep no-op); package verification; resume rules; `/ready` + 503 gating; key endpoint incl. traversal attempts. Plus a smoke run of the real binary.
- **Known gaps:** no API to retry a failed episode (resubmit under a new `job_id`; the in-flight check allows it); orphaned font dirs (ref count never reaching 0) aren't scanned for yet; anacrolix logs harmless `.part` warnings from the *test* seeder.

### AniList discovery ✅ (added after Phase 6)
- [x] **Port** of anitorrent's shared matching (`source/discovery/match.go`, `classify.go`) and six sources (`animetosho.go`, `seadex.go`, `subsplease.go`, `nyaa.go` for nyaa/yameii/toonshub)
- [x] **AniList client** (`anilist.go`): show facts, prequel chain → absolute-episode candidates, not-aired detection; 429/404 handling, 6h cache
- [x] **Merge + rank + pick** (`discover.go`), config `DISCOVERY_*`
- [x] **Endpoints:** `GET /api/anilist/{id}/search`, `POST /api/anilist/{id}/episodes`
- [x] **License:** GPL-3.0 `LICENSE` + `NOTICE` (attribution)
- [x] **Tests:** matching rules incl. every rewritten lookahead, absolute numbering, movies, not-aired, ranking/merge; every source against fake APIs (incl. AnimeTosho host fallback, Nyaa 429 retry, DDoS-Guard UA, base32 magnets); endpoints (partial results, 404 with no job, movie, idempotency, validation); opt-in live test `DISCOVERY_LIVE=1 go test ./source/discovery/ -run Live -v`
- [ ] **Batch support:** pick an episode's file inside a batch torrent and share one download between episodes

### Real-world test run ✅ (2026-10-23)
Five openly licensed Blender films (Caminandes 1–3, Spring, Sprite Fright; CC-BY / public-domain mark) from archive.org torrents, as one job (`blender-shorts`, ep1–ep5), in `testrun/` (gitignored) with `H264_PRESET=veryfast`, `SERVE_MEDIA=true`, keys served locally. Result: all five `done`, 574 MB output (1.19 GB including work dirs), each one decoded end to end by ffmpeg through `/media` + `/keys`, and played in a browser via `/play` (hls.js, AES-128, intro skip). `testrun/verify.sh` re-checks a run.

Bugs it found and fixed:
- **anacrolix v1.61.0 panic** in web-seed requesting (whole process crashed mid-run) → pinned to upstream fix commit.
- **Web seeds lost after a restart:** `AddMagnet` took info + trackers from the metainfo cache but not the `url-list`, and with the info known anacrolix never re-fetches the `xs=` source → web-seed-only torrents stalled forever. Fixed; `TestWebseedOnlyAcrossRestart` fails without the fix.
- **Sample filter rejected short episodes** (fixed 50 MB) → `MIN_VIDEO_FILE_MB`.
- **Player track lists empty:** hls.js fills audio/subtitle tracks on their own events, not `MANIFEST_PARSED`.
- anacrolix logs now at error level (its warnings were thousands of harmless "short write" hashing lines that buried the panic).

Also exercised for real: crash recovery (4 episodes resumed from checkpoints, metadata from cache, finished download reused), graceful shutdown, fast path (Caminandes 2: 1080p copied, BANDWIDTH reflects the source's real 13 Mbit/s peak), no upscaling (720p/804p sources → 720p only).

Observed, not bugs but worth knowing: untagged audio is labelled Japanese (the "first track = jpn" anime rule); the silence/black heuristic marked Spring's quiet 90 s opening as an intro.

### Encoding: GPU/hybrid, then VPS standard (2026-10-23 → 25)

**Hosting target is now a CPU-only VPS** (supersedes the dual-Xeon assumption above). The dev PC (RTX 3070 Ti + i9-13900K) is configured to mirror it in `testrun/.env`: `ENCODER=cpu`, `FFMPEG_THREADS=4`, `MAX_FFMPEG_WORKERS=1`.

**GPU support (kept as an option):** `ENCODER=auto|cpu|gpu`. At startup a real NVENC encode is probed (`transcode.ProbeNVENC`), since ffmpeg builds include `h264_nvenc` even without an NVIDIA GPU. With a GPU, transcodes are scheduled hybrid: a free GPU slot first, else a CPU slot, else whichever frees first (`pipeline.acquireEncoder`). The GPU path stays on the card (`-hwaccel cuda` → `scale_cuda` → `h264_nvenc` with `-forced-idr 1 -no-scenecut 1` and the same forced-keyframe list, so renditions still cut identically — verified with real GPU tests). A failed GPU run is redone on the CPU. Measured (1080p+720p, Caminandes 2): x264 veryfast 14.8× realtime; NVENC p1 22×; 2 GPU jobs = 1 (one NVENC engine on consumer cards, hence `GPU_WORKERS=1`); GPU+CPU together 23.8×. At equal size NVENC p4 scored VMAF 93.9 vs x264 veryfast 92.5. Intel Quick Sync failed to open an MFX session with the local ffmpeg build and isn't implemented.

**Episode size fix.** Real episodes (Bleach TYBW, AMZN/HULU WEB-DLs) came out at 1.5–1.8 GB: the fast path copied 6–9 Mbit/s source streams unchanged. Now:
- `FAST_PATH_MAX_KBPS=4000`: copy only small sources; unknown bitrate counts as large.
- VPS defaults from a 4-thread benchmark on that WEB-DL (5-min clip, per 24-min episode, 1080p+720p video):

| x264 | Size | Encode (4 threads) | VMAF |
|---|---|---|---|
| veryfast CRF 21 | 782 MB | 6.8 min | 98.9 |
| faster CRF 21 | 864 MB | 9.2 min | 99.4 |
| medium CRF 21 | 873 MB | 17.1 min | 99.5 |
| veryfast CRF 23 | 647 MB | 7.1 min | 98.2 |
| **veryfast CRF 25** | **535 MB** | **7.1 min** | **96.8** |
| veryfast CRF 27 | 439 MB | 7.0 min | 94.6 |
| *copied source (old)* | *~1,730 MB* | — | — |

  → `H264_PRESET=veryfast`, `H264_CRF_WEB=25`, `H264_CRF_BD=24`, `MAX_FFMPEG_WORKERS=1`, auto-adjust ceiling raised to CRF 28, NVENC cq 31/30. `FFMPEG_THREADS` caps ffmpeg threads.

**Discovery fixes found in real use** (torClient additions on top of the anitorrent port, each with a regression test using the real titles):
- *Sibling cours:* Bleach TYBW part 4 ep 3 matched part 3's "Soukoku Tan - 03". Words used only by prequels/sequels in AniList's relation chain (now walked transitively in both directions) disqualify a release's show name.
- *Two-word titles:* anitorrent needs 2 title-word hits only from 3 words up; "No Game No Life" ({game, life}) matched "Otome Game…". Now 2 hits from 2 words.
- *Mostly-foreign names:* reject when ≥ 2 and ≥ half of the show-name words appear in none of the show's AniList titles ("…My Next Life as a Villainess" for NGNL; "Tokyo Revengers: Santen Sensou Hen" for season 1, which AniList's chain doesn't link). One stray word ("Journeys") is allowed.
- Live re-check of all known cases picks correctly (Frieren S1/S2, Suzume, One Piece, Bleach parts, NGNL, Tokyo Revengers).

**Web UI** (`SERVE_MEDIA=true`): `/` takes an AniList ID, first episode, count and a dub preference, calls `POST /api/anilist/{id}/episodes`, shows the picked release per episode and live per-episode progress, and links each finished episode to the player. The player (`/play/{anime}/{ep}`, hls.js with SRI) has quality/audio/subtitle pickers and "Skip intro"/"Skip outro".

### Phase 7 — Polish
- [ ] **Infohash dedup:** look up `episodes.infohash`, recreate the existing output tree at the new path with per-file hardlinks
- [x] **Cache-hit fast path:** done in Phase 6 (pipeline start)
- [x] **Graceful shutdown:** done in Phase 6
- [ ] **Retry endpoint:** `POST /job/{id}/retry` to requeue failed episodes from their checkpoint
- [ ] **Font orphan scan:** compare `fonts/` with `font_cache` periodically
- [ ] **Prometheus metrics:** `/metrics` endpoint, counters for jobs/stages/failures
- [ ] **Structured logging audit:** ensure every stage logs begin/end/error with job_id
- [ ] **Error handling review:** verify every edge case from §7 is covered
- [ ] **Integration tests:** end-to-end test with small torrent
- [ ] **Docker finalize:** Ubuntu image with jellyfin-ffmpeg + mkvtoolnix, non-root user
- [ ] **README:** setup instructions, API docs, architecture overview

---

## 10. Releasing Anime Support (Ongoing Series)

### Problem
A 12-episode season is submitted and processed. Weeks later, episode 13 releases. The user needs to add ep13 to the existing `anime_id` without resubmitting the whole series.

### Solution: Append endpoint
`POST /api/anime/{anime_id}/episodes` — accepts a new job with episodes that get placed under the existing `anime_id`'s output directory.

### How it works end-to-end

1. **Initial submission:** `POST /job` with `anime_id="onepiece-s1"`, episodes=[ep1..ep12]
   - Output: `output/onepiece-s1/{S01E01..S01E12}/`
2. **External cron detects new release:** The cron (separate service, not torClient) monitors torrent indexers, finds ep13's magnet, constructs payload
3. **Append call:** `POST /api/anime/onepiece-s1/episodes` with ep13
   - torClient creates a new job (new `job_id`) but reuses `anime_id="onepiece-s1"`
   - Processes ep13 → `output/onepiece-s1/S01E13/`
4. **Callback:** torClient POSTs completion to the cron's webhook
5. **Frontend:** Serves `output/onepiece-s1/` — now sees 13 episodes

### Key design points
- **No special state needed** — the `anime_id` field already groups episodes. The append endpoint is just syntactic sugar over `POST /job` that validates the `anime_id` exists
- **Cache-hit safety** — if ep13 was already processed (e.g. duplicate call), `output/{anime_id}/{ep_id}/master.m3u8` exists → immediate `done`
- **Fingerprint pool grows** — the new episode's audio fingerprint is added to the existing `anime_id`'s pool, improving intro/outro detection for all episodes
- **No locking** — jobs are independent. A new job for an existing `anime_id` doesn't block the old one
- **Discovery is optional** — the cron can send magnets itself, or just AniList ID + episode numbers via `POST /api/anilist/{id}/episodes` (§3a)

### Edge cases
| Scenario | Handling |
|---|---|
| **Same ep_id submitted twice** | Cache-hit → immediate `done`. No re-download |
| **ep_id out of order (ep14 before ep13)** | No problem. Both go to correct output dirs. Order is the caller's concern |
| **anime_id doesn't exist yet** | 404 from append endpoint. Use `POST /job` for first submission |
| **Callback to cron fails** | Standard retry (3x exponential backoff). Cron can also poll GET /job/{job_id} |
| **Cron shuts down mid-wait** | Stateless — on restart, cron can check which episodes are missing and resubmit |
| **Disk space for increment** | Same guard as normal jobs — check before download |
| **Ongoing series finishes (all eps done)** | Cron removes it from its tracked list, or marks it complete. No action needed from torClient |

---

## Key Design Decisions Summary

1. **Single ffmpeg process for multi-resolution** — one decode, two parallel encodes. Saves ~40% time vs sequential encodes. If OOM, fall back to sequential.

2. **Audio-first intro detection** — silence detection on Japanese audio track is most reliable for anime. Black frame detection as fallback.

3. **Subtitle separation** — ASS for quality, WEBVTT for HLS compatibility. Fonts extracted for ASS rendering.

4. **PGS rejection** — PGS is bitmap-based, can't be reliably repackaged for HLS. PGS tracks are skipped; an episode fails only if every subtitle track it has is PGS.

5. **SQLite for job state** — ACID transactions, atomic status transitions across concurrent workers, `UNIQUE` constraint for dedup, queryable. Analysis metadata stays as JSON in work dirs (write-once, never queried).

6. **Crash-safe** — SQLite WAL mode + atomic writes. Recovery on restart: every non-terminal episode resumes from its checkpointed stage (`downloading` resumes from the persisted torrent state). An episode is failed only if its stage fails again after resuming.

7. **Work dir TTL cleanup** — no complex GC. Delete done episodes' work dirs after `WORK_TTL_HOURS`, failed ones after the longer `FAILED_WORK_TTL_HOURS` so their meta/ survives for re-runs.

8. **HLS as output format** — universal browser support, adaptive bitrate, seeking, subtitle switching. Best format for a media-server frontend.

9. **No intermediate re-encode for 720p** — encode once at 1080p, downscale to 720p. Saves encode time.

10. **CRF-based encoding** — targets quality, not bitrate. Maxrate cap prevents bitrate spikes. For 500MB target, CRF 22/21 with maxrate cap naturally converges to the right size for 24min anime.

11. **AES-128 HLS encryption** — one 16-byte key per episode (addressed `{anime_id}/{ep_id}`), random IV, stored in `KEY_DIR` outside the output tree. ffmpeg's `-hls_key_info_file` handles segment encryption natively. Key served separately from segments, enabling access control at the key endpoint. Near-zero CPU cost (AES-NI), huge win against casual leeching.

12. **jellyfin-ffmpeg** — custom ffmpeg build with all codecs + libass + HW accel support. Replaces vanilla ffmpeg. Distributed via `.deb`, requires Ubuntu base image (not Alpine). See [§8 Dependencies](#8-dependencies).

13. **Releasing anime via append endpoint** — external cron calls `POST /api/anime/{anime_id}/episodes` to add new episodes to an existing series. The `anime_id` scoping makes incremental episodes slot into the existing output tree automatically.

14. **Built-in AniList discovery** (§3a) — a port of anitorrent's six sources and matching rules, so callers can submit AniList IDs instead of magnets. Ported GPL-3.0 code makes torClient GPL-3.0 (`LICENSE`, `NOTICE`). Auto-pick is conservative: exact single-episode releases only.

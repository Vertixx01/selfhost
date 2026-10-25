# torClient

Self-hosted anime pipeline: give it an AniList ID and episode numbers (or magnet links), and it finds the torrents, downloads them, analyzes the video, detects intros/outros, transcodes to encrypted HLS, and serves it to a browser player.

```
AniList ID ─► find releases ─► torrent download ─► analyze ─► subs/fonts ─► intro/outro
            (6 sources)        (anacrolix)        (ffprobe)              detection
                                                                             │
browser ◄── /play player ◄── output/{anime}/{ep}/ (HLS) ◄── package ◄── transcode (x264 / NVENC)
```

Design notes, measurements and the full plan are in [PROJECT.MD](PROJECT.MD).

## Features

- **AniList discovery** — searches AnimeTosho, SeaDex, SubsPlease, nyaa.si, Yameii and ToonsHub (a Go port of [anitorrent](https://github.com/anh9000/anitorrent)), handles absolute vs per-season numbering, and auto-picks the best exact single-episode release (resolution → preferred groups → seeders → source accuracy).
- **Torrent download** of only the episode file, with web-seed support, stall/no-peer watchdogs and resume after restart.
- **Analysis** — stream map, BD/WEB detection, subtitle extraction (ASS/SRT → WebVTT, signs vs dialogue), embedded fonts (deduplicated cache), audio language mapping.
- **Intro/outro detection** — chapters, then cross-episode audio fingerprints (Chromaprint), then silence/black-frame heuristics.
- **HLS output** — 1080p + 720p (never upscaled) and per-language audio renditions, segments cut at identical points in every rendition and on OP/ED boundaries, AES-128 encrypted, master playlist with measured bandwidth.
- **Encoders** — CPU (x264) by default; NVIDIA NVENC and a GPU + CPU hybrid scheduler when a GPU is present.
- **Robust pipeline** — SQLite checkpoints per stage, crash recovery, worker pool, graceful shutdown, completion callbacks, work-dir cleanup, `/ready` health checks.
- **Web UI** — request an anime by AniList ID, with search options (resolution, preferred groups, excluded words, sources, dub, prefer H.264). *Search* lists every torrent found per episode (group, resolution, codec, size, seeders, age, sources, match quality) so you can choose; *Quick download* auto-picks. Watch progress, cancel/retry/remove jobs, delete episodes, see storage use, and play episodes (hls.js) with quality/audio/subtitle selection and "Skip intro".
- **Day-to-day controls** — cancel running jobs, retry failed episodes, remove finished jobs, delete packaged episodes, and an optional storage cap (`MAX_STORAGE_GB`) that refuses new downloads or auto-deletes the oldest episodes (`STORAGE_AUTO_DELETE`).

## Requirements

- Go 1.25+ (to build)
- ffmpeg + ffprobe 6.0+ on `PATH` or configured (the Docker plan uses jellyfin-ffmpeg; it needs the `chromaprint` muxer for fingerprint intro detection — optional)
- Optional: `mkvextract` (mkvtoolnix) as a fallback for odd subtitle/font tracks
- Optional: NVIDIA GPU + driver for NVENC

## Quick start

```bash
git clone https://github.com/Vertixx01/selfhost.git
cd selfhost
cp .env.example .env          # then edit: paths, HLS_KEY_URI_BASE, LISTEN_ADDR, ...
CGO_ENABLED=0 go build -o torClient .
./torClient
```

Build with `CGO_ENABLED=0` (pure-Go SQLite; also avoids a broken MinGW toolchain on some Windows machines).

For the built-in web UI, set `SERVE_MEDIA=true`, point `HLS_KEY_URI_BASE` at this server (e.g. `http://127.0.0.1:8080/keys`), and open `http://127.0.0.1:8080/`.

> On first start torClient may re-launch itself once: the torrent library must use classic (non-mmap) file IO, which can only be selected through an environment variable read at process start. You'll see two `torClient` processes; that's expected.

## Hosting on a VPS

The defaults target a CPU-only VPS:

| Setting | Default | Why |
|---|---|---|
| `ENCODER` | `auto` | Uses NVENC only if a working NVIDIA GPU is found; on a VPS that means CPU |
| `H264_PRESET` | `veryfast` | Measured on 4 threads: 3.4× realtime (a 24-min episode encodes in ~7 min); slower presets were 2–4× slower and *larger* |
| `H264_CRF_WEB` / `_BD` | `25` / `24` | ~535 MB per 24-min episode (1080p + 720p video), VMAF 96.8 vs the source |
| `FAST_PATH_MAX_KBPS` | `4000` | Only stream-copy 1080p when the source is small; 6–9 Mbit/s WEB-DLs are re-encoded (they made 1.1–1.5 GB 1080p renditions) |
| `MAX_FFMPEG_WORKERS` | `1` | One encode at a time; a second one on a small VPS just halves both |
| `FFMPEG_THREADS` | `0` (all) | Set to the vCPU count if other services share the box |

Sizing guide (per 24-min 1080p episode): download ~1–1.5 GB temporarily (deleted after packaging), ~550 MB final output, ~7 min encode on 4 vCPUs.

Put a reverse proxy (nginx/Caddy) in front for TLS, and add authentication there for `/keys/...` and the UI: torClient itself has no user accounts. `GET /keys` is what makes the encryption meaningful — restrict who can fetch keys.

## Configuration

All settings are environment variables (or `.env`); see [.env.example](.env.example) for every option with comments. The important ones:

| Variable | Purpose |
|---|---|
| `WORK_DIR`, `OUTPUT_DIR`, `TORRENT_DIR`, `DB_PATH`, `FONT_CACHE_DIR`, `KEY_DIR` | Storage locations (`KEY_DIR` must be outside `OUTPUT_DIR`) |
| `LISTEN_ADDR` | HTTP address, e.g. `:8080` or `127.0.0.1:8080` |
| `HLS_ENCRYPT`, `HLS_KEY_URI_BASE`, `KEY_CORS_ORIGIN` | AES-128 encryption and where players fetch keys |
| `SERVE_MEDIA` | Serve `OUTPUT_DIR` at `/media` plus the web UI (otherwise serve `output/` with your own web server/CDN) |
| `MAX_CONCURRENT_TORRENTS`, `MAX_FFMPEG_WORKERS`, `GPU_WORKERS` | Concurrency |
| `ENCODER`, `NVENC_PRESET`, `NVENC_CQ_BD/WEB` | Encoder selection and GPU quality |
| `DISCOVERY_SOURCES`, `DISCOVERY_RESOLUTION`, `DISCOVERY_PREFERRED_GROUPS`, `DISCOVERY_EXCLUDE` | AniList discovery |
| `WORK_TTL_HOURS`, `FAILED_WORK_TTL_HOURS` | When work dirs are cleaned |
| `MAX_STORAGE_GB`, `STORAGE_AUTO_DELETE` | Cap on work + output disk use (0 = none); at the cap, refuse new downloads or delete the oldest packaged episodes |

## API

| Method | Path | Description |
|---|---|---|
| `POST` | `/api/anilist/{anilist_id}/episodes` | Find releases and queue a job: `{"job_id","episodes":[1,2,3],"callback_url"?,"anime_id"?}` plus optional search options `resolution`, `groups`, `exclude`, `sources`, `dub`, `prefer_h264`, and `picks: {"2": {"magnet","title","source"}}` to use a chosen torrent instead of the auto-pick |
| `GET` | `/api/anilist/{anilist_id}/search?episode=N` | Ranked candidates and the pick (no download). Options: `resolution=1080\|720\|any`, `groups=a,b`, `exclude=x265,dub`, `sources=nyaa,seadex`, `dub=1`, `h264=1` |
| `POST` | `/job/{job_id}/cancel` | Stop running work; unfinished episodes become failed ("cancelled by user") |
| `POST` | `/job/{job_id}/retry` | Re-queue failed episodes; body `{"episodes":["ep2"]}` optional |
| `DELETE` | `/job/{job_id}` | Remove a finished job and its work files (packaged episodes stay) |
| `DELETE` | `/api/anime/{anime_id}/episodes/{ep_id}` | Delete a packaged episode (output, key, fingerprints) |
| `GET` | `/api/storage` | Bytes used, the cap, auto-delete, free disk |
| `POST` | `/job` | Queue a job from magnets: `{"job_id","anime_id","title","episodes":[{"ep_id","magnet_url",...}]}` |
| `POST` | `/api/anime/{anime_id}/episodes` | Append episodes to an existing anime |
| `GET` | `/job/{job_id}` | Job + per-episode status, progress, size, output path |
| `GET` | `/jobs?status=&anime_id=&limit=` | List jobs |
| `GET` | `/health`, `/ready` | Liveness; readiness (ffmpeg, DB, disk) |
| `GET` | `/keys/{anime_id}/{ep_id}.key` | AES-128 key for players |
| `GET` | `/`, `/play`, `/play/{anime_id}/{ep_id}` | Web UI and player (`SERVE_MEDIA=true`) |
| `GET` | `/media/{path}` | Output files (`SERVE_MEDIA=true`) |

`job_id`, `anime_id` and `ep_id` must match `^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$` (they become directory names). When every episode of a job is finished, torClient POSTs a summary to `callback_url`.

Example:

```bash
curl -X POST localhost:8080/api/anilist/154587/episodes \
  -H 'Content-Type: application/json' \
  -d '{"job_id":"frieren-1","episodes":[1,2,3]}'
curl localhost:8080/job/frieren-1
```

## Output layout

```
output/{anime_id}/{ep_id}/
  master.m3u8
  1080p/ 720p/          stream.m3u8 + seg0000.ts … (AES-128)
  audio/{lang}/         stream.m3u8 + segments
  subs/                 {lang}.vtt + .m3u8 (HLS), {lang}.ass, fonts/
  meta/                 timestamps.json (intro/outro), media_info.json, job.json
keys/{anime_id}/{ep_id}.key   (outside output/, served by /keys)
```

## Development

```bash
CGO_ENABLED=0 go test ./...                    # unit + real-ffmpeg tests (skipped without ffmpeg)
DISCOVERY_LIVE=1 CGO_ENABLED=0 go test ./source/discovery/ -run Live -v   # live search against the real sites
```

GPU tests run only where NVENC works. The pipeline test downloads a generated torrent from a local seeder; nothing touches the network except the opt-in live discovery test.

| Package | Role |
|---|---|
| `config` | Environment configuration |
| `source/api` | HTTP API, web UI, key and media serving |
| `source/discovery` | AniList client and the six torrent sources (anitorrent port) |
| `source/resolvers`, `source/torrent` | Magnet → file selection; download engine |
| `source/media` | ffprobe stream map, subtitles, fonts, audio mapping |
| `source/intro` | Intro/outro detection, keyframe planning |
| `source/transcode` | HLS encoding (x264/NVENC), encryption, master playlist |
| `source/pipeline` | Stage orchestration, worker pool, packaging, callbacks, cleanup |
| `source/utils` | SQLite store, font cache, paths |

## License

GPL-3.0 — see [LICENSE](LICENSE). `source/discovery/` is derived from anitorrent (GPL-3.0); see [NOTICE](NOTICE).

You are responsible for what you download. Only use torClient for content you have the right to obtain and host.

# torClient on Ubuntu 24.04 with jellyfin-ffmpeg. See deploy/ for the
# docker-compose setup (Caddy with HTTPS and a login in front).

FROM golang:1.25-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/torClient .

FROM ubuntu:24.04

# jellyfin-ffmpeg comes as .deb packages from Jellyfin's apt repo (it has the
# chromaprint muxer used for fingerprint intro detection). jellyfin-ffmpeg8
# also exists; override with --build-arg FFMPEG_PKG=jellyfin-ffmpeg8.
ARG FFMPEG_PKG=jellyfin-ffmpeg7
RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates curl gnupg tzdata mkvtoolnix \
    && install -d /etc/apt/keyrings \
    && curl -fsSL https://repo.jellyfin.org/jellyfin_team.gpg.key \
        | gpg --dearmor -o /etc/apt/keyrings/jellyfin.gpg \
    && echo "deb [signed-by=/etc/apt/keyrings/jellyfin.gpg] https://repo.jellyfin.org/ubuntu noble main" \
        > /etc/apt/sources.list.d/jellyfin.list \
    && apt-get update && apt-get install -y --no-install-recommends ${FFMPEG_PKG} \
    && ln -s /usr/lib/jellyfin-ffmpeg/ffmpeg /usr/local/bin/ffmpeg \
    && ln -s /usr/lib/jellyfin-ffmpeg/ffprobe /usr/local/bin/ffprobe \
    && apt-get purge -y gnupg && apt-get autoremove -y \
    && rm -rf /var/lib/apt/lists/*

COPY --from=build /out/torClient /usr/local/bin/torClient

# Ubuntu 24.04 images ship a "ubuntu" user with UID 1000; run as that.
RUN install -d -o ubuntu -g ubuntu /srv/torclient
USER ubuntu
WORKDIR /srv/torclient

# TORRENT_STORAGE_DEFAULT_FILE_IO: anacrolix's mmap IO never releases files;
# setting it here saves torClient re-executing itself to set it.
ENV FFMPEG_PATH=/usr/lib/jellyfin-ffmpeg/ffmpeg \
    FFPROBE_PATH=/usr/lib/jellyfin-ffmpeg/ffprobe \
    MKVEXTRACT_PATH=mkvextract \
    TORRENT_STORAGE_DEFAULT_FILE_IO=classic \
    WORK_DIR=/srv/torclient/work \
    OUTPUT_DIR=/srv/torclient/output \
    TORRENT_DIR=/srv/torclient/torrents \
    DB_PATH=/srv/torclient/data/torclient.db \
    FONT_CACHE_DIR=/srv/torclient/fonts \
    KEY_DIR=/srv/torclient/keys \
    LISTEN_ADDR=:8080

EXPOSE 8080 42069/tcp 42069/udp
HEALTHCHECK --interval=30s --timeout=5s --start-period=20s \
    CMD curl -fsS http://127.0.0.1:8080/health || exit 1
ENTRYPOINT ["/usr/local/bin/torClient"]

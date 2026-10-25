// Package config loads torClient settings from the environment (and .env, if present).
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
)

type Config struct {
	// Directories
	WorkDir      string
	OutputDir    string
	TorrentDir   string
	DBPath       string
	FontCacheDir string
	KeyDir       string

	// Concurrency
	MaxConcurrentTorrents int
	MaxFFmpegWorkers      int

	// Network
	TorrentPort int
	DHT         bool
	PeerLimit   int

	MinVideoFileMB int // smaller files are treated as samples/extras

	// Torrent timeouts (PROJECT.MD Stage 1-2)
	ResolveAttempts int
	ResolveTimeout  time.Duration // per metadata attempt
	ResolveBackoff  time.Duration
	NoPeersTimeout  time.Duration
	StallTimeout    time.Duration
	DownloadTimeout time.Duration

	// FFmpeg
	FFmpegPath     string
	FFprobePath    string
	MkvextractPath string // optional fallback for subs/fonts; "" disables

	// Transcode (bitrates in kbit/s)
	CRFBD            int
	MaxrateBDKbps    int
	BufsizeBDKbps    int
	CRFWeb           int
	MaxrateWebKbps   int
	BufsizeWebKbps   int
	Preset           string
	Tune             string
	AudioBitrateKbps int
	AudioChannels    int
	AudioSampleRate  int
	SegmentDuration  int
	FastPathMaxKbps  int // copy 1080p only at or below this source bitrate (0 = always)
	FFmpegThreads    int // 0 = all cores; set to the VPS vCPU count to mirror it

	// Encoder selection: "auto" (GPU + CPU hybrid when an NVIDIA GPU works,
	// else CPU), "cpu", or "gpu" (NVENC only).
	Encoder     string
	GPUWorkers  int // concurrent NVENC encodes (one engine on consumer cards: 1)
	NvencPreset string
	NvencCQBD   int
	NvencCQWeb  int

	// Encryption
	HLSEncrypt    bool
	HLSKeyURIBase string
	KeyCORSOrigin string // Access-Control-Allow-Origin on /keys; "" = no header

	// Intro detection
	FingerprintMinEpisodes int

	// AniList discovery
	DiscoverySources         []string // empty = all
	DiscoveryResolution      string
	DiscoveryPreferredGroups []string
	DiscoveryExclude         []string

	// Cleanup
	WorkTTLHours           int
	FailedWorkTTLHours     int
	CleanupIntervalMinutes int
	MinFreeDiskGB          int  // /ready warns below this
	MaxStorageGB           int  // cap on work + output; 0 = none
	StorageAutoDelete      bool // make room by deleting the oldest episodes

	// HTTP
	ListenAddr string
	ServeMedia bool // GET /media + /play from OUTPUT_DIR (testing / small setups)

	// Logging
	LogLevel slog.Level
}

// Load reads envFile (ignored if missing) and then the process environment.
// Variables already set in the environment win over the file.
func Load(envFile string) (*Config, error) {
	if err := godotenv.Load(envFile); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("load %s: %w", envFile, err)
	}

	p := &parser{}
	c := &Config{
		WorkDir:      p.str("WORK_DIR", "./work"),
		OutputDir:    p.str("OUTPUT_DIR", "./output"),
		TorrentDir:   p.str("TORRENT_DIR", "./torrents"),
		DBPath:       p.str("DB_PATH", "./data/torclient.db"),
		FontCacheDir: p.str("FONT_CACHE_DIR", "./fonts"),
		KeyDir:       p.str("KEY_DIR", "./keys"),

		MaxConcurrentTorrents: p.posInt("MAX_CONCURRENT_TORRENTS", 3),
		MaxFFmpegWorkers:      p.posInt("MAX_FFMPEG_WORKERS", 1),

		TorrentPort: p.posInt("TORRENT_PORT", 42069),
		DHT:         p.boolean("DHT", true),
		PeerLimit:   p.posInt("PEER_LIMIT", 50),

		MinVideoFileMB:  p.posInt("MIN_VIDEO_FILE_MB", 50),
		ResolveAttempts: p.posInt("RESOLVE_ATTEMPTS", 3),
		ResolveTimeout:  p.duration("RESOLVE_TIMEOUT_SECONDS", 60, time.Second),
		ResolveBackoff:  p.duration("RESOLVE_BACKOFF_SECONDS", 10, time.Second),
		NoPeersTimeout:  p.duration("NO_PEERS_TIMEOUT_SECONDS", 120, time.Second),
		StallTimeout:    p.duration("STALL_TIMEOUT_MINUTES", 5, time.Minute),
		DownloadTimeout: p.duration("DOWNLOAD_TIMEOUT_MINUTES", 30, time.Minute),

		FFmpegPath:     p.str("FFMPEG_PATH", "/usr/lib/jellyfin-ffmpeg/ffmpeg"),
		FFprobePath:    p.str("FFPROBE_PATH", "/usr/lib/jellyfin-ffmpeg/ffprobe"),
		MkvextractPath: p.str("MKVEXTRACT_PATH", "mkvextract"),

		CRFBD:            p.posInt("H264_CRF_BD", 24),
		MaxrateBDKbps:    p.kbps("H264_MAXRATE_BD", "6000k"),
		BufsizeBDKbps:    p.kbps("H264_BUFSIZE_BD", "12000k"),
		CRFWeb:           p.posInt("H264_CRF_WEB", 25),
		MaxrateWebKbps:   p.kbps("H264_MAXRATE_WEB", "5000k"),
		BufsizeWebKbps:   p.kbps("H264_BUFSIZE_WEB", "20000k"),
		Preset:           p.str("H264_PRESET", "veryfast"),
		Tune:             p.str("H264_TUNE", "animation"),
		AudioBitrateKbps: p.kbps("AUDIO_BITRATE", "128k"),
		AudioChannels:    p.posInt("AUDIO_CHANNELS", 2),
		AudioSampleRate:  p.posInt("AUDIO_SAMPLERATE", 48000),
		SegmentDuration:  p.posInt("SEGMENT_DURATION", 6),
		FastPathMaxKbps:  p.nonNegInt("FAST_PATH_MAX_KBPS", 4000),
		FFmpegThreads:    p.nonNegInt("FFMPEG_THREADS", 0),

		Encoder:     strings.ToLower(p.str("ENCODER", "auto")),
		GPUWorkers:  p.posInt("GPU_WORKERS", 1),
		NvencPreset: p.str("NVENC_PRESET", "p1"),
		NvencCQBD:   p.posInt("NVENC_CQ_BD", 30),
		NvencCQWeb:  p.posInt("NVENC_CQ_WEB", 31),

		HLSEncrypt:    p.boolean("HLS_ENCRYPT", true),
		HLSKeyURIBase: strings.TrimRight(p.str("HLS_KEY_URI_BASE", "https://keys.example.com/keys"), "/"),
		KeyCORSOrigin: p.str("KEY_CORS_ORIGIN", "*"),

		FingerprintMinEpisodes: p.posInt("FINGERPRINT_MIN_EPISODES", 3),

		DiscoverySources:         csv(p.str("DISCOVERY_SOURCES", "")),
		DiscoveryResolution:      p.str("DISCOVERY_RESOLUTION", "1080"),
		DiscoveryPreferredGroups: csv(p.str("DISCOVERY_PREFERRED_GROUPS", "")),
		DiscoveryExclude:         csv(p.str("DISCOVERY_EXCLUDE", "")),

		WorkTTLHours:           p.posInt("WORK_TTL_HOURS", 3),
		FailedWorkTTLHours:     p.posInt("FAILED_WORK_TTL_HOURS", 72),
		CleanupIntervalMinutes: p.posInt("CLEANUP_INTERVAL_MINUTES", 15),
		MinFreeDiskGB:          p.posInt("MIN_FREE_DISK_GB", 10),
		MaxStorageGB:           p.nonNegInt("MAX_STORAGE_GB", 0),
		StorageAutoDelete:      p.boolean("STORAGE_AUTO_DELETE", false),

		ListenAddr: p.str("LISTEN_ADDR", ":8080"),
		ServeMedia: p.boolean("SERVE_MEDIA", false),

		LogLevel: p.level("LOG_LEVEL", slog.LevelInfo),
	}

	switch c.Encoder {
	case "auto", "cpu", "gpu":
	default:
		p.errs = append(p.errs, fmt.Errorf("ENCODER=%q: want auto, cpu or gpu", c.Encoder))
	}

	if inside, err := isWithin(c.KeyDir, c.OutputDir); err != nil {
		p.errs = append(p.errs, err)
	} else if inside {
		p.errs = append(p.errs, fmt.Errorf("KEY_DIR (%s) must not be inside OUTPUT_DIR (%s): keys would be served next to the segments", c.KeyDir, c.OutputDir))
	}

	if err := errors.Join(p.errs...); err != nil {
		return nil, err
	}
	return c, nil
}

// Dirs returns every directory torClient writes to, for creation at startup.
func (c *Config) Dirs() []string {
	return []string{c.WorkDir, c.OutputDir, c.TorrentDir, filepath.Dir(c.DBPath), c.FontCacheDir, c.KeyDir}
}

// parser collects every bad variable so startup reports them all at once.
type parser struct{ errs []error }

func (p *parser) str(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return def
}

func (p *parser) posInt(key string, def int) int {
	v := p.str(key, "")
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		p.errs = append(p.errs, fmt.Errorf("%s=%q: want a positive integer", key, v))
		return def
	}
	return n
}

func (p *parser) duration(key string, def int, unit time.Duration) time.Duration {
	return time.Duration(p.posInt(key, def)) * unit
}

func (p *parser) nonNegInt(key string, def int) int {
	v := p.str(key, "")
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		p.errs = append(p.errs, fmt.Errorf("%s=%q: want 0 or a positive integer", key, v))
		return def
	}
	return n
}

func (p *parser) boolean(key string, def bool) bool {
	v := p.str(key, "")
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		p.errs = append(p.errs, fmt.Errorf("%s=%q: want true or false", key, v))
		return def
	}
	return b
}

// kbps parses ffmpeg-style bitrates ("5000k", "5M", "128000") into kbit/s.
func (p *parser) kbps(key, def string) int {
	v := p.str(key, def)
	n, err := ParseKbps(v)
	if err != nil {
		p.errs = append(p.errs, fmt.Errorf("%s=%q: %w", key, v, err))
	}
	return n
}

func (p *parser) level(key string, def slog.Level) slog.Level {
	v := p.str(key, "")
	if v == "" {
		return def
	}
	var l slog.Level
	if err := l.UnmarshalText([]byte(v)); err != nil {
		p.errs = append(p.errs, fmt.Errorf("%s=%q: want debug, info, warn or error", key, v))
		return def
	}
	return l
}

func csv(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

// ParseKbps converts an ffmpeg bitrate string to kbit/s.
func ParseKbps(s string) (int, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	mult := 1.0 / 1000 // bare number = bit/s
	switch {
	case strings.HasSuffix(s, "k"):
		mult, s = 1, strings.TrimSuffix(s, "k")
	case strings.HasSuffix(s, "m"):
		mult, s = 1000, strings.TrimSuffix(s, "m")
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f <= 0 {
		return 0, errors.New("want a bitrate like 5000k or 5M")
	}
	return int(f * mult), nil
}

func isWithin(path, dir string) (bool, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return false, err
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return false, err
	}
	rel, err := filepath.Rel(absDir, absPath)
	if err != nil {
		return false, nil // different volumes
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))), nil
}

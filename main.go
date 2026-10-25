package main

import (
	"context"
	"errors"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"torclient/config"
	"torclient/source/api"
	"torclient/source/discovery"
	"torclient/source/intro"
	"torclient/source/media"
	"torclient/source/pipeline"
	"torclient/source/resolvers"
	"torclient/source/torrent"
	"torclient/source/transcode"
	"torclient/source/utils"
)

// shutdownGrace is how long in-flight episodes get to reach their next
// checkpoint after SIGINT/SIGTERM before their work is cancelled. Cancelled
// episodes keep their checkpoint and resume on the next start.
const shutdownGrace = 10 * time.Second

// torrentLogger keeps anacrolix at error level unless LOG_LEVEL=debug: its
// info output is per-peer chatter, and its warnings are dominated by harmless
// "finished hashing piece ... short write" lines for pieces that straddle
// files we deliberately don't download.
func torrentLogger(level slog.Level) *slog.Logger {
	if level > slog.LevelDebug {
		level = max(level, slog.LevelError)
	}
	return slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
}

// encoderSlots turns ENCODER into CPU and GPU transcode slot counts.
func encoderSlots(ctx context.Context, cfg *config.Config, logger *slog.Logger) (cpu, gpu int) {
	if cfg.Encoder == "cpu" {
		logger.Info("encoder: CPU only (x264)", "cpu_slots", cfg.MaxFFmpegWorkers)
		return cfg.MaxFFmpegWorkers, 0
	}
	pctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	err := transcode.ProbeNVENC(pctx, cfg.FFmpegPath)
	switch {
	case err == nil && cfg.Encoder == "gpu":
		logger.Info("encoder: GPU only (NVENC)", "gpu_slots", cfg.GPUWorkers)
		return 0, cfg.GPUWorkers
	case err == nil:
		logger.Info("encoder: hybrid GPU + CPU", "gpu_slots", cfg.GPUWorkers, "cpu_slots", cfg.MaxFFmpegWorkers, "nvenc_preset", cfg.NvencPreset)
		return cfg.MaxFFmpegWorkers, cfg.GPUWorkers
	case cfg.Encoder == "gpu":
		log.Fatalf("ENCODER=gpu but NVENC isn't usable: %v", err)
	}
	logger.Info("encoder: CPU only (no usable NVIDIA GPU)", "cpu_slots", cfg.MaxFFmpegWorkers, "probe", err.Error())
	return cfg.MaxFFmpegWorkers, 0
}

func main() {
	torrent.EnsureClassicFileIO() // may re-launch this process; see fileio.go

	cfg, err := config.Load(".env")
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	logger := utils.SetupLogger(cfg.LogLevel)

	for _, dir := range cfg.Dirs() {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			log.Fatalf("create %s: %v", dir, err)
		}
	}

	sigCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	workCtx, cancelWork := context.WithCancel(context.Background())
	defer cancelWork()

	db, err := utils.OpenDB(workCtx, cfg.DBPath)
	if err != nil {
		log.Fatalf("database: %v", err)
	}
	defer db.Close()

	engine, err := torrent.NewEngine(torrent.Config{
		ListenPort:  cfg.TorrentPort,
		DHT:         cfg.DHT,
		PeerLimit:   cfg.PeerLimit,
		MetainfoDir: filepath.Join(cfg.TorrentDir, "metainfo"),
		Logger:      torrentLogger(cfg.LogLevel),
	})
	if err != nil {
		log.Fatalf("torrent engine: %v", err)
	}
	defer engine.Close()

	layout := utils.Layout{WorkRoot: cfg.WorkDir, OutputRoot: cfg.OutputDir, FontRoot: cfg.FontCacheDir, KeyRoot: cfg.KeyDir}
	store := utils.NewJobStore(db)
	fonts := utils.NewFontCache(db, layout)
	tools := media.Tools{FFmpeg: cfg.FFmpegPath, FFprobe: cfg.FFprobePath}
	if p, err := exec.LookPath(cfg.MkvextractPath); err == nil {
		tools.Mkvextract = p
	} else {
		logger.Info("mkvextract not found; subtitle/font fallbacks off", "path", cfg.MkvextractPath)
	}

	cpuWorkers, gpuWorkers := encoderSlots(workCtx, cfg, logger)
	// Limit 0 only measures usage (for /api/storage); it never refuses work.
	storage := &utils.StorageGuard{
		Limit: int64(cfg.MaxStorageGB) << 30, AutoDelete: cfg.StorageAutoDelete,
		Dirs: []string{cfg.WorkDir, cfg.OutputDir}, Store: store, Layout: layout,
	}
	if cfg.MaxStorageGB > 0 {
		logger.Info("storage limit", "gb", cfg.MaxStorageGB, "auto_delete", cfg.StorageAutoDelete)
	}

	notifier := pipeline.NewNotifier(store, logger)
	pipe := pipeline.New(pipeline.Config{
		Layout:     layout,
		Encrypt:    cfg.HLSEncrypt,
		KeyURIBase: cfg.HLSKeyURIBase,
		Download: torrent.DownloadOptions{
			NoPeersTimeout: cfg.NoPeersTimeout,
			StallTimeout:   cfg.StallTimeout,
			TotalTimeout:   cfg.DownloadTimeout,
		},
		FingerprintMinEpisodes: cfg.FingerprintMinEpisodes,
		MaxConcurrentTorrents:  cfg.MaxConcurrentTorrents,
		MaxFFmpegWorkers:       cpuWorkers,
		Storage:                storage,
		GPUWorkers:             gpuWorkers,
	}, store, fonts,
		resolvers.New(engine, resolvers.Options{
			Attempts: cfg.ResolveAttempts, AttemptTimeout: cfg.ResolveTimeout, Backoff: cfg.ResolveBackoff,
			MinFileSize: int64(cfg.MinVideoFileMB) << 20,
		}, logger),
		tools,
		&intro.Detector{FFmpeg: cfg.FFmpegPath},
		&transcode.Transcoder{FFmpeg: cfg.FFmpegPath, FFprobe: cfg.FFprobePath, Settings: transcode.SettingsFrom(cfg), Log: logger},
		notifier, logger)

	workers := cfg.MaxConcurrentTorrents + cpuWorkers + gpuWorkers
	pool := pipeline.NewPool(pipe, store, workers, logger)
	if err := pool.Start(sigCtx, workCtx); err != nil {
		log.Fatalf("start workers: %v", err)
	}
	cleanerCtx, stopCleaner := context.WithCancel(context.Background())
	defer stopCleaner()
	go pipeline.NewCleaner(store, fonts, layout, cfg.WorkTTLHours, cfg.FailedWorkTTLHours,
		time.Duration(cfg.CleanupIntervalMinutes)*time.Minute, logger).Run(cleanerCtx)

	apiServer := api.NewServer(db, store, logger)
	apiServer.Ready = &api.Readiness{
		FFmpeg: cfg.FFmpegPath, FFprobe: cfg.FFprobePath,
		WorkDir: cfg.WorkDir, OutputDir: cfg.OutputDir,
		MinFreeBytes: uint64(cfg.MinFreeDiskGB) << 30, Store: store, TTL: 10 * time.Second,
	}
	if cfg.HLSEncrypt {
		apiServer.KeyDir, apiServer.KeyCORSOrigin = cfg.KeyDir, cfg.KeyCORSOrigin
	}
	apiServer.OnJobCreated = pool.Wake
	apiServer.Controls = &api.Controls{
		CancelRunning: pipe.CancelRunning, JobFinished: pipe.NotifyFinished,
		Fonts: fonts, Layout: layout, Storage: storage,
	}
	if cfg.ServeMedia {
		apiServer.MediaDir = cfg.OutputDir
	}
	httpClient := &http.Client{Timeout: 20 * time.Second}
	sources, err := discovery.SelectSources(discovery.AllSources(httpClient), cfg.DiscoverySources)
	if err != nil {
		log.Fatalf("discovery: %v", err)
	}
	apiServer.Discovery = &discovery.Discoverer{
		AniList: discovery.NewAniList(httpClient),
		Sources: sources,
		Prefs: discovery.Prefs{
			Resolution: cfg.DiscoveryResolution, PreferredGroups: cfg.DiscoveryPreferredGroups, Exclusions: cfg.DiscoveryExclude,
		},
		Log: logger,
	}
	srv := &http.Server{Addr: cfg.ListenAddr, Handler: apiServer.Handler(), ReadHeaderTimeout: 10 * time.Second}

	errc := make(chan error, 1)
	go func() {
		logger.Info("torClient listening", "addr", cfg.ListenAddr, "db", cfg.DBPath,
			"workers", workers)
		errc <- srv.ListenAndServe()
	}()

	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server", "err", err)
		}
		stop() // make the pool stop claiming too
	case <-sigCtx.Done():
	}

	logger.Info("shutting down: no new episodes; in-flight ones get a grace period", "grace", shutdownGrace)
	httpCtx, cancelHTTP := context.WithTimeout(context.Background(), 5*time.Second)
	srv.Shutdown(httpCtx)
	cancelHTTP()

	drained := make(chan struct{})
	go func() { pool.Wait(); close(drained) }()
	select {
	case <-drained:
	case <-time.After(shutdownGrace):
		logger.Info("cancelling in-flight episodes; they resume from their checkpoint on next start")
		cancelWork()
		<-drained
	}
	stopCleaner()

	cbDone := make(chan struct{})
	go func() { notifier.Wait(); close(cbDone) }()
	select {
	case <-cbDone:
	case <-time.After(30 * time.Second):
		logger.Warn("gave up waiting for callbacks")
	}
	logger.Info("stopped")
}

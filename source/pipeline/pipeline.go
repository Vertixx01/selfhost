// Package pipeline runs one episode through every stage (PROJECT.MD §4,
// §5.2) with a SQLite checkpoint after each, plus the worker pool, the
// packager, callbacks and the cleanup daemon around it.
package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"time"

	"torclient/source/intro"
	"torclient/source/media"
	"torclient/source/models"
	"torclient/source/resolvers"
	"torclient/source/torrent"
	"torclient/source/transcode"
	"torclient/source/utils"
)

type Config struct {
	Layout                 utils.Layout
	Encrypt                bool
	KeyURIBase             string // {base}/{anime_id}/{ep_id}.key
	Download               torrent.DownloadOptions
	FingerprintMinEpisodes int
	MaxConcurrentTorrents  int
	Storage                *utils.StorageGuard // MAX_STORAGE_GB; nil = no limit
	MaxFFmpegWorkers       int                 // CPU (x264) transcode slots; 0 with GPUOnly
	GPUWorkers             int                 // NVENC transcode slots; 0 = no GPU
}

type Pipeline struct {
	cfg        Config
	store      *utils.JobStore
	fonts      *utils.FontCache
	resolver   *resolvers.Resolver
	tools      media.Tools
	detector   *intro.Detector
	transcoder *transcode.Transcoder
	notifier   *Notifier
	log        *slog.Logger

	torrentSlots chan struct{} // MAX_CONCURRENT_TORRENTS: resolve + download
	ffmpegSlots  chan struct{} // MAX_FFMPEG_WORKERS: CPU transcodes (nil = none)
	gpuSlots     chan struct{} // GPU_WORKERS: NVENC transcodes (nil = none)
	series       *seriesRunner

	runMu   sync.Mutex
	running map[string]context.CancelCauseFunc // "job/ep" → cancel of its Run
}

func New(cfg Config, store *utils.JobStore, fonts *utils.FontCache, resolver *resolvers.Resolver,
	tools media.Tools, detector *intro.Detector, tc *transcode.Transcoder, notifier *Notifier, log *slog.Logger) *Pipeline {
	return &Pipeline{
		cfg: cfg, store: store, fonts: fonts, resolver: resolver, tools: tools,
		detector: detector, transcoder: tc, notifier: notifier, log: log,
		torrentSlots: make(chan struct{}, max(1, cfg.MaxConcurrentTorrents)),
		ffmpegSlots:  slots(cfg.MaxFFmpegWorkers),
		gpuSlots:     slots(cfg.GPUWorkers),
		series:       newSeriesRunner(),
		running:      map[string]context.CancelCauseFunc{},
	}
}

// paths is everything on disk for one episode.
type paths struct {
	Raw, RawDir, Meta, Subs, FontsTmp, WorkOut string
	Output                                     string
}

func (p *Pipeline) pathsFor(job *models.Job, ep models.Episode) paths {
	l := p.cfg.Layout
	return paths{
		Raw:      l.RawFile(ep.JobID, ep.EpID),
		RawDir:   filepath.Dir(l.RawFile(ep.JobID, ep.EpID)),
		Meta:     l.MetaDir(ep.JobID, ep.EpID),
		Subs:     l.WorkSubsDir(ep.JobID, ep.EpID),
		FontsTmp: l.FontsTempDir(ep.JobID, ep.EpID),
		WorkOut:  filepath.Join(l.EpWorkDir(ep.JobID, ep.EpID), "work"),
		Output:   l.OutputDir(job.AnimeID, ep.EpID),
	}
}

// NotifyFinished fires the job callback (used after a user cancel finishes a job).
func (p *Pipeline) NotifyFinished(jobID string) {
	if p.notifier != nil {
		p.notifier.JobFinished(jobID)
	}
}

// ErrCancelled is the cause given to a Run that the user cancelled.
var ErrCancelled = errors.New("cancelled by user")

// CancelRunning stops the in-flight work (download, ffmpeg) of the given
// episodes of a job; an empty epIDs means every episode of the job. It only
// interrupts work — the caller records the episodes' status.
func (p *Pipeline) CancelRunning(jobID string, epIDs ...string) int {
	p.runMu.Lock()
	defer p.runMu.Unlock()
	n := 0
	for key, cancel := range p.running {
		job, ep, _ := strings.Cut(key, "/")
		if job != jobID || (len(epIDs) > 0 && !slices.Contains(epIDs, ep)) {
			continue
		}
		cancel(ErrCancelled)
		n++
	}
	return n
}

// stage order; resolving and downloading are one unit (a download can't
// resume without the torrent's metadata).
var stageOrder = []models.EpisodeStatus{
	models.EpResolving, models.EpAnalyzing, models.EpExtracting, models.EpTranscoding, models.EpPackaging,
}

// Run processes one claimed episode to done or failed. If ctx is cancelled
// (shutdown) it returns without touching the status, so the episode resumes
// from its last checkpoint on the next start.
func (p *Pipeline) Run(ctx context.Context, ep models.Episode) {
	log := p.log.With("job_id", ep.JobID, "ep_id", ep.EpID)
	ctx, cancel := context.WithCancelCause(ctx)
	key := ep.JobID + "/" + ep.EpID
	p.runMu.Lock()
	p.running[key] = cancel
	p.runMu.Unlock()
	defer func() {
		p.runMu.Lock()
		delete(p.running, key)
		p.runMu.Unlock()
		cancel(nil)
	}()

	err := func() (err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("panic: %v", r)
				log.Error("pipeline panic", "panic", r, "stack", string(debug.Stack()))
			}
		}()
		return p.process(ctx, ep, log)
	}()
	if errors.Is(context.Cause(ctx), ErrCancelled) {
		log.Info("cancelled by user") // the API already recorded it as failed
		return
	}
	if ctx.Err() != nil {
		log.Info("stopped; will resume from checkpoint", "status", ep.Status)
		return
	}
	if err != nil {
		log.Error("episode failed", "err", err)
		p.finish(ep, models.EpFailed, err.Error())
		return
	}
	log.Info("episode done")
	p.finish(ep, models.EpDone, "")
}

func (p *Pipeline) process(ctx context.Context, ep models.Episode, log *slog.Logger) error {
	job, err := p.store.GetJob(ctx, ep.JobID)
	if err != nil {
		return err
	}
	ps := p.pathsFor(job, ep)

	// A complete output already exists (cache hit, or packaging finished
	// just before a crash): nothing to do.
	if validMaster(ps.Output) {
		log.Info("output already exists; marking done", "path", ps.Output)
		return p.recordOutput(ctx, ep, ps)
	}

	start := resumeIndex(ep.Status)
	// Stages up to transcoding read the raw MKV; if it's gone, start over.
	if start > 0 && start < stageIndex(models.EpPackaging) {
		if _, err := os.Stat(ps.Raw); err != nil {
			log.Warn("raw file missing; restarting from download", "status", ep.Status)
			start = 0
		}
	}
	if start == stageIndex(models.EpPackaging) {
		if _, err := os.Stat(filepath.Join(ps.WorkOut, "master.m3u8")); err != nil {
			start = stageIndex(models.EpTranscoding)
			if _, err := os.Stat(ps.Raw); err != nil {
				start = 0
			}
		}
	}

	stages := []func(context.Context, *models.Job, models.Episode, paths, *slog.Logger) error{
		p.download, p.analyze, p.extract, p.transcode, p.packageStage,
	}
	for i := start; i < len(stages); i++ {
		if err := p.checkpoint(ctx, ep, stageOrder[i], stageProgress[stageOrder[i]]); err != nil {
			return err
		}
		t0 := time.Now()
		if err := stages[i](ctx, job, ep, ps, log); err != nil {
			return fmt.Errorf("%s: %w", stageOrder[i], err)
		}
		log.Info("stage finished", "stage", stageOrder[i], "took", time.Since(t0).Round(time.Millisecond))
	}
	return p.recordOutput(ctx, ep, ps)
}

// Overall progress at the start of each stage (download fills 2→40).
var stageProgress = map[models.EpisodeStatus]float64{
	models.EpResolving: 1, models.EpDownloading: 2, models.EpAnalyzing: 40,
	models.EpExtracting: 42, models.EpTranscoding: 50, models.EpPackaging: 95,
}

func resumeIndex(s models.EpisodeStatus) int {
	if s == models.EpDownloading || s == models.EpQueued {
		return 0
	}
	if i := stageIndex(s); i >= 0 {
		return i
	}
	return 0
}

func stageIndex(s models.EpisodeStatus) int {
	for i, st := range stageOrder {
		if st == s {
			return i
		}
	}
	return -1
}

func (p *Pipeline) checkpoint(ctx context.Context, ep models.Episode, st models.EpisodeStatus, progress float64) error {
	_, err := p.store.UpdateEpisodeStatus(ctx, ep.JobID, ep.EpID, st, progress, "")
	return err
}

// finish records the terminal status (with a fresh context: the work ctx may
// be the reason we're finishing) and fires the job callback once every
// episode of the job is terminal.
func (p *Pipeline) finish(ep models.Episode, st models.EpisodeStatus, errMsg string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	progress := 100.0
	if st == models.EpFailed {
		if cur, err := p.store.GetEpisode(ctx, ep.JobID, ep.EpID); err == nil {
			progress = cur.Progress
		}
	}
	jobFinished, err := p.store.UpdateEpisodeStatus(ctx, ep.JobID, ep.EpID, st, progress, errMsg)
	if err != nil {
		p.log.Error("record episode result", "job_id", ep.JobID, "ep_id", ep.EpID, "err", err)
		return
	}
	if jobFinished && p.notifier != nil {
		p.notifier.JobFinished(ep.JobID)
	}
}

func (p *Pipeline) recordOutput(ctx context.Context, ep models.Episode, ps paths) error {
	size, err := dirSize(ps.Output)
	if err != nil {
		return err
	}
	return p.store.SetOutput(ctx, ep.JobID, ep.EpID, filepath.ToSlash(ps.Output), size)
}

// --- stages -----------------------------------------------------------------

// download: Stage 1 + 2 under a torrent slot.
func (p *Pipeline) download(ctx context.Context, job *models.Job, ep models.Episode, ps paths, log *slog.Logger) error {
	if err := acquire(ctx, p.torrentSlots); err != nil {
		return err
	}
	defer release(p.torrentSlots)

	res, err := p.resolver.Resolve(ctx, ep.MagnetURL, ps.RawDir)
	if err != nil {
		return err
	}
	if err := p.store.SetInfohash(ctx, ep.JobID, ep.EpID, res.Infohash); err != nil {
		res.Handle.Drop()
		return err
	}
	// MAX_STORAGE_GB: the download plus its transcode output must fit.
	if err := p.cfg.Storage.Reserve(ctx, 2*res.File.Length, job.AnimeID); err != nil {
		res.Handle.Drop()
		return err
	}
	// §7 "Disk full": need room for the download and the transcode output.
	if free, err := utils.FreeBytes(p.cfg.Layout.WorkRoot); err == nil && free < uint64(2*res.File.Length) {
		res.Handle.Drop()
		return fmt.Errorf("not enough disk space: %d MB free, need %d MB (2× the episode)", free>>20, (2*res.File.Length)>>20)
	}
	if err := p.checkpoint(ctx, ep, models.EpDownloading, stageProgress[models.EpDownloading]); err != nil {
		res.Handle.Drop()
		return err
	}

	opts := p.cfg.Download
	opts.OnProgress = func(done, total int64) {
		if total > 0 {
			pct := 2 + 38*float64(done)/float64(total)
			if err := p.store.UpdateProgress(ctx, ep.JobID, ep.EpID, pct); err != nil && ctx.Err() == nil {
				log.Warn("progress update", "err", err)
			}
		}
	}
	log.Info("downloading", "file", res.File.Path, "bytes", res.File.Length, "infohash", res.Infohash)
	return torrent.Download(ctx, res.Handle, res.File.Index, ps.Raw, opts)
}

// analyze: Stage 3 + 5.
func (p *Pipeline) analyze(ctx context.Context, job *models.Job, ep models.Episode, ps paths, log *slog.Logger) error {
	sm, err := p.tools.Analyze(ctx, ps.Raw, ps.Meta)
	if err != nil {
		return err
	}
	for _, w := range sm.Warnings {
		log.Warn("analysis", "warning", w)
	}
	am, err := media.MapAudioFor(sm, ep.Title)
	if err != nil {
		return err
	}
	if err := media.WriteJSON(filepath.Join(ps.Meta, "audio_map.json"), am); err != nil {
		return err
	}
	return p.store.SetSourceType(ctx, ep.JobID, ep.EpID, sm.SourceType)
}

// extract: Stage 4 (subs, fonts) + the per-episode half of Stage 6.
func (p *Pipeline) extract(ctx context.Context, job *models.Job, ep models.Episode, ps paths, log *slog.Logger) error {
	var sm media.StreamMap
	var am media.AudioMap
	if err := readJSON(filepath.Join(ps.Meta, "stream_map.json"), &sm); err != nil {
		return err
	}
	if err := readJSON(filepath.Join(ps.Meta, "audio_map.json"), &am); err != nil {
		return err
	}

	subs, err := p.tools.ExtractSubtitles(ctx, ps.Raw, &sm, ps.Subs)
	if err != nil {
		return err
	}
	for _, w := range subs.Warnings {
		log.Warn("subtitles", "warning", w)
	}
	if err := media.WriteJSON(filepath.Join(ps.Meta, "subs.json"), subs); err != nil {
		return err
	}

	fonts, warnings := p.tools.ExtractFonts(ctx, ps.Raw, &sm, ps.FontsTmp)
	for _, w := range warnings {
		log.Warn("fonts", "warning", w)
	}
	for _, f := range fonts {
		if _, err := p.fonts.Register(ctx, ep.JobID, ep.EpID, f.Path, f.Filename); err != nil {
			return fmt.Errorf("register font %s: %w", f.Filename, err)
		}
	}
	os.RemoveAll(ps.FontsTmp)

	det, err := p.detector.DetectEpisode(ctx, ps.Raw, &sm, &am)
	if err != nil {
		return err
	}
	for _, w := range det.Warnings {
		log.Warn("intro detection", "warning", w)
	}
	for part, fp := range det.Fingerprints {
		if err := p.store.SaveFingerprint(ctx, job.AnimeID, ep.EpID, part, fp.Offset, intro.EncodePoints(fp.Points)); err != nil {
			return err
		}
	}
	return intro.Write(filepath.Join(ps.Meta, "timestamps.json"), det.Timestamps)
}

// transcode: Stage 7 under a GPU or CPU slot (hybrid scheduling).
func (p *Pipeline) transcode(ctx context.Context, job *models.Job, ep models.Episode, ps paths, log *slog.Logger) error {
	encoder, slot, err := p.acquireEncoder(ctx)
	if err != nil {
		return err
	}
	defer release(slot)
	log.Info("transcoding", "encoder", encoder)

	var (
		sm   media.StreamMap
		am   media.AudioMap
		subs media.SubtitleResult
		ts   intro.Timestamps
	)
	for name, v := range map[string]any{"stream_map.json": &sm, "audio_map.json": &am, "subs.json": &subs, "timestamps.json": &ts} {
		if err := readJSON(filepath.Join(ps.Meta, name), v); err != nil {
			return err
		}
	}
	req := transcode.Request{
		Input: ps.Raw, OutDir: ps.WorkOut, MetaDir: ps.Meta, SubsDir: ps.Subs,
		Streams: &sm, Audio: &am, Subs: &subs, Timestamps: ts, Encoder: encoder,
	}
	if p.cfg.Encrypt {
		req.KeyURI = fmt.Sprintf("%s/%s/%s.key", p.cfg.KeyURIBase, job.AnimeID, ep.EpID)
	}
	res, err := p.transcoder.Run(ctx, req)
	if err != nil {
		return err
	}
	for _, w := range res.Warnings {
		log.Warn("transcode", "warning", w)
	}
	return media.WriteJSON(filepath.Join(ps.Meta, "transcode.json"), res)
}

// packageStage: Stage 8, then the series-level half of Stage 6.
func (p *Pipeline) packageStage(ctx context.Context, job *models.Job, ep models.Episode, ps paths, log *slog.Logger) error {
	if err := p.packageEpisode(ctx, job, ep, ps, log); err != nil {
		return err
	}
	p.series.Run(ctx, job.AnimeID, func(ctx context.Context) { p.correlateSeries(ctx, job.AnimeID, log) })
	return nil
}

// --- helpers ----------------------------------------------------------------

func slots(n int) chan struct{} {
	if n <= 0 {
		return nil
	}
	return make(chan struct{}, n)
}

// acquireEncoder takes a free GPU slot if there is one, else a free CPU
// slot, else waits for whichever frees up first. Measured on an RTX 3070 Ti +
// i9-13900K: one NVENC job saturates the GPU's single encoder (~12–22x
// realtime) and leaves the CPU idle, so running CPU encodes beside it adds
// their throughput (2 GPU-slot + 2 CPU jobs: 23.8x vs 17.5x CPU-only).
// A nil channel is never ready in a select, so a missing kind just drops out.
func (p *Pipeline) acquireEncoder(ctx context.Context) (string, chan struct{}, error) {
	select {
	case p.gpuSlots <- struct{}{}:
		return transcode.EncoderNVENC, p.gpuSlots, nil
	default:
	}
	select {
	case p.ffmpegSlots <- struct{}{}:
		return transcode.EncoderCPU, p.ffmpegSlots, nil
	default:
	}
	select {
	case p.gpuSlots <- struct{}{}:
		return transcode.EncoderNVENC, p.gpuSlots, nil
	case p.ffmpegSlots <- struct{}{}:
		return transcode.EncoderCPU, p.ffmpegSlots, nil
	case <-ctx.Done():
		return "", nil, ctx.Err()
	}
}

func acquire(ctx context.Context, slots chan struct{}) error {
	select {
	case slots <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func release(slots chan struct{}) { <-slots }

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s (previous stage incomplete?): %w", filepath.Base(path), err)
	}
	return json.Unmarshal(b, v)
}

func dirSize(dir string) (int64, error) {
	var n int64
	err := filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			info, err := d.Info()
			if err != nil {
				return err
			}
			n += info.Size()
		}
		return nil
	})
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	return n, err
}

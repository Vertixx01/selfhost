package pipeline

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	atorrent "github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"

	"torclient/source/intro"
	"torclient/source/media"
	"torclient/source/models"
	"torclient/source/resolvers"
	"torclient/source/torrent"
	"torclient/source/transcode"
	"torclient/source/utils"
)

func TestMain(m *testing.M) {
	torrent.EnsureClassicFileIO()
	os.Exit(m.Run())
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

const fixtureASS = `[Script Info]
ScriptType: v4.00+

[V4+ Styles]
Format: Name, Fontname, Fontsize, PrimaryColour, SecondaryColour, OutlineColour, BackColour, Bold, Italic, Underline, StrikeOut, ScaleX, ScaleY, Spacing, Angle, BorderStyle, Outline, Shadow, Alignment, MarginL, MarginR, MarginV, Encoding
Style: Default,Test Font,48,&H00FFFFFF,&H000000FF,&H00000000,&H00000000,0,0,0,0,100,100,0,0,1,2,0,2,10,10,10,1

[Events]
Format: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text
Dialogue: 0,0:00:01.00,0:00:04.00,Default,,0,0,0,,Hello
`

// makeEpisode: 30s 720p H.264, FLAC jpn + AAC eng, ASS, a font attachment,
// and an "Opening" chapter at 5–17s.
func makeEpisode(t *testing.T, ff, dir string) string {
	t.Helper()
	os.MkdirAll(dir, 0o755)
	w := func(name, body string) string {
		p := filepath.Join(dir, name)
		os.WriteFile(p, []byte(body), 0o644)
		return p
	}
	ass := w("s.ass", fixtureASS)
	ch := w("ch.txt", ";FFMETADATA1\n[CHAPTER]\nTIMEBASE=1/1000\nSTART=0\nEND=5000\ntitle=Avant\n[CHAPTER]\nTIMEBASE=1/1000\nSTART=5000\nEND=17000\ntitle=Opening\n[CHAPTER]\nTIMEBASE=1/1000\nSTART=17000\nEND=30000\ntitle=A Part\n")
	font := w("TestFont.ttf", strings.Repeat("font bytes ", 400))
	out := filepath.Join(dir, "[Grp] Show - 01.mkv")
	cmd := exec.Command(ff, "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc2=size=1280x720:rate=24:duration=30",
		"-f", "lavfi", "-i", "sine=f=440:d=30", "-f", "lavfi", "-i", "sine=f=660:d=30",
		"-i", ass, "-i", ch,
		"-map", "0", "-map", "1", "-map", "2", "-map", "3", "-map_chapters", "4",
		"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p",
		"-c:a:0", "flac", "-c:a:1", "aac", "-c:s", "copy",
		"-metadata:s:a:0", "language=jpn", "-metadata:s:a:1", "language=eng", "-metadata:s:s:0", "language=eng",
		"-attach", font, "-metadata:s:t:0", "mimetype=font/ttf", "-metadata:s:t:0", "filename=TestFont.ttf",
		out)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("make fixture: %v\n%s", err, b)
	}
	for _, f := range []string{ass, ch, font} {
		os.Remove(f)
	}
	return out
}

// seed serves dir as a torrent from a local client; the magnet points at it.
func seed(t *testing.T, dir string) (string, *atorrent.Client) {
	t.Helper()
	info := metainfo.Info{PieceLength: 256 << 10}
	if err := info.BuildFromFilePath(dir); err != nil {
		t.Fatal(err)
	}
	var mi metainfo.MetaInfo
	mi.InfoBytes, _ = bencode.Marshal(info)
	cc := atorrent.NewDefaultClientConfig()
	cc.ListenPort, cc.NoDHT, cc.DisableTrackers, cc.Seed = 0, true, true, true
	cc.DataDir = filepath.Dir(dir)
	cc.Slogger = quiet()
	cl, err := atorrent.NewClient(cc)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cl.Close() })
	tor, err := cl.AddTorrent(&mi)
	if err != nil {
		t.Fatal(err)
	}
	if err := tor.VerifyData(); err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("magnet:?xt=urn:btih:%s&x.pe=127.0.0.1:%d", mi.HashInfoBytes().HexString(), cl.LocalPort()), cl
}

type harness struct {
	t         *testing.T
	db        *sql.DB
	store     *utils.JobStore
	fonts     *utils.FontCache
	layout    utils.Layout
	pipe      *Pipeline
	callbacks chan callbackPayload
	ff        string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	ff, err1 := exec.LookPath("ffmpeg")
	fp, err2 := exec.LookPath("ffprobe")
	if err1 != nil || err2 != nil {
		t.Skip("ffmpeg/ffprobe not on PATH")
	}
	root := t.TempDir()
	layout := utils.Layout{
		WorkRoot: filepath.Join(root, "work"), OutputRoot: filepath.Join(root, "output"),
		FontRoot: filepath.Join(root, "fonts"), KeyRoot: filepath.Join(root, "keys"),
	}
	for _, d := range []string{layout.WorkRoot, layout.OutputRoot} {
		os.MkdirAll(d, 0o755)
	}
	db, err := utils.OpenDB(context.Background(), filepath.Join(root, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	store := utils.NewJobStore(db)
	fonts := utils.NewFontCache(db, layout)

	engine, err := torrent.NewEngine(torrent.Config{DisableTrackers: true, MetainfoDir: filepath.Join(root, "metainfo"), Logger: quiet()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(engine.Close)

	h := &harness{t: t, db: db, store: store, fonts: fonts, layout: layout, callbacks: make(chan callbackPayload, 10), ff: ff}
	cb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p callbackPayload
		json.NewDecoder(r.Body).Decode(&p)
		h.callbacks <- p
	}))
	t.Cleanup(cb.Close)
	h.t.Setenv("TORCLIENT_TEST_CALLBACK", cb.URL)

	notifier := NewNotifier(store, quiet())
	h.pipe = New(Config{
		Layout: layout, Encrypt: true, KeyURIBase: "http://keys.test/keys",
		Download:               torrent.DownloadOptions{NoPeersTimeout: 20 * time.Second, StallTimeout: 20 * time.Second, PollInterval: 100 * time.Millisecond},
		FingerprintMinEpisodes: 3, MaxConcurrentTorrents: 2, MaxFFmpegWorkers: 1,
	}, store, fonts,
		resolvers.New(engine, resolvers.Options{Attempts: 1, AttemptTimeout: 15 * time.Second, MinFileSize: 1 << 20}, quiet()),
		media.Tools{FFmpeg: ff, FFprobe: fp},
		&intro.Detector{FFmpeg: ff},
		&transcode.Transcoder{FFmpeg: ff, FFprobe: fp, Log: quiet(), Settings: transcode.Settings{
			CRFBD: 22, CRFWeb: 21, MaxrateBDKbps: 6000, BufsizeBDKbps: 12000, MaxrateWebKbps: 5000, BufsizeWebKbps: 20000,
			Preset: "ultrafast", Tune: "animation", AudioBitrateKbps: 128, AudioChannels: 2, AudioSampleRate: 48000, SegmentDuration: 6,
		}},
		notifier, quiet())
	return h
}

func (h *harness) submit(jobID, animeID, epID, magnet string) {
	h.t.Helper()
	_, _, err := h.store.CreateJob(context.Background(),
		models.Job{ID: jobID, AnimeID: animeID, Title: "Show", CallbackURL: os.Getenv("TORCLIENT_TEST_CALLBACK")},
		[]models.Episode{{EpID: epID, MagnetURL: magnet}})
	if err != nil {
		h.t.Fatal(err)
	}
}

// runPool starts a pool (as a fresh process start would) and waits for the
// job to finish, returning its callback.
func (h *harness) runPool(jobID string) (*models.Job, callbackPayload) {
	h.t.Helper()
	claimCtx, stop := context.WithCancel(context.Background())
	pool := NewPool(h.pipe, h.store, 3, quiet())
	if err := pool.Start(claimCtx, context.Background()); err != nil {
		h.t.Fatal(err)
	}
	pool.Wake()
	defer func() { stop(); pool.Wait() }()
	deadline := time.After(3 * time.Minute)
	for {
		select {
		case cb := <-h.callbacks:
			if cb.JobID != jobID {
				h.t.Fatalf("callback for %s while waiting for %s", cb.JobID, jobID)
			}
			job, err := h.store.GetJob(context.Background(), jobID)
			if err != nil {
				h.t.Fatal(err)
			}
			return job, cb
		case <-deadline:
			job, _ := h.store.GetJob(context.Background(), jobID)
			h.t.Fatalf("job %s did not finish: %+v", jobID, job)
		}
	}
}

func TestPipelineEndToEnd(t *testing.T) {
	h := newHarness(t)
	srcDir := filepath.Join(t.TempDir(), "[Grp] Show - 01")
	fixture := makeEpisode(t, h.ff, srcDir)
	magnet, seeder := seed(t, srcDir)
	out := h.layout.OutputDir("show", "S01E01")

	t.Run("full run", func(t *testing.T) {
		h.submit("j1", "show", "S01E01", magnet)
		job, cb := h.runPool("j1")
		ep := job.Episodes[0]
		if job.Status != models.JobCompleted || ep.Status != models.EpDone || ep.Progress != 100 {
			t.Fatalf("job %s ep %s (%v) err=%q", job.Status, ep.Status, ep.Progress, ep.Error)
		}
		if ep.Infohash == "" || ep.SourceType != "bd" || ep.SizeBytes == 0 || ep.OutputPath != filepath.ToSlash(out) {
			t.Errorf("episode row: %+v", ep)
		}
		for _, f := range []string{"master.m3u8", "720p/stream.m3u8", "audio/jpn/stream.m3u8", "audio/eng/stream.m3u8",
			"subs/eng.vtt", "subs/eng.m3u8", "subs/eng.ass", "subs/fonts/TestFont.ttf",
			"meta/timestamps.json", "meta/media_info.json", "meta/job.json"} {
			if _, err := os.Stat(filepath.Join(out, f)); err != nil {
				t.Errorf("output missing %s", f)
			}
		}
		if _, err := os.Stat(filepath.Join(out, "1080p")); err == nil {
			t.Error("720p source was upscaled to 1080p")
		}
		// The key is published to KEY_DIR and is nowhere in the output tree.
		if b, err := os.ReadFile(h.layout.KeyFile("show", "S01E01")); err != nil || len(b) != 16 {
			t.Errorf("key not published: %v", err)
		}
		filepath.WalkDir(out, func(p string, d os.DirEntry, _ error) error {
			if strings.HasSuffix(p, ".key") || strings.HasSuffix(p, ".keyinfo") {
				t.Errorf("key material in output: %s", p)
			}
			return nil
		})
		pl, _ := os.ReadFile(filepath.Join(out, "720p", "stream.m3u8"))
		if !strings.Contains(string(pl), `URI="http://keys.test/keys/show/S01E01.key"`) {
			t.Errorf("variant playlist key URI:\n%s", pl)
		}
		if _, err := os.Stat(h.layout.RawFile("j1", "S01E01")); err == nil {
			t.Error("raw MKV not removed after packaging")
		}
		// No staging leftovers next to the output.
		entries, _ := os.ReadDir(filepath.Dir(out))
		if len(entries) != 1 {
			t.Errorf("anime dir has %d entries, want just S01E01", len(entries))
		}

		if cb.Status != "completed" || len(cb.Episodes) != 1 {
			t.Fatalf("callback %+v", cb)
		}
		ce := cb.Episodes[0]
		if ce.Status != "done" || ce.MasterPlaylist != filepath.ToSlash(out)+"/master.m3u8" || ce.SizeBytes != ep.SizeBytes {
			t.Errorf("callback episode %+v", ce)
		}
		if ce.Timestamps == nil || ce.Timestamps.Intro == nil || ce.Timestamps.Intro.Source != "chapter" || ce.Timestamps.Intro.Start != 5 {
			t.Errorf("callback timestamps %+v", ce.Timestamps)
		}
	})

	t.Run("cache hit", func(t *testing.T) {
		seeder.Close() // proves nothing is downloaded
		h.submit("j2", "show", "S01E01", magnet)
		job, cb := h.runPool("j2")
		if job.Status != models.JobCompleted || cb.Episodes[0].OutputPath != filepath.ToSlash(out) {
			t.Fatalf("cache hit: job %+v cb %+v", job, cb)
		}
	})

	t.Run("same torrent under another ID is reused", func(t *testing.T) {
		// The seeder is gone (previous subtest) and metadata comes from the
		// .torrent cache, so finishing proves nothing was downloaded.
		h.submit("j5", "show-alt", "E1", magnet)
		job, _ := h.runPool("j5")
		alt := h.layout.OutputDir("show-alt", "E1")
		if job.Status != models.JobCompleted || job.Episodes[0].OutputPath != filepath.ToSlash(alt) || !validMaster(alt) {
			t.Fatalf("reuse: job %+v", job.Episodes[0])
		}
		pl, _ := os.ReadFile(filepath.Join(alt, "720p", "stream.m3u8"))
		if !strings.Contains(string(pl), `URI="http://keys.test/keys/show-alt/E1.key"`) || strings.Contains(string(pl), "S01E01.key") {
			t.Errorf("reused playlist key URI:\n%s", pl)
		}
		if k1, _ := os.ReadFile(h.layout.KeyFile("show", "S01E01")); len(k1) != 16 {
			t.Error("source key missing")
		} else if k2, _ := os.ReadFile(h.layout.KeyFile("show-alt", "E1")); string(k1) != string(k2) {
			t.Error("reused episode's key differs from the source's")
		}
		meta, _ := os.ReadFile(filepath.Join(alt, "meta", "job.json"))
		orig, _ := os.ReadFile(filepath.Join(out, "meta", "job.json"))
		if !strings.Contains(string(meta), `"show-alt"`) || strings.Contains(string(orig), `"show-alt"`) {
			t.Errorf("job.json: reused %s / original %s", meta, orig)
		}
		origPl, _ := os.ReadFile(filepath.Join(out, "720p", "stream.m3u8"))
		if !strings.Contains(string(origPl), "show/S01E01.key") {
			t.Error("rewriting the copy changed the original playlist")
		}
	})

	t.Run("bad magnet fails the job", func(t *testing.T) {
		h.submit("j3", "other", "E01", "magnet:?xt=urn:btih:"+strings.Repeat("ab", 20))
		job, cb := h.runPool("j3")
		if job.Status != models.JobFailed || job.Episodes[0].Error == "" || cb.Status != "failed" || cb.Episodes[0].Error == "" {
			t.Fatalf("job %+v cb %+v", job, cb)
		}
	})

	t.Run("resume after crash", func(t *testing.T) {
		// A previous process downloaded the file and died during analysis.
		h.submit("j4", "show", "S01E02", "magnet:?xt=urn:btih:"+strings.Repeat("cd", 20)) // unreachable
		raw := h.layout.RawFile("j4", "S01E02")
		os.MkdirAll(filepath.Dir(raw), 0o755)
		b, _ := os.ReadFile(fixture)
		os.WriteFile(raw, b, 0o644)
		if _, err := h.store.UpdateEpisodeStatus(context.Background(), "j4", "S01E02", models.EpAnalyzing, 40, ""); err != nil {
			t.Fatal(err)
		}
		job, _ := h.runPool("j4")
		if job.Status != models.JobCompleted {
			t.Fatalf("resumed job: %+v", job.Episodes[0])
		}
		if !validMaster(h.layout.OutputDir("show", "S01E02")) {
			t.Error("resumed episode has no output")
		}
	})

	t.Run("cleanup", func(t *testing.T) {
		// Age every finished episode past its TTL.
		if _, err := h.db.Exec(`UPDATE episodes SET updated_at = '2000-01-01T00:00:00.000Z'`); err != nil {
			t.Fatal(err)
		}
		c := NewCleaner(h.store, h.fonts, h.layout, 3, 72, time.Hour, quiet())
		n, err := c.Sweep(context.Background())
		if err != nil || n == 0 {
			t.Fatalf("sweep removed %d, err %v", n, err)
		}
		for _, j := range []string{"j1", "j3", "j4"} {
			if _, err := os.Stat(h.layout.WorkDir(j)); err == nil {
				t.Errorf("work dir for %s still there", j)
			}
		}
		if entries, _ := os.ReadDir(h.layout.FontRoot); len(entries) != 0 {
			t.Errorf("font cache not released: %v", entries)
		}
		if !validMaster(out) {
			t.Error("cleanup touched output/")
		}
		if n, _ := c.Sweep(context.Background()); n != 0 {
			t.Errorf("second sweep removed %d", n)
		}
	})
}

// TestBatchTorrent: two episodes of one job come from the same two-file
// batch torrent. Each must get its own file, and the second must wait for the
// first instead of failing on the busy infohash.
func TestBatchTorrent(t *testing.T) {
	h := newHarness(t)
	h.pipe.busyRetry = 100 * time.Millisecond
	srcDir := filepath.Join(t.TempDir(), "[Grp] Show (01-02)")
	ep1 := makeEpisode(t, h.ff, srcDir)
	// Episode 2 is a 20s cut of episode 1, so the outputs tell them apart.
	ep2 := filepath.Join(srcDir, "[Grp] Show - 02.mkv")
	if b, err := exec.Command(h.ff, "-hide_banner", "-loglevel", "error", "-i", ep1, "-t", "20", "-map", "0", "-c", "copy", ep2).CombinedOutput(); err != nil {
		t.Fatalf("cut episode 2: %v\n%s", err, b)
	}
	magnet, _ := seed(t, srcDir)

	_, _, err := h.store.CreateJob(context.Background(),
		models.Job{ID: "b1", AnimeID: "show", Title: "Show", CallbackURL: os.Getenv("TORCLIENT_TEST_CALLBACK")},
		[]models.Episode{{EpID: "ep1", MagnetURL: magnet, FileEpisodes: []int{1}}, {EpID: "ep2", MagnetURL: magnet, FileEpisodes: []int{2}}})
	if err != nil {
		t.Fatal(err)
	}
	job, _ := h.runPool("b1")
	if job.Status != models.JobCompleted {
		t.Fatalf("job %s: %+v", job.Status, job.Episodes)
	}
	for ep, want := range map[string]float64{"ep1": 30, "ep2": 20} {
		if got := playlistSeconds(t, filepath.Join(h.layout.OutputDir("show", ep), "720p", "stream.m3u8")); got < want-1 || got > want+1 {
			t.Errorf("%s: %.1fs of video, want %.0fs (wrong file from the batch?)", ep, got, want)
		}
	}
}

func playlistSeconds(t *testing.T, path string) float64 {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var total float64
	for _, line := range strings.Split(string(b), "\n") {
		if d, ok := strings.CutPrefix(strings.TrimSpace(line), "#EXTINF:"); ok {
			var f float64
			fmt.Sscanf(strings.TrimSuffix(d, ","), "%g", &f)
			total += f
		}
	}
	return total
}

func TestPackageVerification(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "720p"), 0o755)
	os.WriteFile(filepath.Join(dir, "master.m3u8"), []byte("#EXTM3U\n#EXT-X-MEDIA:TYPE=AUDIO,URI=\"audio/jpn/stream.m3u8\"\n#EXT-X-STREAM-INF:BANDWIDTH=1\n720p/stream.m3u8\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "720p", "stream.m3u8"), []byte("#EXTM3U\n#EXT-X-KEY:METHOD=AES-128,URI=\"https://k/x.key\"\n#EXTINF:6,\nseg0000.ts\n#EXT-X-ENDLIST\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "720p", "seg0000.ts"), bytes.Repeat([]byte{1}, 10), 0o644)
	if err := verifyPackage(dir); err == nil || !strings.Contains(err.Error(), "audio/jpn") {
		t.Fatalf("missing audio playlist not caught: %v", err)
	}
	os.MkdirAll(filepath.Join(dir, "audio", "jpn"), 0o755)
	os.WriteFile(filepath.Join(dir, "audio", "jpn", "stream.m3u8"), []byte("#EXTM3U\n#EXTINF:6,\nseg0000.ts\n#EXT-X-ENDLIST\n"), 0o644)
	if err := verifyPackage(dir); err == nil || !strings.Contains(err.Error(), "seg0000.ts") {
		t.Fatalf("missing audio segment not caught: %v", err)
	}
	os.WriteFile(filepath.Join(dir, "audio", "jpn", "seg0000.ts"), []byte{1}, 0o644)
	if err := verifyPackage(dir); err != nil {
		t.Fatal(err)
	}
}

func TestResumeIndex(t *testing.T) {
	for st, want := range map[models.EpisodeStatus]int{
		models.EpQueued: 0, models.EpResolving: 0, models.EpDownloading: 0,
		models.EpAnalyzing: 1, models.EpExtracting: 2, models.EpTranscoding: 3, models.EpPackaging: 4,
	} {
		if got := resumeIndex(st); got != want {
			t.Errorf("resumeIndex(%s) = %d, want %d", st, got, want)
		}
	}
}

// A user cancel interrupts a running stage at once, records "cancelled by
// user" without a later checkpoint overwriting it, and the episode can be
// retried.
func TestCancelRunningEpisode(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.submit("jc", "show", "E01", "magnet:?xt=urn:btih:"+strings.Repeat("ef", 20)) // never resolves
	claimCtx, stop := context.WithCancel(ctx)
	pool := NewPool(h.pipe, h.store, 1, quiet())
	if err := pool.Start(claimCtx, ctx); err != nil {
		t.Fatal(err)
	}
	pool.Wake()
	defer func() { stop(); pool.Wait() }()

	deadline := time.Now().Add(10 * time.Second)
	for {
		ep, _ := h.store.GetEpisode(ctx, "jc", "E01")
		if ep != nil && ep.Status == models.EpResolving {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("never started resolving: %+v", ep)
		}
		time.Sleep(20 * time.Millisecond)
	}

	start := time.Now()
	if n := h.pipe.CancelRunning("jc"); n != 1 {
		t.Fatalf("CancelRunning stopped %d runs, want 1", n)
	}
	eps, finished, err := h.store.CancelJob(ctx, "jc")
	if err != nil || len(eps) != 1 || !finished {
		t.Fatalf("CancelJob %v %v %v", eps, finished, err)
	}
	// The run must be gone quickly (resolve timeout is 15s otherwise).
	for {
		h.pipe.runMu.Lock()
		n := len(h.pipe.running)
		h.pipe.runMu.Unlock()
		if n == 0 {
			break
		}
		if time.Since(start) > 3*time.Second {
			t.Fatal("run still active 3s after cancel")
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond) // give a late checkpoint the chance to misbehave
	ep, _ := h.store.GetEpisode(ctx, "jc", "E01")
	if ep.Status != models.EpFailed || ep.Error != "cancelled by user" {
		t.Fatalf("after cancel: %s %q", ep.Status, ep.Error)
	}
	// The API fires the callback after a cancel that finishes the job.
	h.pipe.NotifyFinished("jc")
	select {
	case cb := <-h.callbacks:
		if cb.JobID != "jc" || cb.Status != "failed" || cb.Episodes[0].Error != "cancelled by user" {
			t.Errorf("callback %+v", cb)
		}
	case <-time.After(10 * time.Second):
		t.Error("no callback after cancel")
	}

	stop() // no worker should pick up the retry in this test
	pool.Wait()
	retried, err := h.store.RetryEpisodes(ctx, "jc", nil)
	if err != nil || len(retried) != 1 {
		t.Fatalf("retry %v %v", retried, err)
	}
	ep, _ = h.store.GetEpisode(ctx, "jc", "E01")
	job, _ := h.store.GetJob(ctx, "jc")
	if ep.Status != models.EpQueued || ep.Error != "" || job.Status != models.JobProcessing {
		t.Fatalf("after retry: ep %s %q job %s", ep.Status, ep.Error, job.Status)
	}
}

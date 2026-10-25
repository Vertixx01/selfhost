package resolvers

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	atorrent "github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"

	"torclient/source/torrent"
)

func TestMain(m *testing.M) {
	torrent.EnsureClassicFileIO() // re-runs the test binary with mmap IO off
	os.Exit(m.Run())
}

func TestSelectMainFile(t *testing.T) {
	const mb = 1 << 20
	files := []torrent.File{
		{Index: 0, Path: "Show - 01.mkv", Length: 300 * mb},
		{Index: 1, Path: "Extras/NCOP.mkv", Length: 80 * mb},
		{Index: 2, Path: "Show - 01 [sample].mkv", Length: 400 * mb},
		{Index: 3, Path: "Sample/clip.mkv", Length: 500 * mb},
		{Index: 4, Path: "Show - 01.mp4", Length: 900 * mb}, // MKV wins even when smaller
		{Index: 5, Path: "tiny.mkv", Length: 10 * mb},
		{Index: 6, Path: "readme.txt", Length: 1000 * mb},
	}
	main, skipped, err := SelectMainFile(files, DefaultMinFileSize)
	if err != nil || main.Index != 0 || len(skipped) != len(files)-1 {
		t.Fatalf("got %+v, %d skipped, %v", main, len(skipped), err)
	}

	// No MKV: fall back to the largest other container.
	main, _, err = SelectMainFile([]torrent.File{
		{Index: 0, Path: "a.avi", Length: 100 * mb},
		{Index: 1, Path: "b.mp4", Length: 200 * mb},
	}, DefaultMinFileSize)
	if err != nil || main.Index != 1 {
		t.Fatalf("fallback: got %+v, %v", main, err)
	}

	if _, _, err := SelectMainFile(files[2:4], DefaultMinFileSize); !errors.Is(err, ErrNoPlayableFile) {
		t.Fatalf("only samples: want ErrNoPlayableFile, got %v", err)
	}
}

// seed builds a two-file torrent and seeds it from a local client. Returns a
// magnet that points straight at the seeder (x.pe), the main file's bytes,
// and the seeding client.
func seed(t *testing.T) (magnet string, episode []byte, seeder *atorrent.Client) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "[Group] Show - 01")
	os.MkdirAll(filepath.Join(root, "Extras"), 0o755)
	episode = randBytes(3 << 20)
	os.WriteFile(filepath.Join(root, "[Group] Show - 01.mkv"), episode, 0o644)
	os.WriteFile(filepath.Join(root, "Extras", "NCOP.mkv"), randBytes(2<<20), 0o644)
	os.WriteFile(filepath.Join(root, "info.txt"), []byte("hi"), 0o644)

	info := metainfo.Info{PieceLength: 256 << 10}
	if err := info.BuildFromFilePath(root); err != nil {
		t.Fatal(err)
	}
	var mi metainfo.MetaInfo
	var err error
	if mi.InfoBytes, err = bencode.Marshal(info); err != nil {
		t.Fatal(err)
	}

	cc := atorrent.NewDefaultClientConfig()
	cc.ListenPort, cc.NoDHT, cc.DisableTrackers, cc.Seed = 0, true, true, true
	cc.DataDir = filepath.Dir(root)
	cc.Slogger = quietLogger()
	seeder, err = atorrent.NewClient(cc)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { seeder.Close() })
	st, err := seeder.AddTorrent(&mi)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.VerifyData(); err != nil {
		t.Fatal(err)
	}
	if st.BytesCompleted() != st.Length() {
		t.Fatalf("seeder has %d of %d bytes", st.BytesCompleted(), st.Length())
	}
	magnet = fmt.Sprintf("magnet:?xt=urn:btih:%s&x.pe=127.0.0.1:%d", mi.HashInfoBytes().HexString(), seeder.LocalPort())
	return magnet, episode, seeder
}

func newEngine(t *testing.T) *torrent.Engine {
	t.Helper()
	e, err := torrent.NewEngine(torrent.Config{
		DisableTrackers: true,
		MetainfoDir:     filepath.Join(t.TempDir(), "metainfo"),
		Logger:          quietLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(e.Close)
	return e
}

func TestResolveAndDownload(t *testing.T) {
	magnet, episode, _ := seed(t)
	engine := newEngine(t)
	r := New(engine, Options{Attempts: 1, AttemptTimeout: 20 * time.Second, MinFileSize: 1 << 20}, quietLogger())

	raw := filepath.Join(t.TempDir(), "job1", "S01E01", "raw")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	res, err := r.Resolve(ctx, magnet, raw)
	if err != nil {
		t.Fatal(err)
	}
	if res.File.Path != "[Group] Show - 01.mkv" || len(res.Skipped) != 2 {
		t.Fatalf("picked %+v, skipped %+v", res.File, res.Skipped)
	}

	// Same infohash can't be active twice.
	if _, err := engine.AddMagnet(magnet, t.TempDir()); !errors.Is(err, torrent.ErrBusy) {
		t.Fatalf("want ErrBusy, got %v", err)
	}

	var lastDone int64
	dest := filepath.Join(raw, "episode.mkv")
	err = torrent.Download(ctx, res.Handle, res.File.Index, dest, torrent.DownloadOptions{
		NoPeersTimeout: 20 * time.Second,
		StallTimeout:   20 * time.Second,
		PollInterval:   50 * time.Millisecond,
		OnProgress:     func(done, total int64) { lastDone = done },
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(dest)
	if err != nil || !bytes.Equal(got, episode) {
		t.Fatalf("episode.mkv mismatch (len %d, err %v)", len(got), err)
	}
	if lastDone != int64(len(episode)) {
		t.Errorf("last progress %d, want %d", lastDone, len(episode))
	}
	entries, _ := os.ReadDir(raw)
	if len(entries) != 1 {
		t.Errorf("raw dir should only hold episode.mkv, has %d entries", len(entries))
	}

	// Handle is dropped, so the infohash is free again, and its metadata is cached.
	h, err := engine.AddMagnet(magnet, t.TempDir())
	if err != nil {
		t.Fatalf("re-add after download: %v", err)
	}
	h.Drop()
}

func TestResolveTimesOutWithoutPeers(t *testing.T) {
	r := New(newEngine(t), Options{Attempts: 2, AttemptTimeout: 300 * time.Millisecond, Backoff: -1}, quietLogger())
	_, err := r.Resolve(context.Background(), "magnet:?xt=urn:btih:"+fmt.Sprintf("%040x", rand.Uint64()), t.TempDir())
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want deadline error, got %v", err)
	}
}

func TestDownloadFailsWhenSeederLeaves(t *testing.T) {
	magnet, _, seeder := seed(t)
	r := New(newEngine(t), Options{Attempts: 1, AttemptTimeout: 20 * time.Second, MinFileSize: 1 << 20}, quietLogger())
	ctx := context.Background()
	res, err := r.Resolve(ctx, magnet, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	seeder.Close() // metadata arrived, but nobody is left to send data

	start := time.Now()
	err = torrent.Download(ctx, res.Handle, res.File.Index, filepath.Join(t.TempDir(), "episode.mkv"), torrent.DownloadOptions{
		NoPeersTimeout: 500 * time.Millisecond,
		StallTimeout:   5 * time.Second,
		PollInterval:   50 * time.Millisecond,
	})
	if !errors.Is(err, torrent.ErrNoSeeders) && !errors.Is(err, torrent.ErrStalled) {
		t.Fatalf("want ErrNoSeeders or ErrStalled, got %v", err)
	}
	if time.Since(start) > 10*time.Second {
		t.Errorf("watchdog took %s", time.Since(start))
	}
}

func randBytes(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(rand.Uint32())
	}
	return b
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestWebseedOnlyAcrossRestart: a torrent with no peers, only an HTTP web seed
// (like archive.org's), fetched via an xs= metainfo source — and fetched
// again by a fresh engine that finds the metainfo in its cache (a restart).
// The second case once stalled: the cache dropped the web seeds.
func TestWebseedOnlyAcrossRestart(t *testing.T) {
	root := t.TempDir()
	item := filepath.Join(root, "files", "show-ep1")
	os.MkdirAll(item, 0o755)
	episode := randBytes(3 << 20)
	os.WriteFile(filepath.Join(item, "episode.mkv"), episode, 0o644)
	os.WriteFile(filepath.Join(item, "notes.txt"), []byte("extra"), 0o644)

	info := metainfo.Info{PieceLength: 256 << 10}
	if err := info.BuildFromFilePath(item); err != nil {
		t.Fatal(err)
	}
	var mi metainfo.MetaInfo
	mi.InfoBytes, _ = bencode.Marshal(info)

	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()
	// BEP 19 multi-file: {webseed}/{torrent name}/{path}
	mi.UrlList = []string{srv.URL + "/files/"}
	mux.Handle("/files/", http.StripPrefix("/files/", http.FileServer(http.Dir(filepath.Join(root, "files")))))
	mux.HandleFunc("/meta.torrent", func(w http.ResponseWriter, r *http.Request) { mi.Write(w) })
	magnet := fmt.Sprintf("magnet:?xt=urn:btih:%s&xs=%s", mi.HashInfoBytes().HexString(), url.QueryEscape(srv.URL+"/meta.torrent"))

	metaDir := filepath.Join(root, "metainfo")
	for run := 1; run <= 2; run++ {
		e, err := torrent.NewEngine(torrent.Config{DisableTrackers: true, MetainfoDir: metaDir, Logger: quietLogger()})
		if err != nil {
			t.Fatal(err)
		}
		r := New(e, Options{Attempts: 1, AttemptTimeout: 20 * time.Second, MinFileSize: 1 << 20}, quietLogger())
		raw := filepath.Join(root, fmt.Sprintf("raw%d", run))
		res, err := r.Resolve(context.Background(), magnet, raw)
		if err != nil {
			t.Fatalf("run %d resolve: %v", run, err)
		}
		dest := filepath.Join(raw, "episode.mkv")
		err = torrent.Download(context.Background(), res.Handle, res.File.Index, dest, torrent.DownloadOptions{
			StallTimeout: 10 * time.Second, PollInterval: 50 * time.Millisecond,
		})
		e.Close()
		if err != nil {
			t.Fatalf("run %d (%s) download: %v", run, map[int]string{1: "via xs source", 2: "from metainfo cache"}[run], err)
		}
		if got, _ := os.ReadFile(dest); !bytes.Equal(got, episode) {
			t.Fatalf("run %d: content mismatch", run)
		}
	}
}

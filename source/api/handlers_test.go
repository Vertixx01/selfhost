package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"torclient/source/utils"
)

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	db, err := utils.OpenDB(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ts := httptest.NewServer(NewServer(db, utils.NewJobStore(db), log).Handler())
	t.Cleanup(ts.Close)
	return ts
}

func do(t *testing.T, ts *httptest.Server, method, path, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

const job1 = `{"job_id":"j1","anime_id":"show","title":"Show","callback_url":"https://cron.local/hook",
 "episodes":[{"ep_id":"S01E01","magnet_url":"magnet:?xt=urn:btih:aaa","priority":1}]}`

func TestJobLifecycle(t *testing.T) {
	ts := newTestServer(t)

	if code, body := do(t, ts, "POST", "/job", job1); code != 201 {
		t.Fatalf("create: %d %s", code, body)
	}
	if code, _ := do(t, ts, "POST", "/job", job1); code != 200 {
		t.Fatalf("idempotent re-POST: want 200, got %d", code)
	}
	code, body := do(t, ts, "GET", "/job/j1", "")
	if code != 200 || !strings.Contains(body, `"status":"queued"`) {
		t.Fatalf("get: %d %s", code, body)
	}
	if code, _ := do(t, ts, "GET", "/job/nope", ""); code != 404 {
		t.Fatalf("missing job: want 404, got %d", code)
	}

	// Append to the existing anime; reuses its title.
	code, body = do(t, ts, "POST", "/api/anime/show/episodes",
		`{"job_id":"j2","episodes":[{"ep_id":"S01E02","magnet_url":"magnet:?xt=urn:btih:bbb"}]}`)
	if code != 201 || !strings.Contains(body, `"title":"Show"`) {
		t.Fatalf("append: %d %s", code, body)
	}
	// Appending an episode still queued under j1 → 409.
	code, body = do(t, ts, "POST", "/api/anime/show/episodes",
		`{"job_id":"j3","episodes":[{"ep_id":"S01E01","magnet_url":"magnet:?xt=urn:btih:aaa"}]}`)
	if code != 409 || !strings.Contains(body, `"ep_ids":["S01E01"]`) {
		t.Fatalf("in-flight dup: %d %s", code, body)
	}
	if code, _ := do(t, ts, "POST", "/api/anime/unknown/episodes",
		`{"job_id":"j4","episodes":[{"ep_id":"e1","magnet_url":"magnet:?xt=urn:btih:ccc"}]}`); code != 404 {
		t.Fatalf("unknown anime: want 404, got %d", code)
	}

	code, body = do(t, ts, "GET", "/jobs?anime_id=show", "")
	if code != 200 || strings.Count(body, `"job_id"`) != 2 {
		t.Fatalf("list: %d %s", code, body)
	}
	if code, body := do(t, ts, "GET", "/health", ""); code != 200 || !strings.Contains(body, `"queued":2`) {
		t.Fatalf("health: %d %s", code, body)
	}
}

func TestCreateJobValidation(t *testing.T) {
	ts := newTestServer(t)
	cases := map[string]string{
		"no episodes":    `{"job_id":"j","anime_id":"a","episodes":[]}`,
		"bad magnet":     `{"job_id":"j","anime_id":"a","episodes":[{"ep_id":"e","magnet_url":"http://x"}]}`,
		"dup ep_id":      `{"job_id":"j","anime_id":"a","episodes":[{"ep_id":"e","magnet_url":"magnet:?xt=urn:btih:a"},{"ep_id":"e","magnet_url":"magnet:?xt=urn:btih:b"}]}`,
		"path traversal": `{"job_id":"j","anime_id":"..","episodes":[{"ep_id":"e","magnet_url":"magnet:?xt=urn:btih:a"}]}`,
		"slash in ep_id": `{"job_id":"j","anime_id":"a","episodes":[{"ep_id":"x/../../etc","magnet_url":"magnet:?xt=urn:btih:a"}]}`,
		"missing job_id": `{"anime_id":"a","episodes":[{"ep_id":"e","magnet_url":"magnet:?xt=urn:btih:a"}]}`,
		"bad callback":   `{"job_id":"j","anime_id":"a","callback_url":"ftp://x","episodes":[{"ep_id":"e","magnet_url":"magnet:?xt=urn:btih:a"}]}`,
		"not json":       `{`,
	}
	for name, body := range cases {
		if code, resp := do(t, ts, "POST", "/job", body); code != 400 {
			t.Errorf("%s: want 400, got %d %s", name, code, resp)
		}
	}
}

func TestReadyAndKeys(t *testing.T) {
	db, err := utils.OpenDB(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	store := utils.NewJobStore(db)
	s := NewServer(db, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	work := t.TempDir()
	s.Ready = &Readiness{FFmpeg: "definitely-not-ffmpeg", FFprobe: "definitely-not-ffprobe", WorkDir: work, OutputDir: work, Store: store}
	keys := t.TempDir()
	os.MkdirAll(filepath.Join(keys, "show"), 0o755)
	os.WriteFile(filepath.Join(keys, "show", "S01E01.key"), []byte("0123456789abcdef"), 0o600)
	s.KeyDir, s.KeyCORSOrigin = keys, "https://player.example"
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	if code, body := do(t, ts, "GET", "/ready", ""); code != 503 || !strings.Contains(body, `"ffmpeg":"not found`) {
		t.Errorf("ready: %d %s", code, body)
	}
	if code, _ := do(t, ts, "POST", "/job", job1); code != 503 {
		t.Errorf("job accepted while not ready: %d", code)
	}

	resp, err := http.Get(ts.URL + "/keys/show/S01E01.key")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(b) != "0123456789abcdef" || resp.Header.Get("Access-Control-Allow-Origin") != "https://player.example" {
		t.Errorf("key: %d %q cors=%q", resp.StatusCode, b, resp.Header.Get("Access-Control-Allow-Origin"))
	}
	for _, p := range []string{"/keys/show/S01E02.key", "/keys/show/S01E01", "/keys/..%2Fshow/S01E01.key", "/keys/show/..%2F..%2Fsecret.key"} {
		if code, _ := do(t, ts, "GET", p, ""); code != 404 {
			t.Errorf("%s: want 404, got %d", p, code)
		}
	}
}

func TestMediaServing(t *testing.T) {
	db, err := utils.OpenDB(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	s := NewServer(db, utils.NewJobStore(db), slog.New(slog.NewTextHandler(io.Discard, nil)))
	out := t.TempDir()
	ep := filepath.Join(out, "show", "ep1")
	os.MkdirAll(filepath.Join(ep, "720p"), 0o755)
	os.WriteFile(filepath.Join(ep, "master.m3u8"), []byte("#EXTM3U\n"), 0o644)
	os.WriteFile(filepath.Join(ep, "720p", "seg0000.ts"), []byte("0123456789"), 0o644)
	os.MkdirAll(filepath.Join(out, ".trash", "x"), 0o755)
	os.WriteFile(filepath.Join(out, ".trash", "x", "a.ts"), []byte("x"), 0o644)
	s.MediaDir = out
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/media/show/ep1/master.m3u8")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "application/vnd.apple.mpegurl" {
		t.Errorf("master: %d %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	req, _ := http.NewRequest("GET", ts.URL+"/media/show/ep1/720p/seg0000.ts", nil)
	req.Header.Set("Range", "bytes=2-4")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 206 || string(b) != "234" || resp.Header.Get("Content-Type") != "video/mp2t" {
		t.Errorf("range: %d %q %q", resp.StatusCode, b, resp.Header.Get("Content-Type"))
	}
	for _, p := range []string{"/media/show/ep1", "/media/.trash/x/a.ts", "/media/show/../.trash/x/a.ts", "/media/..%2F..%2Fetc%2Fpasswd", "/play/show/ep9"} {
		if code, _ := do(t, ts, "GET", p, ""); code != 404 {
			t.Errorf("%s: want 404, got %d", p, code)
		}
	}
	if code, body := do(t, ts, "GET", "/play", ""); code != 200 || !strings.Contains(body, `/play/show/ep1`) {
		t.Errorf("index: %d %s", code, body)
	}
	if code, body := do(t, ts, "GET", "/play/show/ep1", ""); code != 200 || !strings.Contains(body, `/media/show/ep1/`) || !strings.Contains(body, "integrity=") {
		t.Errorf("player: %d", code)
	}
}

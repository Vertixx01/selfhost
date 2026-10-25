package api

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"torclient/source/models"
	"torclient/source/utils"
)

func TestControlEndpoints(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db, err := utils.OpenDB(ctx, filepath.Join(root, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	store := utils.NewJobStore(db)
	layout := utils.Layout{WorkRoot: filepath.Join(root, "work"), OutputRoot: filepath.Join(root, "out"),
		FontRoot: filepath.Join(root, "fonts"), KeyRoot: filepath.Join(root, "keys")}
	s := NewServer(db, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	cancelled := 0
	s.Controls = &Controls{CancelRunning: func(string, ...string) int { cancelled++; return 0 },
		Fonts: utils.NewFontCache(db, layout), Layout: layout}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	mk := func(job string, eps ...string) {
		var es []models.Episode
		for _, e := range eps {
			es = append(es, models.Episode{EpID: e, MagnetURL: "magnet:?xt=urn:btih:" + strings.Repeat("a", 40)})
		}
		if _, _, err := store.CreateJob(ctx, models.Job{ID: job, AnimeID: "show", Title: "Show"}, es); err != nil {
			t.Fatal(err)
		}
	}
	mk("j1", "ep1", "ep2")
	store.UpdateEpisodeStatus(ctx, "j1", "ep1", models.EpDone, 100, "")

	// Cancel: only the unfinished episode, running work interrupted.
	code, body := do(t, ts, "POST", "/job/j1/cancel", "")
	if code != 200 || !strings.Contains(body, `"cancelled":["ep2"]`) || cancelled != 2 {
		t.Fatalf("cancel: %d %s (CancelRunning calls %d)", code, body, cancelled)
	}
	if code, _ := do(t, ts, "POST", "/job/nope/cancel", ""); code != 404 {
		t.Errorf("cancel unknown: %d", code)
	}

	// Retry the cancelled one.
	if code, body := do(t, ts, "POST", "/job/j1/retry", `{"episodes":["ep2"]}`); code != 200 || !strings.Contains(body, `"retried":["ep2"]`) {
		t.Fatalf("retry: %d %s", code, body)
	}
	if code, _ := do(t, ts, "POST", "/job/j1/retry", `{"episodes":["ep1"]}`); code != 400 {
		t.Errorf("retry of a done episode: %d", code)
	}
	// ep2 is queued again → job can't be deleted, nor ep2's output.
	if code, _ := do(t, ts, "DELETE", "/job/j1", ""); code != 409 {
		t.Errorf("delete running job: %d", code)
	}
	os.MkdirAll(layout.OutputDir("show", "ep2"), 0o755)
	if code, _ := do(t, ts, "DELETE", "/api/anime/show/episodes/ep2", ""); code != 409 {
		t.Errorf("delete in-flight episode: %d", code)
	}

	// Delete a packaged episode: output + key gone, row becomes retryable.
	out := layout.OutputDir("show", "ep1")
	os.MkdirAll(out, 0o755)
	os.WriteFile(filepath.Join(out, "master.m3u8"), []byte("#EXTM3U\n"), 0o644)
	os.MkdirAll(filepath.Dir(layout.KeyFile("show", "ep1")), 0o755)
	os.WriteFile(layout.KeyFile("show", "ep1"), []byte("0123456789abcdef"), 0o600)
	if code, body := do(t, ts, "DELETE", "/api/anime/show/episodes/ep1", ""); code != 204 {
		t.Fatalf("delete episode: %d %s", code, body)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Error("output still there")
	}
	if _, err := os.Stat(layout.KeyFile("show", "ep1")); !os.IsNotExist(err) {
		t.Error("key still there")
	}
	ep, _ := store.GetEpisode(ctx, "j1", "ep1")
	if ep.Status != models.EpFailed || ep.Error != "output deleted" || ep.OutputPath != "" {
		t.Errorf("ep1 after delete: %+v", ep)
	}
	if code, _ := do(t, ts, "DELETE", "/api/anime/show/episodes/ep1", ""); code != 404 {
		t.Errorf("second delete: %d", code)
	}
	if code, _ := do(t, ts, "DELETE", "/api/anime/show/episodes/..", ""); code != 400 && code != 404 {
		t.Errorf("traversal: %d", code)
	}

	// Cancel again, then delete the job record + work dir.
	do(t, ts, "POST", "/job/j1/cancel", "")
	os.MkdirAll(layout.EpWorkDir("j1", "ep2"), 0o755)
	if code, body := do(t, ts, "DELETE", "/job/j1", ""); code != 204 {
		t.Fatalf("delete job: %d %s", code, body)
	}
	if code, _ := do(t, ts, "GET", "/job/j1", ""); code != 404 {
		t.Error("job still listed")
	}
	if _, err := os.Stat(layout.WorkDir("j1")); !os.IsNotExist(err) {
		t.Error("work dir still there")
	}
}

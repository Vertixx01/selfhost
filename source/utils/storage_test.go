package utils

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"torclient/source/models"
)

func TestStorageGuard(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	root := t.TempDir()
	layout := Layout{WorkRoot: filepath.Join(root, "work"), OutputRoot: filepath.Join(root, "out"), KeyRoot: filepath.Join(root, "keys")}
	// Three packaged episodes of 1 MB each, oldest first.
	mkEp := func(anime, ep string, age time.Duration) {
		dir := layout.OutputDir(anime, ep)
		os.MkdirAll(dir, 0o755)
		os.WriteFile(filepath.Join(dir, "seg.ts"), make([]byte, 1<<20), 0o644)
		m := filepath.Join(dir, "master.m3u8")
		os.WriteFile(m, []byte("#EXTM3U\n"), 0o644)
		os.Chtimes(m, time.Now().Add(-age), time.Now().Add(-age))
	}
	mkEp("old", "ep1", 3*time.Hour)
	mkEp("mid", "ep1", 2*time.Hour)
	mkEp("cur", "ep1", 1*time.Hour)
	if _, _, err := s.CreateJob(ctx, models.Job{ID: "j", AnimeID: "old", Title: "x"},
		[]models.Episode{{EpID: "ep1", MagnetURL: "magnet:?xt=urn:btih:" + strings.Repeat("a", 40)}}); err != nil {
		t.Fatal(err)
	}
	s.UpdateEpisodeStatus(ctx, "j", "ep1", models.EpDone, 100, "")

	g := &StorageGuard{Limit: 3<<20 + 1<<19, Dirs: []string{layout.WorkRoot, layout.OutputRoot}, Store: s, Layout: layout}
	if g.Full() {
		t.Fatal("3.0 of 3.5 MB counted as full")
	}
	if err := g.Reserve(ctx, 1<<20, "cur"); !errors.Is(err, ErrStorageFull) {
		t.Fatalf("over limit without auto-delete: %v", err)
	}

	g.AutoDelete = true
	if err := g.Reserve(ctx, 1<<20, "cur"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(layout.OutputDir("old", "ep1")); !os.IsNotExist(err) {
		t.Error("oldest episode not deleted")
	}
	if _, err := os.Stat(layout.OutputDir("mid", "ep1")); err != nil {
		t.Error("deleted more than needed")
	}
	ep, _ := s.GetEpisode(ctx, "j", "ep1")
	if ep.Status != models.EpFailed || ep.Error != "output deleted" {
		t.Errorf("deleted episode's row: %+v", ep)
	}

	// Only "cur" (the series being downloaded) and an in-flight one remain
	// as candidates: neither may be deleted.
	if _, _, err := s.CreateJob(ctx, models.Job{ID: "j2", AnimeID: "mid", Title: "x"},
		[]models.Episode{{EpID: "ep1", MagnetURL: "magnet:?xt=urn:btih:" + strings.Repeat("b", 40)}}); err != nil {
		t.Fatal(err)
	}
	if err := g.Reserve(ctx, 3<<20, "cur"); !errors.Is(err, ErrStorageFull) {
		t.Fatalf("want full: %v", err)
	}
	for _, a := range []string{"mid", "cur"} {
		if _, err := os.Stat(layout.OutputDir(a, "ep1")); err != nil {
			t.Errorf("%s deleted although protected", a)
		}
	}
	var nilGuard *StorageGuard
	if nilGuard.Full() || nilGuard.Reserve(ctx, 1<<40, "") != nil {
		t.Error("nil guard should mean no limit")
	}
}

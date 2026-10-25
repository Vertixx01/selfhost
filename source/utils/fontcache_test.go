package utils

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"torclient/source/models"
)

func TestFontCacheDedupAndRelease(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	if _, _, err := s.CreateJob(ctx, models.Job{ID: "j1", AnimeID: "a1"}, eps("e1", "e2")); err != nil {
		t.Fatal(err)
	}
	layout := Layout{FontRoot: filepath.Join(t.TempDir(), "fonts")}
	fc := NewFontCache(s.db, layout)

	src := filepath.Join(t.TempDir(), "font.bin")
	os.WriteFile(src, []byte("same font content"), 0o644)

	h1, err := fc.Register(ctx, "j1", "e1", src, "SourceHanSans-Bold.otf")
	if err != nil {
		t.Fatal(err)
	}
	h2, err := fc.Register(ctx, "j1", "e2", src, "SHS_Bold.otf") // same bytes, other name
	if err != nil || h1 != h2 || len(h1) != 16 {
		t.Fatalf("hashes %q %q, %v", h1, h2, err)
	}
	// Retried stage: no extra reference.
	if _, err := fc.Register(ctx, "j1", "e1", src, "SourceHanSans-Bold.otf"); err != nil {
		t.Fatal(err)
	}

	var refs int
	s.db.QueryRow(`SELECT ref_count FROM font_cache WHERE hash = ?`, h1).Scan(&refs)
	if refs != 2 {
		t.Fatalf("ref_count = %d, want 2", refs)
	}
	entries, _ := os.ReadDir(layout.FontCacheDir(h1))
	if len(entries) != 1 || entries[0].Name() != "SourceHanSans-Bold.otf" {
		t.Fatalf("cache dir should hold one canonical copy, has %v", entries)
	}

	got, err := fc.EpisodeFonts(ctx, "j1", "e2")
	if err != nil || len(got) != 1 || got[0].Filename != "SHS_Bold.otf" || filepath.Base(got[0].CachePath) != "SourceHanSans-Bold.otf" {
		t.Fatalf("EpisodeFonts: %+v %v", got, err)
	}

	if err := fc.Release(ctx, "j1", "e1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(got[0].CachePath); err != nil {
		t.Fatalf("font removed while e2 still uses it: %v", err)
	}
	if err := fc.Release(ctx, "j1", "e2"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(layout.FontCacheDir(h1)); !os.IsNotExist(err) {
		t.Fatalf("cache dir should be gone, stat err = %v", err)
	}
	var rows int
	s.db.QueryRow(`SELECT COUNT(*) FROM font_cache`).Scan(&rows)
	if rows != 0 {
		t.Fatalf("font_cache has %d rows after release", rows)
	}
}

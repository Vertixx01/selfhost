package config

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestParseKbps(t *testing.T) {
	for in, want := range map[string]int{"5000k": 5000, "5M": 5000, "128000": 128, " 2.5m ": 2500} {
		if got, err := ParseKbps(in); err != nil || got != want {
			t.Errorf("ParseKbps(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "fast", "-5k", "0"} {
		if _, err := ParseKbps(in); err == nil {
			t.Errorf("ParseKbps(%q): want error", in)
		}
	}
}

func TestLoadRejectsKeyDirInsideOutput(t *testing.T) {
	out := t.TempDir()
	t.Setenv("OUTPUT_DIR", out)
	t.Setenv("KEY_DIR", filepath.Join(out, "keys"))
	if _, err := Load("does-not-exist.env"); err == nil || !strings.Contains(err.Error(), "KEY_DIR") {
		t.Fatalf("want KEY_DIR error, got %v", err)
	}
}

func TestLoadReportsAllBadValues(t *testing.T) {
	t.Setenv("MAX_FFMPEG_WORKERS", "zero")
	t.Setenv("HLS_ENCRYPT", "maybe")
	_, err := Load("does-not-exist.env")
	if err == nil || !strings.Contains(err.Error(), "MAX_FFMPEG_WORKERS") || !strings.Contains(err.Error(), "HLS_ENCRYPT") {
		t.Fatalf("want both errors, got %v", err)
	}
}

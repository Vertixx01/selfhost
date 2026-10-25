// Package media is the analysis half of the pipeline (PROJECT.MD Stages 3-5):
// ffprobe → stream map, subtitle and font extraction, audio mapping.
package media

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Tools holds the external binaries. Mkvextract may be empty (fallbacks off).
type Tools struct {
	FFmpeg     string
	FFprobe    string
	Mkvextract string
}

// run executes a tool and returns stdout. On failure the error carries the
// last lines of stderr, which is where ffmpeg explains itself.
func run(ctx context.Context, bin string, args ...string) ([]byte, error) {
	return Run(ctx, bin, args...)
}

// Run executes a tool and returns stdout; errors carry the tail of stderr.
func Run(ctx context.Context, bin string, args ...string) ([]byte, error) {
	stdout, _, err := RunCapture(ctx, bin, args...)
	return stdout, err
}

// RunCapture is run but also returns stderr, for filters like silencedetect
// that report through the log.
func RunCapture(ctx context.Context, bin string, args ...string) (stdout, stderr []byte, err error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	var out, errBuf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errBuf
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		return nil, nil, fmt.Errorf("%s: %w: %s", filepath.Base(bin), err, tail(errBuf.String(), 5))
	}
	return out.Bytes(), errBuf.Bytes(), nil
}

func tail(s string, lines int) string {
	parts := strings.Split(strings.TrimSpace(s), "\n")
	if len(parts) > lines {
		parts = parts[len(parts)-lines:]
	}
	return strings.Join(parts, " | ")
}

// WriteJSON writes v as indented JSON via a temp file + rename, so a crash
// never leaves a half-written meta file.
func WriteJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, b)
}

func writeFileAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

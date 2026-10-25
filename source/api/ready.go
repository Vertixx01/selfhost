package api

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"torclient/source/utils"
)

// Readiness is the deep /ready check (PROJECT.MD Stage 10): binaries,
// database, writable work dir, disk space. Results are cached briefly so a
// load balancer polling /ready doesn't run it constantly.
type Readiness struct {
	FFmpeg, FFprobe    string
	WorkDir, OutputDir string
	MinFreeBytes       uint64 // below this: warning, not failure
	Store              *utils.JobStore
	TTL                time.Duration

	mu     sync.Mutex
	at     time.Time
	cached ReadyReport
}

type ReadyReport struct {
	Ready    bool              `json:"ready"`
	Checks   map[string]string `json:"checks"`
	Warnings []string          `json:"warnings,omitempty"`
}

func (r *Readiness) Check(ctx context.Context) ReadyReport {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.at.IsZero() && time.Since(r.at) < r.TTL {
		return r.cached
	}

	rep := ReadyReport{Ready: true, Checks: map[string]string{}}
	fail := func(name string, err error) {
		rep.Ready = false
		rep.Checks[name] = err.Error()
	}
	for name, bin := range map[string]string{"ffmpeg": r.FFmpeg, "ffprobe": r.FFprobe} {
		if _, err := exec.LookPath(bin); err != nil {
			fail(name, fmt.Errorf("not found: %s", bin))
		} else {
			rep.Checks[name] = "ok"
		}
	}
	if err := r.Store.Ping(ctx); err != nil {
		fail("database", err)
	} else {
		rep.Checks["database"] = "ok"
	}
	if err := writable(r.WorkDir); err != nil {
		fail("work_dir", err)
	} else {
		rep.Checks["work_dir"] = "ok"
	}
	for name, dir := range map[string]string{"work_disk": r.WorkDir, "output_disk": r.OutputDir} {
		free, err := utils.FreeBytes(dir)
		switch {
		case err != nil:
			rep.Warnings = append(rep.Warnings, fmt.Sprintf("%s: can't read free space: %v", name, err))
		case free < r.MinFreeBytes:
			rep.Warnings = append(rep.Warnings, fmt.Sprintf("%s: only %d GB free", name, free>>30))
		}
		rep.Checks[name] = "ok"
	}

	r.at, r.cached = time.Now(), rep
	return rep
}

func writable(dir string) error {
	f, err := os.CreateTemp(dir, ".ready-*")
	if err != nil {
		return err
	}
	name := f.Name()
	f.Close()
	return os.Remove(filepath.Clean(name))
}

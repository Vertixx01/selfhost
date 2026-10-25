package torrent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	atorrent "github.com/anacrolix/torrent"
)

var (
	ErrNoSeeders = errors.New("no peers and no data within the connect timeout")
	ErrStalled   = errors.New("download stalled: no progress within the stall timeout")
	ErrTimeout   = errors.New("download exceeded the total timeout")
)

// minEpisodeBytes rejects zero-byte / truncated files (PROJECT.MD §7).
const minEpisodeBytes = 1 << 20

type DownloadOptions struct {
	NoPeersTimeout time.Duration // fail if no peer and no data after this long
	StallTimeout   time.Duration // fail if bytes stop increasing for this long
	TotalTimeout   time.Duration // fail if the whole download takes longer
	PollInterval   time.Duration // how often progress is sampled (default 2s)
	// OnProgress is called after every sample with verified-or-dirty bytes so far.
	OnProgress func(done, total int64)
}

// Download fetches only file idx of h, then drops the torrent and moves the
// file to dest (work/{job_id}/{ep_id}/raw/episode.mkv). The rest of the raw dir
// (other files' boundary pieces, the piece completion db) is removed.
//
// The handle is always dropped when Download returns. If dest already exists
// with the file's length (crash after the move but before the DB checkpoint)
// it returns immediately.
func Download(ctx context.Context, h *Handle, idx int, dest string, opts DownloadOptions) error {
	defer h.Drop()
	if opts.PollInterval <= 0 {
		opts.PollInterval = 2 * time.Second
	}

	files := h.t.Files()
	if idx < 0 || idx >= len(files) {
		return fmt.Errorf("file index %d out of range (torrent has %d files)", idx, len(files))
	}
	f := files[idx]
	total := f.Length()
	if st, err := os.Stat(dest); err == nil && st.Size() == total {
		return nil
	}

	for i, other := range files {
		if i != idx {
			other.SetPriority(atorrent.PiecePriorityNone)
		}
	}
	f.Download()

	start := time.Now()
	lastDone, lastChange := int64(-1), start
	tick := time.NewTicker(opts.PollInterval)
	defer tick.Stop()
	for {
		done := f.BytesCompleted()
		if opts.OnProgress != nil {
			opts.OnProgress(done, total)
		}
		if fileComplete(h, idx) {
			break
		}

		now := time.Now()
		if done != lastDone {
			lastDone, lastChange = done, now
		}
		switch {
		case opts.NoPeersTimeout > 0 && done == 0 && len(h.t.PeerConns()) == 0 && now.Sub(start) > opts.NoPeersTimeout:
			return ErrNoSeeders
		case opts.StallTimeout > 0 && now.Sub(lastChange) > opts.StallTimeout:
			return ErrStalled
		case opts.TotalTimeout > 0 && now.Sub(start) > opts.TotalTimeout:
			return ErrTimeout
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}

	src := h.DiskPath(idx)
	h.Drop() // release file handles before moving (required on Windows)

	st, err := os.Stat(src)
	if err != nil {
		return fmt.Errorf("downloaded file missing: %w", err)
	}
	if st.Size() != total || st.Size() < minEpisodeBytes {
		return fmt.Errorf("downloaded file is %d bytes, want %d (and at least %d)", st.Size(), total, minEpisodeBytes)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	if err := os.Rename(src, dest); err != nil {
		return fmt.Errorf("move %s to %s: %w", src, dest, err)
	}
	cleanRawDir(h.dataDir, dest)
	return nil
}

// fileComplete reports whether every piece overlapping file idx is verified.
// (File.BytesCompleted also counts unverified dirty chunks, so it can reach
// Length before the data is actually checked.)
func fileComplete(h *Handle, idx int) bool {
	for _, ps := range h.t.Files()[idx].State() {
		if !ps.Complete {
			return false
		}
	}
	return true
}

// cleanRawDir removes everything in dir except keep.
func cleanRawDir(dir, keep string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	keepAbs, _ := filepath.Abs(keep)
	for _, e := range entries {
		p := filepath.Join(dir, e.Name())
		if abs, _ := filepath.Abs(p); abs == keepAbs {
			continue
		}
		os.RemoveAll(p)
	}
}

// Package torrent wraps anacrolix/torrent for torClient (PROJECT.MD §5.3):
// one shared client, per-episode storage under work/{job_id}/{ep_id}/raw/.
package torrent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	atorrent "github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"

	g "github.com/anacrolix/generics"
)

// ErrBusy means another episode is already downloading the same infohash.
// Two handles can't share one anacrolix torrent: they would fight over its
// storage directory and dropping one would kill the other.
var ErrBusy = errors.New("torrent is already active for another episode")

type Config struct {
	ListenPort int  // 0 = random (tests)
	DHT        bool // PEX is always on; anacrolix has no LPD support
	PeerLimit  int  // established connections per torrent
	// MetainfoDir caches {infohash}.torrent files so a restart skips the
	// metadata exchange for torrents it has already resolved.
	MetainfoDir string
	// DisableTrackers is for tests that only talk to a local seeder.
	DisableTrackers bool
	Logger          *slog.Logger
}

type Engine struct {
	cl          *atorrent.Client
	metainfoDir string
	log         *slog.Logger

	mu     sync.Mutex
	active map[metainfo.Hash]*Handle
}

func NewEngine(cfg Config) (*Engine, error) {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if err := os.MkdirAll(cfg.MetainfoDir, 0o755); err != nil {
		return nil, err
	}

	cc := atorrent.NewDefaultClientConfig()
	cc.ListenPort = cfg.ListenPort
	cc.NoDHT = !cfg.DHT
	cc.DisablePEX = false
	cc.DisableTrackers = cfg.DisableTrackers
	if cfg.PeerLimit > 0 {
		cc.EstablishedConnsPerTorrent = cfg.PeerLimit
	}
	// Every torrent gets its own storage (AddMagnet); this only catches strays.
	cc.DataDir = cfg.MetainfoDir
	cc.Slogger = cfg.Logger.With("component", "anacrolix")

	cl, err := atorrent.NewClient(cc)
	if err != nil {
		return nil, fmt.Errorf("start torrent client: %w", err)
	}
	return &Engine{
		cl:          cl,
		metainfoDir: cfg.MetainfoDir,
		log:         cfg.Logger,
		active:      map[metainfo.Hash]*Handle{},
	}, nil
}

// Close drops every torrent and stops the client.
func (e *Engine) Close() {
	e.mu.Lock()
	handles := make([]*Handle, 0, len(e.active))
	for _, h := range e.active {
		handles = append(handles, h)
	}
	e.mu.Unlock()
	for _, h := range handles {
		h.Drop()
	}
	for _, err := range e.cl.Close() {
		e.log.Warn("torrent client close", "err", err)
	}
}

// Handle is one episode's torrent. Data is written below DataDir/torrent/.
type Handle struct {
	e       *Engine
	t       *atorrent.Torrent
	store   storage.ClientImplCloser
	dataDir string

	dropOnce sync.Once
}

func (h *Handle) Infohash() string { return h.t.InfoHash().HexString() }

// AddMagnet starts fetching a magnet's metadata, storing data under dataDir.
// If a cached .torrent exists for the infohash it is used, so no metadata
// exchange is needed. Call WaitInfo before reading files.
func (e *Engine) AddMagnet(magnet, dataDir string) (*Handle, error) {
	spec, err := atorrent.TorrentSpecFromMagnetUri(magnet)
	if err != nil {
		return nil, fmt.Errorf("parse magnet: %w", err)
	}
	if spec.InfoHash.IsZero() {
		return nil, errors.New("magnet has no v1 (btih) infohash")
	}
	if cached, err := metainfo.LoadFromFile(e.metainfoPath(spec.InfoHash)); err == nil {
		if fromCache, err := atorrent.TorrentSpecFromMetaInfoErr(cached); err == nil {
			spec.InfoBytes = fromCache.InfoBytes
			spec.Trackers = append(spec.Trackers, fromCache.Trackers...)
			// With the info already known, anacrolix never re-fetches an xs=
			// source, so web seeds learned from it must come from the cache
			// too; otherwise a web-seed-only torrent stalls after a restart.
			spec.Webseeds = append(spec.Webseeds, fromCache.Webseeds...)
		}
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	if _, ok := e.active[spec.InfoHash]; ok {
		return nil, ErrBusy
	}

	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, err
	}
	completion, err := storage.NewDefaultPieceCompletionForDir(dataDir)
	if err != nil {
		return nil, fmt.Errorf("piece completion db: %w", err)
	}
	store := storage.NewFileOpts(storage.NewFileClientOpts{
		ClientBaseDir:   dataDir,
		FilePathMaker:   func(o storage.FilePathMakerOpts) string { return relDataPath(o.Info, o.File) },
		PieceCompletion: completion,
		UsePartFiles:    g.Some(false),
		Logger:          e.log,
	})
	spec.Storage = store

	t, _, err := e.cl.AddTorrentSpec(spec)
	if err != nil {
		store.Close()
		return nil, fmt.Errorf("add torrent: %w", err)
	}
	h := &Handle{e: e, t: t, store: store, dataDir: dataDir}
	e.active[spec.InfoHash] = h
	return h, nil
}

// WaitInfo blocks until the torrent's metadata is known (or ctx ends), then
// caches the .torrent file.
func (h *Handle) WaitInfo(ctx context.Context) error {
	select {
	case <-h.t.GotInfo():
	case <-ctx.Done():
		return ctx.Err()
	}
	path := h.e.metainfoPath(h.t.InfoHash())
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		if err := writeMetainfo(path, h.t.Metainfo()); err != nil {
			h.e.log.Warn("cache metainfo", "infohash", h.Infohash(), "err", err)
		}
	}
	return nil
}

// File describes one file of a resolved torrent.
type File struct {
	Index  int
	Path   string // path inside the torrent, '/'-separated, without the torrent name
	Length int64
}

// Files lists the torrent's files. Only valid after WaitInfo.
func (h *Handle) Files() []File {
	tfs := h.t.Files()
	out := make([]File, len(tfs))
	for i, f := range tfs {
		out[i] = File{Index: i, Path: f.DisplayPath(), Length: f.Length()}
	}
	return out
}

// Name is the torrent's name from its metadata. Only valid after WaitInfo.
func (h *Handle) Name() string { return h.t.Name() }

// DiskPath is where file idx is written on disk.
func (h *Handle) DiskPath(idx int) string {
	info := h.t.Info()
	fi := h.t.Files()[idx].FileInfo()
	return filepath.Join(h.dataDir, relDataPath(info, &fi))
}

// Drop removes the torrent from the client and closes its storage (releasing
// file handles, which Windows needs before the data can be moved). Safe to
// call more than once.
func (h *Handle) Drop() {
	h.dropOnce.Do(func() {
		h.t.Drop()
		<-h.t.Closed()
		if err := h.store.Close(); err != nil {
			h.e.log.Warn("close torrent storage", "infohash", h.Infohash(), "err", err)
		}
		h.e.mu.Lock()
		delete(h.e.active, h.t.InfoHash())
		h.e.mu.Unlock()
	})
}

func (e *Engine) metainfoPath(ih metainfo.Hash) string {
	return filepath.Join(e.metainfoDir, ih.HexString()+".torrent")
}

// relDataPath places every file under "torrent/<name>/<path>" inside the
// episode's raw dir, so the downloaded tree never collides with episode.mkv
// or the piece completion db.
func relDataPath(info *metainfo.Info, fi *metainfo.FileInfo) string {
	parts := []string{"torrent"}
	if name := info.BestName(); name != metainfo.NoName {
		parts = append(parts, name)
	}
	return filepath.Join(append(parts, fi.BestPath()...)...)
}

func writeMetainfo(path string, mi metainfo.MetaInfo) error {
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if err := mi.Write(f); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

package utils

import "path/filepath"

// Layout builds every on-disk path from the configured roots (PROJECT.MD §2, §5.5).
// IDs must already be validated (see api.validID) — they are joined into paths as-is.
type Layout struct {
	WorkRoot   string
	OutputRoot string
	FontRoot   string
	KeyRoot    string
}

func (l Layout) WorkDir(jobID string) string { return filepath.Join(l.WorkRoot, jobID) }

func (l Layout) EpWorkDir(jobID, epID string) string {
	return filepath.Join(l.WorkRoot, jobID, epID)
}

func (l Layout) RawFile(jobID, epID string) string {
	return filepath.Join(l.EpWorkDir(jobID, epID), "raw", "episode.mkv")
}

func (l Layout) MetaDir(jobID, epID string) string {
	return filepath.Join(l.EpWorkDir(jobID, epID), "meta")
}

// WorkSubsDir holds extracted subtitles until packaging.
func (l Layout) WorkSubsDir(jobID, epID string) string {
	return filepath.Join(l.EpWorkDir(jobID, epID), "subs")
}

// FontsTempDir holds dumped attachments until they're registered in the cache.
func (l Layout) FontsTempDir(jobID, epID string) string {
	return filepath.Join(l.EpWorkDir(jobID, epID), ".fonts_temp")
}

func (l Layout) OutputDir(animeID, epID string) string {
	return filepath.Join(l.OutputRoot, animeID, epID)
}

func (l Layout) MasterPlaylist(animeID, epID string) string {
	return filepath.Join(l.OutputDir(animeID, epID), "master.m3u8")
}

func (l Layout) AudioDir(animeID, epID, lang string) string {
	return filepath.Join(l.OutputDir(animeID, epID), "audio", lang)
}

func (l Layout) SubsDir(animeID, epID string) string {
	return filepath.Join(l.OutputDir(animeID, epID), "subs")
}

func (l Layout) OutputFontsDir(animeID, epID string) string {
	return filepath.Join(l.SubsDir(animeID, epID), "fonts")
}

// FontCacheDir takes a full or already-truncated sha256 hex; only the first 16 chars are used.
func (l Layout) FontCacheDir(hash string) string {
	if len(hash) > 16 {
		hash = hash[:16]
	}
	return filepath.Join(l.FontRoot, hash)
}

func (l Layout) FontCacheFile(hash, filename string) string {
	return filepath.Join(l.FontCacheDir(hash), filename)
}

// KeyFile is where an episode's AES-128 key lives — outside OutputRoot on purpose.
func (l Layout) KeyFile(animeID, epID string) string {
	return filepath.Join(l.KeyRoot, animeID, epID+".key")
}

// Package models mirrors the SQLite jobs/episodes tables and the HTTP payloads.
package models

type JobStatus string

const (
	JobPending    JobStatus = "pending"
	JobProcessing JobStatus = "processing"
	JobCompleted  JobStatus = "completed"
	JobFailed     JobStatus = "failed"
)

// EpisodeStatus is the per-episode pipeline stage (see PROJECT.MD §3 state machine).
type EpisodeStatus string

const (
	EpQueued      EpisodeStatus = "queued"
	EpResolving   EpisodeStatus = "resolving"
	EpDownloading EpisodeStatus = "downloading"
	EpAnalyzing   EpisodeStatus = "analyzing"
	EpExtracting  EpisodeStatus = "extracting"
	EpTranscoding EpisodeStatus = "transcoding"
	EpPackaging   EpisodeStatus = "packaging"
	EpDone        EpisodeStatus = "done"
	EpFailed      EpisodeStatus = "failed"
)

func (s EpisodeStatus) Terminal() bool { return s == EpDone || s == EpFailed }

func (s EpisodeStatus) Valid() bool {
	switch s {
	case EpQueued, EpResolving, EpDownloading, EpAnalyzing, EpExtracting,
		EpTranscoding, EpPackaging, EpDone, EpFailed:
		return true
	}
	return false
}

// Job is one row of the jobs table. Episodes is filled by reads that join them in.
type Job struct {
	ID           string    `json:"job_id"`
	AnimeID      string    `json:"anime_id"`
	Title        string    `json:"title"`
	Status       JobStatus `json:"status"`
	CallbackURL  string    `json:"callback_url,omitempty"`
	EpisodeCount int       `json:"episode_count"`
	CreatedAt    string    `json:"created_at"`
	UpdatedAt    string    `json:"updated_at"`
	Episodes     []Episode `json:"episodes,omitempty"`
}

// Episode is one row of the episodes table.
type Episode struct {
	JobID       string `json:"-"`
	EpID        string `json:"ep_id"`
	Title       string `json:"title"`
	MagnetURL   string `json:"magnet_url"`
	Infohash    string `json:"infohash,omitempty"`
	Priority    int    `json:"priority"`
	NeedsPoster bool   `json:"needs_poster"`
	Provider    string `json:"provider"`
	// FileEpisodes picks the file inside a batch torrent by episode number
	// (season and absolute numbering); empty = the largest video file.
	FileEpisodes []int         `json:"file_episodes,omitempty"`
	SourceType   string        `json:"source_type,omitempty"`
	Status       EpisodeStatus `json:"status"`
	Progress     float64       `json:"progress"`
	Error        string        `json:"error,omitempty"`
	SizeBytes    int64         `json:"size_bytes"`
	OutputPath   string        `json:"output_path"`
	CreatedAt    string        `json:"-"`
	UpdatedAt    string        `json:"-"`
}

// NewJobRequest is the body of POST /job.
type NewJobRequest struct {
	JobID       string              `json:"job_id"`
	AnimeID     string              `json:"anime_id"`
	Title       string              `json:"title"`
	Status      string              `json:"status"` // reserved, ignored
	CallbackURL string              `json:"callback_url"`
	Episodes    []NewEpisodeRequest `json:"episodes"`
}

// AppendEpisodesRequest is the body of POST /api/anime/{anime_id}/episodes.
type AppendEpisodesRequest struct {
	JobID       string              `json:"job_id"`
	CallbackURL string              `json:"callback_url"`
	Episodes    []NewEpisodeRequest `json:"episodes"`
}

type NewEpisodeRequest struct {
	EpID        string `json:"ep_id"`
	Title       string `json:"title"`
	MagnetURL   string `json:"magnet_url"`
	Priority    int    `json:"priority"`
	NeedsPoster bool   `json:"needs_poster"`
	Provider    string `json:"provider"`
	// FileEpisodes: for a batch torrent, the episode numbers its file names
	// may use for this episode (e.g. [5, 29]).
	FileEpisodes []int `json:"file_episodes,omitempty"`
}

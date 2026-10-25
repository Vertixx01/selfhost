package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"torclient/source/models"
	"torclient/source/utils"
)

const maxBodyBytes = 1 << 20

// IDs end up in filesystem paths (work/{job_id}/{ep_id}, output/{anime_id}/{ep_id}),
// so they are restricted to a safe charset. A leading alphanumeric rules out
// ".", ".." and hidden names.
var idPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

const magnetPrefix = "magnet:?xt=urn:btih:"

func validID(s string) bool { return idPattern.MatchString(s) }

func (s *Server) handleCreateJob(w http.ResponseWriter, r *http.Request) {
	if !s.acceptingJobs(w, r) {
		return
	}
	var req models.NewJobRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if !validID(req.AnimeID) {
		writeError(w, http.StatusBadRequest, "anime_id is required and must match "+idPattern.String())
		return
	}
	job := models.Job{ID: req.JobID, AnimeID: req.AnimeID, Title: req.Title, CallbackURL: req.CallbackURL}
	s.createJob(w, r, job, req.Episodes)
}

func (s *Server) handleAppendEpisodes(w http.ResponseWriter, r *http.Request) {
	if !s.acceptingJobs(w, r) {
		return
	}
	animeID := r.PathValue("anime_id")
	if !validID(animeID) {
		writeError(w, http.StatusBadRequest, "invalid anime_id")
		return
	}
	var req models.AppendEpisodesRequest
	if !decodeBody(w, r, &req) {
		return
	}
	title, err := s.store.AnimeTitle(r.Context(), animeID)
	if errors.Is(err, utils.ErrNotFound) {
		writeError(w, http.StatusNotFound, "unknown anime_id; submit the first episodes with POST /job")
		return
	}
	if err != nil {
		s.internalError(w, "lookup anime", err)
		return
	}
	job := models.Job{ID: req.JobID, AnimeID: animeID, Title: title, CallbackURL: req.CallbackURL}
	s.createJob(w, r, job, req.Episodes)
}

// createJob is shared by POST /job and the append endpoint: validate, insert, respond.
func (s *Server) createJob(w http.ResponseWriter, r *http.Request, job models.Job, reqEps []models.NewEpisodeRequest) {
	eps, err := validateJob(job, reqEps)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	stored, created, err := s.store.CreateJob(r.Context(), job, eps)
	var conflict *utils.ConflictError
	switch {
	case errors.As(err, &conflict):
		writeJSON(w, http.StatusConflict, errorBody{Error: conflict.Error(), EpIDs: conflict.EpIDs})
		return
	case err != nil:
		s.internalError(w, "create job", err)
		return
	}

	status := http.StatusOK // idempotent re-POST
	if created {
		status = http.StatusCreated
		s.log.Info("job created", "job_id", stored.ID, "anime_id", stored.AnimeID, "episodes", len(eps))
		if s.OnJobCreated != nil {
			s.OnJobCreated()
		}
	}
	writeJSON(w, status, stored)
}

func validateJob(job models.Job, reqEps []models.NewEpisodeRequest) ([]models.Episode, error) {
	if !validID(job.ID) {
		return nil, fmt.Errorf("job_id is required and must match %s", idPattern)
	}
	if job.CallbackURL != "" {
		u, err := url.Parse(job.CallbackURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, errors.New("callback_url must be an absolute http(s) URL")
		}
	}
	if len(reqEps) == 0 {
		return nil, errors.New("episodes must contain at least one episode")
	}

	seen := make(map[string]bool, len(reqEps))
	eps := make([]models.Episode, 0, len(reqEps))
	for i, e := range reqEps {
		if !validID(e.EpID) {
			return nil, fmt.Errorf("episodes[%d].ep_id is required and must match %s", i, idPattern)
		}
		if seen[e.EpID] {
			return nil, fmt.Errorf("episodes[%d].ep_id %q appears more than once", i, e.EpID)
		}
		seen[e.EpID] = true
		if !strings.HasPrefix(e.MagnetURL, magnetPrefix) {
			return nil, fmt.Errorf("episodes[%d].magnet_url must start with %s", i, magnetPrefix)
		}
		eps = append(eps, models.Episode{
			EpID:         e.EpID,
			Title:        e.Title,
			MagnetURL:    e.MagnetURL,
			Priority:     e.Priority,
			NeedsPoster:  e.NeedsPoster,
			Provider:     e.Provider,
			FileEpisodes: e.FileEpisodes,
		})
	}
	return eps, nil
}

func (s *Server) handleGetJob(w http.ResponseWriter, r *http.Request) {
	job, err := s.store.GetJob(r.Context(), r.PathValue("job_id"))
	if errors.Is(err, utils.ErrNotFound) {
		writeError(w, http.StatusNotFound, "job not found")
		return
	}
	if err != nil {
		s.internalError(w, "get job", err)
		return
	}
	writeJSON(w, http.StatusOK, job)
}

// handleListJobs serves GET /jobs?status=&anime_id=&limit=&offset=
func (s *Server) handleListJobs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := utils.JobFilter{
		Status:  models.JobStatus(q.Get("status")),
		AnimeID: q.Get("anime_id"),
		Limit:   50,
	}
	switch f.Status {
	case "", models.JobPending, models.JobProcessing, models.JobCompleted, models.JobFailed:
	default:
		writeError(w, http.StatusBadRequest, "status must be pending, processing, completed or failed")
		return
	}
	var ok bool
	if f.Limit, ok = intParam(w, q, "limit", f.Limit, 1, 500); !ok {
		return
	}
	if f.Offset, ok = intParam(w, q, "offset", 0, 0, 1<<31-1); !ok {
		return
	}

	jobs, err := s.store.ListJobs(r.Context(), f)
	if err != nil {
		s.internalError(w, "list jobs", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"jobs": jobs})
}

// handleHealth is the lightweight check: SQLite reachable + queue depth.
// The deep /ready check (ffmpeg, disk space) comes in Phase 6.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if err := s.db.PingContext(r.Context()); err != nil {
		s.log.Error("health: db ping", "err", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "unhealthy", "error": "database unreachable"})
		return
	}
	depth, err := s.store.QueueDepth(r.Context())
	if err != nil {
		s.log.Error("health: queue depth", "err", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "unhealthy", "error": "database query failed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "episodes": depth})
}

func (s *Server) internalError(w http.ResponseWriter, op string, err error) {
	s.log.Error(op, "err", err)
	writeError(w, http.StatusInternalServerError, "internal error")
}

func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
		} else {
			writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		}
		return false
	}
	return true
}

func intParam(w http.ResponseWriter, q url.Values, key string, def, lo, hi int) (int, bool) {
	v := q.Get(key)
	if v == "" {
		return def, true
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < lo || n > hi {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("%s must be an integer between %d and %d", key, lo, hi))
		return 0, false
	}
	return n, true
}

// acceptingJobs refuses submissions with 503 while /ready fails (Stage 10).
func (s *Server) acceptingJobs(w http.ResponseWriter, r *http.Request) bool {
	if s.Controls != nil && s.Controls.Storage.Full() {
		g := s.Controls.Storage
		writeJSON(w, http.StatusInsufficientStorage, map[string]any{
			"error": fmt.Sprintf("storage limit reached: %.1f of %.1f GB used — delete episodes or raise MAX_STORAGE_GB",
				float64(g.Used())/(1<<30), float64(g.Limit)/(1<<30))})
		return false
	}
	if s.Ready == nil {
		return true
	}
	if rep := s.Ready.Check(r.Context()); !rep.Ready {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "not ready", "checks": rep.Checks})
		return false
	}
	return true
}

func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	if s.Ready == nil {
		writeError(w, http.StatusNotFound, "readiness check not configured")
		return
	}
	rep := s.Ready.Check(r.Context())
	status := http.StatusOK
	if !rep.Ready {
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, rep)
}

// handleKey serves an episode's AES-128 key (§7f). Access control (tokens,
// auth in a reverse proxy) belongs in front of this endpoint.
func (s *Server) handleKey(w http.ResponseWriter, r *http.Request) {
	if s.KeyDir == "" {
		writeError(w, http.StatusNotFound, "key serving disabled")
		return
	}
	animeID, file := r.PathValue("anime_id"), r.PathValue("file")
	epID, ok := strings.CutSuffix(file, ".key")
	if !ok || !validID(animeID) || !validID(epID) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	key, err := os.ReadFile(filepath.Join(s.KeyDir, animeID, epID+".key"))
	if err != nil || len(key) != 16 {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if s.KeyCORSOrigin != "" {
		w.Header().Set("Access-Control-Allow-Origin", s.KeyCORSOrigin)
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Cache-Control", "private, max-age=3600")
	w.Write(key)
}

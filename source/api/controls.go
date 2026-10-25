package api

import (
	"errors"
	"net/http"
	"os"

	"torclient/source/utils"
)

// Controls is what the cancel/retry/delete endpoints need beyond the store;
// nil disables them (e.g. in tests of other endpoints).
type Controls struct {
	CancelRunning func(jobID string, epIDs ...string) int // Pipeline.CancelRunning
	JobFinished   func(jobID string)                      // fires the job callback
	Fonts         *utils.FontCache
	Layout        utils.Layout
	Storage       *utils.StorageGuard // nil = no limit
}

// handleCancelJob serves POST /job/{job_id}/cancel: stop running work and
// mark every unfinished episode failed ("cancelled by user").
func (s *Server) handleCancelJob(w http.ResponseWriter, r *http.Request) {
	if s.Controls == nil {
		writeError(w, http.StatusNotFound, "controls not configured")
		return
	}
	jobID := r.PathValue("job_id")
	// Interrupt first so no running stage can checkpoint over the cancel,
	// then record, then interrupt again for anything claimed in between.
	s.Controls.CancelRunning(jobID)
	eps, finished, err := s.store.CancelJob(r.Context(), jobID)
	if errors.Is(err, utils.ErrNotFound) {
		writeError(w, http.StatusNotFound, "job not found")
		return
	}
	if err != nil {
		s.internalError(w, "cancel job", err)
		return
	}
	s.Controls.CancelRunning(jobID)
	if finished && s.Controls.JobFinished != nil {
		s.Controls.JobFinished(jobID)
	}
	s.log.Info("job cancelled", "job_id", jobID, "episodes", eps)
	writeJSON(w, http.StatusOK, map[string]any{"cancelled": nonNil(eps)})
}

// handleRetryJob serves POST /job/{job_id}/retry with an optional body
// {"episodes": ["ep2"]} (default: every failed episode).
func (s *Server) handleRetryJob(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Episodes []string `json:"episodes"`
	}
	if r.ContentLength != 0 && !decodeBody(w, r, &req) {
		return
	}
	eps, err := s.store.RetryEpisodes(r.Context(), r.PathValue("job_id"), req.Episodes)
	var conflict *utils.ConflictError
	switch {
	case errors.Is(err, utils.ErrNotFound):
		writeError(w, http.StatusNotFound, "job not found")
		return
	case errors.As(err, &conflict):
		writeJSON(w, http.StatusConflict, errorBody{Error: conflict.Error(), EpIDs: conflict.EpIDs})
		return
	case err != nil:
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(eps) > 0 && s.OnJobCreated != nil {
		s.OnJobCreated()
	}
	writeJSON(w, http.StatusOK, map[string]any{"retried": nonNil(eps)})
}

// handleDeleteJob serves DELETE /job/{job_id}: forget a finished job and its
// work files. Packaged episodes stay in the library.
func (s *Server) handleDeleteJob(w http.ResponseWriter, r *http.Request) {
	if s.Controls == nil {
		writeError(w, http.StatusNotFound, "controls not configured")
		return
	}
	jobID := r.PathValue("job_id")
	job, err := s.store.GetJob(r.Context(), jobID)
	if errors.Is(err, utils.ErrNotFound) {
		writeError(w, http.StatusNotFound, "job not found")
		return
	}
	if err != nil {
		s.internalError(w, "get job", err)
		return
	}
	for _, ep := range job.Episodes {
		if !ep.Status.Terminal() {
			writeError(w, http.StatusConflict, "job still running: cancel it first")
			return
		}
	}
	for _, ep := range job.Episodes {
		if err := s.Controls.Fonts.Release(r.Context(), jobID, ep.EpID); err != nil {
			s.internalError(w, "release fonts", err)
			return
		}
	}
	if err := os.RemoveAll(s.Controls.Layout.WorkDir(jobID)); err != nil {
		s.internalError(w, "remove work dir", err)
		return
	}
	if err := s.store.DeleteJob(r.Context(), jobID); err != nil {
		if errors.Is(err, utils.ErrBusy) {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		s.internalError(w, "delete job", err)
		return
	}
	s.log.Info("job deleted", "job_id", jobID)
	w.WriteHeader(http.StatusNoContent)
}

// handleDeleteEpisode serves DELETE /api/anime/{anime_id}/episodes/{ep_id}:
// delete a packaged episode (output, key, fingerprints). Its job rows become
// failed ("output deleted"), so Retry downloads it again.
func (s *Server) handleDeleteEpisode(w http.ResponseWriter, r *http.Request) {
	if s.Controls == nil {
		writeError(w, http.StatusNotFound, "controls not configured")
		return
	}
	animeID, epID := r.PathValue("anime_id"), r.PathValue("ep_id")
	if !validID(animeID) || !validID(epID) {
		writeError(w, http.StatusBadRequest, "invalid anime_id or ep_id")
		return
	}
	err := utils.DeleteEpisodeOutput(r.Context(), s.store, s.Controls.Layout, animeID, epID)
	switch {
	case errors.Is(err, utils.ErrBusy):
		writeError(w, http.StatusConflict, "episode is being processed: cancel its job first")
		return
	case errors.Is(err, utils.ErrNotFound):
		writeError(w, http.StatusNotFound, "no such packaged episode")
		return
	case err != nil:
		s.internalError(w, "delete episode", err)
		return
	}
	s.log.Info("episode deleted", "anime_id", animeID, "ep_id", epID)
	w.WriteHeader(http.StatusNoContent)
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// handleStorage serves GET /api/storage: usage against MAX_STORAGE_GB and
// free disk space.
func (s *Server) handleStorage(w http.ResponseWriter, r *http.Request) {
	resp := map[string]any{"used_bytes": int64(0), "limit_bytes": int64(0), "auto_delete": false}
	if s.Controls != nil && s.Controls.Storage != nil {
		g := s.Controls.Storage
		resp["used_bytes"], resp["limit_bytes"], resp["auto_delete"] = g.Used(), g.Limit, g.AutoDelete
	}
	if s.Controls != nil {
		if free, err := utils.FreeBytes(s.Controls.Layout.OutputRoot); err == nil {
			resp["disk_free_bytes"] = free
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

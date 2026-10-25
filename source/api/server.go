// Package api is torClient's HTTP surface (PROJECT.MD §3, §5.1).
package api

import (
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"torclient/source/discovery"
	"torclient/source/utils"
)

type Server struct {
	db    *sql.DB
	store *utils.JobStore
	log   *slog.Logger

	// Optional wiring (all may be left zero, e.g. in tests):
	Ready         *Readiness            // GET /ready; when set, new jobs are refused while not ready
	KeyDir        string                // GET /keys/{anime_id}/{ep_id}.key serves from here; "" = off
	KeyCORSOrigin string                // Access-Control-Allow-Origin for keys (browser players fetch cross-origin)
	OnJobCreated  func()                // wakes the worker pool
	Discovery     *discovery.Discoverer // AniList endpoints; nil = off
	MediaDir      string                // GET /media, /play serve this (OUTPUT_DIR); "" = off
	Controls      *Controls             // cancel/retry/delete; nil = off
}

func NewServer(db *sql.DB, store *utils.JobStore, log *slog.Logger) *Server {
	return &Server{db: db, store: store, log: log}
}

// Handler returns the routed handler with logging and panic recovery applied.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /job", s.handleCreateJob)
	mux.HandleFunc("GET /job/{job_id}", s.handleGetJob)
	mux.HandleFunc("GET /jobs", s.handleListJobs)
	mux.HandleFunc("POST /api/anime/{anime_id}/episodes", s.handleAppendEpisodes)
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /ready", s.handleReady)
	mux.HandleFunc("GET /keys/{anime_id}/{file}", s.handleKey)
	mux.HandleFunc("GET /api/anilist/{anilist_id}/search", s.handleAnilistSearch)
	mux.HandleFunc("POST /job/{job_id}/cancel", s.handleCancelJob)
	mux.HandleFunc("GET /api/storage", s.handleStorage)
	mux.HandleFunc("POST /job/{job_id}/retry", s.handleRetryJob)
	mux.HandleFunc("DELETE /job/{job_id}", s.handleDeleteJob)
	mux.HandleFunc("DELETE /api/anime/{anime_id}/episodes/{ep_id}", s.handleDeleteEpisode)
	mux.HandleFunc("GET /media/{path...}", s.handleMedia)
	mux.HandleFunc("GET /play", s.handlePlayIndex)
	mux.HandleFunc("GET /{$}", s.handlePlayIndex) // home page
	mux.HandleFunc("GET /play/{anime_id}/{ep_id}", s.handlePlay)
	mux.HandleFunc("POST /api/anilist/{anilist_id}/episodes", s.handleAnilistEpisodes)
	return s.recoverer(s.logRequests(mux))
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		s.log.Info("http", "method", r.Method, "path", r.URL.Path, "status", rec.status, "dur", time.Since(start))
	})
}

func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				if v == http.ErrAbortHandler {
					panic(v)
				}
				s.log.Error("panic in handler", "path", r.URL.Path, "panic", v)
				writeError(w, http.StatusInternalServerError, "internal error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

type errorBody struct {
	Error string   `json:"error"`
	EpIDs []string `json:"ep_ids,omitempty"`
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, errorBody{Error: msg})
}

package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"sync"

	"torclient/source/discovery"
	"torclient/source/models"
	"torclient/source/utils"
)

// AniList-driven discovery: torClient finds the torrents itself (sources
// ported from anitorrent) instead of being handed magnets.

const maxDiscoverEpisodes = 200

func (s *Server) anilistShow(w http.ResponseWriter, r *http.Request) (*discovery.Show, bool) {
	if s.Discovery == nil {
		writeError(w, http.StatusNotFound, "discovery not configured")
		return nil, false
	}
	id, err := strconv.Atoi(r.PathValue("anilist_id"))
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "anilist_id must be a positive integer")
		return nil, false
	}
	show, err := s.Discovery.AniList.Show(r.Context(), id)
	switch {
	case errors.Is(err, discovery.ErrNotFound):
		writeError(w, http.StatusNotFound, "no anime with that AniList ID")
		return nil, false
	case err != nil:
		s.log.Warn("anilist lookup", "anilist_id", id, "err", err)
		writeError(w, http.StatusBadGateway, "AniList lookup failed: "+err.Error())
		return nil, false
	}
	return show, true
}

// handleAnilistSearch serves GET /api/anilist/{anilist_id}/search?episode=N[&dub=1]
// — the ranked candidates and the pick, without creating a job.
func (s *Server) handleAnilistSearch(w http.ResponseWriter, r *http.Request) {
	show, ok := s.anilistShow(w, r)
	if !ok {
		return
	}
	ep := -1
	if v := r.URL.Query().Get("episode"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, "episode must be a non-negative integer")
			return
		}
		ep = n
	} else if show.Episodes != 1 && show.Format != "MOVIE" {
		writeError(w, http.StatusBadRequest, "episode is required for a series")
		return
	}
	writeJSON(w, http.StatusOK, s.Discovery.Search(r.Context(), show, ep, r.URL.Query().Get("dub") == "1"))
}

type anilistEpisodesRequest struct {
	JobID       string `json:"job_id"`
	AnimeID     string `json:"anime_id"` // default: the AniList ID
	Episodes    []int  `json:"episodes"` // omit for a movie
	CallbackURL string `json:"callback_url"`
	Priority    int    `json:"priority"`
	Dub         bool   `json:"dub"` // prefer English-dub releases
}

type episodeDiscovery struct {
	Episode int                  `json:"episode"`
	EpID    string               `json:"ep_id"`
	Picked  *discovery.Candidate `json:"picked,omitempty"`
	Error   string               `json:"error,omitempty"`
	Errors  map[string]string    `json:"source_errors,omitempty"`
}

// handleAnilistEpisodes serves POST /api/anilist/{anilist_id}/episodes: find
// a release for each requested episode, then submit them as one job (the
// same path as POST /job). Episodes with no suitable release are reported
// and left out; if none is found, no job is created.
func (s *Server) handleAnilistEpisodes(w http.ResponseWriter, r *http.Request) {
	if !s.acceptingJobs(w, r) {
		return
	}
	var req anilistEpisodesRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if !validID(req.JobID) {
		writeError(w, http.StatusBadRequest, "job_id is required and must match "+idPattern.String())
		return
	}
	// Idempotent re-POST: don't search again.
	if job, err := s.store.GetJob(r.Context(), req.JobID); err == nil {
		writeJSON(w, http.StatusOK, map[string]any{"job": job})
		return
	} else if !errors.Is(err, utils.ErrNotFound) {
		s.internalError(w, "get job", err)
		return
	}

	show, ok := s.anilistShow(w, r)
	if !ok {
		return
	}
	animeID := req.AnimeID
	if animeID == "" {
		animeID = strconv.Itoa(show.ID)
	}
	if !validID(animeID) {
		writeError(w, http.StatusBadRequest, "anime_id must match "+idPattern.String())
		return
	}

	movie := show.Episodes == 1 || show.Format == "MOVIE"
	eps := slices.Clone(req.Episodes)
	switch {
	case movie && len(eps) == 0:
		eps = []int{1}
	case movie && !(len(eps) == 1 && eps[0] == 1):
		writeError(w, http.StatusBadRequest, "this is a movie/single-episode show: omit episodes or send [1]")
		return
	case len(eps) == 0 || len(eps) > maxDiscoverEpisodes:
		writeError(w, http.StatusBadRequest, fmt.Sprintf("episodes must list 1–%d episode numbers", maxDiscoverEpisodes))
		return
	}
	slices.Sort(eps)
	if len(slices.Compact(slices.Clone(eps))) != len(eps) || eps[0] < 0 {
		writeError(w, http.StatusBadRequest, "episodes must be unique, non-negative numbers")
		return
	}

	results := s.discoverEpisodes(r.Context(), show, eps, movie, req.Dub)
	var newEps []models.NewEpisodeRequest
	for _, d := range results {
		if d.Picked == nil {
			continue
		}
		newEps = append(newEps, models.NewEpisodeRequest{
			EpID: d.EpID, Title: d.Picked.Title, MagnetURL: d.Picked.Link,
			Priority: req.Priority, Provider: d.Picked.Source,
		})
	}
	if len(newEps) == 0 {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "no suitable release found for any requested episode", "discovery": results})
		return
	}
	job := models.Job{ID: req.JobID, AnimeID: animeID, Title: firstTitle(show), CallbackURL: req.CallbackURL}
	eps2, err := validateJob(job, newEps)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	stored, created, err := s.store.CreateJob(r.Context(), job, eps2)
	var conflict *utils.ConflictError
	switch {
	case errors.As(err, &conflict):
		writeJSON(w, http.StatusConflict, map[string]any{"error": conflict.Error(), "ep_ids": conflict.EpIDs, "discovery": results})
		return
	case err != nil:
		s.internalError(w, "create job", err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
		s.log.Info("job created from AniList discovery", "job_id", stored.ID, "anilist_id", show.ID, "episodes", len(eps2), "requested", len(eps))
		if s.OnJobCreated != nil {
			s.OnJobCreated()
		}
	}
	writeJSON(w, status, map[string]any{"job": stored, "discovery": results})
}

// discoverEpisodes searches episodes three at a time (nyaa.si rate limits).
func (s *Server) discoverEpisodes(ctx context.Context, show *discovery.Show, eps []int, movie, dub bool) []episodeDiscovery {
	out := make([]episodeDiscovery, len(eps))
	sem := make(chan struct{}, 3)
	var wg sync.WaitGroup
	for i, ep := range eps {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			d := episodeDiscovery{Episode: ep, EpID: fmt.Sprintf("ep%d", ep)}
			if movie {
				d.EpID = "movie"
			}
			res := s.Discovery.Search(ctx, show, ep, dub)
			d.Picked, d.Errors = res.Picked, res.Errors
			if d.Picked == nil {
				d.Error = discovery.ErrNoRelease.Error()
				for _, c := range res.Candidates {
					if c.Why != "" {
						d.Error += " (best candidate: " + c.Why + ")"
						break
					}
				}
			}
			out[i] = d
		}()
	}
	wg.Wait()
	return out
}

func firstTitle(show *discovery.Show) string {
	if len(show.Titles) > 0 {
		return show.Titles[0]
	}
	return strconv.Itoa(show.ID)
}

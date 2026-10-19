package server

import (
	"cmp"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/sirrobot01/decypharr/pkg/repair"
)

// handleTautulli handles webhooks from Tautulli. When the payload includes a
// tvdb/tmdb id (or a generic media_id), the repair system runs a targeted
// recheck against that specific media — the v2 equivalent of v1's
// "media-id-scoped repair job". When no media id is supplied the webhook
// falls back to a full manual sweep.
func (s *Server) handleTautulli(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		Topic   string `json:"topic"`
		Arr     string `json:"arr,omitempty"`
		MediaID string `json:"media_id,omitempty"`
		TvdbID  string `json:"tvdb_id,omitempty"`
		TmdbID  string `json:"tmdb_id,omitempty"`
		Fix     bool   `json:"fix,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		s.logger.Error().Err(err).Msg("Failed to parse webhook body")
		http.Error(w, "Failed to parse webhook body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if payload.Topic != "tautulli" {
		http.Error(w, "Invalid topic", http.StatusBadRequest)
		return
	}

	svc := s.manager.Repair()
	if svc == nil {
		http.Error(w, "Repair service not available", http.StatusServiceUnavailable)
		return
	}

	mediaID := strings.TrimSpace(cmp.Or(payload.MediaID, payload.TmdbID, payload.TvdbID))
	if mediaID == "" {
		// No targeting → fall back to a full sweep.
		if _, err := svc.RunNow(repair.RunOptions{}); err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		w.WriteHeader(http.StatusOK)
		return
	}

	run, err := svc.RecheckMedia(s.manager.Context(), strings.TrimSpace(payload.Arr), mediaID, payload.Fix)
	if err != nil {
		status := repairErrStatus(err)
		http.Error(w, err.Error(), status)
		return
	}

	if run != nil {
		s.logger.Info().
			Str("run_id", run.ID).
			Str("arr", payload.Arr).
			Str("media_id", mediaID).
			Bool("fix", payload.Fix).
			Msg("Tautulli webhook: media recheck triggered")
	}
	w.WriteHeader(http.StatusOK)
}

// handleArrWebhook binds an arr import to its decypharr entry as soon as the arr reports it.
func (s *Server) handleArrWebhook(w http.ResponseWriter, r *http.Request) {
	arrName := chi.URLParam(r, "name")
	if instance, ok := s.manager.Arr().Get(arrName); !ok || !instance.ImportWebhook {
		http.Error(w, "import webhook is not enabled for this arr", http.StatusNotFound)
		return
	}
	entryID, err := arrImportEntry(r.Body)
	if err != nil {
		http.Error(w, "Failed to parse webhook body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if entryID == "" {
		w.WriteHeader(http.StatusOK)
		return
	}
	if entry, getErr := s.manager.Storage().Get(entryID); getErr != nil || entry == nil {
		s.logger.Debug().Str("arr", arrName).Str("download_id", entryID).
			Msg("Arr imported a download decypharr does not own")
		w.WriteHeader(http.StatusOK)
		return
	}
	queued := s.manager.ReindexArrEntry(arrName, entryID)
	s.logger.Info().Str("arr", arrName).Str("entry_id", entryID).Bool("queued", queued).
		Msg("Arr imported a download; indexing it")
	w.WriteHeader(http.StatusAccepted)
}

// arrImportEntry returns the entry an arr's import webhook names, or "" for any other event.
func arrImportEntry(body io.Reader) (string, error) {
	var payload struct {
		EventType  string `json:"eventType"`
		DownloadID string `json:"downloadId"`
	}
	if err := json.NewDecoder(body).Decode(&payload); err != nil {
		return "", err
	}
	if payload.EventType != "Download" {
		return "", nil
	}
	return strings.ToLower(strings.TrimSpace(payload.DownloadID)), nil
}

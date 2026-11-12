package server

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/sirrobot01/decypharr/internal/task/keyed"
	"github.com/sirrobot01/decypharr/internal/utils"
	"github.com/sirrobot01/decypharr/pkg/manager"
)

type debugTask struct {
	Path    string    `json:"path"`
	Started time.Time `json:"started"`
}

// handleDebugTasks lists the tasks the running generation owns, sorted by
// path: what the process is doing and since when. The actors field lists the
// keyed runners with each key's mailbox depth and the age of its oldest
// message. The refused_moves field counts, by state machine, the moves each
// refused (rclone.mount also counts moves dropped under contention): any
// count above zero is worth a look.
func (s *Server) handleDebugTasks(w http.ResponseWriter, _ *http.Request) {
	tasks := []debugTask{}
	if s.tasks != nil {
		for _, info := range s.tasks() {
			tasks = append(tasks, debugTask{Path: info.Path, Started: info.Started})
		}
	}
	actors := []keyed.Info{}
	if s.actors != nil {
		actors = append(actors, s.actors()...)
	}
	refused := map[string]uint64{}
	if s.refusals != nil {
		refused = s.refusals()
	}
	utils.JSONResponse(w, map[string]any{"tasks": tasks, "actors": actors, "refused_moves": refused}, http.StatusOK)
}

func (s *Server) handleIngests(w http.ResponseWriter, _ *http.Request) {
	ingests, err := s.manager.GetIngests()
	if err != nil {
		s.logger.Error().Err(err).Msg("Failed to get ingests")
		http.Error(w, "Failed to get ingests: "+err.Error(), http.StatusInternalServerError)
		return
	}

	utils.JSONResponse(w, ingests, http.StatusOK)
}

func (s *Server) handleIngestsByDebrid(w http.ResponseWriter, r *http.Request) {
	debridName := chi.URLParam(r, "debrid")
	if debridName == "" {
		http.Error(w, "Provider name is required", http.StatusBadRequest)
		return
	}
	ingests, err := s.manager.GetIngestsByDebrid(debridName)
	if err != nil {
		s.logger.Error().Err(err).Str("debrid", debridName).Msg("Failed to get ingests by debrid")
		http.Error(w, "Failed to get ingests: "+err.Error(), http.StatusInternalServerError)
		return
	}

	utils.JSONResponse(w, ingests, http.StatusOK)
}

// handleSpeedTest runs a speed test for a specific provider.
func (s *Server) handleSpeedTest(w http.ResponseWriter, r *http.Request) {
	var req manager.SpeedTestRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
		return
	}

	if req.Protocol == "" {
		http.Error(w, "protocol is required", http.StatusBadRequest)
		return
	}
	if req.Provider == "" {
		http.Error(w, "provider is required", http.StatusBadRequest)
		return
	}

	result := s.manager.SpeedTest(r.Context(), req)
	utils.JSONResponse(w, result, http.StatusOK)
}

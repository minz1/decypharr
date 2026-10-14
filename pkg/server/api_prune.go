package server

import (
	"errors"
	"net/http"

	"github.com/sirrobot01/decypharr/internal/utils"
	"github.com/sirrobot01/decypharr/pkg/manager"
)

type pruner interface {
	LastPruneReport() manager.PruneReport
	RunPruneNow(opts manager.PruneOptions) (manager.PruneReport, error)
}

func (s *Server) handlePruneStatus(w http.ResponseWriter, _ *http.Request) {
	utils.JSONResponse(w, s.prune.LastPruneReport(), http.StatusOK)
}

func (s *Server) handlePruneRun(w http.ResponseWriter, r *http.Request) {
	opts := manager.PruneOptions{AcceptBaseline: r.URL.Query().Get("accept_baseline") == "true"}
	report, err := s.prune.RunPruneNow(opts)
	switch {
	case errors.Is(err, manager.ErrPruneDisabled):
		s.sendJSONError(w, err.Error(), http.StatusConflict)
	case err != nil && report.Aborted == "":
		s.sendJSONError(w, err.Error(), http.StatusInternalServerError)
	default:
		utils.JSONResponse(w, report, http.StatusOK)
	}
}

package server

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"sort"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/utils"
	"github.com/sirrobot01/decypharr/pkg/repair"
	"github.com/sirrobot01/decypharr/pkg/storage"
	"github.com/sirrobot01/decypharr/pkg/version"
)

type mountCacheCleaner interface {
	CleanupCache() (map[string]any, error)
}

type mountCachePurger interface {
	PurgeCache() (map[string]any, error)
}

func (s *Server) handleGetArrs(w http.ResponseWriter, _ *http.Request) {
	utils.JSONResponse(w, s.manager.Arr().All(), http.StatusOK)
}

func (s *Server) handleGetVersion(w http.ResponseWriter, _ *http.Request) {
	v := version.GetInfo()
	utils.JSONResponse(w, v, http.StatusOK)
}

func (s *Server) handleRunMountCacheCleanup(w http.ResponseWriter, _ *http.Request) {
	mountMgr := s.manager.MountManager()
	if mountMgr == nil || !mountMgr.IsReady() {
		http.Error(w, "Mount is not ready", http.StatusServiceUnavailable)
		return
	}

	cleaner, ok := mountMgr.(mountCacheCleaner)
	if !ok {
		http.Error(w, "Manual cache cleanup is only available for DFS mounts", http.StatusBadRequest)
		return
	}

	cleanupStats, err := cleaner.CleanupCache()
	if err != nil {
		s.logger.Error().Err(err).Msg("Failed to run mount cache cleanup")
		http.Error(w, "Failed to run mount cache cleanup", http.StatusInternalServerError)
		return
	}

	if s.stats != nil {
		s.stats.Refresh()
	}

	utils.JSONResponse(w, map[string]any{
		keyStatus: keySuccess,
		"cache":   cleanupStats,
	}, http.StatusOK)
}

func (s *Server) handlePurgeMountCache(w http.ResponseWriter, _ *http.Request) {
	mountMgr := s.manager.MountManager()
	if mountMgr == nil || !mountMgr.IsReady() {
		http.Error(w, "Mount is not ready", http.StatusServiceUnavailable)
		return
	}

	purger, ok := mountMgr.(mountCachePurger)
	if !ok {
		http.Error(w, "Cache purge is only available for DFS mounts", http.StatusBadRequest)
		return
	}

	purgeStats, err := purger.PurgeCache()
	if err != nil {
		s.logger.Error().Err(err).Msg("Failed to purge mount cache")
		http.Error(w, "Failed to purge mount cache", http.StatusInternalServerError)
		return
	}

	if s.stats != nil {
		s.stats.Refresh()
	}

	utils.JSONResponse(w, map[string]any{
		keyStatus: keySuccess,
		"cache":   purgeStats,
	}, http.StatusOK)
}

func (s *Server) handleGetTorrents(w http.ResponseWriter, r *http.Request) {
	page, limit := pageParams(r.URL.Query(), defaultQueuePageLimit)
	search := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("search")))
	category := strings.TrimSpace(r.URL.Query().Get("category"))
	state := strings.TrimSpace(r.URL.Query().Get("state"))
	sortBy := strings.TrimSpace(r.URL.Query().Get("sort_by"))
	sortOrder := strings.TrimSpace(r.URL.Query().Get("sort_order"))

	if sortBy == "" {
		sortBy = "added_on"
	}
	if sortOrder == "" {
		sortOrder = sortDesc
	}

	allTorrents, err := s.manager.Queue().ListFilter("", config.ProtocolAll, "", nil, "added_on", false)
	if err != nil {
		s.logger.Error().Err(err).Msg("Failed to read the download queue")
		s.sendJSONError(w, "Failed to read the download queue", http.StatusInternalServerError)
		return
	}
	for _, t := range allTorrents {
		t.Sanitize()
	}

	filteredTorrents := make([]*storage.Entry, 0)
	categorySet := make(map[string]struct{})
	for _, t := range allTorrents {
		if t.Category != "" {
			categorySet[t.Category] = struct{}{}
		}
		if (search == "" || strings.Contains(strings.ToLower(t.Name+" "+t.InfoHash), search)) &&
			(category == "" || t.Category == category) &&
			(state == "" || t.State == storage.TorrentState(state)) {
			filteredTorrents = append(filteredTorrents, t)
		}
	}
	sortQueuedTorrents(filteredTorrents, sortBy, sortOrder)

	total := len(filteredTorrents)
	paginatedTorrents, totalPages := paginate(filteredTorrents, page, limit)
	categories := slices.Collect(maps.Keys(categorySet))

	utils.JSONResponse(w, map[string]any{
		"torrents":    paginatedTorrents,
		"total":       total,
		"page":        page,
		"limit":       limit,
		"total_pages": totalPages,
		"has_prev":    page > 1,
		"has_next":    page < totalPages,
		"categories":  categories,
	}, http.StatusOK)
}

func sortQueuedTorrents(torrents []*storage.Entry, sortBy, sortOrder string) {
	if len(torrents) == 0 {
		return
	}

	less := func(i, j int) bool {
		var result bool
		switch sortBy {
		case sortByName:
			result = strings.ToLower(torrents[i].Name) < strings.ToLower(torrents[j].Name)
		case sortBySize:
			result = torrents[i].Size < torrents[j].Size
		case "added_on":
			result = torrents[i].AddedOn.Before(torrents[j].AddedOn)
		case "progress":
			result = torrents[i].Progress < torrents[j].Progress
		case "category":
			result = strings.ToLower(torrents[i].Category) < strings.ToLower(torrents[j].Category)
		case "state":
			result = torrents[i].State < torrents[j].State
		default:
			result = torrents[i].AddedOn.Before(torrents[j].AddedOn)
		}

		if sortOrder == sortDesc {
			return !result
		}
		return result
	}

	sort.Slice(torrents, less)
}

// queueDeleteCleanup returns the per-entry hook for queue deletes: with
// removeFromDebrid it also removes the entry (and its provider placement).
func (s *Server) queueDeleteCleanup(removeFromDebrid bool) func(*storage.Entry) error {
	if !removeFromDebrid {
		return nil
	}
	return func(t *storage.Entry) error {
		if exists, _ := s.manager.EntryExists(t.InfoHash); exists {
			return s.manager.DeleteEntry(t.InfoHash, true)
		}
		go s.manager.RemoveTorrentPlacements(t)
		return nil
	}
}

func (s *Server) handleDeleteTorrent(w http.ResponseWriter, r *http.Request) {
	hash := chi.URLParam(r, "hash")
	removeFromDebrid := boolOr(queryBool(r.URL.Query(), "removeFromDebrid"), false)
	if hash == "" {
		http.Error(w, "No hash provided", http.StatusBadRequest)
		return
	}
	cleanup := s.queueDeleteCleanup(removeFromDebrid)

	if err := s.manager.Queue().Delete(hash, true, cleanup); err != nil {
		s.logger.Error().Err(err).Str("hash", hash).Msg("Failed to delete entry from queue")
		http.Error(w, "Failed to delete entry from queue", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleDeleteTorrents(w http.ResponseWriter, r *http.Request) {
	hashesStr := r.URL.Query().Get("hashes")
	removeFromDebrid := boolOr(queryBool(r.URL.Query(), "removeFromDebrid"), false)
	if hashesStr == "" {
		http.Error(w, "No hashes provided", http.StatusBadRequest)
		return
	}
	hashes := strings.Split(hashesStr, ",")
	cleanup := s.queueDeleteCleanup(removeFromDebrid)
	if err := s.manager.Queue().DeleteWhere("", config.ProtocolAll, "", hashes, cleanup); err != nil {
		s.logger.Error().Err(err).Msg("Failed to delete torrents")
		http.Error(w, "Failed to delete torrents", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleGetConfig(w http.ResponseWriter, _ *http.Request) {
	arrStorage := s.manager.Arr()
	cfg := *s.config.Get()
	cfg.Arrs = arrStorage.SyncToConfig()

	// Create response with API token info
	type ConfigResponse struct {
		*config.Config

		SessionSecret string `json:"session_secret,omitempty"`
		APIToken      string `json:"api_token,omitempty"`
		AuthUsername  string `json:"auth_username,omitempty"`
		AuthTokenOnly bool   `json:"auth_token_only"`
	}

	response := &ConfigResponse{Config: &cfg}

	// AddOrUpdate API token and auth information
	auth := cfg.GetAuth()
	if auth != nil {
		if auth.APIToken != "" {
			response.APIToken = auth.APIToken
		}
		response.AuthUsername = auth.Username
		response.AuthTokenOnly = auth.TokenOnly
	}

	utils.JSONResponse(w, response, http.StatusOK)
}

func (s *Server) handleUpdateConfig(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}
	var before config.Config
	invalid := false
	updated, err := s.config.Update(func(current *config.Config) error {
		next, prepareErr := prepareConfigUpdate(current, body)
		if prepareErr != nil {
			invalid = true
			return prepareErr
		}
		before = *current
		*current = next
		return nil
	})
	if err != nil {
		status := http.StatusInternalServerError
		if invalid {
			status = http.StatusBadRequest
		}
		s.logger.Error().Err(err).Msg("Failed to update config")
		http.Error(w, err.Error(), status)
		return
	}
	s.manager.Arr().SyncFromConfig(updated.Arrs)
	restarted := before.RequiresRestart(updated)
	if restarted {
		go s.Restart()
	} else if applyErr := s.applyLiveConfig(&before, updated); applyErr != nil {
		s.logger.Error().Err(applyErr).Msg("Failed to apply virtual folders")
		http.Error(
			w,
			"Configuration was saved, but virtual folders could not be applied: "+applyErr.Error(),
			http.StatusInternalServerError,
		)
		return
	}
	utils.JSONResponse(w, map[string]any{keyStatus: keySuccess, "restarted": restarted}, http.StatusOK)
}

// prepareConfigUpdate merges body onto current and validates it. Auth and
// secrets are never taken from the request.
func prepareConfigUpdate(current *config.Config, body []byte) (config.Config, error) {
	next, err := mergeConfigUpdate(current, bytes.NewReader(body))
	if err != nil {
		return config.Config{}, fmt.Errorf("invalid request body: %w", err)
	}
	next.MigrateVirtualFolders()
	if validateErr := next.ValidateVirtualFolders(); validateErr != nil {
		return config.Config{}, fmt.Errorf("invalid virtual folders: %w", validateErr)
	}
	next.Auth = current.Auth
	next.SessionSecret = current.SessionSecret
	next.UseAuth = current.UseAuth
	next.EnableWebdavAuth = current.EnableWebdavAuth
	next.Strm.Secret = cmp.Or(next.Strm.Secret, current.Strm.Secret)
	validArrs := make([]config.Arr, 0, len(next.Arrs))
	for _, a := range next.Arrs {
		if a.Name != "" && a.Host != "" && a.Token != "" {
			validArrs = append(validArrs, a)
		}
	}
	next.Arrs = validArrs
	return next, nil
}

// applyLiveConfig pushes a saved config that needs no restart into the
// running services. Only a virtual-folder failure is reported.
func (s *Server) applyLiveConfig(before, updated *config.Config) error {
	if before.AppURL != updated.AppURL || !reflect.DeepEqual(before.Strm, updated.Strm) {
		s.manager.Strm().SweepAsync("config_change")
	}
	if err := s.manager.ApplyVirtualFolders(updated.VirtualFolders); err != nil {
		return err
	}
	if svc := s.manager.Repair(); svc != nil {
		if err := svc.ApplyConfig(); err != nil {
			s.logger.Warn().Err(err).Msg("Failed to apply repair config")
		}
	}
	return nil
}

func mergeConfigUpdate(current *config.Config, update io.Reader) (config.Config, error) {
	if current == nil {
		return config.Config{}, fmt.Errorf("current config is unavailable")
	}

	merged, err := current.Clone()
	if err != nil {
		return config.Config{}, fmt.Errorf("copy current config: %w", err)
	}
	if decodeErr := json.NewDecoder(update).Decode(merged); decodeErr != nil {
		return config.Config{}, decodeErr
	}
	return *merged, nil
}

func (s *Server) handlePreviewVirtualFolder(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Folder config.VirtualFolder `json:"folder"`
		Limit  int                  `json:"limit"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body: "+err.Error(), http.StatusBadRequest)
		return
	}
	config.NormalizeVirtualFolder(&req.Folder)
	if err := config.ValidateVirtualFolder(req.Folder, nil); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	total, samples, err := s.manager.PreviewVirtualFolder(req.Folder, req.Limit)
	if err != nil {
		s.logger.Error().Err(err).Msg("Failed to preview virtual folder")
		http.Error(w, "Failed to preview virtual folder: "+err.Error(), http.StatusInternalServerError)
		return
	}
	utils.JSONResponse(w, map[string]any{
		"total":   total,
		"samples": samples,
	}, http.StatusOK)
}

func (s *Server) handleStrmRegenerate(w http.ResponseWriter, _ *http.Request) {
	if !s.config.Get().Strm.Active() {
		http.Error(w, "STRM is disabled or has no path configured", http.StatusBadRequest)
		return
	}
	s.manager.Strm().SweepAsync("regenerate")
	utils.JSONResponse(w, map[string]string{keyStatus: "started"}, http.StatusAccepted)
}

func (s *Server) handleGetRepairConfig(w http.ResponseWriter, _ *http.Request) {
	utils.JSONResponse(w, s.config.Get().Repair, http.StatusOK)
}

func (s *Server) handleUpdateRepairConfig(w http.ResponseWriter, r *http.Request) {
	var req config.RepairConfig
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body: "+err.Error(), http.StatusBadRequest)
		return
	}

	if msg := validateRepairConfig(&req); msg != "" {
		http.Error(w, msg, http.StatusBadRequest)
		return
	}
	if req.NNTPConnectionPercent < 0 || req.NNTPConnectionPercent > 100 {
		http.Error(w, "Invalid nntp_connection_percent (must be between 0 and 100)", http.StatusBadRequest)
		return
	}

	cfg, err := s.config.Update(func(next *config.Config) error { next.Repair = req; return nil })
	if err != nil {
		s.logger.Error().Err(err).Msg("Failed to save repair config")
		http.Error(w, "Failed to save config: "+err.Error(), http.StatusInternalServerError)
		return
	}

	if svc := s.manager.Repair(); svc != nil {
		if applyConfigErr := svc.ApplyConfig(); applyConfigErr != nil {
			s.logger.Warn().Err(applyConfigErr).Msg("Failed to apply repair config")
			http.Error(w, "Saved, but failed to apply: "+applyConfigErr.Error(), http.StatusInternalServerError)
			return
		}
	}

	utils.JSONResponse(w, cfg.Repair, http.StatusOK)
}

// validateRepairConfig returns a client-facing message for an invalid
// enabled config, or "".
func validateRepairConfig(req *config.RepairConfig) string {
	if !req.Enabled {
		return ""
	}
	if strings.TrimSpace(req.Schedule) == "" {
		return "Schedule is required when repair is enabled"
	}
	if _, err := utils.ConvertToJobDef(req.Schedule); err != nil {
		return fmt.Sprintf("Invalid schedule: %v", err)
	}
	if req.RecheckInterval != "" {
		if _, err := utils.ParseDuration(req.RecheckInterval); err != nil {
			return fmt.Sprintf("Invalid recheck_interval: %v", err)
		}
	}
	if req.Source != "" && req.Source != config.RepairSourceArr && req.Source != config.RepairSourceManaged {
		return "Invalid source (must be 'arr' or 'managed')"
	}
	return ""
}

func (s *Server) handleRepairStatus(w http.ResponseWriter, _ *http.Request) {
	svc := s.manager.Repair()
	if svc == nil {
		utils.JSONResponse(w, repair.Status{}, http.StatusOK)
		return
	}
	utils.JSONResponse(w, svc.Status(), http.StatusOK)
}

func (s *Server) handleRunRepair(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IgnoreLastChecked bool   `json:"ignore_last_checked,omitempty"`
		Force             bool   `json:"force,omitempty"`
		AutoRepair        *bool  `json:"auto_repair,omitempty"`
		UnrestrictLink    bool   `json:"unrestrict_link,omitempty"`
		VerifyContent     *bool  `json:"verify_content,omitempty"`
		Protocol          string `json:"protocol,omitempty"`
	}
	if r.Body != nil && r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			http.Error(w, "Invalid request body: "+err.Error(), http.StatusBadRequest)
			return
		}
	}
	// Query parameters override the body. ignore_last_checked/force can only
	// turn the flag on.
	q := r.URL.Query()
	ignoreLastChecked := req.IgnoreLastChecked || req.Force ||
		boolOr(queryBool(q, "ignore_last_checked"), false) || boolOr(queryBool(q, "force"), false)
	autoRepair := cmp.Or(queryBool(q, "auto_repair"), req.AutoRepair)
	unrestrictLink := boolOr(queryBool(q, "unrestrict_link"), req.UnrestrictLink)
	verifyContent := cmp.Or(queryBool(q, "verify_content"), req.VerifyContent)
	protocolScope := strings.ToLower(strings.TrimSpace(req.Protocol))
	if queryProtocol := strings.TrimSpace(r.URL.Query().Get("protocol")); queryProtocol != "" {
		protocolScope = strings.ToLower(queryProtocol)
	}
	switch protocolScope {
	case "", "all", "both", "torrent", "nzb":
		if protocolScope == "both" {
			protocolScope = "all"
		}
	default:
		http.Error(w, "Invalid protocol; expected all, torrent, or nzb", http.StatusBadRequest)
		return
	}

	svc := s.manager.Repair()
	if svc == nil {
		http.Error(w, "Repair service not available", http.StatusServiceUnavailable)
		return
	}
	id, err := svc.RunNow(repair.RunOptions{
		IgnoreLastChecked: ignoreLastChecked,
		AutoRepair:        autoRepair,
		UnrestrictLink:    unrestrictLink,
		VerifyContent:     verifyContent,
		ProtocolScope:     protocolScope,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	utils.JSONResponse(w, map[string]string{"run_id": id}, http.StatusOK)
}

// queryBool parses a lenient boolean query value; nil when absent or
// unrecognized.
func queryBool(q url.Values, key string) *bool {
	switch strings.ToLower(strings.TrimSpace(q.Get(key))) {
	case "1", "true", "yes", "on":
		return new(true)
	case "0", "false", "no", "off":
		return new(false)
	}
	return nil
}

func boolOr(v *bool, fallback bool) bool {
	if v != nil {
		return *v
	}
	return fallback
}

// repairErrStatus maps a repair-service error to an HTTP status.
func repairErrStatus(err error) int {
	if strings.Contains(err.Error(), "already running") {
		return http.StatusConflict
	}
	return http.StatusBadRequest
}

func (s *Server) handleStopRepair(w http.ResponseWriter, _ *http.Request) {
	svc := s.manager.Repair()
	if svc == nil {
		http.Error(w, "Repair service not available", http.StatusServiceUnavailable)
		return
	}
	if err := svc.StopRun(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleListRepairRuns(w http.ResponseWriter, _ *http.Request) {
	runs, err := s.manager.Storage().ListRepairRuns()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	utils.JSONResponse(w, runs, http.StatusOK)
}

func (s *Server) handleGetRepairRun(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if id == "" {
		http.Error(w, "No run ID provided", http.StatusBadRequest)
		return
	}
	run, err := s.manager.Storage().GetRepairRun(id)
	if err != nil {
		http.Error(w, "Run not found", http.StatusNotFound)
		return
	}
	utils.JSONResponse(w, run, http.StatusOK)
}

func (s *Server) handleClearRepairRuns(w http.ResponseWriter, _ *http.Request) {
	if err := s.manager.Storage().ClearRepairRuns(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleListEntryHealth(w http.ResponseWriter, r *http.Request) {
	statusFilter := strings.TrimSpace(r.URL.Query().Get("status"))
	out := make([]*storage.EntryHealth, 0)
	_ = s.manager.Storage().ForEachEntryHealth(func(state *storage.EntryHealth) error {
		if statusFilter != "" && string(state.Status) != statusFilter {
			return nil
		}
		out = append(out, state)
		return nil
	})
	sort.Slice(out, func(i, j int) bool {
		return out[i].EntryName < out[j].EntryName
	})
	utils.JSONResponse(w, out, http.StatusOK)
}

func (s *Server) handleGetEntryHealth(w http.ResponseWriter, r *http.Request) {
	name := utils.PathUnescape(chi.URLParam(r, "name"))
	if name == "" {
		http.Error(w, "No entry name provided", http.StatusBadRequest)
		return
	}
	state, err := s.manager.Storage().GetEntryHealth(name)
	if err != nil {
		http.Error(w, "Entry health not found", http.StatusNotFound)
		return
	}
	utils.JSONResponse(w, state, http.StatusOK)
}

func (s *Server) handleRecheckMedia(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Arr     string `json:"arr"`
		MediaID string `json:"media_id"`
		Fix     bool   `json:"fix"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.MediaID) == "" {
		http.Error(w, "media_id is required", http.StatusBadRequest)
		return
	}
	svc := s.manager.Repair()
	if svc == nil {
		http.Error(w, "Repair service not available", http.StatusServiceUnavailable)
		return
	}
	run, err := svc.RecheckMedia(
		s.manager.Context(),
		strings.TrimSpace(req.Arr),
		strings.TrimSpace(req.MediaID),
		req.Fix,
	)
	if err != nil {
		status := repairErrStatus(err)
		// Returning the run record (when present) gives the caller the
		// failure detail captured in storage as well as the message.
		if run != nil {
			utils.JSONResponse(w, map[string]any{
				keyError: err.Error(),
				"run":    run,
			}, status)
			return
		}
		http.Error(w, err.Error(), status)
		return
	}
	utils.JSONResponse(w, run, http.StatusOK)
}

func (s *Server) handleRecheckEntry(w http.ResponseWriter, r *http.Request) {
	name := utils.PathUnescape(chi.URLParam(r, "name"))
	if name == "" {
		http.Error(w, "No entry name provided", http.StatusBadRequest)
		return
	}
	fix := boolOr(queryBool(r.URL.Query(), "fix"), false)
	svc := s.manager.Repair()
	if svc == nil {
		http.Error(w, "Repair service not available", http.StatusServiceUnavailable)
		return
	}
	state, err := svc.RecheckEntry(s.manager.Context(), name, fix)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	utils.JSONResponse(w, state, http.StatusOK)
}

// handleFixBroken kicks off the Arr delete + re-search pass on currently
// broken entries. Body: {"names": ["...", ...]}. Empty/missing names ⇒ fix
// every broken entry in storage.
func (s *Server) handleFixBroken(w http.ResponseWriter, r *http.Request) {
	s.runBrokenAction(w, r, (*repair.Service).FixBroken)
}

// handleClearBroken clears currently broken files without asking the Arr to
// re-search for replacements. Body: {"names": ["...", ...]}. Empty/missing
// names ⇒ clear every broken entry in storage.
func (s *Server) handleClearBroken(w http.ResponseWriter, r *http.Request) {
	s.runBrokenAction(w, r, (*repair.Service).ClearBroken)
}

func (s *Server) runBrokenAction(
	w http.ResponseWriter,
	r *http.Request,
	action func(*repair.Service, context.Context, []string) (*storage.RepairRun, error),
) {
	var req struct {
		Names []string `json:"names,omitempty"`
	}
	// Body is optional.
	if r.Body != nil && r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request body: "+err.Error(), http.StatusBadRequest)
			return
		}
	}
	svc := s.manager.Repair()
	if svc == nil {
		http.Error(w, "Repair service not available", http.StatusServiceUnavailable)
		return
	}
	run, err := action(svc, s.manager.Context(), req.Names)
	if err != nil {
		http.Error(w, err.Error(), repairErrStatus(err))
		return
	}
	utils.JSONResponse(w, run, http.StatusOK)
}

func (s *Server) handleClearRepairState(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Statuses []string `json:"statuses"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body: "+err.Error(), http.StatusBadRequest)
		return
	}

	statuses := make([]storage.HealthStatus, 0, len(req.Statuses))
	for _, raw := range req.Statuses {
		status, ok := parseRepairHealthStatus(raw)
		if !ok {
			http.Error(w, "Invalid repair health status: "+raw, http.StatusBadRequest)
			return
		}
		statuses = append(statuses, status)
	}
	if len(statuses) == 0 {
		http.Error(w, "At least one status is required", http.StatusBadRequest)
		return
	}

	svc := s.manager.Repair()
	if svc == nil {
		http.Error(w, "Repair service not available", http.StatusServiceUnavailable)
		return
	}
	result, err := svc.ClearStates(statuses)
	if err != nil {
		status := repairErrStatus(err)
		http.Error(w, err.Error(), status)
		return
	}
	utils.JSONResponse(w, result, http.StatusOK)
}

func parseRepairHealthStatus(raw string) (storage.HealthStatus, bool) {
	switch storage.HealthStatus(strings.ToLower(strings.TrimSpace(raw))) {
	case storage.HealthHealthy:
		return storage.HealthHealthy, true
	case storage.HealthBroken:
		return storage.HealthBroken, true
	case storage.HealthRepairing:
		return storage.HealthRepairing, true
	case storage.HealthStale:
		return storage.HealthStale, true
	case storage.HealthUnknown:
		return storage.HealthUnknown, true
	case storage.HealthUnsupported:
		return storage.HealthUnsupported, true
	default:
		return "", false
	}
}

func (s *Server) handleRefreshAPIToken(w http.ResponseWriter, _ *http.Request) {
	token, err := s.refreshAPIToken()
	if err != nil {
		s.logger.Error().Err(err).Msg("Failed to refresh API token")
		http.Error(w, "Failed to refresh token: "+err.Error(), http.StatusInternalServerError)
		return
	}

	utils.JSONResponse(w, map[string]any{
		"token":    token,
		keyMessage: "API token refreshed successfully",
	}, http.StatusOK)
}

// validateNewCredentials returns a client-facing message, or "".
func validateNewCredentials(username, password, confirm string) string {
	switch {
	case username == "":
		return "Username is required"
	case password == "":
		return "Password is required"
	case password != confirm:
		return "Passwords do not match"
	}
	return ""
}

// clearPasswordAuth drops the username/password: token-only mode keeps the
// API token, otherwise auth is disabled and the token removed too.
func clearPasswordAuth(next *config.Config, tokenOnly bool) error {
	auth := next.GetAuth()
	if auth == nil {
		auth = &config.Auth{}
	}
	next.UseAuth = tokenOnly
	auth.Username, auth.Password = "", ""
	auth.TokenOnly = tokenOnly
	if !tokenOnly {
		auth.APIToken = ""
	}
	return next.SaveAuth(auth)
}

func (s *Server) handleUpdateAuth(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username        string `json:"username"`
		Password        string `json:"password"`
		ConfirmPassword string `json:"confirm_password"`
		TokenOnly       bool   `json:"token_only"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	disable := !req.TokenOnly && req.Username == "" && req.Password == ""
	setPassword := !req.TokenOnly && !disable
	if setPassword {
		if msg := validateNewCredentials(req.Username, req.Password, req.ConfirmPassword); msg != "" {
			http.Error(w, msg, http.StatusBadRequest)
			return
		}
	}
	cfg, err := s.config.Update(func(next *config.Config) error {
		if setPassword {
			return next.SetCredentials(req.Username, req.Password)
		}
		return clearPasswordAuth(next, req.TokenOnly)
	})
	if err != nil {
		s.logger.Error().Err(err).Msg("Failed to update authentication")
		http.Error(w, "Failed to save authentication settings", http.StatusInternalServerError)
		return
	}
	message := "Authentication settings updated successfully"
	if disable {
		message = "Authentication disabled successfully"
	}
	response := map[string]string{keyMessage: message}
	if req.TokenOnly {
		response[keyMessage] = "Token-only authentication enabled"
		if auth := cfg.GetAuth(); auth != nil {
			response["token"] = auth.APIToken
		}
		if cfg.EnableWebdavAuth {
			response[keyMessage] += ". WebDAV auth is still enabled but has no credential to accept — turn it off, or WebDAV clients will be rejected"
			s.logger.Warn().Msg("Token-only auth enabled while WebDAV auth is on")
		}
	}
	utils.JSONResponse(w, response, http.StatusOK)
}

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
	"slices"
	"sort"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/sirrobot01/hearsay/transport"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/utils"
	"github.com/sirrobot01/decypharr/pkg/arr"
	"github.com/sirrobot01/decypharr/pkg/manager"
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

// arrView is what GET /api/arrs returns for an Arr: everything but its API token.
type arrView struct {
	Name             string     `json:"name"`
	Host             string     `json:"host"`
	Type             arr.Type   `json:"type"`
	SkipRepair       bool       `json:"skip_repair"`
	DownloadUncached *bool      `json:"download_uncached"`
	SelectedDebrid   string     `json:"selected_debrid,omitempty"`
	Source           arr.Source `json:"source,omitempty"`
	ImportWebhook    bool       `json:"import_webhook,omitempty"`
}

func (s *Server) handleGetArrs(w http.ResponseWriter, _ *http.Request) {
	instances := s.manager.Arr().All()
	views := make([]arrView, 0, len(instances))
	for _, instance := range instances {
		views = append(views, arrView{
			Name:             instance.Name,
			Host:             instance.Host,
			Type:             instance.Type,
			SkipRepair:       instance.SkipRepair,
			DownloadUncached: instance.DownloadUncached,
			SelectedDebrid:   instance.SelectedDebrid,
			Source:           instance.Source,
			ImportWebhook:    instance.ImportWebhook,
		})
	}
	utils.JSONResponse(w, views, http.StatusOK)
}

func (s *Server) handleGetVersion(w http.ResponseWriter, _ *http.Request) {
	v := version.GetInfo()
	utils.JSONResponse(w, v, http.StatusOK)
}

func (s *Server) handleRunMountCacheCleanup(w http.ResponseWriter, r *http.Request) {
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
		s.stats.Refresh(r.Context())
	}

	utils.JSONResponse(w, map[string]any{
		keyStatus: keySuccess,
		"cache":   cleanupStats,
	}, http.StatusOK)
}

func (s *Server) handlePurgeMountCache(w http.ResponseWriter, r *http.Request) {
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
		s.stats.Refresh(r.Context())
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

	views := s.manager.Queue().ListFilter(manager.ListQuery{Dashboard: true}, "added_on", false)
	allTorrents := make([]*storage.Entry, len(views))
	for i := range views {
		allTorrents[i] = &views[i].Entry
		allTorrents[i].Sanitize()
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

func (s *Server) handleDeleteTorrent(w http.ResponseWriter, r *http.Request) {
	hash := chi.URLParam(r, "hash")
	removeFromDebrid := boolOr(queryBool(r.URL.Query(), "removeFromDebrid"), false)
	if hash == "" {
		http.Error(w, "No hash provided", http.StatusBadRequest)
		return
	}
	if err := s.manager.DeleteTorrent(r.Context(), hash, removeFromDebrid); err != nil {
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
	var errs []error
	for _, hash := range hashes {
		errs = append(errs, s.manager.DeleteTorrent(r.Context(), hash, removeFromDebrid))
	}
	if err := errors.Join(errs...); err != nil {
		s.logger.Error().Err(err).Msg("Failed to delete torrents")
		http.Error(w, "Failed to delete torrents", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
}

type configResponseBody struct {
	*config.Config

	APIToken      string `json:"api_token,omitempty"`
	AuthUsername  string `json:"auth_username,omitempty"`
	AuthTokenOnly bool   `json:"auth_token_only"`
}

func configResponse(cfg config.Config, arrs []config.Arr) configResponseBody {
	cfg.Arrs = arrs
	response := configResponseBody{Config: &cfg}
	if auth := cfg.GetAuth(); auth != nil {
		response.APIToken = auth.APIToken
		response.AuthUsername = auth.Username
		response.AuthTokenOnly = auth.TokenOnly
	}
	return response
}

type configGetBody struct {
	configResponseBody

	// Restart says whether the saved configuration has reached the running
	// services: "none", "pending" (a restart is on its way) or "failed" (it
	// could not be requested; the next save retries).
	Restart string `json:"restart"`
}

func (s *Server) handleGetConfig(w http.ResponseWriter, _ *http.Request) {
	body := configGetBody{
		configResponseBody: configResponse(*s.config.Get(), s.manager.Arr().SyncToConfig()),
		Restart:            s.config.Pending().String(),
	}
	utils.JSONResponse(w, body, http.StatusOK)
}

// requestRestart asks the generation for a restart once a saved setting needs
// one and records in the store whether the request could be made. It returns
// the resulting state; a state of failed carries the cause in the store.
func (s *Server) requestRestart() config.Pending {
	s.config.RestartRequested() // a retry after a failed request
	if err := s.Restart(); err != nil {
		s.logger.Error().Err(err).Msg("Restart could not be requested")
		s.config.MarkRestartFailed(err)
	}
	return s.config.Pending()
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
	s.manager.Arr().SyncFromConfig(before.Arrs, updated.Arrs)
	// The store compares with the snapshot the services run, so an earlier
	// save that still needs a restart keeps this one pending as well.
	pending := s.config.Pending()
	if pending != config.PendingNone {
		pending = s.requestRestart()
	} else if applyErr := s.applyLiveConfig(&before, updated); applyErr != nil {
		s.logger.Error().Err(applyErr).Msg("Failed to apply virtual folders")
		http.Error(
			w,
			"Configuration was saved, but virtual folders could not be applied: "+applyErr.Error(),
			http.StatusInternalServerError,
		)
		return
	}
	response := map[string]any{
		keyStatus:   keySuccess,
		"restarted": pending == config.PendingRestart,
		"restart":   pending.String(),
	}
	if cause := s.config.RestartError(); pending == config.PendingFailed && cause != nil {
		// The configuration is saved; the next save retries the restart.
		response["restart_error"] = cause.Error()
	}
	var warnings []string
	if locked := webdavAuthWarnings(updated); len(locked) > 0 {
		warnings = append(warnings, locked...)
		s.logger.Warn().Strs("warnings", locked).Msg("WebDAV auth locks out its clients")
	}
	if len(warnings) > 0 {
		response["warnings"] = warnings
	}
	status := http.StatusOK
	if pending == config.PendingRestart {
		status = http.StatusAccepted
	}
	utils.JSONResponse(w, response, status)
}

func webdavAuthWarnings(cfg *config.Config) []string {
	if cfg.DisableWebDav || !cfg.UseAuth || !cfg.EnableWebdavAuth {
		return nil
	}
	var warnings []string
	if auth := cfg.GetAuth(); auth != nil && auth.TokenOnly {
		warnings = append(
			warnings,
			"WebDAV auth is on but token-only auth has no credential WebDAV accepts — turn it off, or WebDAV clients will be rejected",
		)
	}
	if cfg.Mount.Type == config.MountTypeRclone {
		warnings = append(
			warnings,
			"WebDAV auth is on but the built-in rclone mount connects without credentials — it will be rejected",
		)
	}
	return warnings
}

// prepareConfigUpdate merges body onto current and validates it.
// Credentials, use_auth and secrets are never taken from the request.
func prepareConfigUpdate(current *config.Config, body []byte) (config.Config, error) {
	next, err := mergeConfigUpdate(current, bytes.NewReader(body))
	if err != nil {
		return config.Config{}, fmt.Errorf("invalid request body: %w", err)
	}
	next.MigrateVirtualFolders()
	if validateErr := next.ValidateVirtualFolders(); validateErr != nil {
		return config.Config{}, fmt.Errorf("invalid virtual folders: %w", validateErr)
	}
	if _, blockErr := transport.ParseBlockedPeers(
		next.Hearsay.BlockPrivatePeers,
		next.Hearsay.Blocklist,
	); blockErr != nil {
		return config.Config{}, fmt.Errorf("invalid hearsay blocklist: %w", blockErr)
	}
	next.Auth = current.Auth
	next.SessionSecret = current.SessionSecret
	next.UseAuth = current.UseAuth
	next.Strm.Secret = cmp.Or(next.Strm.Secret, current.Strm.Secret)
	validArrs := make([]config.Arr, 0, len(next.Arrs))
	for _, a := range next.Arrs {
		if a.Name != "" && a.Host != "" && a.Token != "" {
			validArrs = append(validArrs, a)
		}
	}
	next.Arrs = validArrs
	if checkErr := next.CheckLoadable(); checkErr != nil {
		return config.Config{}, checkErr
	}
	return next, nil
}

// applyLiveConfig pushes a saved config that needs no restart into the
// running services. Only a virtual-folder failure is reported.
func (s *Server) applyLiveConfig(before, updated *config.Config) error {
	if before.AppURL != updated.AppURL || !before.Strm.Equal(updated.Strm) {
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
	body, err := io.ReadAll(update)
	if err != nil {
		return config.Config{}, err
	}
	if decodeErr := json.Unmarshal(body, merged); decodeErr != nil {
		return config.Config{}, decodeErr
	}
	// encoding/json decodes an array into a slice's existing elements by
	// index, so an item that moved would keep fields it omits from the item
	// that used to be there. Merge the keyed lists by key instead.
	var lists struct {
		Debrids json.RawMessage `json:"debrids"`
		Arrs    json.RawMessage `json:"arrs"`
		Usenet  struct {
			Providers json.RawMessage `json:"providers"`
		} `json:"usenet"`
	}
	if decodeErr := json.Unmarshal(body, &lists); decodeErr != nil {
		return config.Config{}, decodeErr
	}
	// Items are decoded over copies of the old ones; take them from a deep
	// copy so their nested lists never alias the published snapshot.
	base, err := current.Clone()
	if err != nil {
		return config.Config{}, fmt.Errorf("copy current config: %w", err)
	}
	if merged.Debrids, err = mergeKeyed(base.Debrids, lists.Debrids, merged.Debrids,
		func(d config.Debrid) string { return d.Name }); err != nil {
		return config.Config{}, err
	}
	if merged.Arrs, err = mergeKeyed(base.Arrs, lists.Arrs, merged.Arrs,
		func(a config.Arr) string { return a.Name }); err != nil {
		return config.Config{}, err
	}
	if merged.Usenet.Providers, err = mergeKeyed(base.Usenet.Providers, lists.Usenet.Providers,
		merged.Usenet.Providers, func(p config.UsenetProvider) string { return p.Host }); err != nil {
		return config.Config{}, err
	}
	return *merged, nil
}

// mergeKeyed merges the JSON array raw onto old by key: each item starts
// from a copy of the old element with the same key (a new key starts empty)
// and the item's fields are decoded over it, so fields it omits keep that
// element's values. When raw is absent the list is unchanged (decoded).
func mergeKeyed[T any](old []T, raw json.RawMessage, decoded []T, key func(T) string) ([]T, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return decoded, nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, err
	}
	used := make([]bool, len(old))
	merged := make([]T, len(items))
	for i, item := range items {
		var probe T
		if err := json.Unmarshal(item, &probe); err != nil {
			return nil, err
		}
		if k := key(probe); k != "" {
			for j := range old {
				if !used[j] && key(old[j]) == k {
					merged[i], used[j] = old[j], true
					break
				}
			}
		}
		if err := json.Unmarshal(item, &merged[i]); err != nil {
			return nil, err
		}
	}
	return merged, nil
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
	if msg := validateBreakerConfig(req); msg != "" {
		return msg
	}
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

// validateBreakerConfig rejects breaker keys that could not be honoured; zero stays valid and means the default.
func validateBreakerConfig(req *config.RepairConfig) string {
	for key, raw := range map[string]string{
		"confirm_gap": req.ConfirmGap, "breaker_release_after": req.BreakerReleaseAfter,
		"canary_recent": req.CanaryRecent, "canary_fallback": req.CanaryFallback,
		"canary_cache": req.CanaryCache, "breaker_window": req.BreakerWindow,
	} {
		if raw == "" {
			continue
		}
		if d, err := utils.ParseDuration(raw); err != nil || d <= 0 {
			return fmt.Sprintf("Invalid %s (must be a positive duration)", key)
		}
	}
	for key, value := range map[string]int{
		"breaker_min_confirmed": req.BreakerMinConfirmed, "breaker_percent": req.BreakerPercent,
		"breaker_confirmed": req.BreakerConfirmed, "heal_cap_per_bucket": req.HealCapPerBucket,
		"heal_cap_per_run": req.HealCapPerRun, "canary_size": req.CanarySize,
	} {
		if value < 0 {
			return fmt.Sprintf("Invalid %s (must not be negative; 0 means the default)", key)
		}
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

type repairRunRequest struct {
	IgnoreLastChecked bool   `json:"ignore_last_checked,omitempty"`
	Force             bool   `json:"force,omitempty"`
	AutoRepair        *bool  `json:"auto_repair,omitempty"`
	UnrestrictLink    bool   `json:"unrestrict_link,omitempty"`
	VerifyContent     *bool  `json:"verify_content,omitempty"`
	Protocol          string `json:"protocol,omitempty"`
}

type repairRunResponse struct {
	RunID string `json:"run_id"`
}

func (s *Server) handleRunRepair(w http.ResponseWriter, r *http.Request) {
	var req repairRunRequest
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
	utils.JSONResponse(w, repairRunResponse{RunID: id}, http.StatusOK)
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
		// Force heals past the repair breaker for this request only.
		Force bool `json:"force"`
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
		repair.RecheckOptions{Fix: req.Fix, Source: repair.SourceOperator, Force: req.Force, Caller: requestCaller(r)},
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
	svc := s.manager.Repair()
	if svc == nil {
		http.Error(w, "Repair service not available", http.StatusServiceUnavailable)
		return
	}
	state, err := svc.RecheckEntry(s.manager.Context(), name, recheckEntryOptions(r))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	utils.JSONResponse(w, state, http.StatusOK)
}

func recheckEntryOptions(r *http.Request) repair.RecheckOptions {
	q := r.URL.Query()
	return repair.RecheckOptions{
		Fix:            boolOr(queryBool(q, "fix"), false),
		Source:         repair.SourceOperator,
		Force:          boolOr(queryBool(q, "force"), false),
		Caller:         requestCaller(r),
		UnrestrictLink: boolOr(queryBool(q, "unrestrict_link"), false),
	}
}

// handleFixBroken kicks off the Arr delete + re-search pass on currently
// broken entries. Body: {"names": ["...", ...]}. Empty/missing names ⇒ fix
// every broken entry in storage.
// Every fix passes the repair breaker; "force": true overrides it for this request only.
func (s *Server) handleFixBroken(w http.ResponseWriter, r *http.Request) {
	s.runBrokenAction(
		w,
		r,
		func(svc *repair.Service, ctx context.Context, names []string, force bool) (*storage.RepairRun, error) {
			return svc.FixBroken(ctx, names, repair.FixOptions{Force: force, Caller: requestCaller(r)})
		},
	)
}

// handleClearBroken clears currently broken files without asking the Arr to
// re-search for replacements. Body: {"names": ["...", ...]}. Empty/missing
// names ⇒ clear every broken entry in storage.
func (s *Server) handleClearBroken(w http.ResponseWriter, r *http.Request) {
	s.runBrokenAction(
		w,
		r,
		func(svc *repair.Service, ctx context.Context, names []string, _ bool) (*storage.RepairRun, error) {
			return svc.ClearBroken(ctx, names)
		},
	)
}

func (s *Server) runBrokenAction(
	w http.ResponseWriter,
	r *http.Request,
	action func(*repair.Service, context.Context, []string, bool) (*storage.RepairRun, error),
) {
	var req struct {
		Names []string `json:"names,omitempty"`
		// Force heals past the repair breaker for this request only (fix only).
		Force bool `json:"force,omitempty"`
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
	run, err := action(svc, s.manager.Context(), req.Names, req.Force)
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
	case storage.HealthHeld:
		return storage.HealthHeld, true
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
	response := map[string]any{}
	if req.TokenOnly {
		message = "Token-only authentication enabled"
		if auth := cfg.GetAuth(); auth != nil {
			response["token"] = auth.APIToken
		}
	}
	if warnings := webdavAuthWarnings(cfg); len(warnings) > 0 {
		message += ". " + strings.Join(warnings, ". ")
		response["warnings"] = warnings
		s.logger.Warn().Strs("warnings", warnings).Msg("WebDAV auth locks out its clients")
	}
	response[keyMessage] = message
	utils.JSONResponse(w, response, http.StatusOK)
}

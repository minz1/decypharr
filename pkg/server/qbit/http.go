package qbit

import (
	"errors"
	"net/http"
	"path/filepath"
	"slices"
	"strings"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/customerror"
	"github.com/sirrobot01/decypharr/internal/utils"
	"github.com/sirrobot01/decypharr/pkg/arr"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

func (q *QBit) handleLogin(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	username := r.FormValue("username")
	password := r.FormValue("password")
	_, err := q.authenticate(ctx, getCategory(ctx), username, password)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	// The SID carries the credentials, so it is Secure: browsers and strict
	// cookie jars only return it over HTTPS. The *arr download clients parse
	// Set-Cookie themselves and send SID back over plain HTTP too; any other
	// client on plain HTTP can authenticate each request with Basic auth.
	cookie := &http.Cookie{
		Name:     "SID",
		Value:    createSID(q.config.Get().SecretKey(), username, password),
		Path:     "/",
		Secure:   true,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	}
	http.SetCookie(w, cookie)
	_, _ = w.Write([]byte("Ok."))
}

func (q *QBit) handleVersion(w http.ResponseWriter, _ *http.Request) {
	_, _ = w.Write([]byte("v4.3.2"))
}

func (q *QBit) handleWebAPIVersion(w http.ResponseWriter, _ *http.Request) {
	_, _ = w.Write([]byte("2.7"))
}

func (q *QBit) handlePreferences(w http.ResponseWriter, _ *http.Request) {
	preferences := getAppPreferences(q.config.Get().MaxActiveDownloads)

	preferences.SavePath = q.downloadFolder
	preferences.TempPath = filepath.Join(q.downloadFolder, "temp")

	utils.JSONResponse(w, preferences, http.StatusOK)
}

// reportedBitness is the build bitness the fake qBittorrent reports.
const reportedBitness = 64

func (q *QBit) handleBuildInfo(w http.ResponseWriter, _ *http.Request) {
	res := BuildInfo{
		Bitness:    reportedBitness,
		Boost:      "1.75.0",
		Libtorrent: "1.2.11.0",
		Openssl:    "1.1.1i",
		Qt:         "5.15.2",
		Zlib:       "1.2.11",
	}
	utils.JSONResponse(w, res, http.StatusOK)
}

func (q *QBit) handleShutdown(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
}

func (q *QBit) handleTorrentsInfo(w http.ResponseWriter, r *http.Request) {
	// log all url params
	ctx := r.Context()
	category := getCategory(ctx)
	state := strings.Trim(r.URL.Query().Get("filter"), "")
	hashes := getHashes(ctx)

	// Convert hashes to filter function
	torrents, err := q.manager.Queue().
		ListFilter(category, config.ProtocolTorrent, storage.TorrentState(state), hashes, "added_on", false)
	if err != nil {
		http.Error(w, "Failed to read the download queue", http.StatusInternalServerError)
		return
	}
	qbitTorrents := make([]Torrent, len(torrents))
	for i, t := range torrents {
		qbitTorrents[i] = convertToQBitTorrentTorrent(t, q.config.Get().FolderNaming)
	}
	utils.JSONResponse(w, qbitTorrents, http.StatusOK)
}

// multipartMemory is how much of a multipart form is held in memory before
// spilling to temp files; the body itself is capped at maxRequestBody.
const multipartMemory = 32 << 20

func parseAddForm(w http.ResponseWriter, r *http.Request) error {
	contentType := r.Header.Get("Content-Type")
	switch {
	case strings.Contains(contentType, "multipart/form-data"):
		return utils.ParseBoundedMultipartForm(w, r, maxRequestBody, multipartMemory)
	case strings.Contains(contentType, "application/x-www-form-urlencoded"):
		return r.ParseForm()
	default:
		return errors.New("invalid content type")
	}
}

func (q *QBit) handleTorrentsAdd(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := parseAddForm(w, r); err != nil {
		q.logger.Error().Err(err).Msg("Error parsing torrent add form")
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	cfg := q.config.Get()
	action := cfg.DefaultDownloadAction
	if strings.EqualFold(r.FormValue("sequentialDownload"), "true") {
		action = config.DownloadActionDownload
	}
	rmTrackerUrls := q.alwaysRemoveTrackerURLS || strings.EqualFold(r.FormValue("firstLastPiecePrio"), "true")
	debridName := r.FormValue("debrid")
	instance := getArrFromContext(ctx)
	if instance.Name == "" {
		instance = arr.Arr{Name: r.FormValue("category")}
	}
	callbackURL := cfg.Notifications.CallbackURL

	var sources []func() error
	if urls := r.FormValue("urls"); urls != "" {
		for u := range strings.SplitSeq(urls, "\n") {
			sources = append(sources, func() error {
				return q.addMagnet(
					ctx,
					strings.TrimSpace(u),
					instance,
					debridName,
					action,
					callbackURL,
					rmTrackerUrls,
					cfg.SkipMultiSeason,
				)
			})
		}
	}
	if r.MultipartForm != nil {
		for _, fileHeader := range r.MultipartForm.File["torrents"] {
			sources = append(sources, func() error {
				return q.addTorrent(
					ctx,
					fileHeader,
					instance,
					debridName,
					action,
					callbackURL,
					rmTrackerUrls,
					cfg.SkipMultiSeason,
				)
			})
		}
	}
	if len(sources) == 0 {
		http.Error(w, "No valid URLs or torrents provided", http.StatusBadRequest)
		return
	}
	for _, add := range sources {
		if err := add(); err != nil {
			q.logger.Debug().Err(err).Msg("Error adding torrent")
			writeTorrentAddError(w, err)
			return
		}
	}
	w.WriteHeader(http.StatusOK)
}

type torrentAddErrorResponse struct {
	Error     string `json:"error"`
	Code      string `json:"code"`
	Retryable bool   `json:"retryable"`
	Permanent bool   `json:"permanent"`
}

func writeTorrentAddError(w http.ResponseWriter, err error) {
	status := http.StatusBadGateway
	response := torrentAddErrorResponse{
		Error:     err.Error(),
		Code:      "provider_error",
		Retryable: true,
	}

	if typed, ok := errors.AsType[*customerror.Error](err); ok {
		status = typed.StatusCode()
		response.Code = typed.Code
		response.Retryable = typed.IsRetryable()
		response.Permanent = typed.IsPermanent()
	}

	utils.JSONResponse(w, response, status)
}

func (q *QBit) handleTorrentsDelete(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	hashes := getHashes(ctx)
	deleteFiles := strings.EqualFold(r.FormValue("deleteFiles"), "true")

	if len(hashes) == 0 {
		http.Error(w, "No hashes provided", http.StatusBadRequest)
		return
	}
	for _, hash := range hashes {
		err := q.manager.Queue().Delete(hash, deleteFiles, nil)
		if err != nil && !strings.Contains(err.Error(), "not found") {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}

	w.WriteHeader(http.StatusOK)
}

// handleTorrentsNoop answers pause/resume/recheck: debrid-backed entries have
// no local transfer to act on, so these always succeed.
func (q *QBit) handleTorrentsNoop(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
}

func (q *QBit) handleCategories(w http.ResponseWriter, _ *http.Request) {
	q.mu.Lock()
	names := slices.Clone(q.categories)
	q.mu.Unlock()
	categories := make(map[string]TorrentCategory, len(names))
	for _, cat := range names {
		path := filepath.Join(q.downloadFolder, cat)
		categories[cat] = TorrentCategory{
			Name:     cat,
			SavePath: path,
		}
	}
	utils.JSONResponse(w, categories, http.StatusOK)
}

func (q *QBit) handleCreateCategory(w http.ResponseWriter, r *http.Request) {
	err := r.ParseForm()
	if err != nil {
		http.Error(w, "Failed to parse form data", http.StatusBadRequest)
		return
	}

	name := r.Form.Get("category")
	if name == "" {
		http.Error(w, "No name provided", http.StatusBadRequest)
		return
	}

	q.mu.Lock()
	q.categories = append(q.categories, name)
	q.mu.Unlock()

	utils.JSONResponse(w, nil, http.StatusOK)
}

func (q *QBit) handleTorrentProperties(w http.ResponseWriter, r *http.Request) {
	hash := r.URL.Query().Get("hash")
	torrent, err := q.manager.Queue().GetTorrent(hash)
	if err != nil {
		http.Error(w, "Entry not found", http.StatusNotFound)
		return
	}

	properties := q.GetTorrentProperties(torrent)
	utils.JSONResponse(w, properties, http.StatusOK)
}

func (q *QBit) handleTorrentFiles(w http.ResponseWriter, r *http.Request) {
	hash := r.URL.Query().Get("hash")
	torrent, err := q.manager.Queue().GetTorrent(hash)
	if err != nil {
		http.Error(w, "Entry not found", http.StatusNotFound)
		return
	}
	utils.JSONResponse(w, getTorrentFiles(torrent), http.StatusOK)
}

func (q *QBit) handleSetCategory(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	category := getCategory(ctx)
	hashes := getHashes(ctx)
	if len(hashes) == 0 {
		// Without hashes the queue filter matches everything; recategorizing
		// the whole queue is never what a client asked for.
		http.Error(w, "No hashes provided", http.StatusBadRequest)
		return
	}
	filterFunc := q.manager.Queue().ListFilterFunc("", config.ProtocolTorrent, "", hashes)

	updateFunc := func(t *storage.Entry) bool {
		if t.Category != category {
			t.Category = category
			return true
		}
		return false
	}

	if err := q.manager.Queue().UpdateWhere(filterFunc, updateFunc); err != nil {
		q.logger.Warn().Err(err).Msgf("Error setting torrent category")
		http.Error(w, "Failed to update torrents", http.StatusInternalServerError)
		return
	}
	utils.JSONResponse(w, nil, http.StatusOK)
}

func (q *QBit) handleAddTorrentTags(w http.ResponseWriter, r *http.Request) {
	err := r.ParseForm()
	if err != nil {
		http.Error(w, "Failed to parse form data", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	hashes := getHashes(ctx)
	tags := strings.Split(r.FormValue("tags"), ",")
	for i, tag := range tags {
		tags[i] = strings.TrimSpace(tag)
	}
	torrents, err := q.manager.Queue().ListFilter("", config.ProtocolTorrent, "", hashes, "", false)
	if err != nil {
		http.Error(w, "Failed to read the download queue", http.StatusInternalServerError)
		return
	}
	for _, t := range torrents {
		q.setTorrentTags(t, tags)
	}
	utils.JSONResponse(w, nil, http.StatusOK)
}

func (q *QBit) handleRemoveTorrentTags(w http.ResponseWriter, r *http.Request) {
	err := r.ParseForm()
	if err != nil {
		http.Error(w, "Failed to parse form data", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	hashes := getHashes(ctx)
	tags := strings.Split(r.FormValue("tags"), ",")
	for i, tag := range tags {
		tags[i] = strings.TrimSpace(tag)
	}
	torrents, err := q.manager.Queue().ListFilter("", config.ProtocolTorrent, "", hashes, "", false)
	if err != nil {
		http.Error(w, "Failed to read the download queue", http.StatusInternalServerError)
		return
	}
	for _, torrent := range torrents {
		q.removeTorrentTags(torrent, tags)
	}
	utils.JSONResponse(w, nil, http.StatusOK)
}

func (q *QBit) handleGetTags(w http.ResponseWriter, _ *http.Request) {
	q.mu.Lock()
	tags := slices.Clone(q.tags)
	q.mu.Unlock()
	utils.JSONResponse(w, tags, http.StatusOK)
}

func (q *QBit) handleCreateTags(w http.ResponseWriter, r *http.Request) {
	err := r.ParseForm()
	if err != nil {
		http.Error(w, "Failed to parse form data", http.StatusBadRequest)
		return
	}
	tags := strings.Split(r.FormValue("tags"), ",")
	for i, tag := range tags {
		tags[i] = strings.TrimSpace(tag)
	}
	q.addTags(tags)
	utils.JSONResponse(w, nil, http.StatusOK)
}

package torbox

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"go.uber.org/ratelimit"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/customerror"
	"github.com/sirrobot01/decypharr/internal/request"
	"github.com/sirrobot01/decypharr/internal/utils"
	"github.com/sirrobot01/decypharr/pkg/debrid/account"
	"github.com/sirrobot01/decypharr/pkg/debrid/common"
	"github.com/sirrobot01/decypharr/pkg/debrid/types"
	"github.com/sirrobot01/decypharr/pkg/version"
)

// torboxStatusDetail strips parenthesised detail such as "stalled (no seeds)".
var torboxStatusDetail = regexp.MustCompile(`\s*\(.*?\)\s*`)

const (
	// requestsPerMinute is TorBox's per-key API limit; burstRequests is the
	// slack allowed above the steady rate.
	requestsPerMinute = 300
	burstRequests     = 30
	// defaultLinkExpiry applies when auto_expire_links_after is unset.
	defaultLinkExpiry = 48 * time.Hour
	// batchSize is the most hashes per checkcached call.
	batchSize = 100
	// percent converts TorBox's 0..1 progress to a percentage.
	percent = 100

	queryTrue = "true"

	planEssential   = "essential"
	planStandard    = "standard"
	planPro         = "pro"
	planIDEssential = 1
	planIDPro       = 2
	planIDStandard  = 3
	essentialSlots  = 3
	standardSlots   = 5
	proSlots        = 10
)

// downloadPresentTTL bounds how stale the repair presence snapshot may be, so
// torrents added after the first load are not reported as missing.
const downloadPresentTTL = 10 * time.Minute

type Torbox struct {
	Host                  string `json:"host"`
	APIKey                string
	accountsManager       *account.Manager
	autoExpiresLinksAfter time.Duration
	client                *request.Client
	submitClient          *request.Client
	logger                zerolog.Logger
	options               types.ProviderOptions
	profile               types.ProfileCache
	config                config.Debrid
	downloadPresentMu     sync.Mutex
	downloadPresent       map[string]bool // torrent ID -> download_present; nil until loaded
	downloadPresentAt     time.Time
}

func New(dc config.Debrid, ratelimits map[string]ratelimit.Limiter, options types.ProviderOptions) (*Torbox, error) {
	headers := map[string]string{
		"Authorization": fmt.Sprintf("Bearer %s", dc.APIKey),
	}
	if dc.UserAgent != "" {
		headers["User-Agent"] = dc.UserAgent
	} else {
		headers["User-Agent"] = fmt.Sprintf("Decypharr/%s (%s; %s)", version.GetInfo(), runtime.GOOS, runtime.GOARCH)
	}
	_log := options.Logger

	// TorBox enforces a hard cap of 300 req/min per API key, applied
	// synchronously across all servers since v8.4 (Feb 2026, GAP-002).
	// Default to that limit if the user has not configured one explicitly.
	mainRL := ratelimits["main"]
	if mainRL == nil {
		mainRL = ratelimit.New(requestsPerMinute, ratelimit.Per(time.Minute), ratelimit.WithSlack(burstRequests))
	}
	// The cap counts per key, not per workload, so list traffic and submit
	// traffic authenticated with the same key have to share one bucket. Giving
	// each its own would let a refresh sweep and an import burst together emit
	// twice the configured limit against a single key and earn the 429s the
	// limiter exists to prevent.
	submitRL := mainRL
	if !onlyUsesKey(dc.DownloadAPIKeys, dc.APIKey) {
		submitRL = ratelimits["download"]
		if submitRL == nil {
			submitRL = ratelimit.New(requestsPerMinute, ratelimit.Per(time.Minute), ratelimit.WithSlack(burstRequests))
		}
	}

	newClient := func(rateLimiter ratelimit.Limiter) *request.Client {
		opts := []request.ClientOption{
			request.WithHeaders(headers),
			request.WithRateLimiter(rateLimiter),
			request.WithMaxRetries(options.Retries),
			request.WithRetryableStatus(http.StatusTooManyRequests, http.StatusBadGateway),
		}
		if dc.Proxy != "" {
			opts = append(opts, request.WithProxy(dc.Proxy))
		}
		return request.New(_log, options.TLSConfig, opts...)
	}

	autoExpiresLinksAfter, err := utils.ParseDuration(dc.AutoExpireLinksAfter)
	if autoExpiresLinksAfter == 0 || err != nil {
		autoExpiresLinksAfter = defaultLinkExpiry
	}

	tb := &Torbox{
		Host:                  "https://api.torbox.app/v1",
		APIKey:                dc.APIKey,
		accountsManager:       account.NewManager(dc, options, submitRL),
		options:               options,
		config:                dc,
		autoExpiresLinksAfter: autoExpiresLinksAfter,
		client:                newClient(mainRL),
		submitClient:          newClient(submitRL),
		logger:                _log,
	}
	return tb, nil
}

// onlyUsesKey reports whether the download keys are just the main key, in which
// case download traffic spends the same per-key budget as everything else.
func onlyUsesKey(downloadKeys []string, apiKey string) bool {
	for _, key := range downloadKeys {
		if key != "" && key != apiKey {
			return false
		}
	}
	return true
}

func (tb *Torbox) Config() config.Debrid {
	return tb.config
}

func (tb *Torbox) Logger() zerolog.Logger {
	return tb.logger
}

func (tb *Torbox) submissionClient() *request.Client {
	if tb.submitClient != nil {
		return tb.submitClient
	}
	return tb.client
}

// doGet performs a GET request and unmarshals the response.
func (tb *Torbox) doGet(endpoint string, queryParams map[string]string, result any) (int, error) {
	return tb.doGetWithClient(context.Background(), tb.client, endpoint, queryParams, result)
}

func (tb *Torbox) doGetWithClient(
	ctx context.Context,
	client *request.Client,
	endpoint string,
	queryParams map[string]string,
	result any,
) (int, error) {
	u, err := url.Parse(tb.Host + endpoint)
	if err != nil {
		return 0, err
	}

	if queryParams != nil {
		q := u.Query()
		for k, v := range queryParams {
			q.Set(k, v)
		}
		u.RawQuery = q.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return 0, err
	}

	return common.DoJSON(client, req, result)
}

func (tb *Torbox) doPostFormWithClient(
	ctx context.Context,
	client *request.Client,
	endpoint string,
	formData map[string]string,
	result any,
) (int, error) {
	form := url.Values{}
	for k, v := range formData {
		form.Set(k, v)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tb.Host+endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	return common.DoJSON(client, req, result)
}

// doPostJSON performs a POST request with a JSON body.
func (tb *Torbox) doPostJSON(ctx context.Context, endpoint string, payload any, result any) (int, error) {
	var body io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return 0, err
		}
		body = bytes.NewReader(data)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tb.Host+endpoint, body)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")

	return common.DoJSON(tb.client, req, result)
}

func (tb *Torbox) IsAvailable(hashes []string) (map[string]bool, error) {
	result := make(map[string]bool)

	for _, validHashes := range common.HashBatches(hashes, batchSize) {
		hashStr := strings.Join(validHashes, ",")
		var res AvailableResponse

		status, err := tb.doGet("/api/torrents/checkcached", map[string]string{"hash": hashStr}, &res)
		if err != nil {
			return result, fmt.Errorf("check availability: %w", err)
		}
		if !common.IsSuccess(status) {
			return result, fmt.Errorf("check availability: HTTP %d", status)
		}
		if !res.Success {
			return result, fmt.Errorf("check availability: %v", res.Error)
		}
		cached := make(map[string]bool)
		if res.Data != nil {
			for h, item := range *res.Data {
				cached[strings.ToLower(h)] = item.Size > 0
			}
		}
		for _, h := range validHashes {
			result[h] = cached[strings.ToLower(h)]
		}
	}
	return result, nil
}

func (tb *Torbox) SubmitMagnet(torrent *types.Torrent) (*types.Torrent, error) {
	var data AddMagnetResponse

	formData := map[string]string{
		"magnet": torrent.Magnet.Link,
	}
	if !torrent.DownloadUncached {
		formData["add_only_if_cached"] = queryTrue
	}

	status, err := tb.doPostFormWithClient(
		context.Background(),
		tb.submissionClient(),
		"/api/torrents/createtorrent",
		formData,
		&data,
	)
	if err != nil {
		return nil, err
	}

	if !common.IsSuccess(status) {
		return nil, fmt.Errorf("torbox API error: Status: %d", status)
	}
	if data.Data == nil {
		return nil, fmt.Errorf("error adding torrent")
	}
	dt := *data.Data
	torrentID := strconv.Itoa(dt.ID)
	torrent.ID = torrentID
	torrent.Debrid = tb.config.Name
	torrent.Added = time.Now()

	return torrent, nil
}

func (tb *Torbox) getTorboxStatus(status string, finished bool) types.TorrentStatus {
	if finished {
		return types.TorrentStatusDownloaded
	}
	status = torboxStatusDetail.ReplaceAllString(status, "")

	switch status {
	case "paused", "downloading", "checkingResumeData", "metaDL", "pausedUP",
		"queuedUP", "checkingUP", "forcedUP", "allocating", "pausedDL",
		"queuedDL", "checkingDL", "forcedDL", "moving", "incomplete":
		return types.TorrentStatusDownloading
	case "completed", "cached", "uploading", "downloaded":
		return types.TorrentStatusDownloaded
	default:
		return types.TorrentStatusError
	}
}

func (tb *Torbox) GetTorrent(torrentID string) (*types.Torrent, error) {
	var res InfoResponse

	status, err := tb.doGet("/api/torrents/mylist", map[string]string{"id": torrentID}, &res)
	if err != nil {
		return nil, err
	}

	if status == http.StatusNotFound {
		return nil, customerror.ErrTorrentNotFound
	}
	if !common.IsSuccess(status) {
		return nil, fmt.Errorf("torbox API error: Status: %d", status)
	}
	if !res.Success {
		reason := "unsuccessful response"
		switch {
		case res.Detail != "":
			reason = res.Detail
		case res.Error != nil:
			reason = fmt.Sprint(res.Error)
		}
		return nil, fmt.Errorf("torbox API error: %s", reason)
	}
	data := res.Data
	if data == nil {
		return nil, customerror.ErrTorrentNotFound
	}
	t := &types.Torrent{
		ID:               strconv.Itoa(data.ID),
		InfoHash:         data.Hash,
		Name:             data.Name,
		Bytes:            data.Size,
		Progress:         data.Progress * percent,
		Status:           tb.getTorboxStatus(data.DownloadState, data.DownloadFinished),
		Speed:            data.DownloadSpeed,
		Seeders:          data.Seeds,
		Filename:         data.Name,
		OriginalFilename: data.Name,
		Debrid:           tb.config.Name,
		Files:            make(map[string]types.File),
		Added:            data.CreatedAt,
	}
	for _, f := range data.Files {
		fileName := filepath.Base(f.Name)
		if validateFileAllowedErr := tb.options.FileAllowed(f.AbsolutePath, f.Size); validateFileAllowedErr != nil {
			continue
		}

		file := types.File{
			TorrentID: t.ID,
			ID:        strconv.Itoa(f.ID),
			Name:      fileName,
			Size:      f.Size,
			Path:      f.Name,
		}

		if data.DownloadFinished {
			file.Link = fmt.Sprintf("torbox://%s/%d", t.ID, f.ID)
		}

		t.Files[fileName] = file
	}
	var cleanPath string
	if len(t.Files) > 0 {
		cleanPath = path.Clean(data.Files[0].Name)
	} else {
		cleanPath = path.Clean(data.Name)
	}

	t.OriginalFilename = strings.Split(cleanPath, "/")[0]
	t.Debrid = tb.config.Name

	return t, nil
}

func (tb *Torbox) loadDownloadPresent(ctx context.Context) (map[string]bool, error) {
	present := make(map[string]bool)
	offset := 0
	for {
		var res TorrentsListResponse
		status, err := tb.doGetWithClient(
			ctx,
			tb.client,
			"/api/torrents/mylist",
			map[string]string{"offset": strconv.Itoa(offset)},
			&res,
		)
		if err != nil {
			return nil, err
		}
		if !common.IsSuccess(status) {
			return nil, fmt.Errorf("torbox API error: Status: %d", status)
		}
		if res.Data == nil || len(*res.Data) == 0 {
			break
		}
		for _, t := range *res.Data {
			present[strconv.Itoa(t.ID)] = t.DownloadPresent
		}
		offset += len(*res.Data)
	}
	tb.logger.Info().Int("count", len(present)).Msg("loaded download_present cache for repair")
	return present, nil
}

func (tb *Torbox) UpdateTorrent(t *types.Torrent) error {
	return tb.updateTorrentWithClient(tb.client, t)
}

func (tb *Torbox) updateTorrentWithClient(client *request.Client, t *types.Torrent) error {
	var res InfoResponse

	status, err := tb.doGetWithClient(
		context.Background(),
		client,
		"/api/torrents/mylist",
		map[string]string{"id": t.ID},
		&res,
	)
	if err != nil {
		return err
	}

	if !common.IsSuccess(status) {
		return fmt.Errorf("torbox API error: Status: %d", status)
	}
	data := res.Data
	if data == nil {
		return fmt.Errorf("torbox API error: no data for torrent %s: %v %s", t.ID, res.Error, res.Detail)
	}
	name := data.Name

	t.Name = name
	t.Bytes = data.Size
	t.Progress = data.Progress * percent
	t.Status = tb.getTorboxStatus(data.DownloadState, data.DownloadFinished)
	t.Speed = data.DownloadSpeed
	t.Seeders = data.Seeds
	t.Filename = name
	t.OriginalFilename = name
	if data.Hash != "" {
		t.InfoHash = data.Hash
	}
	t.Debrid = tb.config.Name

	t.Files = make(map[string]types.File)

	for _, f := range data.Files {
		fileName := filepath.Base(f.Name)

		if validateFileAllowedErr := tb.options.FileAllowed(f.AbsolutePath, f.Size); validateFileAllowedErr != nil {
			continue
		}

		file := types.File{
			TorrentID: t.ID,
			ID:        strconv.Itoa(f.ID),
			Name:      fileName,
			Size:      f.Size,
			Path:      fileName,
		}

		if data.DownloadFinished {
			file.Link = fmt.Sprintf("torbox://%s/%s", t.ID, strconv.Itoa(f.ID))
		}

		t.Files[fileName] = file
	}

	var cleanPath string
	if len(t.Files) > 0 {
		cleanPath = path.Clean(data.Files[0].Name)
	} else {
		cleanPath = path.Clean(data.Name)
	}

	t.OriginalFilename = strings.Split(cleanPath, "/")[0]
	t.Debrid = tb.config.Name
	return nil
}

func (tb *Torbox) CheckStatus(torrent *types.Torrent) (*types.Torrent, error) {
	if err := tb.updateTorrentWithClient(tb.submissionClient(), torrent); err != nil || torrent == nil {
		return torrent, err
	}

	switch torrent.Status {
	case types.TorrentStatusDownloaded:
		tb.logger.Info().Msgf("Torrent: %s downloaded", torrent.Name)
		return torrent, nil
	case types.TorrentStatusDownloading:
		if !torrent.DownloadUncached {
			return torrent, fmt.Errorf("torrent %s: %w", torrent.Name, customerror.ErrTorrentNotCached)
		}
		return torrent, nil
	case types.TorrentStatusQueued, types.TorrentStatusError:
	}
	return torrent, fmt.Errorf("torrent: %s has error", torrent.Name)
}

func (tb *Torbox) DeleteTorrent(torrentID string) error {
	id, err := strconv.Atoi(torrentID)
	if err != nil {
		return fmt.Errorf("invalid TorBox torrent id %q: %w", torrentID, err)
	}
	payload := struct {
		TorrentID int    `json:"torrent_id"`
		Operation string `json:"operation"`
		All       bool   `json:"all"`
	}{
		TorrentID: id,
		Operation: "delete",
	}

	status, err := tb.doPostJSON(context.Background(), "/api/torrents/controltorrent", payload, nil)
	if err != nil {
		return err
	}

	if !common.IsSuccess(status) {
		return fmt.Errorf("torbox API error: Status: %d", status)
	}

	tb.logger.Info().Msgf("Torrent %s deleted from Torbox", torrentID)
	return nil
}

func (tb *Torbox) GetDownloadLink(ctx context.Context, id string, file *types.File) (types.DownloadLink, error) {
	return tb.accountsManager.GetDownloadLink(ctx, id, file, tb.fetchDownloadLink)
}

func (tb *Torbox) fetchDownloadLink(
	ctx context.Context,
	account *account.Account,
	id string,
	file *types.File,
) (types.DownloadLink, error) {
	var res DownloadLinksResponse
	status, err := tb.doGetWithClient(ctx, account.Client(), "/api/torrents/requestdl", map[string]string{
		"token":      account.Token,
		"torrent_id": id,
		"file_id":    file.ID,
	}, &res)
	if err != nil {
		return types.DownloadLink{}, err
	}
	if !common.IsSuccess(status) {
		return types.DownloadLink{}, fmt.Errorf("torbox API error: Status: %d", status)
	}
	if res.Data == nil || *res.Data == "" {
		return types.DownloadLink{}, fmt.Errorf("torbox API error: no download link: %v %s", res.Error, res.Detail)
	}

	now := time.Now()

	// Always expires
	dl := types.DownloadLink{
		Filename:     file.Name,
		Size:         file.Size,
		Token:        account.Token,
		Link:         file.Link,
		DownloadLink: *res.Data,
		Debrid:       tb.config.Name,
		ID:           file.ID,
		Generated:    now,
		ExpiresAt:    now.Add(tb.autoExpiresLinksAfter),
	}
	return dl, nil
}

func (tb *Torbox) GetTorrents() ([]*types.Torrent, error) {
	offset := 0
	allTorrents := make([]*types.Torrent, 0)

	for {
		torrents, err := tb.getTorrents(offset)
		if err != nil {
			return nil, fmt.Errorf("get TorBox torrents at offset %d: %w", offset, err)
		}
		if len(torrents) == 0 {
			break
		}
		allTorrents = append(allTorrents, torrents...)
		offset += len(torrents)
	}
	return allTorrents, nil
}

func (tb *Torbox) getTorrents(offset int) ([]*types.Torrent, error) {
	var res TorrentsListResponse

	status, err := tb.doGet("/api/torrents/mylist", map[string]string{
		"bypass_cache": queryTrue,
		"offset":       strconv.Itoa(offset),
	}, &res)
	if err != nil {
		return nil, err
	}

	if !common.IsSuccess(status) {
		return nil, fmt.Errorf("torbox API error: Status: %d", status)
	}

	if !res.Success || res.Data == nil {
		return nil, fmt.Errorf("torbox API error: %v", res.Error)
	}

	torrents := make([]*types.Torrent, 0, len(*res.Data))
	for _, data := range *res.Data {
		t := &types.Torrent{
			ID:               strconv.Itoa(data.ID),
			Name:             data.Name,
			Bytes:            data.Size,
			Progress:         data.Progress * percent,
			Status:           tb.getTorboxStatus(data.DownloadState, data.DownloadFinished),
			Speed:            data.DownloadSpeed,
			Seeders:          data.Seeds,
			Filename:         data.Name,
			OriginalFilename: data.Name,
			Debrid:           tb.config.Name,
			Files:            make(map[string]types.File),
			Added:            data.CreatedAt,
			InfoHash:         data.Hash,
		}

		for _, f := range data.Files {
			fileName := filepath.Base(f.Name)
			if validateFileAllowedErr := tb.options.FileAllowed(
				f.AbsolutePath,
				f.Size,
			); validateFileAllowedErr != nil {
				continue
			}
			file := types.File{
				TorrentID: t.ID,
				ID:        strconv.Itoa(f.ID),
				Name:      fileName,
				Size:      f.Size,
				Path:      f.Name,
			}

			if data.DownloadFinished {
				file.Link = fmt.Sprintf("torbox://%s/%d", t.ID, f.ID)
			}

			t.Files[fileName] = file
		}

		var cleanPath string
		if len(t.Files) > 0 {
			cleanPath = path.Clean(data.Files[0].Name)
		} else {
			cleanPath = path.Clean(data.Name)
		}
		t.OriginalFilename = strings.Split(cleanPath, "/")[0]

		torrents = append(torrents, t)
	}

	return torrents, nil
}

func (tb *Torbox) fetchDownloadLinks(_ *account.Account) ([]types.DownloadLink, error) {
	return []types.DownloadLink{}, nil
}

func (tb *Torbox) RefreshDownloadLinks() error {
	return tb.accountsManager.RefreshLinks(tb.fetchDownloadLinks)
}

func (tb *Torbox) CheckFile(ctx context.Context, _, link string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	torrentID := link
	if after, ok := strings.CutPrefix(link, "torbox://"); ok {
		torrentID, _, _ = strings.Cut(after, "/")
	}

	tb.downloadPresentMu.Lock()
	if err := ctx.Err(); err != nil {
		tb.downloadPresentMu.Unlock()
		return err
	}
	if tb.downloadPresent == nil || time.Since(tb.downloadPresentAt) > downloadPresentTTL {
		loaded, err := tb.loadDownloadPresent(ctx)
		if err != nil {
			tb.downloadPresentMu.Unlock()
			return err
		}
		tb.downloadPresent, tb.downloadPresentAt = loaded, time.Now()
	}
	present := tb.downloadPresent[torrentID]
	tb.downloadPresentMu.Unlock()

	if err := ctx.Err(); err != nil {
		return err
	}
	if !present {
		return customerror.ErrHosterUnavailable
	}
	return nil
}

// planSlots returns the concurrent download slots of a TorBox plan.
func planSlots(plan string) int {
	switch plan {
	case planEssential:
		return essentialSlots
	case planStandard:
		return standardSlots
	case planPro:
		return proSlots
	default:
		return 1
	}
}

func (tb *Torbox) GetAvailableSlots() (int, error) {
	profile, err := tb.GetProfile()
	if err != nil {
		return 0, err
	}
	return planSlots(profile.Type), nil
}

// GetProfile returns the account profile, fetched once and then cached.
func (tb *Torbox) GetProfile() (*types.Profile, error) {
	return tb.profile.Get(0, tb.fetchProfile)
}

func (tb *Torbox) fetchProfile() (*types.Profile, error) {
	var data ProfileResponse

	status, err := tb.doGet("/api/user/me", map[string]string{"settings": queryTrue}, &data)
	if err != nil {
		return nil, err
	}

	if !common.IsSuccess(status) {
		return nil, fmt.Errorf("torbox API error: Status: %d", status)
	}

	userData := data.Data
	if userData == nil {
		return nil, fmt.Errorf("error getting user profile")
	}

	expiration, err := time.Parse(time.RFC3339, userData.PremiumExpiresAt)
	if err != nil {
		expiration = time.Time{}
	}

	profile := &types.Profile{
		Name:       tb.config.Name,
		ID:         userData.ID,
		Username:   userData.Email,
		Email:      userData.Email,
		Expiration: expiration,
	}

	switch userData.Plan {
	case planIDEssential:
		profile.Type = planEssential
	case planIDPro:
		profile.Type = planPro
	case planIDStandard:
		profile.Type = planStandard
	default:
		profile.Type = "free"
	}

	return profile, nil
}

func (tb *Torbox) AccountManager() *account.Manager {
	return tb.accountsManager
}

func (tb *Torbox) syncAccount(_ *account.Account) error {
	return nil
}

func (tb *Torbox) SyncAccounts() {
	tb.accountsManager.Sync(tb.syncAccount)
}

func (tb *Torbox) deleteDownloadLink(*account.Account, types.DownloadLink) error {
	return nil
}

func (tb *Torbox) DeleteLink(downloadLink types.DownloadLink) error {
	return tb.accountsManager.DeleteDownloadLink(downloadLink, tb.deleteDownloadLink)
}

// SpeedTest measures API latency and download speed using cached links.
func (tb *Torbox) SpeedTest(ctx context.Context) types.SpeedTestResult {
	result := types.SpeedTestResult{
		Provider: tb.config.Name,
		TestedAt: time.Now(),
	}

	start := time.Now()
	status, err := tb.doGet("/api/user/me", nil, nil)
	latency := time.Since(start)

	if err != nil {
		result.Error = fmt.Sprintf("latency test failed: %v", err)
		return result
	}

	if !common.IsSuccess(status) {
		result.Error = fmt.Sprintf("latency test unexpected status: %d", status)
		return result
	}
	result.LatencyMs = latency.Milliseconds()

	tb.accountsManager.MeasureDownload(ctx, &result)
	return result
}

func (tb *Torbox) SupportsCheck() bool {
	return true
}

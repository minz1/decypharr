package debridlink

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
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
)

type DebridLink struct {
	Host             string `json:"host"`
	APIKey           string
	accountsManager  *account.Manager
	DownloadUncached bool
	client           *request.Client
	repairClient     *request.Client

	autoExpiresLinksAfter time.Duration
	logger                zerolog.Logger
	options               types.ProviderOptions
	config                config.Debrid

	profile types.ProfileCache
}

const (
	// repairRetries bounds retries of repair-time link checks.
	repairRetries = 4
	// defaultLinkExpiry applies when auto_expire_links_after is unset.
	defaultLinkExpiry = 48 * time.Hour
	// pageSize is the page size for list endpoints and hashes per cache check.
	pageSize = 100
	// seedboxDone is the seedbox status of a finished torrent.
	seedboxDone = 100
)

func New(
	dc config.Debrid,
	ratelimits map[string]ratelimit.Limiter,
	options types.ProviderOptions,
) (*DebridLink, error) {
	headers := map[string]string{
		"Authorization": fmt.Sprintf("Bearer %s", dc.APIKey),
		"Content-Type":  "application/json",
	}
	if dc.UserAgent != "" {
		headers["User-Agent"] = dc.UserAgent
	}
	log := options.Logger

	opts := []request.ClientOption{
		request.WithHeaders(headers),
		request.WithRateLimiter(ratelimits["main"]),
		request.WithMaxRetries(options.Retries),
		request.WithRetryableStatus(http.StatusTooManyRequests, http.StatusBadGateway),
	}
	if dc.Proxy != "" {
		opts = append(opts, request.WithProxy(dc.Proxy))
	}
	repairOpts := []request.ClientOption{
		request.WithHeaders(headers),
		request.WithRateLimiter(ratelimits["repair"]),
		request.WithMaxRetries(repairRetries),
		request.WithRetryableStatus(http.StatusTooManyRequests),
	}
	if dc.Proxy != "" {
		repairOpts = append(repairOpts, request.WithProxy(dc.Proxy))
	}

	autoExpiresLinksAfter, err := utils.ParseDuration(dc.AutoExpireLinksAfter)
	if autoExpiresLinksAfter == 0 || err != nil {
		autoExpiresLinksAfter = defaultLinkExpiry
	}
	dbl := &DebridLink{
		Host:                  "https://debrid-link.com/api/v2",
		APIKey:                dc.APIKey,
		accountsManager:       account.NewManager(dc, options, ratelimits["download"]),
		options:               options,
		DownloadUncached:      dc.DownloadUncached,
		autoExpiresLinksAfter: autoExpiresLinksAfter,
		client:                request.New(log, options.TLSConfig, opts...),
		repairClient:          request.New(log, options.TLSConfig, repairOpts...),
		logger:                log,
		config:                dc,
	}
	return dbl, nil
}

func (dl *DebridLink) Config() config.Debrid {
	return dl.config
}

func (dl *DebridLink) Logger() zerolog.Logger {
	return dl.logger
}

// doGet performs a GET request and unmarshals the response.
func (dl *DebridLink) doGet(endpoint string, queryParams map[string]string, result any) (int, error) {
	u, err := url.Parse(dl.Host + endpoint)
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

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, u.String(), nil)
	if err != nil {
		return 0, err
	}
	return common.DoJSON(dl.client, req, result)
}

func (dl *DebridLink) IsAvailable(hashes []string) (map[string]bool, error) {
	result := make(map[string]bool)

	for _, validHashes := range common.HashBatches(hashes, pageSize) {
		hashStr := strings.Join(validHashes, ",")
		endpoint := fmt.Sprintf("/seedbox/cached/%s", hashStr)
		var data AvailableResponse

		httpStatus, err := dl.doGet(endpoint, nil, &data)
		if err != nil {
			return result, fmt.Errorf("check availability: %w", err)
		}
		if !common.IsSuccess(httpStatus) {
			return result, fmt.Errorf("check availability: HTTP %d", httpStatus)
		}
		if !data.Success {
			return result, fmt.Errorf("check availability: provider rejected the request")
		}
		for _, h := range validHashes {
			result[h] = false
			if data.Value != nil {
				_, result[h] = (*data.Value)[h]
			}
		}
	}
	return result, nil
}

func (dl *DebridLink) GetTorrent(torrentID string) (*types.Torrent, error) {
	endpoint := fmt.Sprintf("/seedbox/%s", torrentID)
	var res torrentInfo

	httpStatus, err := dl.doGet(endpoint, nil, &res)
	if err != nil {
		return nil, err
	}

	if httpStatus == http.StatusNotFound {
		return nil, customerror.ErrTorrentNotFound
	}
	if !common.IsSuccess(httpStatus) {
		return nil, fmt.Errorf("debridlink API error: Status: %d", httpStatus)
	}
	if !res.Success {
		return nil, fmt.Errorf("error getting torrent")
	}
	if res.Value == nil || len(*res.Value) == 0 {
		return nil, customerror.ErrTorrentNotFound
	}
	data := *res.Value
	t := data[0]
	name := utils.RemoveInvalidChars(t.Name)
	torrent := &types.Torrent{
		ID:               t.ID,
		Name:             name,
		Bytes:            t.TotalSize,
		Status:           types.TorrentStatusDownloaded,
		Filename:         name,
		OriginalFilename: name,
		Debrid:           dl.config.Name,
		Added:            time.Unix(t.Created, 0),
		Files:            make(map[string]types.File, len(t.Files)),
		InfoHash:         t.HashString,
	}
	for _, f := range t.Files {
		if validateFileAllowedErr := dl.options.FileAllowed(f.Name, f.Size); validateFileAllowedErr != nil {
			continue
		}
		file := types.File{
			TorrentID: t.ID,
			ID:        f.ID,
			Name:      f.Name,
			Size:      f.Size,
			Path:      f.Name,
			Link:      f.DownloadURL,
		}
		torrent.Files[file.Name] = file
	}

	return torrent, nil
}

func (dl *DebridLink) UpdateTorrent(t *types.Torrent) error {
	var res torrentInfo

	httpStatus, err := dl.doGet("/seedbox/list", map[string]string{"ids": t.ID}, &res)
	if err != nil {
		return err
	}

	if !common.IsSuccess(httpStatus) {
		return fmt.Errorf("debridlink API error: Status: %d", httpStatus)
	}
	if !res.Success {
		return fmt.Errorf("error getting torrent")
	}
	if res.Value == nil {
		return fmt.Errorf("torrent not found")
	}
	dt := *res.Value

	if len(dt) == 0 {
		return fmt.Errorf("torrent not found")
	}
	data := dt[0]
	status := types.TorrentStatusDownloading
	if data.Status == seedboxDone {
		status = types.TorrentStatusDownloaded
	}
	name := utils.RemoveInvalidChars(data.Name)
	t.ID = data.ID
	t.Name = name
	t.Bytes = data.TotalSize
	t.Progress = data.DownloadPercent
	t.Status = status
	t.Speed = data.DownloadSpeed
	t.Seeders = data.PeersConnected
	t.Filename = name
	t.OriginalFilename = name
	if data.HashString != "" {
		t.InfoHash = data.HashString
	}
	t.Added = time.Unix(data.Created, 0)
	now := time.Now()
	for _, f := range data.Files {
		if validateFileAllowedErr := dl.options.FileAllowed(f.Name, f.Size); validateFileAllowedErr != nil {
			continue
		}
		file := types.File{
			TorrentID: t.ID,
			ID:        f.ID,
			Name:      f.Name,
			Size:      f.Size,
			Path:      f.Name,
			Link:      f.DownloadURL,
		}
		link := types.DownloadLink{
			Debrid:       dl.config.Name,
			Token:        dl.APIKey,
			Filename:     f.Name,
			Link:         f.DownloadURL,
			DownloadLink: f.DownloadURL,
			Generated:    now,
			ExpiresAt:    now.Add(dl.autoExpiresLinksAfter),
		}
		file.DownloadLink = link
		t.Files[f.Name] = file
		dl.accountsManager.StoreDownloadLink(link)
	}

	return nil
}

func (dl *DebridLink) SubmitMagnet(t *types.Torrent) (*types.Torrent, error) {
	payload := map[string]string{"url": t.Magnet.Link}
	var res SubmitTorrentInfo

	dt, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	body := bytes.NewReader(dt)

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, dl.Host+"/seedbox/add", body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := dl.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if !common.IsSuccess(resp.StatusCode) {
		bd, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("error adding torrent(status %d): %s", resp.StatusCode, string(bd))
	}
	if resp.ContentLength == 0 {
		return nil, fmt.Errorf("empty response from debridlink API")
	}
	if decodeJSONErr := request.DecodeJSON(resp, &res); decodeJSONErr != nil {
		return nil, decodeJSONErr
	}
	if !res.Success || res.Value == nil {
		return nil, fmt.Errorf("error adding torrent")
	}
	data := *res.Value
	name := utils.RemoveInvalidChars(data.Name)
	t.ID = data.ID
	t.Name = name
	t.Bytes = data.TotalSize
	t.Progress = data.DownloadPercent
	t.Status = types.TorrentStatusDownloading
	t.Speed = data.DownloadSpeed
	t.Seeders = data.PeersConnected
	t.Filename = name
	t.OriginalFilename = name
	t.Debrid = dl.config.Name
	t.Added = time.Unix(data.Created, 0)
	now := time.Now()
	for _, f := range data.Files {
		file := types.File{
			TorrentID: t.ID,
			ID:        f.ID,
			Name:      f.Name,
			Size:      f.Size,
			Path:      f.Name,
			Link:      f.DownloadURL,
			Generated: now,
		}
		link := types.DownloadLink{
			Debrid:       dl.config.Name,
			Token:        dl.APIKey,
			Filename:     f.Name,
			Link:         f.DownloadURL,
			DownloadLink: f.DownloadURL,
			Generated:    now,
			ExpiresAt:    now.Add(dl.autoExpiresLinksAfter),
		}
		file.DownloadLink = link
		t.Files[f.Name] = file
		dl.accountsManager.StoreDownloadLink(link)
	}

	return t, nil
}

func (dl *DebridLink) CheckStatus(torrent *types.Torrent) (*types.Torrent, error) {
	if err := dl.UpdateTorrent(torrent); err != nil {
		return torrent, err
	}
	switch torrent.Status {
	case types.TorrentStatusDownloading:
		if !torrent.DownloadUncached {
			return torrent, fmt.Errorf("torrent %s: %w", torrent.Name, customerror.ErrTorrentNotCached)
		}
		return torrent, nil
	case types.TorrentStatusDownloaded:
		dl.logger.Info().Msgf("Torrent: %s downloaded", torrent.Name)
		return torrent, nil
	case types.TorrentStatusQueued, types.TorrentStatusError:
	}
	return torrent, fmt.Errorf("torrent: %s has error", torrent.Name)
}

func (dl *DebridLink) DeleteTorrent(torrentID string) error {
	endpoint := fmt.Sprintf("/seedbox/%s/remove", torrentID)

	req, err := http.NewRequestWithContext(context.Background(), http.MethodDelete, dl.Host+endpoint, nil)
	if err != nil {
		return err
	}

	resp, err := dl.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if !common.IsSuccess(resp.StatusCode) {
		return fmt.Errorf("debridlink API error: Status: %d", resp.StatusCode)
	}

	dl.logger.Info().Msgf("Torrent: %s deleted from DebridLink", torrentID)
	return nil
}

func (dl *DebridLink) fetchDownloadLink(
	_ context.Context,
	account *account.Account,
	_ string,
	file *types.File,
) (types.DownloadLink, error) {
	now := time.Now()
	link := types.DownloadLink{
		Debrid:       dl.config.Name,
		Token:        account.Token,
		Filename:     file.Name,
		Link:         file.Link,
		DownloadLink: file.Link,
		Generated:    now,
		ExpiresAt:    now.Add(dl.autoExpiresLinksAfter),
	}
	return link, nil
}

func (dl *DebridLink) GetDownloadLink(ctx context.Context, id string, file *types.File) (types.DownloadLink, error) {
	return dl.accountsManager.GetDownloadLink(ctx, id, file, dl.fetchDownloadLink)
}

func (dl *DebridLink) GetDownloadUncached() bool {
	return dl.DownloadUncached
}

func (dl *DebridLink) GetTorrents() ([]*types.Torrent, error) {
	page := 0
	perPage := pageSize
	torrents := make([]*types.Torrent, 0)
	var fetchErr error
	for {
		// Pagination follows the provider's page size; unfinished torrents
		// are filtered out of t, so an all-unfinished page must not stop it.
		t, seen, err := dl.getTorrents(page, perPage)
		if err != nil {
			fetchErr = err
			break
		}
		if seen == 0 {
			break
		}
		torrents = append(torrents, t...)
		page++
	}
	if fetchErr != nil {
		return torrents, fetchErr
	}
	return torrents, nil
}

func (dl *DebridLink) fetchDownloadLinks(account *account.Account) ([]types.DownloadLink, error) {
	links := make([]types.DownloadLink, 0)
	limit := pageSize
	page := 0
	for {
		data, seen, err := dl._fetchDownloadLinks(account, page, limit)
		if err != nil {
			return links, err
		}
		links = append(links, data...)
		// Expired links are filtered out of data; paginate on the raw count.
		if seen < limit {
			break
		}
		page++
	}
	return links, nil
}

// _fetchDownloadLinks returns the unexpired links on one page and the number
// of entries the page contained before filtering.
func (dl *DebridLink) _fetchDownloadLinks(
	account *account.Account,
	page, limit int,
) ([]types.DownloadLink, int, error) {
	links := make([]types.DownloadLink, 0)

	req, err := http.NewRequestWithContext(
		context.Background(),
		http.MethodGet,
		fmt.Sprintf("%s/downloader/list?page=%d&perPage=%d", dl.Host, page, limit),
		nil,
	)
	if err != nil {
		return links, 0, err
	}

	resp, err := account.Client().Do(req)
	if err != nil {
		return links, 0, err
	}
	defer resp.Body.Close()

	if !common.IsSuccess(resp.StatusCode) {
		return links, 0, fmt.Errorf("debridlink API error: Status: %d", resp.StatusCode)
	}
	var res DownloadLinksResponse

	if resp.ContentLength == 0 {
		return links, 0, fmt.Errorf("empty response from debridlink API")
	}
	if decodeJSONErr := request.DecodeJSON(resp, &res); decodeJSONErr != nil {
		return links, 0, decodeJSONErr
	}
	if !res.Success || res.Value == nil {
		return links, 0, fmt.Errorf("error getting download links")
	}
	data := *res.Value
	for _, l := range data {
		created := time.Unix(l.Created, 0)
		if created.IsZero() {
			continue
		}
		// Then check if created has expired
		if time.Since(created) > dl.autoExpiresLinksAfter {
			continue
		}
		link := types.DownloadLink{
			Debrid:       dl.config.Name,
			ID:           l.ID,
			Token:        account.Token,
			Filename:     l.Name,
			Link:         l.URL,
			DownloadLink: l.DownloadURL,
			Generated:    created,
			ExpiresAt:    created.Add(dl.autoExpiresLinksAfter),
		}
		links = append(links, link)
	}
	return links, len(data), nil
}

func (dl *DebridLink) RefreshDownloadLinks() error {
	return dl.accountsManager.RefreshLinks(dl.fetchDownloadLinks)
}

// getTorrents returns the finished torrents on one page and the number of
// entries the page contained before filtering.
func (dl *DebridLink) getTorrents(page, perPage int) ([]*types.Torrent, int, error) {
	torrents := make([]*types.Torrent, 0)
	var res torrentInfo

	params := map[string]string{
		"page":    strconv.Itoa(page),
		"perPage": strconv.Itoa(perPage),
	}

	httpStatus, err := dl.doGet("/seedbox/list", params, &res)
	if err != nil {
		return torrents, 0, err
	}

	if !common.IsSuccess(httpStatus) {
		return torrents, 0, fmt.Errorf("debridlink API error: Status: %d", httpStatus)
	}

	if !res.Success || res.Value == nil {
		return nil, 0, fmt.Errorf("error getting torrents")
	}

	data := *res.Value

	for _, t := range data {
		if t.Status != seedboxDone {
			continue
		}
		torrent := &types.Torrent{
			ID:               t.ID,
			Name:             t.Name,
			Bytes:            t.TotalSize,
			Status:           types.TorrentStatusDownloaded,
			Filename:         t.Name,
			OriginalFilename: t.Name,
			InfoHash:         t.HashString,
			Files:            make(map[string]types.File, len(t.Files)),
			Debrid:           dl.config.Name,
			Added:            time.Unix(t.Created, 0),
		}
		now := time.Now()
		for _, f := range t.Files {
			if validateFileAllowedErr := dl.options.FileAllowed(f.Name, f.Size); validateFileAllowedErr != nil {
				continue
			}
			file := types.File{
				TorrentID: torrent.ID,
				ID:        f.ID,
				Name:      f.Name,
				Size:      f.Size,
				Path:      f.Name,
				Link:      f.DownloadURL,
			}
			link := types.DownloadLink{
				Debrid:       dl.config.Name,
				Token:        dl.APIKey,
				Filename:     f.Name,
				Link:         f.DownloadURL,
				DownloadLink: f.DownloadURL,
				Generated:    now,
				ExpiresAt:    now.Add(dl.autoExpiresLinksAfter),
			}
			file.DownloadLink = link
			torrent.Files[f.Name] = file
			dl.accountsManager.StoreDownloadLink(link)
		}
		torrents = append(torrents, torrent)
	}

	return torrents, len(data), nil
}

func (dl *DebridLink) CheckFile(ctx context.Context, _, link string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, link, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Range", "bytes=0-0")

	resp, err := dl.repairClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone {
		return customerror.ErrHosterUnavailable
	}
	if !common.IsSuccess(resp.StatusCode) {
		return fmt.Errorf("debridlink file check error: Status: %d", resp.StatusCode)
	}
	return nil
}

func (dl *DebridLink) GetAvailableSlots() (int, error) {
	// AllDebrid does not provide available slots info
	return config.DefaultAvailableSlots, nil
}

// GetProfile returns the account profile, fetched once and then cached.
func (dl *DebridLink) GetProfile() (*types.Profile, error) {
	return dl.profile.Get(0, dl.fetchProfile)
}

func (dl *DebridLink) fetchProfile() (*types.Profile, error) {
	var res UserInfo

	httpStatus, err := dl.doGet("/account/infos", nil, &res)
	if err != nil {
		return nil, err
	}

	if !common.IsSuccess(httpStatus) {
		return nil, fmt.Errorf("debridlink API error: Status: %d", httpStatus)
	}
	if !res.Success || res.Value == nil {
		return nil, fmt.Errorf("error getting user info")
	}
	data := *res.Value
	expiration := time.Unix(data.PremiumLeft, 0)
	profile := &types.Profile{
		ID:         1,
		Username:   data.Username,
		Name:       dl.config.Name,
		Email:      data.Email,
		Points:     data.Points,
		Premium:    data.PremiumLeft,
		Expiration: expiration,
	}
	if expiration.IsZero() {
		profile.Expiration = time.Now().AddDate(1, 0, 0)
	}
	if data.PremiumLeft > 0 {
		profile.Type = "premium"
	} else {
		profile.Type = "free"
	}
	return profile, nil
}

func (dl *DebridLink) AccountManager() *account.Manager {
	return dl.accountsManager
}

func (dl *DebridLink) syncAccount(*account.Account) error {
	// Currently no account-specific data to sync
	return nil
}

func (dl *DebridLink) SyncAccounts() {
	dl.accountsManager.Sync(dl.syncAccount)
}

func (dl *DebridLink) deleteDownloadLink(account *account.Account, downloadLink types.DownloadLink) error {
	deleteURL := fmt.Sprintf("%s/downloader/%s/remove", dl.Host, downloadLink.ID)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodDelete, deleteURL, nil)
	if err != nil {
		return err
	}

	resp, err := account.Client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if !common.IsSuccess(resp.StatusCode) {
		return fmt.Errorf("debridlink API error: Status: %d", resp.StatusCode)
	}

	dl.logger.Info().Msgf("Download link: %s deleted from DebridLink", downloadLink.Filename)
	return nil
}

func (dl *DebridLink) DeleteLink(downloadLink types.DownloadLink) error {
	return dl.accountsManager.DeleteDownloadLink(downloadLink, dl.deleteDownloadLink)
}

// SpeedTest measures API latency and download speed using cached links.
func (dl *DebridLink) SpeedTest(ctx context.Context) types.SpeedTestResult {
	result := types.SpeedTestResult{
		Provider: dl.config.Name,
		TestedAt: time.Now(),
	}

	start := time.Now()
	httpStatus, err := dl.doGet("/account/infos", nil, nil)
	latency := time.Since(start)

	if err != nil {
		result.Error = fmt.Sprintf("latency test failed: %v", err)
		return result
	}

	if !common.IsSuccess(httpStatus) {
		result.Error = fmt.Sprintf("latency test unexpected status: %d", httpStatus)
		return result
	}
	result.LatencyMs = latency.Milliseconds()

	dl.accountsManager.MeasureDownload(ctx, &result)
	return result
}

func (dl *DebridLink) SupportsCheck() bool {
	return true
}

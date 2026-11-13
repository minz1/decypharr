package premiumize

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"path"
	"path/filepath"
	"runtime"
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
	"github.com/sirrobot01/decypharr/pkg/version"
)

const (
	// defaultLinkExpiry applies when auto_expire_links_after is unset.
	defaultLinkExpiry    = 48 * time.Hour
	percent              = 100
	defaultHost          = "https://www.premiumize.me"
	profileCacheDuration = time.Hour
	// statusPollInterval is the delay between CheckStatus polls of a finished
	// transfer whose files are not linked yet.
	statusPollInterval = 2 * time.Second
	// maxFinalizingPolls bounds that wait (about 30 seconds at the default
	// interval). It runs inside the add request and a queue slot, so a longer wait
	// is left to the queue's next pass.
	maxFinalizingPolls = 15

	statusError = "error"
)

var _ common.Client = (*Premiumize)(nil)

type Premiumize struct {
	Host                  string `json:"host"`
	APIKey                string
	client                *request.Client
	accountsManager       *account.Manager
	autoExpiresLinksAfter time.Duration
	logger                zerolog.Logger
	config                config.Debrid
	profile               types.ProfileCache
	options               types.ProviderOptions

	// CheckStatus polling knobs; zero values select the defaults.
	pollInterval       time.Duration
	maxFinalizingPolls int
}

func New(
	dc config.Debrid,
	ratelimits map[string]ratelimit.Limiter,
	options types.ProviderOptions,
) (*Premiumize, error) {
	_log := options.Logger
	headers := map[string]string{
		"Authorization": fmt.Sprintf("Bearer %s", dc.APIKey),
	}
	if dc.UserAgent != "" {
		headers["User-Agent"] = dc.UserAgent
	} else {
		headers["User-Agent"] = fmt.Sprintf("Decypharr/%s (%s; %s)", version.GetInfo(), runtime.GOOS, runtime.GOARCH)
	}

	autoExpiresLinksAfter, err := utils.ParseDuration(dc.AutoExpireLinksAfter)
	if autoExpiresLinksAfter == 0 || err != nil {
		autoExpiresLinksAfter = defaultLinkExpiry
	}

	opts := []request.ClientOption{
		request.WithHeaders(headers),
		request.WithMaxRetries(options.Retries),
		request.WithRateLimiter(ratelimits["main"]),
		request.WithRetryableStatus(
			http.StatusTooManyRequests,
			http.StatusBadGateway,
			http.StatusServiceUnavailable,
			http.StatusGatewayTimeout,
		),
	}
	if dc.Proxy != "" {
		opts = append(opts, request.WithProxy(dc.Proxy))
	}

	return &Premiumize{
		Host:                  defaultHost,
		APIKey:                dc.APIKey,
		client:                request.New(_log, options.TLSConfig, opts...),
		accountsManager:       account.NewManager(dc, options, ratelimits["download"]),
		autoExpiresLinksAfter: autoExpiresLinksAfter,
		logger:                _log,
		config:                dc,
		options:               options,
	}, nil
}

func (pm *Premiumize) Logger() zerolog.Logger {
	return pm.logger
}

func (pm *Premiumize) Config() config.Debrid {
	return pm.config
}

func (pm *Premiumize) endpoint(apiPath string) string {
	if strings.HasPrefix(apiPath, "/") {
		return pm.Host + apiPath
	}
	return pm.Host + "/" + apiPath
}

// do sends req, rejects non-2xx and "status":"error" envelopes, and decodes
// the body into out.
func (pm *Premiumize) do(req *http.Request, out any) error {
	resp, err := pm.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("reading response body: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return customerror.NewProviderStatusError(
			fmt.Errorf("premiumize API error: Status: %d || Body: %s", resp.StatusCode, string(body)), resp.StatusCode)
	}

	if len(bytes.TrimSpace(body)) == 0 || out == nil {
		return nil
	}

	if unmarshalErr := json.Unmarshal(body, out); unmarshalErr != nil {
		return unmarshalErr
	}

	var envelope apiError
	if unmarshalErr := json.Unmarshal(body, &envelope); unmarshalErr == nil && envelope.Status == statusError {
		return fmt.Errorf("premiumize API error: %s (%s)", envelope.Message, envelope.Code)
	}

	return nil
}

func (pm *Premiumize) doForm(
	ctx context.Context,
	method, apiPath string,
	values url.Values,
	out any,
) error {
	req, err := http.NewRequestWithContext(ctx, method, pm.endpoint(apiPath), strings.NewReader(values.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return pm.do(req, out)
}

func (pm *Premiumize) SubmitMagnet(ctx context.Context, t *types.Torrent) (*types.Torrent, error) {
	if t.Magnet == nil {
		return nil, fmt.Errorf("missing magnet")
	}
	if t.Magnet.IsTorrent() {
		return pm.addTorrent(ctx, t)
	}
	return pm.addMagnet(ctx, t)
}

func (pm *Premiumize) addMagnet(ctx context.Context, t *types.Torrent) (*types.Torrent, error) {
	src := t.Magnet.Link
	if src == "" && t.InfoHash != "" {
		src = utils.ConstructMagnet(t.InfoHash, t.Name).Link
	}
	var data transferCreateResponse
	err := pm.doForm(ctx, http.MethodPost, "/api/transfer/create", url.Values{"src": {src}}, &data)
	if err != nil {
		return nil, err
	}
	pm.applySubmittedTorrent(t, data)
	return t, nil
}

func (pm *Premiumize) addTorrent(ctx context.Context, t *types.Torrent) (*types.Torrent, error) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("src", cmp.Or(t.Filename, t.Magnet.Name, "upload.torrent"))
	if err != nil {
		return nil, err
	}
	if _, writeErr := part.Write(t.Magnet.File); writeErr != nil {
		return nil, writeErr
	}
	if closeErr := writer.Close(); closeErr != nil {
		return nil, closeErr
	}

	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		pm.endpoint("/api/transfer/create"),
		&body,
	)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())

	var data transferCreateResponse
	if doErr := pm.do(req, &data); doErr != nil {
		return nil, doErr
	}
	pm.applySubmittedTorrent(t, data)
	return t, nil
}

func (pm *Premiumize) applySubmittedTorrent(t *types.Torrent, data transferCreateResponse) {
	t.ID = data.ID
	t.Debrid = pm.config.Name
	t.Status = types.TorrentStatusRemoteQueued
	if data.Name != "" {
		t.Name = data.Name
		t.Filename = data.Name
	}
	if t.OriginalFilename == "" {
		t.OriginalFilename = cmp.Or(t.Magnet.Name, t.Name)
	}
}

func (pm *Premiumize) CheckStatus(ctx context.Context, t *types.Torrent) (*types.Torrent, error) {
	return pm.UpdateAndReturnTorrent(ctx, t)
}

func (pm *Premiumize) UpdateAndReturnTorrent(ctx context.Context, t *types.Torrent) (*types.Torrent, error) {
	interval := cmp.Or(pm.pollInterval, statusPollInterval)
	maxFinalizing := cmp.Or(pm.maxFinalizingPolls, maxFinalizingPolls)
	var obs types.Observation
	var err error
	for polls := 0; ; polls++ {
		obs, err = pm.refreshTorrent(ctx, t)
		if err != nil {
			return t, err
		}
		if obs.Phase != types.PhaseFinalizing {
			break
		}
		// Premiumize reports the transfer finished but its links are not visible
		// yet. That says nothing about the cache, so keep polling (bounded).
		if polls >= maxFinalizing {
			return t, fmt.Errorf(
				"torrent %s finished but its files are still not ready after %d polls: %w",
				t.Name,
				maxFinalizing,
				customerror.ErrProviderStatusTimeout,
			)
		}
		if waitErr := pm.options.Wait(ctx, interval); waitErr != nil {
			return t, customerror.StatusWaitStopped(t.Name, waitErr)
		}
	}
	if t.Status != types.TorrentStatusError {
		switch obs.Phase {
		case types.PhaseDownloaded:
			return t, nil
		case types.PhaseDownloading, types.PhaseRemoteQueued:
			if !t.DownloadUncached {
				return t, fmt.Errorf("torrent %s: %w", t.Name, customerror.ErrTorrentNotCached)
			}
			return t, nil
		case types.PhaseNone, types.PhaseUnknown, types.PhaseConverting, types.PhaseAwaitingSelection,
			types.PhaseFinalizing, types.PhaseStalled, types.PhaseFailed:
		}
	}
	return t, fmt.Errorf("torrent: %s has error status: %s", t.Name, t.Status)
}

func (pm *Premiumize) GetTorrent(ctx context.Context, torrentID string) (*types.Torrent, error) {
	transfers, err := pm.listTransfers(ctx)
	if err != nil {
		return nil, err
	}
	for _, tr := range transfers {
		if tr.ID == torrentID {
			return pm.transferToTorrent(ctx, tr, "")
		}
	}
	return nil, customerror.ErrTorrentNotFound
}

func (pm *Premiumize) UpdateTorrent(ctx context.Context, t *types.Torrent) error {
	_, err := pm.refreshTorrent(ctx, t)
	return err
}

func (pm *Premiumize) refreshTorrent(ctx context.Context, t *types.Torrent) (types.Observation, error) {
	transfers, err := pm.listTransfers(ctx)
	if err != nil {
		return types.Observation{}, err
	}
	for _, tr := range transfers {
		if tr.ID == t.ID {
			updated, obs, transferErr := pm.transferState(ctx, tr, t.InfoHash)
			if transferErr != nil {
				return types.Observation{}, transferErr
			}
			t.Name = updated.Name
			t.Filename = updated.Filename
			t.OriginalFilename = updated.OriginalFilename
			t.Bytes = updated.Bytes
			t.Size = updated.Size
			t.Progress = updated.Progress
			t.Status = updated.Status
			t.Files = updated.Files
			t.Links = updated.Links
			t.Debrid = updated.Debrid
			t.InfoHash = cmp.Or(updated.InfoHash, t.InfoHash)
			return obs, nil
		}
	}
	return types.Observation{}, customerror.ErrTorrentNotFound
}

func (pm *Premiumize) DeleteTorrent(ctx context.Context, torrentID string) error {
	return pm.doForm(
		ctx,
		http.MethodPost,
		"/api/transfer/delete",
		url.Values{"id": {torrentID}},
		nil,
	)
}

func (pm *Premiumize) IsAvailable(ctx context.Context, infohashes []string) (map[string]bool, error) {
	result := make(map[string]bool, len(infohashes))
	const batchSize = 100
	for i := 0; i < len(infohashes); i += batchSize {
		end := min(i+batchSize, len(infohashes))
		values := url.Values{}
		validHashes := make([]string, 0, end-i)
		for _, hash := range infohashes[i:end] {
			if hash == "" {
				continue
			}
			item := utils.ConstructMagnet(hash, "").Link
			values.Add("items[]", item)
			validHashes = append(validHashes, hash)
		}
		if len(values) == 0 {
			continue
		}
		var data cacheCheckResponse
		if err := pm.doForm(ctx, http.MethodPost, "/api/cache/check", values, &data); err != nil {
			return result, fmt.Errorf("check availability: %w", err)
		}
		if len(data.Response) != len(validHashes) {
			return result, fmt.Errorf(
				"check availability: got %d results for %d hashes",
				len(data.Response),
				len(validHashes),
			)
		}
		for idx, available := range data.Response {
			result[validHashes[idx]] = available
		}
	}
	return result, nil
}

func (pm *Premiumize) GetTorrents(ctx context.Context) ([]*types.Torrent, error) {
	transfers, err := pm.listTransfers(ctx)
	if err != nil {
		return nil, err
	}
	torrents := make([]*types.Torrent, 0, len(transfers))
	for _, tr := range transfers {
		torrent, transferToTorrentErr := pm.transferToTorrent(ctx, tr, "")
		if transferToTorrentErr != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				// A listing is whole or an error, never a partial list.
				return nil, fmt.Errorf("list premiumize transfers: %w", ctxErr)
			}
			pm.logger.Warn().Err(transferToTorrentErr).Str("transfer_id", tr.ID).Msg("Skipping Premiumize transfer")
			continue
		}
		if torrent.Status == types.TorrentStatusDownloaded {
			torrents = append(torrents, torrent)
		}
	}
	return torrents, nil
}

func (pm *Premiumize) listTransfers(ctx context.Context) ([]premiumizeTransfer, error) {
	var data transferListResponse
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pm.endpoint("/api/transfer/list"), nil)
	if err != nil {
		return nil, err
	}
	if doErr := pm.do(req, &data); doErr != nil {
		return nil, doErr
	}
	return data.Transfers, nil
}

func (pm *Premiumize) transferToTorrent(
	ctx context.Context,
	tr premiumizeTransfer,
	fallbackInfoHash string,
) (*types.Torrent, error) {
	torrent, _, err := pm.transferState(ctx, tr, fallbackInfoHash)
	return torrent, err
}

// transferState converts a transfer. finalizing is true when Premiumize marks
// it finished but the folder tree has no usable file links yet; the torrent is
// then reported as downloading.
func (pm *Premiumize) transferState(
	ctx context.Context,
	tr premiumizeTransfer,
	fallbackInfoHash string,
) (*types.Torrent, types.Observation, error) {
	status := mapStatus(tr.Status)
	files := make(map[string]types.File)
	var links []string
	obs := observe(tr.Status, true)
	if status == types.TorrentStatusDownloaded {
		var ready bool
		var filesErr error
		files, links, ready, filesErr = pm.filesForTransfer(ctx, tr)
		if filesErr != nil {
			return nil, types.Observation{}, filesErr
		}
		// Premiumize can mark a transfer finished before its folder tree has
		// usable file links. Keep it active until linked files are visible.
		if !ready {
			status = types.TorrentStatusDownloading
			obs = observe(tr.Status, false)
		}
	}
	size := int64(0)
	for _, file := range files {
		size += file.Size
	}
	name := cmp.Or(tr.Name, tr.ID)
	added := time.Unix(tr.Created.Unix, 0)
	if tr.Created.Unix == 0 {
		added = time.Time{}
	}
	return &types.Torrent{
		ID:               tr.ID,
		InfoHash:         pm.transferInfoHash(tr, fallbackInfoHash),
		Name:             name,
		Filename:         name,
		OriginalFilename: name,
		Bytes:            size,
		Size:             size,
		Files:            files,
		Status:           status,
		Progress:         normalizeProgress(tr.Progress),
		Links:            links,
		Debrid:           pm.config.Name,
		Added:            added,
	}, obs, nil
}

func (pm *Premiumize) transferInfoHash(tr premiumizeTransfer, fallbackInfoHash string) string {
	if infoHash := cmp.Or(utils.ExtractInfoHash(tr.Src), fallbackInfoHash); infoHash != "" {
		return infoHash
	}

	// Premiumize documents src as a deprecated proxy URL, not the original
	// magnet, so completed transfers commonly have no recoverable infohash.
	// Use a provider-scoped, deterministic 40-character key so separate cloud
	// transfers cannot collapse into the same empty storage record during sync.
	digest := sha256.Sum256([]byte("premiumize\x00" + pm.config.Name + "\x00" + tr.ID))
	return hex.EncodeToString(digest[:20])
}

func (pm *Premiumize) filesForTransfer(
	ctx context.Context,
	tr premiumizeTransfer,
) (map[string]types.File, []string, bool, error) {
	files := make(map[string]types.File)
	links := make([]string, 0)
	if fileID := tr.FileID.String(); fileID != "" {
		item, err := pm.itemDetails(ctx, fileID)
		if err != nil {
			return nil, nil, false, err
		}
		pm.addFile(files, &links, tr.ID, item.Name, item.Name, item.Size, item.ID, item.Link)
		return files, links, item.Link != "", nil
	}
	if folderID := tr.FolderID.String(); folderID != "" {
		linkedFiles, err := pm.addFolderFiles(ctx, files, &links, tr.ID, folderID, "")
		if err != nil {
			return nil, nil, false, err
		}
		return files, links, linkedFiles > 0, nil
	}
	return files, links, false, nil
}

func (pm *Premiumize) itemDetails(ctx context.Context, id string) (*itemDetailsResponse, error) {
	var data itemDetailsResponse
	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodGet,
		pm.endpoint("/api/item/details?id="+url.QueryEscape(id)),
		nil,
	)
	if err != nil {
		return nil, err
	}
	if doErr := pm.do(req, &data); doErr != nil {
		return nil, doErr
	}
	return &data, nil
}

func (pm *Premiumize) addFolderFiles(
	ctx context.Context,
	files map[string]types.File,
	links *[]string,
	transferID, folderID, prefix string,
) (int, error) {
	var data folderListResponse
	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodGet,
		pm.endpoint("/api/folder/list?id="+url.QueryEscape(folderID)),
		nil,
	)
	if err != nil {
		return 0, err
	}
	if doErr := pm.do(req, &data); doErr != nil {
		return 0, doErr
	}
	// Count links from the full recursive tree, not just files that pass
	// Decypharr's extension/size filters, so readiness reflects Premiumize.
	linkedFiles := 0
	for _, item := range data.Content {
		itemPath := path.Join(prefix, item.Name)
		if item.Type == "folder" {
			n, addFolderFilesErr := pm.addFolderFiles(ctx, files, links, transferID, item.ID, itemPath)
			if addFolderFilesErr != nil {
				return 0, addFolderFilesErr
			}
			linkedFiles += n
			continue
		}
		if item.Link != "" {
			linkedFiles++
		}
		pm.addFile(files, links, transferID, item.Name, itemPath, item.Size, item.ID, item.Link)
	}
	return linkedFiles, nil
}

func (pm *Premiumize) addFile(
	files map[string]types.File,
	links *[]string,
	transferID, name, itemPath string,
	size int64,
	id, link string,
) {
	if link == "" {
		return
	}
	if itemPath == "" {
		itemPath = name
	}
	if pm.options.FileAllowed(itemPath, size) != nil {
		return
	}
	fileName := filepath.Base(itemPath)
	if _, exists := files[fileName]; exists {
		fileName = itemPath
	}
	files[fileName] = types.File{
		TorrentID: transferID,
		ID:        id,
		Name:      fileName,
		Path:      itemPath,
		Size:      size,
		Link:      link,
	}
	*links = append(*links, link)
}

func (pm *Premiumize) GetDownloadLink(ctx context.Context, id string, file *types.File) (types.DownloadLink, error) {
	return pm.accountsManager.GetDownloadLink(ctx, id, file, pm.fetchDownloadLink)
}

func (pm *Premiumize) fetchDownloadLink(
	ctx context.Context,
	acc *account.Account,
	_ string,
	file *types.File,
) (types.DownloadLink, error) {
	link := file.Link
	size := file.Size
	filename := file.Name
	if file.ID != "" {
		item, err := pm.itemDetails(ctx, file.ID)
		if err != nil {
			return types.DownloadLink{}, err
		}
		link = item.Link
		size = item.Size
		filename = item.Name
	}
	if link == "" {
		return types.DownloadLink{}, customerror.ErrHosterUnavailable
	}
	now := time.Now()
	return types.DownloadLink{
		Debrid:       pm.config.Name,
		Token:        acc.Token,
		Filename:     filename,
		Size:         size,
		Link:         cmp.Or(file.Link, link),
		DownloadLink: link,
		Generated:    now,
		ExpiresAt:    now.Add(pm.autoExpiresLinksAfter),
		ID:           file.ID,
	}, nil
}

func (pm *Premiumize) RefreshDownloadLinks(ctx context.Context) error {
	return pm.accountsManager.RefreshLinks(
		ctx,
		func(fetchCtx context.Context, _ *account.Account) ([]types.DownloadLink, error) {
			// Premiumize links do not expire, so there is nothing to fetch; an ended ctx is still reported.
			return []types.DownloadLink{}, fetchCtx.Err()
		},
	)
}

func (pm *Premiumize) CheckFile(ctx context.Context, _, fileID string) error {
	if strings.HasPrefix(fileID, "http://") || strings.HasPrefix(fileID, "https://") {
		req, err := http.NewRequestWithContext(ctx, http.MethodHead, fileID, nil)
		if err != nil {
			return err
		}
		resp, err := pm.client.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone {
			return customerror.ErrHosterUnavailable
		}
		if resp.StatusCode >= http.StatusBadRequest {
			return fmt.Errorf("premiumize link check failed: Status: %d", resp.StatusCode)
		}
		return nil
	}
	if fileID == "" {
		return customerror.ErrHosterUnavailable
	}
	if _, err := pm.itemDetails(ctx, fileID); err != nil {
		return err
	}
	return nil
}

func (pm *Premiumize) GetProfile(ctx context.Context) (*types.Profile, error) {
	return pm.profile.Get(profileCacheDuration, func() (*types.Profile, error) {
		return pm.getClientProfile(ctx, pm.client)
	})
}

func (pm *Premiumize) getClientProfile(ctx context.Context, client *request.Client) (*types.Profile, error) {
	var data accountInfoResponse
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pm.endpoint("/api/account/info"), nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("premiumize API error: Status: %d || Body: %s", resp.StatusCode, string(body))
	}
	if unmarshalErr := json.Unmarshal(body, &data); unmarshalErr != nil {
		return nil, unmarshalErr
	}
	if data.Status == statusError {
		return nil, fmt.Errorf("premiumize API error: %s (%s)", data.Message, data.Code)
	}
	expiration := time.Time{}
	premium := int64(0)
	accountType := "free"
	if data.PremiumUntil != nil {
		premium = *data.PremiumUntil
		expiration = time.Unix(*data.PremiumUntil, 0)
		if premium > time.Now().Unix() {
			accountType = "premium"
		}
	}
	return &types.Profile{
		Name:       pm.config.Name,
		ID:         data.customerIDInt64(),
		Username:   strconv.FormatInt(data.customerIDInt64(), 10),
		Points:     data.BoosterPoints,
		Premium:    premium,
		Expiration: expiration,
		Type:       accountType,
	}, nil
}

func (pm *Premiumize) GetAvailableSlots(ctx context.Context) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	// Premiumize does not provide active slot info without listing transfers.
	return config.DefaultAvailableSlots, nil
}

func (pm *Premiumize) AccountManager() *account.Manager {
	return pm.accountsManager
}

func (pm *Premiumize) SyncAccounts(ctx context.Context) {
	pm.accountsManager.Sync(ctx, pm.syncAccount)
}

func (pm *Premiumize) syncAccount(ctx context.Context, acc *account.Account) error {
	profile, err := pm.getClientProfile(ctx, acc.Client())
	if err != nil {
		return err
	}
	acc.SetProfile(profile.Username, profile.Expiration)
	return nil
}

func (pm *Premiumize) DeleteLink(ctx context.Context, downloadLink types.DownloadLink) error {
	return pm.accountsManager.DeleteDownloadLink(
		ctx,
		downloadLink,
		func(deleteCtx context.Context, _ *account.Account, _ types.DownloadLink) error {
			// Premiumize exposes item and transfer deletion, but not a safe
			// generated-link deletion endpoint. Deleting dl.Id here would delete
			// the user's cloud file, not just invalidate this cached CDN link.
			return deleteCtx.Err()
		},
	)
}

func (pm *Premiumize) SpeedTest(ctx context.Context) types.SpeedTestResult {
	result := types.SpeedTestResult{
		Provider: pm.config.Name,
		TestedAt: time.Now(),
	}
	start := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pm.endpoint("/api/account/info"), nil)
	if err != nil {
		result.Error = err.Error()
		return result
	}
	resp, err := pm.client.Do(req)
	latency := time.Since(start)
	if err != nil {
		result.Error = fmt.Sprintf("latency test failed: %v", err)
		return result
	}
	_ = resp.Body.Close() // only the status is needed
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		result.Error = fmt.Sprintf("latency test unexpected status: %d", resp.StatusCode)
		return result
	}
	result.LatencyMs = latency.Milliseconds()

	pm.accountsManager.MeasureDownload(ctx, &result)
	return result
}

func (pm *Premiumize) SupportsCheck() bool {
	return true
}

func mapStatus(status string) types.TorrentStatus {
	switch strings.ToLower(status) {
	case "finished", "seeding":
		return types.TorrentStatusDownloaded
	case "queued":
		return types.TorrentStatusRemoteQueued
	case "running":
		return types.TorrentStatusDownloading
	default:
		return types.TorrentStatusError
	}
}

func normalizeProgress(progress float64) float64 {
	if progress <= 1 {
		return progress * percent
	}
	return progress
}

func (pm *Premiumize) Poll(
	ctx context.Context,
	t *types.Torrent,
	_ bool,
) (*types.Torrent, types.Observation, error) {
	obs, err := pm.refreshTorrent(ctx, t)
	if err != nil {
		return t, types.Observation{}, err
	}
	obs.Provider, obs.ID = pm.config.Name, t.ID
	obs.Progress, obs.Seeders = t.Progress, t.Seeders
	if (obs.Phase == types.PhaseDownloading || obs.Phase == types.PhaseRemoteQueued) &&
		t.Status != types.TorrentStatusError {
		obs.Cached = types.CachedNo
	}
	return t, types.WithStall(obs), nil
}

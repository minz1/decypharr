package realdebrid

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
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
	"github.com/sirrobot01/decypharr/pkg/debrid/common/rar"
	"github.com/sirrobot01/decypharr/pkg/debrid/types"
)

const (
	profileCacheDuration = 1 * time.Hour
	// defaultLinkExpiry applies when auto_expire_links_after is unset.
	defaultLinkExpiry = 48 * time.Hour
	// repairRetries bounds retries of repair-time link checks.
	repairRetries = 4
	// maxConcurrentRarReads bounds simultaneous RAR header scans.
	maxConcurrentRarReads = 2
	// availabilityBatchSize is the most hashes per instantAvailability call.
	availabilityBatchSize = 200
	// statusPollInterval is the delay before each CheckStatus poll.
	statusPollInterval = 2 * time.Second

	statusDownloaded            = "downloaded"
	statusWaitingFilesSelection = "waiting_files_selection"

	// Real-Debrid unrestrict error codes.
	errHosterUnavailable = 19
	errTrafficExhausted  = 23
	errUnavailableFile   = 24
	errTooManyRequests   = 34
	errInfringingFile    = 35
	errFairUsageLimit    = 36
	// statusTooManyActive is Real-Debrid's non-standard "too many active downloads" status.
	statusTooManyActive = 509
)

type RealDebrid struct {
	Host string `json:"host"`

	APIKey                string
	accountsManager       *account.Manager
	client                *request.Client
	repairClient          *request.Client
	autoExpiresLinksAfter time.Duration
	logger                zerolog.Logger
	options               types.ProviderOptions

	rarSemaphore chan struct{}
	profile      types.ProfileCache
	config       config.Debrid
	retries      int
}

func New(
	dc config.Debrid,
	ratelimits map[string]ratelimit.Limiter,
	options types.ProviderOptions,
) (*RealDebrid, error) {
	headers := map[string]string{
		"Authorization": fmt.Sprintf("Bearer %s", dc.APIKey),
	}
	if dc.UserAgent != "" {
		headers["User-Agent"] = dc.UserAgent
	}
	_log := options.Logger

	autoExpiresLinksAfter, err := utils.ParseDuration(dc.AutoExpireLinksAfter)
	if autoExpiresLinksAfter == 0 || err != nil {
		autoExpiresLinksAfter = defaultLinkExpiry
	}

	opts := []request.ClientOption{
		request.WithTLSConfig(options.TLSConfig),
		request.WithHeaders(headers),
		request.WithMaxRetries(options.Retries),
		request.WithRateLimiter(ratelimits["main"]),
		request.WithRetryableStatus(http.StatusTooManyRequests),
		request.WithProxy(dc.Proxy),
	}

	repairOpts := []request.ClientOption{
		request.WithTLSConfig(options.TLSConfig),
		request.WithHeaders(headers),
		request.WithLogger(_log),
		request.WithMaxRetries(repairRetries),
		request.WithRetryableStatus(http.StatusTooManyRequests),
		request.WithRateLimiter(ratelimits["repair"]),
		request.WithProxy(dc.Proxy),
	}

	r := &RealDebrid{
		Host:                  "https://api.real-debrid.com/rest/1.0",
		APIKey:                dc.APIKey,
		accountsManager:       account.NewManager(dc, options, ratelimits["download"]),
		options:               options,
		autoExpiresLinksAfter: autoExpiresLinksAfter,
		client:                request.New(opts...),
		repairClient:          request.New(repairOpts...),
		logger:                _log,
		rarSemaphore:          make(chan struct{}, maxConcurrentRarReads),
		config:                dc,
		retries:               options.Retries,
	}

	go func() {
		if _, profileErr := r.GetProfile(); profileErr != nil {
			r.logger.Error().Err(profileErr).Msg("Failed to get RealDebrid profile")
		}
	}()
	return r, nil
}

func (r *RealDebrid) Logger() zerolog.Logger {
	return r.logger
}

// doGet performs a GET request using the main client.
func (r *RealDebrid) doGet(ctx context.Context, endpoint string, result any) (int, error) {
	return r.doGetWithClient(ctx, r.client, r.Host+endpoint, nil, result)
}

// newFormRequest builds a form-encoded POST request.
func newFormRequest(ctx context.Context, fullURL string, formData map[string]string) (*http.Request, error) {
	form := url.Values{}
	for k, v := range formData {
		form.Set(k, v)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fullURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return req, nil
}

// doPostForm performs a POST request with form data.
func (r *RealDebrid) doPostForm(
	ctx context.Context,
	endpoint string,
	formData map[string]string,
	result any,
) (int, error) {
	req, err := newFormRequest(ctx, r.Host+endpoint, formData)
	if err != nil {
		return 0, err
	}
	return common.DoJSON(r.client, req, result)
}

// doPut performs a PUT request with body.
func (r *RealDebrid) doPut(
	ctx context.Context,
	endpoint string,
	body []byte,
	contentType string,
	result any,
) (int, error) {
	var bodyReader io.Reader
	if body != nil {
		bodyReader = bytes.NewReader(body)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, r.Host+endpoint, bodyReader)
	if err != nil {
		return 0, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	return common.DoJSON(r.client, req, result)
}

// doGetWithClient performs a GET using a specific client.
func (r *RealDebrid) doGetWithClient(
	ctx context.Context,
	client *request.Client,
	fullURL string,
	queryParams map[string]string,
	result any,
) (int, error) {
	u, err := url.Parse(fullURL)
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

// doPostFormWithClient POSTs form data with client. It decodes a 2xx body into
// result and any other body into errorResult.
func (r *RealDebrid) doPostFormWithClient(
	ctx context.Context,
	client *request.Client,
	fullURL string,
	formData map[string]string,
	result any,
	errorResult any,
) (int, error) {
	req, err := newFormRequest(ctx, fullURL, formData)
	if err != nil {
		return 0, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	out := errorResult
	if common.IsSuccess(resp.StatusCode) {
		out = result
	}
	if out != nil && resp.ContentLength != 0 {
		if decodeJSONErr := request.DecodeJSON(resp, out); decodeJSONErr != nil {
			return resp.StatusCode, decodeJSONErr
		}
	}
	return resp.StatusCode, nil
}

func (r *RealDebrid) getSelectedFiles(t *types.Torrent, data torrentInfo) (map[string]types.File, error) {
	files := make(map[string]types.File)
	selectedFiles := make([]types.File, 0)

	for _, f := range data.Files {
		if f.Selected == 1 {
			selectedFiles = append(selectedFiles, types.File{
				TorrentID: t.ID,
				Name:      filepath.Base(f.Path),
				Path:      filepath.Base(f.Path),
				Size:      f.Bytes,
				ID:        strconv.Itoa(f.ID),
			})
		}
	}

	if len(selectedFiles) == 0 {
		return files, nil
	}

	// Handle RARed torrents (single link, multiple files)
	if len(data.Links) == 1 && len(selectedFiles) > 1 {
		return r.handleRarArchive(t, data, selectedFiles)
	}

	// Standard case - map files to links
	if len(selectedFiles) > len(data.Links) {
		return files, nil
	}

	for i, f := range selectedFiles {
		if i < len(data.Links) {
			f.Link = data.Links[i]
			files[f.Name] = f
		}
	}

	return files, nil
}

func (r *RealDebrid) handleRarFallback(t *types.Torrent, data torrentInfo) map[string]types.File {
	files := make(map[string]types.File)
	file := types.File{
		TorrentID: t.ID,
		ID:        "0",
		Name:      t.Name + ".rar",
		Size:      data.Bytes,
		IsRar:     true,
		ByteRange: nil,
		Path:      t.Name + ".rar",
		Link:      data.Links[0],
		Generated: time.Now(),
	}
	files[file.Name] = file
	return files
}

// handleRarArchive processes RAR archives with multiple files.
func (r *RealDebrid) handleRarArchive(
	t *types.Torrent,
	data torrentInfo,
	selectedFiles []types.File,
) (map[string]types.File, error) {
	// This will block if 2 RAR operations are already in progress
	r.rarSemaphore <- struct{}{}
	defer func() {
		<-r.rarSemaphore
	}()

	files := make(map[string]types.File)

	if !r.config.UnpackRar {
		r.logger.Debug().
			Msgf("RAR file detected, but unpacking is disabled: %s. Falling back to single file representation.", t.Name)
		return r.handleRarFallback(t, data), nil
	}

	r.logger.Info().Msgf("RAR file detected, unpacking: %s", t.Name)
	linkFile := &types.File{TorrentID: t.ID, Link: data.Links[0]}
	downloadLinkObj, err := r.GetDownloadLink(context.Background(), t.ID, linkFile)

	if err != nil {
		r.logger.Debug().
			Err(err).
			Msgf("Error getting download link for RAR file: %s. Falling back to single file representation.", t.Name)
		return r.handleRarFallback(t, data), nil
	}

	dlLink := downloadLinkObj.DownloadLink
	reader, err := rar.NewReader(context.Background(), dlLink, r.retries)

	if err != nil {
		r.logger.Debug().
			Err(err).
			Msgf("Error creating RAR reader for %s. Falling back to single file representation.", t.Name)
		return r.handleRarFallback(t, data), nil
	}

	rarFiles, err := reader.GetFiles(context.Background())

	if err != nil {
		r.logger.Debug().
			Err(err).
			Msgf("Error reading RAR files for %s. Falling back to single file representation.", t.Name)
		return r.handleRarFallback(t, data), nil
	}

	// Create lookup map for faster matching
	fileMap := make(map[string]*types.File)
	for i := range selectedFiles {
		// RD converts special chars to '_' for RAR file paths
		safeName := strings.NewReplacer("|", "_", "\"", "_", "\\", "_", "?", "_", "*", "_", ":", "_", "<", "_", ">", "_").
			Replace(selectedFiles[i].Name)
		fileMap[safeName] = &selectedFiles[i]
	}

	now := time.Now()

	for _, rarFile := range rarFiles {
		if file, exists := fileMap[rarFile.Name()]; exists {
			file.IsRar = true
			file.ByteRange = rarFile.ByteRange()
			file.Link = data.Links[0]
			file.Generated = now
			files[file.Name] = *file
		} else if !rarFile.IsDirectory {
			r.logger.Warn().Msgf("RAR file %s not found in torrent files", rarFile.Name())
		}
	}
	if len(files) == 0 {
		r.logger.Warn().Msgf("No valid files found in RAR archive for torrent: %s", t.Name)
		return r.handleRarFallback(t, data), nil
	}
	r.logger.Info().Msgf("Unpacked RAR archive for torrent: %s with %d files", t.Name, len(files))
	return files, nil
}

func (r *RealDebrid) getTorrentFiles(t *types.Torrent, data torrentInfo) map[string]types.File {
	files := make(map[string]types.File)
	idx := 0

	for _, f := range data.Files {
		name := filepath.Base(f.Path)
		if err := r.options.FileAllowed(name, f.Bytes); err != nil {
			continue
		}

		file := types.File{
			TorrentID: t.ID,
			Name:      name,
			Path:      name,
			Size:      f.Bytes,
			ID:        strconv.Itoa(f.ID),
		}
		files[name] = file
		idx++
	}
	return files
}

func (r *RealDebrid) IsAvailable(hashes []string) (map[string]bool, error) {
	result := make(map[string]bool)

	for _, validHashes := range common.HashBatches(hashes, availabilityBatchSize) {
		hashStr := strings.Join(validHashes, "/")
		var data AvailabilityResponse

		status, err := r.doGet(context.Background(), fmt.Sprintf("/torrents/instantAvailability/%s", hashStr), &data)
		if err != nil {
			return result, fmt.Errorf("check availability: %w", err)
		}

		if status < 200 || status >= 300 {
			return result, fmt.Errorf("check availability: HTTP %d", status)
		}
		for _, h := range validHashes {
			result[h] = len(data[strings.ToLower(h)].Rd) > 0
		}
	}
	return result, nil
}

func (r *RealDebrid) SubmitMagnet(t *types.Torrent) (*types.Torrent, error) {
	if t.Magnet.IsTorrent() {
		return r.addTorrent(t)
	}
	return r.addMagnet(t)
}

func (r *RealDebrid) addTorrent(t *types.Torrent) (*types.Torrent, error) {
	var data AddMagnetSchema

	status, err := r.doPut(
		context.Background(),
		"/torrents/addTorrent",
		t.Magnet.File,
		"application/x-bittorrent",
		&data,
	)
	if err != nil {
		return nil, err
	}

	if status != http.StatusOK && status != http.StatusCreated {
		if status == statusTooManyActive {
			return nil, customerror.ErrTooManyActiveDownloads
		}
		if status == http.StatusUnavailableForLegalReasons {
			return nil, customerror.ErrTorrentBlocked
		}
		return nil, fmt.Errorf("unexpected status code: %d", status)
	}

	t.ID = data.ID
	t.Debrid = r.config.Name
	t.Added = time.Now()

	return t, nil
}

func (r *RealDebrid) addMagnet(t *types.Torrent) (*types.Torrent, error) {
	var data AddMagnetSchema

	formData := map[string]string{"magnet": t.Magnet.Link}
	status, err := r.doPostForm(context.Background(), "/torrents/addMagnet", formData, &data)
	if err != nil {
		return nil, err
	}

	switch status {
	case http.StatusOK, http.StatusCreated:
		t.ID = data.ID
		t.Debrid = r.config.Name
		t.Added = time.Now()
		return t, nil

	case statusTooManyActive:
		return nil, customerror.ErrTooManyActiveDownloads

	case http.StatusUnavailableForLegalReasons:
		return nil, customerror.ErrTorrentBlocked

	default:
		return nil, fmt.Errorf("realdebrid API error: Status: %d", status)
	}
}

func (r *RealDebrid) GetTorrent(torrentID string) (*types.Torrent, error) {
	var data torrentInfo

	status, err := r.doGet(context.Background(), "/torrents/info/"+torrentID, &data)
	if err != nil {
		return nil, err
	}

	switch status {
	case http.StatusOK:
		addedOn := data.Added
		if addedOn.IsZero() {
			addedOn = time.Now()
		}
		t := &types.Torrent{
			ID:               data.ID,
			Name:             data.Filename,
			Bytes:            data.Bytes,
			Progress:         data.Progress,
			Speed:            data.Speed,
			Seeders:          data.Seeders,
			Added:            addedOn,
			Status:           types.TorrentStatus(data.Status),
			Filename:         data.Filename,
			OriginalFilename: data.OriginalFilename,
			Links:            data.Links,
			Debrid:           r.config.Name,
		}

		t.Files = r.getTorrentFiles(t, data)
		return t, nil
	case http.StatusNotFound:
		return nil, customerror.ErrTorrentNotFound

	default:
		return nil, fmt.Errorf("realdebrid API error: Status: %d", status)
	}
}

func (r *RealDebrid) GetDownloadingStatus() []string {
	return []string{"downloading", "magnet_conversion", "queued", "compressing", "uploading"}
}

func getStatus(status string) types.TorrentStatus {
	switch status {
	case "downloading", "magnet_conversion", "queued", "compressing", "uploading", "waiting_files_selection":
		return types.TorrentStatusDownloading
	case statusDownloaded:
		return types.TorrentStatusDownloaded
	default:
		return types.TorrentStatusError
	}
}

func (r *RealDebrid) UpdateTorrent(t *types.Torrent) error {
	var data torrentInfo

	status, err := r.doGet(context.Background(), fmt.Sprintf("/torrents/info/%s", t.ID), &data)
	if err != nil {
		return err
	}

	switch status {
	case http.StatusOK:
		t.Name = data.Filename
		t.Bytes = data.Bytes
		t.Progress = data.Progress
		t.Status = types.TorrentStatus(data.Status)
		t.Speed = data.Speed
		t.Seeders = data.Seeders
		t.Status = getStatus(data.Status)
		t.Filename = data.Filename
		t.OriginalFilename = data.OriginalFilename
		t.Links = data.Links
		t.Debrid = r.config.Name
		t.Files, _ = r.getSelectedFiles(t, data)

		return nil

	case http.StatusNotFound:
		return customerror.ErrTorrentNotFound

	default:
		return fmt.Errorf("realdebrid API error: Status: %d", status)
	}
}

// CheckStatus polls the torrent, selecting its allowed files when Real-Debrid
// waits for a selection, until it is downloaded, still downloading or failed.
func (r *RealDebrid) CheckStatus(t *types.Torrent) (*types.Torrent, error) {
	for {
		time.Sleep(statusPollInterval)

		var data torrentInfo
		status, err := r.doGet(context.Background(), "/torrents/info/"+t.ID, &data)
		if err != nil {
			r.logger.Info().Msgf("ERROR Checking file: %v", err)
			return t, err
		}
		if status != http.StatusOK {
			return t, fmt.Errorf("realdebrid API error: Status: %d", status)
		}

		debridStatus := data.Status
		r.applyStatus(t, data)

		switch {
		case debridStatus == statusWaitingFilesSelection:
			if selectErr := r.selectFiles(t, data); selectErr != nil {
				return t, selectErr
			}
		case debridStatus == statusDownloaded:
			t.Status = types.TorrentStatusDownloaded
			t.Files, err = r.getSelectedFiles(t, data)
			if err != nil {
				return t, err
			}
			r.logger.Info().Msgf("Torrent: %s downloaded to RD", t.Name)
			return t, nil
		case t.Status == types.TorrentStatusDownloading:
			if !t.DownloadUncached {
				return t, fmt.Errorf("torrent %s: %w", t.Name, customerror.ErrTorrentNotCached)
			}
			return t, nil
		default:
			r.logger.Warn().
				Str("torrent_id", t.ID).
				Str("debrid_status", debridStatus).
				Str("mapped_status", string(t.Status)).
				Msg("Unexpected debrid status, treating as error")
			return t, fmt.Errorf("torrent: %s has error status: %s", t.Name, debridStatus)
		}
	}
}

// applyStatus copies polled torrent state onto t.
func (r *RealDebrid) applyStatus(t *types.Torrent, data torrentInfo) {
	t.Name = data.Filename
	t.Filename = data.Filename
	t.OriginalFilename = data.OriginalFilename
	t.Bytes = data.Bytes
	t.Progress = data.Progress
	t.Speed = data.Speed
	t.Seeders = data.Seeders
	t.Links = data.Links
	t.Status = getStatus(data.Status)
	t.Debrid = r.config.Name
	t.Added = data.Added
	if data.Hash != "" {
		t.InfoHash = data.Hash
	}
}

// selectFiles asks Real-Debrid to download the torrent's allowed files.
func (r *RealDebrid) selectFiles(t *types.Torrent, data torrentInfo) error {
	t.Status = types.TorrentStatusDownloading
	t.Files = r.getTorrentFiles(t, data)
	if len(t.Files) == 0 {
		return fmt.Errorf("no valid files found")
	}
	fileIDs := make([]string, 0, len(t.Files))
	for _, f := range t.Files {
		fileIDs = append(fileIDs, f.ID)
	}

	status, err := r.doPostForm(
		context.Background(),
		"/torrents/selectFiles/"+t.ID,
		map[string]string{"files": strings.Join(fileIDs, ",")},
		nil,
	)
	switch {
	case err != nil:
		return err
	case status == http.StatusNoContent:
		return nil
	case status == statusTooManyActive:
		return customerror.ErrTooManyActiveDownloads
	default:
		return fmt.Errorf("realdebrid API error: Status: %d", status)
	}
}

func (r *RealDebrid) DeleteTorrent(torrentID string) error {
	req, err := http.NewRequestWithContext(
		context.Background(),
		http.MethodDelete,
		r.Host+"/torrents/delete/"+torrentID,
		nil,
	)
	if err != nil {
		return err
	}

	resp, err := r.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("realdebrid API error: Status: %d", resp.StatusCode)
	}
	r.logger.Info().Msgf("Torrent: %s deleted from RD", torrentID)
	return nil
}

func (r *RealDebrid) GetFileDownloadLinks(t *types.Torrent) (map[string]types.DownloadLink, error) {
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error

	files := make(map[string]types.File)
	links := make(map[string]types.DownloadLink)

	_files := t.GetFiles()
	wg.Add(len(_files))

	for _, f := range _files {
		go func(file types.File) {
			defer wg.Done()
			link, err := r.GetDownloadLink(context.Background(), t.ID, &file)
			if err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				mu.Unlock()
				return
			}
			if link.Empty() {
				mu.Lock()
				if firstErr == nil {
					firstErr = fmt.Errorf("realdebrid API error: download link not found for file %s", file.Name)
				}
				mu.Unlock()
				return
			}

			file.DownloadLink = link
			mu.Lock()
			files[file.Name] = file
			links[file.Name] = link
			mu.Unlock()
		}(f)
	}

	wg.Wait()

	if firstErr != nil {
		return nil, firstErr
	}

	t.Files = files
	return links, nil
}

// CheckFile reports customerror.HosterUnavailableError when Real-Debrid no
// longer serves link. Other statuses are treated as available.
func (r *RealDebrid) CheckFile(ctx context.Context, _, link string) error {
	req, err := newFormRequest(ctx, r.Host+"/unrestrict/check", map[string]string{"link": link})
	if err != nil {
		return err
	}

	resp, err := r.repairClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return customerror.ErrHosterUnavailable
	}

	return nil
}

func (r *RealDebrid) fetchDownloadLink(
	ctx context.Context,
	account *account.Account,
	_ string,
	file *types.File,
) (types.DownloadLink, error) {
	emptyLink := types.DownloadLink{}
	link := file.Link
	if strings.HasPrefix(file.Link, "https://real-debrid.com/d/") && len(file.Link) > 39 {
		link = file.Link[0:39]
	}

	formData := map[string]string{"link": link}
	var errResp ErrorResponse
	var data UnrestrictResponse

	status, err := r.doPostFormWithClient(
		ctx,
		account.Client(),
		fmt.Sprintf("%s/unrestrict/link/", r.Host),
		formData,
		&data,
		&errResp,
	)
	if err != nil {
		return emptyLink, err
	}
	if status != http.StatusOK {
		switch errResp.ErrorCode {
		case errHosterUnavailable, errUnavailableFile, errInfringingFile:
			return emptyLink, customerror.ErrHosterUnavailable
		case errTrafficExhausted, errTooManyRequests, errFairUsageLimit:
			return emptyLink, customerror.ErrTrafficExceeded
		default:
			return emptyLink, fmt.Errorf(
				"realdebrid API error: Status: %d || Code: %d",
				status,
				errResp.ErrorCode,
			)
		}
	}
	if data.Download == "" {
		return emptyLink, fmt.Errorf("realdebrid API error: download link not found")
	}
	now := time.Now()
	dl := types.DownloadLink{
		Debrid:       r.config.Name,
		Token:        account.Token,
		Filename:     data.Filename,
		Size:         data.Filesize,
		Link:         data.Link,
		DownloadLink: data.Download,
		Generated:    now,
		ExpiresAt:    now.Add(r.autoExpiresLinksAfter),
	}
	return dl, nil
}

func (r *RealDebrid) GetDownloadLink(ctx context.Context, id string, file *types.File) (types.DownloadLink, error) {
	return r.accountsManager.GetDownloadLink(ctx, id, file, r.fetchDownloadLink)
}

// getTorrents returns the number of entries on the page before filtering and
// the downloaded torrents among them.
func (r *RealDebrid) getTorrents(offset int, limit int) (int, []*types.Torrent, error) {
	torrents := make([]*types.Torrent, 0)

	queryParams := make(map[string]string)
	if offset > 0 {
		queryParams["offset"] = strconv.Itoa(offset)
	}
	if limit > 0 {
		queryParams["limit"] = strconv.Itoa(limit)
	}

	// Need to get headers, so we create request manually
	u, err := url.Parse(r.Host + "/torrents")
	if err != nil {
		return 0, torrents, err
	}
	q := u.Query()
	for k, v := range queryParams {
		q.Set(k, v)
	}
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, u.String(), nil)
	if err != nil {
		return 0, torrents, err
	}

	resp, err := r.client.Do(req)
	if err != nil {
		return 0, torrents, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNoContent {
		return 0, torrents, nil
	}

	if resp.StatusCode != http.StatusOK {
		return 0, torrents, fmt.Errorf("realdebrid API error: %d", resp.StatusCode)
	}

	var data []TorrentsResponse
	if decodeJSONErr := request.DecodeJSON(resp, &data); decodeJSONErr != nil {
		return 0, torrents, decodeJSONErr
	}

	for _, t := range data {
		if t.Status != statusDownloaded {
			continue
		}
		t := &types.Torrent{
			ID:               t.ID,
			Name:             t.Filename,
			Bytes:            t.Bytes,
			Progress:         t.Progress,
			Status:           types.TorrentStatusDownloaded,
			Filename:         t.Filename,
			OriginalFilename: t.Filename,
			Links:            t.Links,
			Files:            make(map[string]types.File),
			InfoHash:         t.Hash,
			Debrid:           r.config.Name,
			Added:            t.Added,
		}
		torrents = append(torrents, t)
	}
	return len(data), torrents, nil
}

func (r *RealDebrid) GetTorrents() ([]*types.Torrent, error) {
	limit := 1000
	if r.config.Limit != 0 {
		limit = r.config.Limit
	}
	hardLimit := r.config.Limit

	allTorrents := make([]*types.Torrent, 0)
	var fetchError error
	offset := 0
	for {
		seen, torrents, err := r.getTorrents(offset, limit)
		if err != nil {
			fetchError = err
			break
		}
		// Advance by the raw page size: non-downloaded torrents are filtered
		// out of torrents, and using the filtered count re-reads or stops early.
		if seen == 0 {
			break
		}
		allTorrents = append(allTorrents, torrents...)
		offset += seen
		if hardLimit != 0 && len(allTorrents) >= hardLimit {
			break
		}
	}

	if fetchError != nil {
		return nil, fetchError
	}

	return allTorrents, nil
}

func (r *RealDebrid) RefreshDownloadLinks() error {
	return r.accountsManager.RefreshLinks(r.fetchDownloadLinks)
}

func (r *RealDebrid) fetchDownloadLinks(acc *account.Account) ([]types.DownloadLink, error) {
	links := make([]types.DownloadLink, 0)
	limit := 1000
	offset := 0
	for {
		batchLinks, err := r._getDownloadLinks(acc, offset, limit)
		if err != nil {
			return nil, err
		}
		if len(batchLinks) == 0 {
			break
		}
		links = append(links, batchLinks...)
		offset += len(batchLinks)
	}
	return links, nil
}

func (r *RealDebrid) _getDownloadLinks(acc *account.Account, offset int, limit int) ([]types.DownloadLink, error) {
	var data []DownloadsResponse

	queryParams := map[string]string{
		"limit": strconv.Itoa(limit),
	}
	if offset > 0 {
		queryParams["offset"] = strconv.Itoa(offset)
	}

	status, err := r.doGetWithClient(
		context.Background(),
		acc.Client(),
		fmt.Sprintf("%s/downloads", r.Host),
		queryParams,
		&data,
	)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("realdebrid API error: Status: %d", status)
	}
	links := make([]types.DownloadLink, 0)
	for _, d := range data {
		links = append(links, types.DownloadLink{
			Debrid:       r.config.Name,
			Token:        acc.Token,
			Filename:     d.Filename,
			Size:         d.Filesize,
			Link:         d.Link,
			DownloadLink: d.Download,
			Generated:    d.Generated,
			ExpiresAt:    d.Generated.Add(r.autoExpiresLinksAfter),
			ID:           d.ID,
		})
	}
	return links, nil
}

func (r *RealDebrid) Config() config.Debrid {
	return r.config
}

func (r *RealDebrid) getClientProfile(client *request.Client) (*types.Profile, error) {
	var data profileResponse

	status, err := r.doGetWithClient(context.Background(), client, fmt.Sprintf("%s/user", r.Host), nil, &data)
	if err != nil {
		return nil, err
	}

	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("realdebrid API error: Status: %d", status)
	}

	profile := &types.Profile{
		Name:       r.config.Name,
		ID:         data.ID,
		Username:   data.Username,
		Email:      data.Email,
		Points:     data.Points,
		Premium:    data.Premium,
		Expiration: data.Expiration,
		Type:       data.Type,
	}
	return profile, nil
}

func (r *RealDebrid) GetProfile() (*types.Profile, error) {
	return r.profile.Get(profileCacheDuration, func() (*types.Profile, error) {
		return r.getClientProfile(r.client)
	})
}

func (r *RealDebrid) GetAvailableSlots() (int, error) {
	var data AvailableSlotsResponse

	status, err := r.doGet(context.Background(), "/torrents/activeCount", &data)
	if err != nil {
		return 0, err
	}

	if status < 200 || status >= 300 {
		return 0, fmt.Errorf("realdebrid API error: Status: %d", status)
	}

	return data.TotalSlots - data.ActiveSlots - r.config.MinimumFreeSlot, nil
}

func (r *RealDebrid) AccountManager() *account.Manager {
	return r.accountsManager
}

func (r *RealDebrid) SyncAccounts() {
	r.accountsManager.Sync(r.syncAccount)
}

func (r *RealDebrid) syncAccount(acc *account.Account) error {
	if acc.Token == "" {
		return fmt.Errorf("account %s has no token", acc.Username)
	}
	profile, err := r.getClientProfile(acc.Client())
	if err != nil {
		return fmt.Errorf("error syncing account %s: %w", acc.Username, err)
	}
	acc.Username = profile.Username
	acc.Expiration = profile.Expiration

	var trafficData TrafficResponse
	trafficStatus, err := r.doGetWithClient(
		context.Background(),
		acc.Client(),
		fmt.Sprintf("%s/traffic/details", r.Host),
		nil,
		&trafficData,
	)
	if err != nil || trafficStatus != http.StatusOK {
		// Traffic is informational; a failed lookup must not fail the sync.
		r.logger.Debug().Err(err).Int("status", trafficStatus).Msg("Failed to fetch Real-Debrid traffic")
		return nil
	}

	if len(trafficData) == 0 {
		acc.TrafficUsed.Store(0)
	} else {
		today := time.Now().Format(time.DateOnly)
		if todayData, exists := trafficData[today]; exists {
			acc.TrafficUsed.Store(todayData.Bytes)
		}
	}
	return nil
}

func (r *RealDebrid) deleteDownloadLink(account *account.Account, downloadLink types.DownloadLink) error {
	req, err := http.NewRequestWithContext(
		context.Background(),
		http.MethodDelete,
		fmt.Sprintf("%s/downloads/delete/%s", r.Host, downloadLink.ID),
		nil,
	)
	if err != nil {
		return err
	}
	status, err := common.DoJSON(account.Client(), req, nil)
	if err != nil {
		return err
	}
	if !common.IsSuccess(status) {
		return fmt.Errorf("realdebrid API error: Status: %d", status)
	}
	return nil
}

func (r *RealDebrid) DeleteLink(downloadLink types.DownloadLink) error {
	return r.accountsManager.DeleteDownloadLink(downloadLink, r.deleteDownloadLink)
}

// SpeedTest measures API latency and download speed using cached links.
func (r *RealDebrid) SpeedTest(ctx context.Context) types.SpeedTestResult {
	result := types.SpeedTestResult{
		Provider: r.config.Name,
		TestedAt: time.Now(),
	}

	// Measure latency by hitting the user endpoint
	start := time.Now()
	status, err := r.doGet(ctx, "/user", nil)
	latency := time.Since(start)

	if err != nil {
		result.Error = fmt.Sprintf("latency test failed: %v", err)
		return result
	}

	if status < 200 || status >= 300 {
		result.Error = fmt.Sprintf("latency test unexpected status: %d", status)
		return result
	}
	result.LatencyMs = latency.Milliseconds()

	r.accountsManager.MeasureDownload(ctx, &result)
	return result
}

func (r *RealDebrid) SupportsCheck() bool {
	return true
}

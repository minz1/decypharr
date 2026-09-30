package qbit

import (
	"context"
	"fmt"
	"mime/multipart"
	"net/http"
	"slices"
	"time"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/customerror"
	"github.com/sirrobot01/decypharr/internal/utils"
	"github.com/sirrobot01/decypharr/pkg/arr"
	"github.com/sirrobot01/decypharr/pkg/manager"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

func (q *QBit) addMagnet(
	ctx context.Context,
	url string,
	arr arr.Arr,
	debrid string,
	action config.DownloadAction,
	callbackURL string,
	rmTrackerUrls, skipMultiSeason bool,
) error {
	magnet, err := utils.GetMagnetFromUrl(url, rmTrackerUrls)
	if err != nil {
		return customerror.NewError(fmt.Errorf("error parsing magnet link: %w", err), http.StatusBadRequest, "invalid_magnet", false, false).
			Permanent()
	}

	importReq := manager.NewTorrentRequest(
		debrid,
		q.downloadFolder,
		magnet,
		arr,
		action,
		arr.DownloadUncached,
		callbackURL,
		manager.ImportTypeQBit,
		skipMultiSeason,
	)

	err = q.manager.AddNewTorrent(ctx, importReq)
	if err != nil {
		return fmt.Errorf("failed to process torrent: %w", err)
	}
	return nil
}

func (q *QBit) addTorrent(
	ctx context.Context,
	fileHeader *multipart.FileHeader,
	arr arr.Arr,
	debrid string,
	action config.DownloadAction,
	callbackURL string,
	rmTrackerUrls, skipMultiSeason bool,
) error {
	file, err := fileHeader.Open()
	if err != nil {
		return customerror.NewError(fmt.Errorf("error opening torrent file: %w", err), http.StatusBadRequest, "invalid_torrent", false, false).
			Permanent()
	}
	defer file.Close()
	magnet, err := utils.GetMagnetFromFile(file, fileHeader.Filename, rmTrackerUrls)
	if err != nil {
		return customerror.NewError(fmt.Errorf("error reading file %s: %w", fileHeader.Filename, err), http.StatusBadRequest, "invalid_torrent", false, false).
			Permanent()
	}
	importReq := manager.NewTorrentRequest(
		debrid,
		q.downloadFolder,
		magnet,
		arr,
		action,
		arr.DownloadUncached,
		callbackURL,
		manager.ImportTypeQBit,
		skipMultiSeason,
	)
	err = q.manager.AddNewTorrent(ctx, importReq)
	if err != nil {
		return fmt.Errorf("failed to process torrent: %w", err)
	}
	return nil
}

// Plausible swarm figures for properties: debrid entries have no real swarm.
const (
	fakeSwarmSize  = 100
	fakePeersTotal = 2
)

// GetTorrentProperties reports qBittorrent-style properties for t.
func (q *QBit) GetTorrentProperties(t *storage.Entry) *TorrentProperties {
	return &TorrentProperties{
		AdditionDate:       t.AddedOn.Unix(),
		Comment:            "Provider Blackhole <https://github.com/sirrobot01/decypharr>",
		CreatedBy:          "Provider Blackhole <https://github.com/sirrobot01/decypharr>",
		CreationDate:       t.AddedOn.Unix(),
		DlLimit:            -1,
		UpLimit:            -1,
		DlSpeed:            t.Speed,
		UpSpeed:            t.Speed,
		TotalSize:          t.Size,
		TotalUploaded:      t.Bytes,
		TotalDownloaded:    t.Bytes,
		LastSeen:           time.Now().Unix(),
		NbConnectionsLimit: fakeSwarmSize,
		Peers:              0,
		PeersTotal:         fakePeersTotal,
		SeedingTime:        1,
		Seeds:              fakeSwarmSize,
		ShareRatio:         fakeSwarmSize,
	}
}

func (q *QBit) setTorrentTags(t *storage.Entry, tags []string) {
	for _, tag := range tags {
		if tag != "" && !slices.Contains(t.Tags, tag) {
			t.Tags = append(t.Tags, tag)
		}
	}
	q.addTags(tags)
	_ = q.manager.Queue().Update(t)
}

func (q *QBit) removeTorrentTags(t *storage.Entry, tags []string) {
	t.Tags = utils.RemoveItem(t.Tags, tags...)
	q.mu.Lock()
	q.tags = utils.RemoveItem(q.tags, tags...)
	q.mu.Unlock()
	_ = q.manager.Queue().Update(t)
}

func (q *QBit) addTags(tags []string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, tag := range tags {
		if tag != "" && !slices.Contains(q.tags, tag) {
			q.tags = append(q.tags, tag)
		}
	}
}

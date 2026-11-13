package debridlink

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/sirrobot01/decypharr/pkg/debrid/common"
	"github.com/sirrobot01/decypharr/pkg/debrid/types"
)

func (dl *DebridLink) lookupPage(ctx context.Context, page int) ([]_torrentInfo, error) {
	var res torrentInfo
	params := map[string]string{"page": strconv.Itoa(page), "perPage": strconv.Itoa(pageSize)}
	status, err := dl.doGet(ctx, "/seedbox/list", params, &res)
	if err != nil {
		return nil, err
	}
	if !common.IsSuccess(status) {
		return nil, fmt.Errorf("debridlink API error: Status: %d", status)
	}
	if !res.Success || res.Value == nil {
		return nil, fmt.Errorf("error getting torrents")
	}
	return *res.Value, nil
}

func (dl *DebridLink) Lookup(ctx context.Context, infohash string) (types.LookupResult, *types.Torrent) {
	for page := 0; ; page++ {
		entries, err := dl.lookupPage(ctx, page)
		if err != nil || ctx.Err() != nil {
			return types.LookupUnknown, nil
		}
		if len(entries) == 0 {
			return types.LookupAbsent, nil
		}
		for _, e := range entries {
			if strings.EqualFold(e.HashString, infohash) {
				return types.LookupFound, dl.lookupTorrent(e)
			}
		}
	}
}

func (dl *DebridLink) lookupTorrent(e _torrentInfo) *types.Torrent {
	status := types.TorrentStatusDownloading
	if e.Status == seedboxDone {
		status = types.TorrentStatusDownloaded
	}
	return &types.Torrent{
		ID:               e.ID,
		Name:             e.Name,
		Bytes:            e.TotalSize,
		Status:           status,
		Filename:         e.Name,
		OriginalFilename: e.Name,
		InfoHash:         e.HashString,
		Files:            make(map[string]types.File),
		Debrid:           dl.config.Name,
		Added:            time.Unix(e.Created, 0),
	}
}

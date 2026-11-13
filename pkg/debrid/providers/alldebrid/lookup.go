package alldebrid

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/sirrobot01/decypharr/pkg/debrid/common"
	"github.com/sirrobot01/decypharr/pkg/debrid/types"
)

func (ad *AllDebrid) Lookup(ctx context.Context, infohash string) (types.LookupResult, *types.Torrent) {
	var res TorrentsListResponse
	status, err := ad.doRequest(ctx, ad.client, "/magnet/status", nil, &res)
	if err != nil || ctx.Err() != nil || !common.IsSuccess(status) || res.Status != statusSuccess {
		return types.LookupUnknown, nil
	}
	for _, magnet := range res.Data.Magnets {
		if strings.EqualFold(magnet.Hash, infohash) {
			return types.LookupFound, ad.lookupTorrent(magnet)
		}
	}
	return types.LookupUnknown, nil
}

func (ad *AllDebrid) lookupTorrent(magnet magnetInfo) *types.Torrent {
	return &types.Torrent{
		ID:               strconv.Itoa(magnet.ID),
		Name:             magnet.Filename,
		Bytes:            magnet.Size,
		Status:           getAlldebridStatus(magnet.StatusCode),
		Filename:         magnet.Filename,
		OriginalFilename: magnet.Filename,
		Files:            make(map[string]types.File),
		InfoHash:         magnet.Hash,
		Debrid:           ad.config.Name,
		Added:            time.Unix(magnet.CompletionDate, 0),
	}
}

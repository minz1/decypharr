package torbox

import (
	"context"
	"strings"

	"github.com/sirrobot01/decypharr/pkg/debrid/common"
	"github.com/sirrobot01/decypharr/pkg/debrid/types"
)

func (tb *Torbox) Lookup(ctx context.Context, infohash string) (types.LookupResult, *types.Torrent) {
	torrents, err := tb.GetTorrents(ctx)
	if err != nil {
		return types.LookupUnknown, nil
	}
	for _, t := range torrents {
		if strings.EqualFold(t.InfoHash, infohash) {
			return types.LookupFound, t
		}
	}
	queued, ok := tb.queuedHashes(ctx)
	if !ok {
		return types.LookupUnknown, nil
	}
	for _, hash := range queued {
		if strings.EqualFold(hash, infohash) {
			return types.LookupUnknown, nil
		}
	}
	return types.LookupAbsent, nil
}

func (tb *Torbox) queuedHashes(ctx context.Context) ([]string, bool) {
	var queued APIResponse[[]struct {
		Hash string `json:"hash"`
	}]
	params := queuedParams()
	status, err := tb.doGet(ctx, "/api/queued/getqueued", params, &queued)
	if err != nil || ctx.Err() != nil || !common.IsSuccess(status) || !queued.Success {
		return nil, false
	}
	hashes := make([]string, 0)
	if queued.Data != nil {
		for _, item := range *queued.Data {
			hashes = append(hashes, item.Hash)
		}
	}
	return hashes, true
}

const (
	queryBypassCache = "bypass_cache"
	queueTypeTorrent = "torrent"
)

func queuedParams() map[string]string {
	return map[string]string{"type": queueTypeTorrent, queryBypassCache: queryTrue}
}

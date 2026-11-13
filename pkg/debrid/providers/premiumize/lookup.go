package premiumize

import (
	"cmp"
	"context"
	"strings"
	"time"

	"github.com/sirrobot01/decypharr/internal/utils"
	"github.com/sirrobot01/decypharr/pkg/debrid/types"
)

func (pm *Premiumize) Lookup(ctx context.Context, infohash string) (types.LookupResult, *types.Torrent) {
	transfers, err := pm.listTransfers(ctx)
	if err != nil || ctx.Err() != nil {
		return types.LookupUnknown, nil
	}
	for _, tr := range transfers {
		hash := utils.ExtractInfoHash(tr.Src)
		if hash != "" && strings.EqualFold(hash, infohash) {
			return types.LookupFound, pm.lookupTorrent(tr, hash)
		}
	}
	return types.LookupUnknown, nil
}

func (pm *Premiumize) lookupTorrent(tr premiumizeTransfer, hash string) *types.Torrent {
	name := cmp.Or(tr.Name, tr.ID)
	added := time.Time{}
	if tr.Created.Unix != 0 {
		added = time.Unix(tr.Created.Unix, 0)
	}
	return &types.Torrent{
		ID:               tr.ID,
		InfoHash:         hash,
		Name:             name,
		Filename:         name,
		OriginalFilename: name,
		Files:            make(map[string]types.File),
		Status:           mapStatus(tr.Status),
		Progress:         normalizeProgress(tr.Progress),
		Debrid:           pm.config.Name,
		Added:            added,
	}
}

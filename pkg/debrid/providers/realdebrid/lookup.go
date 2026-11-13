package realdebrid

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/sirrobot01/decypharr/internal/request"
	"github.com/sirrobot01/decypharr/pkg/debrid/types"
)

const lookupPageSize = 1000

type lookupPage struct {
	entries []TorrentsResponse
	total   string
}

func (r *RealDebrid) lookupFetch(ctx context.Context, page int) (lookupPage, error) {
	u, err := url.Parse(r.Host + "/torrents")
	if err != nil {
		return lookupPage{}, err
	}
	q := u.Query()
	q.Set("limit", strconv.Itoa(lookupPageSize))
	q.Set("page", strconv.Itoa(page))
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return lookupPage{}, err
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return lookupPage{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent {
		return lookupPage{}, nil
	}
	if resp.StatusCode != http.StatusOK {
		return lookupPage{}, fmt.Errorf("realdebrid API error: %d", resp.StatusCode)
	}
	var entries []TorrentsResponse
	if decodeErr := request.DecodeJSON(resp, &entries); decodeErr != nil {
		return lookupPage{}, decodeErr
	}
	return lookupPage{entries: entries, total: resp.Header.Get("X-Total-Count")}, nil
}

func (r *RealDebrid) Lookup(ctx context.Context, infohash string) (types.LookupResult, *types.Torrent) {
	var firstTotal, lastTotal string
	for page := 1; ; page++ {
		got, err := r.lookupFetch(ctx, page)
		if err != nil || ctx.Err() != nil {
			return types.LookupUnknown, nil
		}
		if len(got.entries) == 0 {
			break
		}
		if page == 1 {
			firstTotal = got.total
		}
		lastTotal = got.total
		for _, e := range got.entries {
			if strings.EqualFold(e.Hash, infohash) {
				return types.LookupFound, r.lookupTorrent(e)
			}
		}
	}
	if firstTotal != lastTotal {
		return types.LookupUnknown, nil
	}
	return types.LookupAbsent, nil
}

func (r *RealDebrid) lookupTorrent(e TorrentsResponse) *types.Torrent {
	return &types.Torrent{
		ID:               e.ID,
		Name:             e.Filename,
		Bytes:            e.Bytes,
		Progress:         e.Progress,
		Status:           getStatus(e.Status),
		Filename:         e.Filename,
		OriginalFilename: e.Filename,
		Links:            e.Links,
		Files:            make(map[string]types.File),
		InfoHash:         e.Hash,
		Debrid:           r.config.Name,
		Added:            e.Added,
	}
}

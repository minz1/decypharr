package realdebrid

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/sirrobot01/decypharr/internal/request"
	"github.com/sirrobot01/decypharr/pkg/debrid/types"
)

const lookupPageSize = 1000

type torrentsListing struct {
	status     int
	rows       []TorrentsResponse
	totals     []string
	redirected bool
}

func (r *RealDebrid) listTorrents(ctx context.Context, q url.Values) (torrentsListing, error) {
	u, err := url.Parse(r.Host + "/torrents")
	if err != nil {
		return torrentsListing{}, err
	}
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return torrentsListing{}, err
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return torrentsListing{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return torrentsListing{status: resp.StatusCode}, nil
	}
	var rows []TorrentsResponse
	if decodeErr := request.DecodeJSON(resp, &rows); decodeErr != nil {
		return torrentsListing{}, decodeErr
	}
	return torrentsListing{
		status:     resp.StatusCode,
		rows:       rows,
		totals:     resp.Header.Values("X-Total-Count"),
		redirected: resp.Request.URL.String() != u.String(),
	}, nil
}

func (r *RealDebrid) Lookup(ctx context.Context, infohash string) (types.LookupResult, *types.Torrent) {
	var firstTotal, lastTotal string
	for page := 1; ; page++ {
		got, err := r.listTorrents(ctx, url.Values{
			"limit": {strconv.Itoa(lookupPageSize)},
			"page":  {strconv.Itoa(page)},
		})
		if err != nil || ctx.Err() != nil {
			return types.LookupUnknown, nil
		}
		if got.status == http.StatusNoContent {
			break
		}
		if got.status != http.StatusOK {
			return types.LookupUnknown, nil
		}
		if len(got.rows) == 0 {
			break
		}
		total := ""
		if len(got.totals) > 0 {
			total = got.totals[0]
		}
		if page == 1 {
			firstTotal = total
		}
		lastTotal = total
		for _, e := range got.rows {
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

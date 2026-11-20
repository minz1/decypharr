package realdebrid

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/sirrobot01/decypharr/internal/customerror"
	"github.com/sirrobot01/decypharr/internal/request"
	"github.com/sirrobot01/decypharr/pkg/debrid/types"
)

const (
	lookupPageSize = 1000
	limitParam     = "limit"
)

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
			limitParam: {strconv.Itoa(lookupPageSize)},
			"page":     {strconv.Itoa(page)},
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

type listingVerdict int

const (
	listingGone listingVerdict = iota
	listingListed
	listingUnproven
	listingUnreadable
)

const confirmListingLimit = 5000

func (r *RealDebrid) confirmGone(ctx context.Context, id string) (listingVerdict, string) {
	got, err := r.listTorrents(ctx, url.Values{
		"offset":   {"0"},
		limitParam: {strconv.Itoa(confirmListingLimit)},
	})
	if err != nil {
		return listingUnreadable, err.Error()
	}
	for _, row := range got.rows {
		if row.ID == id {
			return listingListed, ""
		}
	}
	switch {
	case got.status == http.StatusNoContent:
		return listingUnproven, "no content"
	case got.status != http.StatusOK:
		return listingUnreadable, "status " + strconv.Itoa(got.status)
	case got.redirected:
		return listingUnproven, "redirected"
	case got.rows == nil:
		return listingUnproven, "no rows"
	}
	seen := make(map[string]struct{}, len(got.rows))
	for _, row := range got.rows {
		if _, dup := seen[row.ID]; row.ID == "" || dup {
			return listingUnproven, "empty or repeated id"
		}
		seen[row.ID] = struct{}{}
	}
	if len(got.totals) != 1 {
		return listingUnproven, "total count header missing or repeated"
	}
	n, convErr := strconv.Atoi(got.totals[0])
	if convErr != nil || n < 0 || strconv.Itoa(n) != got.totals[0] {
		return listingUnproven, "total count not canonical"
	}
	if len(got.rows) != n {
		return listingUnproven, "total count differs from the rows"
	}
	if n >= confirmListingLimit {
		return listingUnproven, "listing may be truncated"
	}
	return listingGone, ""
}

func (r *RealDebrid) pollNotFound(ctx context.Context, t *types.Torrent) error {
	why := "not settled"
	if !t.Added.IsZero() && time.Since(t.Added) >= types.ListingSettle {
		verdict, detail := r.confirmGone(ctx, t.ID)
		switch verdict {
		case listingGone:
			return fmt.Errorf("torrent %s: not in the complete listing: %w", t.Name, customerror.ErrTorrentNotFound)
		case listingListed:
			why = "listed"
		case listingUnproven:
			why = "listing unproven: " + detail
		case listingUnreadable:
			why = "listing unreadable: " + detail
		}
	}
	return fmt.Errorf(
		"realdebrid: /torrents/info/%s answered 404, %s: %w", t.ID, why, customerror.ErrProviderStatusTimeout,
	)
}

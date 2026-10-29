package repair

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/customerror"
	"github.com/sirrobot01/decypharr/pkg/debrid/account"
	debrid "github.com/sirrobot01/decypharr/pkg/debrid/common"
	debridTypes "github.com/sirrobot01/decypharr/pkg/debrid/types"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

const confirmHash = "abcdefabcdefabcdefabcdefabcdefabcdefabcd"

type linkResult struct {
	link debridTypes.DownloadLink
	err  error
}

// linkClient serves queued GetDownloadLink results per file name and counts calls.
type linkClient struct {
	debrid.Client

	mu       sync.Mutex
	links    map[string][]linkResult
	fetches  int
	checks   int
	accounts *account.Manager
}

func (c *linkClient) Config() config.Debrid { return config.Debrid{Name: "rd", Provider: "realdebrid"} }

func (c *linkClient) AccountManager() *account.Manager { return c.accounts }

func (c *linkClient) GetDownloadLink(
	_ context.Context,
	_ string,
	file *debridTypes.File,
) (debridTypes.DownloadLink, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.fetches++
	queue := c.links[file.Name]
	if len(queue) == 0 {
		return freshLink(file.Name), nil
	}
	next := queue[0]
	if len(queue) > 1 {
		c.links[file.Name] = queue[1:]
	}
	return next.link, next.err
}

func (c *linkClient) CheckFile(context.Context, string, string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.checks++
	return nil
}

func (c *linkClient) SupportsCheck() bool { return true }

func newLinkClient(links map[string][]linkResult) *linkClient {
	return &linkClient{
		links:    links,
		accounts: account.NewManager(config.Debrid{}, debridTypes.ProviderOptions{}, nil),
	}
}

func freshLink(name string) debridTypes.DownloadLink {
	return debridTypes.DownloadLink{
		Filename: name, Link: "https://rd/" + name, DownloadLink: "https://cdn/" + name, Generated: time.Now(),
	}
}

func aged(link debridTypes.DownloadLink) debridTypes.DownloadLink {
	link.Generated = time.Now().Add(-time.Hour)
	return link
}

// clientBackend hands out one client, or none, and records ClearBad calls.
type clientBackend struct {
	Backend

	client  debrid.Client
	mu      sync.Mutex
	cleared []string
}

func (b *clientBackend) ProviderClient(string) debrid.Client { return b.client }

func (b *clientBackend) ClearBad(_ context.Context, infohash string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.cleared = append(b.cleared, infohash)
	return nil
}

func confirmEntry(names ...string) *storage.Entry {
	entry := &storage.Entry{
		InfoHash: confirmHash, Name: "release", Protocol: config.ProtocolTorrent, ActiveProvider: "rd",
		Files: map[string]*storage.File{},
		Providers: map[string]*storage.ProviderEntry{
			"rd": {Provider: "rd", ID: "rd-id", Status: debridTypes.TorrentStatusDownloaded,
				Files: map[string]*storage.ProviderFile{}},
		},
	}
	for _, name := range names {
		entry.Files[name] = &storage.File{Name: name, InfoHash: confirmHash}
		entry.Providers["rd"].Files[name] = &storage.ProviderFile{Link: "https://rd/" + name}
	}
	return entry
}

func confirmService(t *testing.T, client debrid.Client) *Service {
	t.Helper()
	return New(Dependencies{Storage: newRepairTestStorage(t), Backend: &clientBackend{client: client}})
}

func TestConfirmBroken(t *testing.T) {
	t.Parallel()
	statusErr := func(status int) error { return customerror.NewProviderStatusError(errors.New("x"), status) }
	fail := func(err error) []linkResult { return []linkResult{{err: err}} }
	for _, tc := range []struct {
		name  string
		links map[string][]linkResult
		nzb   bool
		none  bool
		want  bool
	}{
		{name: "both links fine", want: false},
		{name: "hoster unavailable", links: map[string][]linkResult{"b.mkv": fail(customerror.ErrHosterUnavailable)}, want: true},
		{name: "empty link", links: map[string][]linkResult{"b.mkv": {{}}}, want: true},
		{name: "torrent not found", links: map[string][]linkResult{
			"b.mkv": fail(fmt.Errorf("x: %w", customerror.ErrTorrentNotFound)),
		}, want: true},
		{name: "forbidden is inconclusive", links: map[string][]linkResult{"a.mkv": fail(statusErr(403))}, want: false},
		{name: "unauthorized and outage", links: map[string][]linkResult{
			"a.mkv": fail(statusErr(401)), "b.mkv": fail(statusErr(503)),
		}, want: false},
		{name: "outage then hoster unavailable", links: map[string][]linkResult{
			"a.mkv": fail(statusErr(503)), "b.mkv": fail(customerror.ErrHosterUnavailable),
		}, want: true},
		{name: "no client is inconclusive", none: true, want: false},
		{name: "nzb entry", nzb: true, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var client debrid.Client
			if !tc.none {
				client = newLinkClient(tc.links)
			}
			entry := confirmEntry("a.mkv", "b.mkv")
			if tc.nzb {
				entry.Protocol = config.ProtocolNZB
			}
			if got := confirmService(t, client).ConfirmBroken(t.Context(), entry); got != tc.want {
				t.Fatalf("ConfirmBroken = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestConfirmBrokenSkipsDeletedFiles(t *testing.T) {
	t.Parallel()
	entry := confirmEntry("a.mkv", "gone.mkv")
	entry.Files["gone.mkv"].Deleted = true
	client := newLinkClient(map[string][]linkResult{"gone.mkv": {{err: customerror.ErrHosterUnavailable}}})
	if confirmService(t, client).ConfirmBroken(t.Context(), entry) {
		t.Fatal("ConfirmBroken = true, want a deleted file left unprobed")
	}
}

// An old cached link never decides: it is dropped and fetched again.
func TestFreshProbeRefetchesCachedLink(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		second linkResult
		want   bool
	}{
		{name: "dead on refetch", second: linkResult{err: customerror.ErrHosterUnavailable}, want: true},
		{name: "fine on refetch", second: linkResult{link: freshLink("a.mkv")}, want: false},
		{name: "still old on refetch", second: linkResult{link: aged(freshLink("a.mkv"))}, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			client := newLinkClient(map[string][]linkResult{"a.mkv": {{link: aged(freshLink("a.mkv"))}, tc.second}})
			if got := confirmService(t, client).ConfirmBroken(t.Context(), confirmEntry("a.mkv")); got != tc.want {
				t.Fatalf("ConfirmBroken = %v, want %v", got, tc.want)
			}
			if client.fetches != 2 {
				t.Fatalf("GetDownloadLink calls = %d, want 2", client.fetches)
			}
		})
	}
}

func TestProbeUsesFreshUnrestrictForMarkedEntries(t *testing.T) {
	t.Parallel()
	for _, bad := range []bool{true, false} {
		client := newLinkClient(map[string][]linkResult{"a.mkv": {{link: aged(freshLink("a.mkv"))}}})
		service := confirmService(t, client)
		entry := confirmEntry("a.mkv")
		entry.Bad = bad
		result := service.probeTorrentFile(t.Context(), entry, entry.Files["a.mkv"], "a.mkv",
			fileResult{name: "a.mkv", infoHash: confirmHash}, RunOptions{})
		if bad && (client.fetches != 2 || client.checks != 0 || result.healthy) {
			t.Fatalf("marked: fetches = %d, checks = %d, result = %+v; want a refetch and no CheckFile",
				client.fetches, client.checks, result)
		}
		if !bad && (client.fetches != 0 || client.checks != 1 || !result.healthy) {
			t.Fatalf("unmarked: fetches = %d, checks = %d, result = %+v; want CheckFile only",
				client.fetches, client.checks, result)
		}
	}
}

func TestUnrestrictProbeTorrentNotFoundIsBroken(t *testing.T) {
	t.Parallel()
	client := newLinkClient(map[string][]linkResult{
		"a.mkv": {{err: fmt.Errorf("x: %w", customerror.ErrTorrentNotFound)}},
	})
	entry := confirmEntry("a.mkv")
	result := confirmService(t, client).probeTorrentFileByUnrestrict(t.Context(), entry, entry.Files["a.mkv"], "a.mkv",
		fileResult{name: "a.mkv", infoHash: confirmHash}, client, false)
	if !result.broken || result.reason != "torrent_not_found" {
		t.Fatalf("result = %+v, want broken with reason torrent_not_found", result)
	}
}

// cachingClient serves links through a real account cache, as providers do, over linkClient's queue.
type cachingClient struct {
	*linkClient
}

func (c cachingClient) GetDownloadLink(
	ctx context.Context,
	id string,
	file *debridTypes.File,
) (debridTypes.DownloadLink, error) {
	return c.accounts.GetDownloadLink(
		ctx,
		id,
		file,
		func(ctx context.Context, acc *account.Account, id string, file *debridTypes.File) (debridTypes.DownloadLink, error) {
			link, err := c.linkClient.GetDownloadLink(ctx, id, file)
			link.Token = acc.Token
			return link, err
		},
	)
}

// With a real cache, the old link must be evicted, or the refetch returns it again and a dead file looks unclear.
func TestFreshProbeEvictsFromTheAccountCache(t *testing.T) {
	t.Parallel()
	inner := newLinkClient(map[string][]linkResult{
		"a.mkv": {{link: aged(freshLink("a.mkv"))}, {err: customerror.ErrHosterUnavailable}},
	})
	inner.accounts = account.NewManager(
		config.Debrid{Name: "torbox", DownloadAPIKeys: []string{"download-token"}}, debridTypes.ProviderOptions{}, nil)
	client := cachingClient{inner}
	entry := confirmEntry("a.mkv")
	if _, err := client.GetDownloadLink(
		t.Context(),
		"rd-id",
		&debridTypes.File{Name: "a.mkv", Link: "https://rd/a.mkv"},
	); err != nil {
		t.Fatal(err)
	}
	if !confirmService(t, client).ConfirmBroken(t.Context(), entry) {
		t.Fatal("ConfirmBroken = false, want the cached old link evicted and the dead refetch seen")
	}
	if inner.fetches != 2 {
		t.Fatalf("provider fetches = %d, want 2 (warm-up, then the refetch after eviction)", inner.fetches)
	}
}

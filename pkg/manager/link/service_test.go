package link_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/puzpuzpuz/xsync/v4"
	"github.com/rs/zerolog"

	debrid "github.com/sirrobot01/decypharr/pkg/debrid/common"
	"github.com/sirrobot01/decypharr/pkg/debrid/types"
	"github.com/sirrobot01/decypharr/pkg/manager/link"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

// linkClient serves a download link for any file that carries a locator.
type linkClient struct {
	debrid.Client

	url string
}

func (c *linkClient) GetDownloadLink(_ context.Context, _ string, file *types.File) (types.DownloadLink, error) {
	return types.DownloadLink{
		Debrid:       "fake",
		Filename:     file.Name,
		Link:         file.Link,
		ID:           file.ID,
		DownloadLink: c.url + "/" + file.ID,
	}, nil
}

func testEntry(withLocators bool) *storage.Entry {
	entry := &storage.Entry{
		InfoHash:       "hash",
		Name:           "Entry",
		ActiveProvider: "fake",
		Files:          map[string]*storage.File{},
		Providers: map[string]*storage.ProviderEntry{
			"fake": {Provider: "fake", ID: "torrent", Files: map[string]*storage.ProviderFile{}},
		},
	}
	for _, name := range []string{"a.mkv", "b.mkv"} {
		entry.Files[name] = &storage.File{Name: name, Size: 1}
		file := &storage.ProviderFile{}
		if withLocators {
			file.ID = "id-" + name
		}
		entry.Providers["fake"].Files[name] = file
	}
	return entry
}

// A placement refresh resolves the link from the refreshed entry and leaves
// the caller's entry untouched: streams share that pointer and read it
// concurrently.
func TestRefreshDoesNotOverwriteSharedEntry(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	clients := xsync.NewMap[string, debrid.Client]()
	clients.Store("fake", &linkClient{url: server.URL})
	refresher := func(string) (*storage.Entry, error) { return testEntry(true), nil }
	service := link.New(clients, refresher, nil, nil, server.Client(), 0, nil, zerolog.Nop())

	shared := testEntry(false)
	var wg sync.WaitGroup
	for _, name := range []string{"a.mkv", "b.mkv"} {
		wg.Go(func() {
			dl, err := service.GetLink(t.Context(), shared, name)
			if err != nil {
				t.Errorf("GetLink(%s): %v", name, err)
				return
			}
			if dl.ID != "id-"+name {
				t.Errorf("GetLink(%s) used locator %q, want the refreshed one", name, dl.ID)
			}
		})
		wg.Go(func() {
			_ = shared.Name
			_ = shared.Providers["fake"].Files[name].ID
		})
	}
	wg.Wait()

	for name, file := range shared.Providers["fake"].Files {
		if file.ID != "" {
			t.Fatalf("shared entry file %s was overwritten with locator %q", name, file.ID)
		}
	}
}

// headServer answers HEAD on every link with first, then 200 OK, and
// counts the requests.
func headServer(t *testing.T, first int) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(first)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)
	return server, &calls
}

func (c *linkClient) DeleteLink(types.DownloadLink) error { return nil }

func newHeadService(server *httptest.Server) *link.Service {
	clients := xsync.NewMap[string, debrid.Client]()
	clients.Store("fake", &linkClient{url: server.URL})
	return link.New(clients, nil, nil, nil, server.Client(), 0, nil, zerolog.Nop())
}

// A bare 400 or 404 on the validation HEAD comes from the CDN: the link
// rotated or isn't served yet (TorBox answers 400 right after an instant
// "downloaded"). As on the streaming path, fetch the link again instead of
// failing the file for good.
func TestGetLinkRefetchesOnCDNReject(t *testing.T) {
	t.Parallel()
	for _, status := range []int{http.StatusBadRequest, http.StatusNotFound} {
		server, _ := headServer(t, status)
		service := newHeadService(server)
		if _, err := service.GetLink(t.Context(), testEntry(true), "a.mkv"); err != nil {
			t.Errorf("GetLink after a CDN %d: %v", status, err)
		}
	}
}

// A failed validation is remembered only briefly: repeat reads inside the
// window don't hit the CDN again, and after it the link is checked afresh,
// so one bad HEAD cannot fail the file until a restart.
func TestGetLinkRechecksFailureAfterTTL(t *testing.T) {
	t.Parallel()
	server, heads := headServer(t, http.StatusMethodNotAllowed)
	service := newHeadService(server)
	entry := testEntry(true)

	if _, err := service.GetLink(t.Context(), entry, "a.mkv"); err == nil {
		t.Fatal("first GetLink passed a 405 HEAD")
	}
	if _, err := service.GetLink(t.Context(), entry, "a.mkv"); err == nil || heads.Load() != 1 {
		t.Fatalf("repeat inside the window: err=%v, HEADs=%d, want the cached failure and 1 HEAD", err, heads.Load())
	}
	link.SetFailureTTL(service, 0)
	if _, err := service.GetLink(t.Context(), entry, "a.mkv"); err != nil {
		t.Fatalf("GetLink after the window still fails though the link recovered: %v", err)
	}
}

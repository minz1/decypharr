package link_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
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

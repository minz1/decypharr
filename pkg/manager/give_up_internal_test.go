package manager

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/pkg/arr"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

// queueArr is a Sonarr whose queue lists the given download IDs, or refuses the token when broken.
type queueArr struct {
	downloadIDs []string
	broken      bool
	requests    atomic.Int32
}

func (a *queueArr) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/api/v3/queue" {
		http.NotFound(w, r)
		return
	}
	a.requests.Add(1)
	if a.broken {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	records := make([]arr.QueueSchema, len(a.downloadIDs))
	for i, id := range a.downloadIDs {
		records[i] = arr.QueueSchema{ID: i + 1, DownloadID: id}
	}
	_ = json.NewEncoder(w).Encode(arr.QueueResponseScheme{TotalRecords: len(records), Records: records})
}

func newGiveUpTestManager(t *testing.T, fakeArr *queueArr, recovery ArrRecovery) (*Manager, *storage.Entry) {
	t.Helper()
	server := httptest.NewServer(fakeArr)
	t.Cleanup(server.Close)
	m, entry := newDroppedTestManager(t, recovery)
	m.arr = arr.New(config.NewStore(&config.Config{Arrs: []config.Arr{
		{Name: "sonarr", Host: server.URL, Token: "x"},
	}}), nil, zerolog.Nop())
	entry.Protocol = config.ProtocolTorrent
	return m, entry
}

func TestFailGrabIfQueued(t *testing.T) {
	t.Parallel()
	upper := strings.ToUpper(droppedHash)
	for name, tc := range map[string]struct {
		queued       []string
		broken       bool
		edit         func(*storage.Entry)
		wantFail     bool
		wantRequests int32
	}{
		"queued":           {queued: []string{"OTHER", upper}, wantFail: true, wantRequests: 1},
		"not queued":       {queued: []string{"OTHER"}, wantRequests: 1},
		"queue unreadable": {broken: true, wantRequests: 1},
		"manual torrent":   {queued: []string{upper}, edit: func(e *storage.Entry) { e.Manual = true }},
		"nzb":              {queued: []string{upper}, edit: func(e *storage.Entry) { e.Protocol = config.ProtocolNZB }},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			fakeArr := &queueArr{downloadIDs: tc.queued, broken: tc.broken}
			fake := &fakeArrRecovery{}
			m, entry := newGiveUpTestManager(t, fakeArr, fake)
			if tc.edit != nil {
				tc.edit(entry)
			}
			m.failGrabIfQueued(t.Context(), entry)
			want := [3]string{"sonarr", upper, droppedHash}
			if got := len(fake.fails) == 1 && fake.fails[0] == want; got != tc.wantFail {
				t.Fatalf("FailDownload calls = %v, want failed = %v", fake.fails, tc.wantFail)
			}
			if got := fakeArr.requests.Load(); got != tc.wantRequests {
				t.Fatalf("queue requests = %d, want %d", got, tc.wantRequests)
			}
		})
	}
}

func TestGiveUpOnEntryMarksBadAndFailsGrab(t *testing.T) {
	t.Parallel()
	fake := &fakeArrRecovery{}
	m, entry := newGiveUpTestManager(t, &queueArr{downloadIDs: []string{strings.ToUpper(droppedHash)}}, fake)
	m = withTestConfig(t, m)
	m.entry = newEntryCache(func(string) bool { return false }, nil, nil)

	m.giveUpOnEntry(entry)

	stored, err := m.storage.Get(droppedHash)
	if err != nil || stored == nil || !stored.Bad {
		t.Fatalf("stored entry = %+v, %v; want Bad", stored, err)
	}
	waitForFails(t, fake, 1)
}

// When every debrid refuses the torrent, the fixer gives up through giveUpOnEntry, failing the queued grab.
func TestFixTorrentGivingUpFailsQueuedGrab(t *testing.T) {
	t.Parallel()
	fake := &fakeArrRecovery{}
	m, entry := newGiveUpTestManager(t, &queueArr{downloadIDs: []string{strings.ToUpper(droppedHash)}}, fake)
	m = withTestConfig(t, m)
	m.entry = newEntryCache(func(string) bool { return false }, nil, nil)
	fixer := NewFixer(m)
	fixer.providerOrder = nil

	if result, err := fixer.FixTorrent(t.Context(), entry, false); err == nil || result.Success {
		t.Fatalf("FixTorrent = %+v, %v; want a failure", result, err)
	}
	waitForFails(t, fake, 1)
}

func waitForFails(t *testing.T, fake *fakeArrRecovery, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		fake.failMu.Lock()
		calls := len(fake.fails)
		fake.failMu.Unlock()
		if calls == want {
			return
		}
		if calls > want || time.Now().After(deadline) {
			t.Fatalf("FailDownload calls = %d, want %d", calls, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

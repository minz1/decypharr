package arr

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
)

func TestResolveQueueActionFailedDownloadLeavesArrHandledItems(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		state string
		want  QueueAction
	}{
		{name: "failed pending", state: "failedPending", want: QueueActionNone},
		{name: "failed", state: "failed", want: QueueActionNone},
		{name: "not grabbed by the arr", state: "downloading", want: QueueActionBlocklistResearch},
		{name: "arr without tracked state", state: "", want: QueueActionBlocklistResearch},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			item := QueueSchema{Status: "failed", TrackedDownloadState: test.state}
			if got := resolveQueueAction(item, config.DefaultQueueCleanupRules()); got != test.want {
				t.Fatalf("resolveQueueAction() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestResolveQueueActionCustomRuleStillMatchesArrHandledItems(t *testing.T) {
	t.Parallel()

	item := QueueSchema{Status: "failed", TrackedDownloadState: "failed"}
	item.StatusMessages = append(item.StatusMessages, struct {
		Title    string   `json:"title"`
		Messages []string `json:"messages"`
	}{Title: "Failed download detected"})
	rules := []config.QueueCleanupRule{{Match: "failed download", Action: string(QueueActionBlocklist)}}

	if got := resolveQueueAction(item, rules); got != QueueActionBlocklist {
		t.Fatalf("resolveQueueAction() = %q, want %q", got, QueueActionBlocklist)
	}
}

func TestResolveQueueActionCatalogLeavesArrHandledItems(t *testing.T) {
	t.Parallel()

	for _, title := range []string{"Unable to parse download", "Title mismatch; automatic import is not possible"} {
		t.Run(title, func(t *testing.T) {
			t.Parallel()
			item := QueueSchema{Status: "failed", TrackedDownloadState: "FailedPending"}
			item.StatusMessages = append(item.StatusMessages, struct {
				Title    string   `json:"title"`
				Messages []string `json:"messages"`
			}{Title: title})
			if got := resolveQueueAction(item, config.DefaultQueueCleanupRules()); got != QueueActionNone {
				t.Fatalf("resolveQueueAction() = %q, want none", got)
			}
		})
	}
}

type cleanupServer struct {
	*httptest.Server

	records atomic.Pointer[[]QueueSchema]
	fail    atomic.Bool
	succeed atomic.Bool
	lookups atomic.Int64
}

func newCleanupServer(t *testing.T, records []QueueSchema) *cleanupServer {
	t.Helper()
	fake := &cleanupServer{}
	fake.records.Store(&records)
	fake.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v3/queue":
			if fake.fail.Load() {
				http.Error(w, "no", http.StatusUnauthorized)
				return
			}
			current := *fake.records.Load()
			body, _ := json.Marshal(QueueResponseScheme{
				Page: 1, PageSize: 200, TotalRecords: len(current), Records: current,
			})
			_, _ = w.Write(body)
		case "/api/v3/manualimport":
			fake.lookups.Add(1)
			if fake.succeed.Load() {
				_, _ = w.Write([]byte(`[{"path":"/d/A.mkv","series":{"id":7},"quality":{"quality":{"id":3}}}]`))
				return
			}
			_, _ = w.Write([]byte(`[]`))
		case "/api/v3/command":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":1,"status":"queued"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(fake.Close)
	return fake
}

func stuckRecords(title string) []QueueSchema {
	message := struct {
		Title    string   `json:"title"`
		Messages []string `json:"messages"`
	}{Title: title}
	record := QueueSchema{
		Status:                "completed",
		TrackedDownloadStatus: "warning",
		TrackedDownloadState:  "importBlocked",
		DownloadID:            "hash",
	}
	record.StatusMessages = append(record.StatusMessages, message)
	first, second := record, record
	first.ID, second.ID = 1, 2
	return []QueueSchema{first, second}
}

const matchedByIDTitle = "One or more episodes was matched to series by ID"

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func cleanupTestService(clock *fakeClock, instances map[string]*cleanupServer) *Service {
	cfg := config.NewStore(&config.Config{
		QueueCleanup: config.QueueCleanup{Rules: config.DefaultQueueCleanupRules()},
	})
	service := New(cfg, nil, zerolog.Nop())
	service.now = clock.Now
	for name, fake := range instances {
		service.arrs[name] = Arr{Name: name, Host: fake.URL, Token: "secret", Type: Sonarr}
	}
	return service
}

func runCleanup(t *testing.T, service *Service, name string, times int) {
	t.Helper()
	for range times {
		if err := service.CleanupQueue(t.Context(), name); err != nil {
			t.Fatalf("CleanupQueue() = %v", err)
		}
	}
}

func wantLookups(t *testing.T, fake *cleanupServer, want int64) {
	t.Helper()
	if got := fake.lookups.Load(); got != want {
		t.Fatalf("manual import lookups = %d, want %d", got, want)
	}
}

func TestCleanupQueueDedupesByDownloadID(t *testing.T) {
	t.Parallel()

	fake := newCleanupServer(t, stuckRecords(matchedByIDTitle))
	service := cleanupTestService(&fakeClock{now: time.Unix(1000, 0)}, map[string]*cleanupServer{"arr": fake})

	runCleanup(t, service, "arr", 1)
	wantLookups(t, fake, 1)
}

func TestCleanupQueueBacksOffAfterMaxAttempts(t *testing.T) {
	t.Parallel()

	fake := newCleanupServer(t, stuckRecords(matchedByIDTitle))
	clock := &fakeClock{now: time.Unix(1000, 0)}
	service := cleanupTestService(clock, map[string]*cleanupServer{"arr": fake})

	runCleanup(t, service, "arr", maxImportAttempts)
	wantLookups(t, fake, maxImportAttempts)
	runCleanup(t, service, "arr", 3)
	wantLookups(t, fake, maxImportAttempts)

	clock.advance(importRetryDelay)
	runCleanup(t, service, "arr", 3)
	wantLookups(t, fake, maxImportAttempts+1)

	clock.advance(importRetryDelay)
	runCleanup(t, service, "arr", 1)
	wantLookups(t, fake, maxImportAttempts+2)
}

func TestCleanupQueueRetriesWhenStatusChanges(t *testing.T) {
	t.Parallel()

	fake := newCleanupServer(t, stuckRecords(matchedByIDTitle))
	service := cleanupTestService(&fakeClock{now: time.Unix(1000, 0)}, map[string]*cleanupServer{"arr": fake})

	runCleanup(t, service, "arr", maxImportAttempts+1)
	wantLookups(t, fake, maxImportAttempts)

	changed := stuckRecords("Title mismatch; automatic import is not possible")
	fake.records.Store(&changed)
	runCleanup(t, service, "arr", 1)
	wantLookups(t, fake, maxImportAttempts+1)
}

func TestCleanupQueueForgetsItemsThatLeftTheQueue(t *testing.T) {
	t.Parallel()

	records := stuckRecords(matchedByIDTitle)
	fake := newCleanupServer(t, records)
	service := cleanupTestService(&fakeClock{now: time.Unix(1000, 0)}, map[string]*cleanupServer{"arr": fake})

	runCleanup(t, service, "arr", maxImportAttempts)
	empty := []QueueSchema{}
	fake.records.Store(&empty)
	runCleanup(t, service, "arr", 1)
	fake.records.Store(&records)
	runCleanup(t, service, "arr", 1)
	wantLookups(t, fake, maxImportAttempts+1)
}

func TestCleanupQueueKeepsLedgerWhenQueueFetchFails(t *testing.T) {
	t.Parallel()

	fake := newCleanupServer(t, stuckRecords(matchedByIDTitle))
	service := cleanupTestService(&fakeClock{now: time.Unix(1000, 0)}, map[string]*cleanupServer{"arr": fake})

	runCleanup(t, service, "arr", maxImportAttempts)
	fake.fail.Store(true)
	if err := service.CleanupQueue(t.Context(), "arr"); err == nil {
		t.Fatal("CleanupQueue() = nil, want the queue fetch error")
	}
	fake.fail.Store(false)
	runCleanup(t, service, "arr", 1)
	wantLookups(t, fake, maxImportAttempts)
}

func TestCleanupQueueLedgerIsPerArr(t *testing.T) {
	t.Parallel()

	sonarr := newCleanupServer(t, stuckRecords(matchedByIDTitle))
	radarr := newCleanupServer(t, stuckRecords(matchedByIDTitle))
	service := cleanupTestService(&fakeClock{now: time.Unix(1000, 0)}, map[string]*cleanupServer{
		"sonarr": sonarr,
		"radarr": radarr,
	})

	runCleanup(t, service, "sonarr", maxImportAttempts+1)
	wantLookups(t, sonarr, maxImportAttempts)
	runCleanup(t, service, "radarr", 1)
	wantLookups(t, radarr, 1)
}

func TestCleanupQueueSuccessClearsFailureCount(t *testing.T) {
	t.Parallel()

	fake := newCleanupServer(t, stuckRecords(matchedByIDTitle))
	service := cleanupTestService(&fakeClock{now: time.Unix(1000, 0)}, map[string]*cleanupServer{"arr": fake})

	runCleanup(t, service, "arr", maxImportAttempts-1)
	fake.succeed.Store(true)
	runCleanup(t, service, "arr", 1)
	fake.succeed.Store(false)
	runCleanup(t, service, "arr", maxImportAttempts)
	wantLookups(t, fake, 2*maxImportAttempts)
}

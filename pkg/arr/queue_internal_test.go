package arr

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
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
			if got := resolveQueueAction(item, config.DefaultQueueCleanupRules(), false); got != test.want {
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

	if got := resolveQueueAction(item, rules, false); got != QueueActionBlocklist {
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
			if got := resolveQueueAction(item, config.DefaultQueueCleanupRules(), false); got != QueueActionNone {
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
	// rejectAll makes every manual import candidate rejected by the arr.
	rejectAll atomic.Bool
	lookups   atomic.Int64
	// commandStatus is what GET /api/v3/command/1 reports; "" answers 404 and "error" answers 401.
	commandStatus atomic.Pointer[string]
	statusChecks  atomic.Int64
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
			if fake.rejectAll.Load() {
				_, _ = w.Write([]byte(`[` + rejectedCandidate + `]`))
				return
			}
			if fake.succeed.Load() {
				_, _ = w.Write([]byte(`[{"path":"/d/A.mkv","series":{"id":7},"quality":{"quality":{"id":3}}}]`))
				return
			}
			_, _ = w.Write([]byte(`[]`))
		case "/api/v3/command":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":1,"status":"queued"}`))
		case "/api/v3/command/1":
			fake.statusChecks.Add(1)
			status := ""
			if current := fake.commandStatus.Load(); current != nil {
				status = *current
			}
			switch status {
			case "":
				http.NotFound(w, r)
			case "error":
				http.Error(w, "no", http.StatusUnauthorized)
			default:
				_, _ = fmt.Fprintf(w, `{"id":1,"status":%q,"message":"Import failed"}`, status)
			}
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

	clock.advance(importRetryDelay - time.Second)
	runCleanup(t, service, "arr", 1)
	wantLookups(t, fake, maxImportAttempts)
	clock.advance(time.Second)
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

func acceptingCleanup(t *testing.T, commandStatus string) (*cleanupServer, *Service) {
	t.Helper()
	fake := newCleanupServer(t, stuckRecords(matchedByIDTitle))
	fake.succeed.Store(true)
	fake.commandStatus.Store(&commandStatus)
	return fake, cleanupTestService(&fakeClock{now: time.Unix(1000, 0)}, map[string]*cleanupServer{"arr": fake})
}

func TestCleanupQueueWaitsWhileImportCommandRuns(t *testing.T) {
	t.Parallel()

	fake, service := acceptingCleanup(t, "started")
	runCleanup(t, service, "arr", 5)
	wantLookups(t, fake, 1)
	if got := fake.statusChecks.Load(); got != 4 {
		t.Fatalf("command status checks = %d, want 4", got)
	}
}

func TestCleanupQueueCountsFinishedCommandWhenItemStaysQueued(t *testing.T) {
	t.Parallel()

	fake, service := acceptingCleanup(t, "completed")
	runCleanup(t, service, "arr", 2*maxImportAttempts+3)
	wantLookups(t, fake, maxImportAttempts)
}

func TestCleanupQueueTreatsUnknownCommandAsFinished(t *testing.T) {
	t.Parallel()

	fake, service := acceptingCleanup(t, "")
	runCleanup(t, service, "arr", 3)
	wantLookups(t, fake, 2)
}

func TestCleanupQueueKeepsWaitingWhenCommandLookupFails(t *testing.T) {
	t.Parallel()

	fake, service := acceptingCleanup(t, "error")
	runCleanup(t, service, "arr", 4)
	wantLookups(t, fake, 1)
}

func TestCleanupQueueKeepsCommandAcrossStatusChange(t *testing.T) {
	t.Parallel()

	fake, service := acceptingCleanup(t, "started")
	runCleanup(t, service, "arr", 1)
	changed := stuckRecords("Title mismatch; automatic import is not possible")
	fake.records.Store(&changed)
	runCleanup(t, service, "arr", 2)
	wantLookups(t, fake, 1)
}

func TestCleanupQueueWarnsHourlyAboutAStuckCommand(t *testing.T) {
	t.Parallel()

	fake := newCleanupServer(t, stuckRecords(matchedByIDTitle))
	fake.succeed.Store(true)
	started := "started"
	fake.commandStatus.Store(&started)
	clock := &fakeClock{now: time.Unix(1000, 0)}
	service := cleanupTestService(clock, map[string]*cleanupServer{"arr": fake})
	var logs bytes.Buffer
	service.logger = zerolog.New(&logs).Level(zerolog.WarnLevel)

	runCleanup(t, service, "arr", 3)
	clock.advance(importRetryDelay)
	runCleanup(t, service, "arr", 3)
	clock.advance(importRetryDelay)
	runCleanup(t, service, "arr", 1)

	wantLookups(t, fake, 1)
	if got := strings.Count(logs.String(), `"command_id":1`); got != 2 {
		t.Fatalf("stuck-command warnings = %d, want 2; logs:\n%s", got, logs.String())
	}
}

func TestCleanupQueueLedgerSeparatesNestedArrNames(t *testing.T) {
	t.Parallel()

	parent := newCleanupServer(t, stuckRecords(matchedByIDTitle))
	child := newCleanupServer(t, stuckRecords(matchedByIDTitle))
	service := cleanupTestService(
		&fakeClock{now: time.Unix(1000, 0)},
		map[string]*cleanupServer{"tv": parent, "tv/anime": child},
	)

	runCleanup(t, service, "tv/anime", maxImportAttempts)
	runCleanup(t, service, "tv", 1)
	runCleanup(t, service, "tv/anime", 1)
	wantLookups(t, child, maxImportAttempts)
}

func TestCleanupQueueLogsErrorsThenOneWarnPerRetry(t *testing.T) {
	t.Parallel()

	fake := newCleanupServer(t, stuckRecords(matchedByIDTitle))
	clock := &fakeClock{now: time.Unix(1000, 0)}
	service := cleanupTestService(clock, map[string]*cleanupServer{"arr": fake})
	var logs bytes.Buffer
	service.logger = zerolog.New(&logs)

	runCleanup(t, service, "arr", maxImportAttempts+2)
	clock.advance(importRetryDelay)
	runCleanup(t, service, "arr", 1)

	var levels []string
	for line := range strings.Lines(logs.String()) {
		var entry struct {
			Level      string `json:"level"`
			DownloadID string `json:"download_id"`
		}
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatal(err)
		}
		if entry.DownloadID != "hash" {
			t.Errorf("log line without download_id: %s", line)
		}
		levels = append(levels, entry.Level)
	}
	want := []string{"error", "error", "error", "error", "warn", "warn"}
	if !slices.Equal(levels, want) {
		t.Fatalf("log levels = %v, want %v", levels, want)
	}
}

// A cleanup blocklist is a "Manually marked as failed" in the arr; it must name what it removed.
func TestCleanupQueueLogsEachBlocklistedItem(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v3/queue":
			_, _ = w.Write([]byte(`{"page":1,"pageSize":200,"totalRecords":1,"records":[{"id":9,` +
				`"title":"Show.S01E14","downloadId":"7C8A7C56","status":"failed",` +
				`"trackedDownloadStatus":"warning","trackedDownloadState":"downloading"}]}`))
		case r.Method == http.MethodDelete && r.URL.Path == "/api/v3/queue/bulk":
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	var logs bytes.Buffer
	cfg := config.NewStore(&config.Config{QueueCleanup: config.QueueCleanup{Rules: config.DefaultQueueCleanupRules()}})
	service := New(cfg, nil, zerolog.New(&logs))
	service.arrs["sonarr"] = Arr{Name: "sonarr", Host: server.URL, Token: "secret", Type: Sonarr}

	if err := service.CleanupQueue(t.Context(), "sonarr"); err != nil {
		t.Fatal(err)
	}
	line := logs.String()
	for _, want := range []string{`"level":"info"`, "Show.S01E14", "7C8A7C56", "blacklist_research"} {
		if !strings.Contains(line, want) {
			t.Fatalf("cleanup log = %s, want it to contain %s", line, want)
		}
	}
}

// An imported download is never blocklisted; a partial pack the arr blocked keeps the owner's blocklist policy.
func TestResolveQueueActionLeavesImportedDownloads(t *testing.T) {
	t.Parallel()
	for state, want := range map[string]QueueAction{
		"imported": QueueActionNone, "importing": QueueActionNone, "importBlocked": QueueActionBlocklistResearch,
	} {
		item := QueueSchema{Status: "completed", TrackedDownloadStatus: "warning", TrackedDownloadState: state}
		item.StatusMessages = append(item.StatusMessages, struct {
			Title    string   `json:"title"`
			Messages []string `json:"messages"`
		}{Title: "One or more episodes expected in this release were not imported or missing from the release"})
		if got := resolveQueueAction(item, config.DefaultQueueCleanupRules(), false); got != want {
			t.Errorf("state %s: resolveQueueAction() = %q, want %q", state, got, want)
		}
	}
}

// When the arr rejects every file, retrying soon cannot help: one warning, then a daily recheck.
func TestCleanupQueueBacksOffADayWhenEveryFileIsRejected(t *testing.T) {
	t.Parallel()

	fake := newCleanupServer(t, stuckRecords(matchedByIDTitle))
	fake.rejectAll.Store(true)
	clock := &fakeClock{now: time.Unix(1000, 0)}
	service := cleanupTestService(clock, map[string]*cleanupServer{"arr": fake})
	var logs bytes.Buffer
	service.logger = zerolog.New(&logs).Level(zerolog.WarnLevel)

	runCleanup(t, service, "arr", maxImportAttempts+1)
	wantLookups(t, fake, 1)
	clock.advance(importRejectedDelay - time.Minute)
	runCleanup(t, service, "arr", 1)
	wantLookups(t, fake, 1)
	clock.advance(time.Minute)
	runCleanup(t, service, "arr", maxImportAttempts+1)
	wantLookups(t, fake, 2)
	if got := strings.Count(logs.String(), "Not an upgrade"); got != 2 {
		t.Fatalf("rejection warnings = %d, want 2 (one per lookup); logs:\n%s", got, logs.String())
	}
}

// A changed queue status means the arr's view moved, so the daily backoff gives way to an immediate retry.
func TestCleanupQueueRetriesRejectedImportWhenStatusChanges(t *testing.T) {
	t.Parallel()

	fake := newCleanupServer(t, stuckRecords(matchedByIDTitle))
	fake.rejectAll.Store(true)
	service := cleanupTestService(&fakeClock{now: time.Unix(1000, 0)}, map[string]*cleanupServer{"arr": fake})

	runCleanup(t, service, "arr", 2)
	wantLookups(t, fake, 1)
	changed := stuckRecords("Title mismatch; automatic import is not possible")
	fake.records.Store(&changed)
	runCleanup(t, service, "arr", 1)
	wantLookups(t, fake, 2)
}

// A pack whose every episode has a file did its job: episodes_missing removes its leftover row instead of blocklisting.
func TestResolveQueueActionRemovesCompletePackLeftovers(t *testing.T) {
	t.Parallel()
	defaults := config.DefaultQueueCleanupRules()
	disabled := config.DefaultQueueCleanupRules()
	for i := range disabled {
		if disabled[i].ID == "episodes_missing" {
			disabled[i].Action = ""
		}
	}
	plain := config.DefaultQueueCleanupRules()
	internal := config.DefaultQueueCleanupRules()
	for i := range plain {
		if plain[i].ID == "episodes_missing" {
			plain[i].Action = string(QueueActionBlocklist)
			internal[i].Action = string(QueueActionRemoveLeftover)
		}
	}
	customFirst := append(
		[]config.QueueCleanupRule{{Match: "not imported", Action: string(QueueActionBlocklist)}},
		defaults...)
	tests := []struct {
		name     string
		state    string
		rules    []config.QueueCleanupRule
		complete bool
		want     QueueAction
	}{
		{
			name:     "complete pack",
			state:    "importBlocked",
			rules:    defaults,
			complete: true,
			want:     QueueActionRemoveLeftover,
		},
		{name: "incomplete pack", state: "importBlocked", rules: defaults, want: QueueActionBlocklistResearch},
		{name: "still importing", state: "importing", rules: defaults, complete: true, want: QueueActionNone},
		{
			name:     "custom rule first",
			state:    "importBlocked",
			rules:    customFirst,
			complete: true,
			want:     QueueActionBlocklist,
		},
		{name: "rule disabled", state: "importBlocked", rules: disabled, complete: true, want: QueueActionNone},
		{
			name:     "plain blacklist",
			state:    "importBlocked",
			rules:    plain,
			complete: true,
			want:     QueueActionRemoveLeftover,
		},
		{name: "internal action in config", state: "importBlocked", rules: internal, complete: true,
			want: QueueActionNone},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			item := QueueSchema{Status: "completed", TrackedDownloadStatus: "warning", TrackedDownloadState: tt.state}
			item.StatusMessages = append(item.StatusMessages, struct {
				Title    string   `json:"title"`
				Messages []string `json:"messages"`
			}{Title: "One or more episodes expected in this release were not imported or missing from the release"})
			if got := resolveQueueAction(item, tt.rules, tt.complete); got != tt.want {
				t.Fatalf("resolveQueueAction() = %q, want %q", got, tt.want)
			}
		})
	}
}

// Sonarr prints the missing-episodes header also when the only leftovers are files that are no upgrade.
func TestMatchCatalogRuleEpisodesMissingIgnoresNonUpgradeLeftovers(t *testing.T) {
	t.Parallel()
	type file struct{ name, message string }
	tests := []struct {
		name  string
		files []file
		want  bool
	}{
		{name: "quality revision leftover", want: false, files: []file{
			{"Show.S01E03.mkv", "Not a quality revision upgrade for existing episode file(s)"},
		}},
		{name: "lower quality leftover", want: false, files: []file{
			{"Show.S01E05.mkv", "Not an upgrade for existing episode file(s). Existing quality: WEBDL-1080p. " +
				"New Quality HDTV-1080p."},
		}},
		{name: "every kind of leftover", want: false, files: []file{
			{"Show.S01E01.mkv", "Not a custom format upgrade for existing episode file(s)"},
			{"Show.S01E02.mkv", "not an upgrade for existing episode file(s)"},
		}},
		{name: "leftover and missing episode", want: true, files: []file{
			{"Show.S01E03.mkv", "Not a quality revision upgrade for existing episode file(s)"},
			{"Show.S01E04.mkv", "Unable to determine if file is a sample"},
		}},
		{name: "no per-file messages", want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			item := missingRecord(1, 11, "HASH", false)
			for _, f := range tt.files {
				item.StatusMessages = append(item.StatusMessages, struct {
					Title    string   `json:"title"`
					Messages []string `json:"messages"`
				}{Title: f.name, Messages: []string{f.message}})
			}
			if got, known := matchCatalogRule("episodes_missing", item, statusText(item)); !known || got != tt.want {
				t.Fatalf("matchCatalogRule() = %v, %v, want %v, true", got, known, tt.want)
			}
		})
	}
}

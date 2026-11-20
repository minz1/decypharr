package arr

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
)

const (
	noFilesRule      = "import_end:no_files"
	rejectedRule     = "import_end:rejected"
	wrongEpisodeRule = "import_end:wrong_episode"
	changedStatus    = "Title mismatch; automatic import is not possible"
	wrongEpisodeUnit = "arr/" + wrongEpisodeRule + "/unknown\x00hash"
)

func notUpgradeBody() string { return rejectedBody([]string{notUpgradeReason}) }

func wrongEpisodeBody() string { return rejectedBody([]string{notUpgradeReason, wrongEpisodeOne}) }

type deleteCall struct {
	query url.Values
	ids   []int
}

type importEndServer struct {
	*httptest.Server

	records       atomic.Pointer[[]QueueSchema]
	manualimport  atomic.Pointer[string]
	bulkStatus    atomic.Int64
	onEpisodeRead atomic.Pointer[func(n int)]
	imported      atomic.Bool
	historyFails  atomic.Bool

	mu       sync.Mutex
	episodes []map[int]int
	reads    int
	requests []string
	removals []deleteCall
}

func newImportEndServer(
	t *testing.T, records []QueueSchema, manualimport string, episodes ...map[int]int,
) *importEndServer {
	t.Helper()
	fake := &importEndServer{episodes: episodes}
	fake.records.Store(&records)
	fake.setManualImport(manualimport)
	fake.Server = httptest.NewServer(http.HandlerFunc(fake.serve))
	t.Cleanup(fake.Close)
	return fake
}

func (f *importEndServer) setManualImport(body string) { f.manualimport.Store(&body) }

func (f *importEndServer) nextEpisodes() (int, map[int]int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	if len(f.episodes) == 0 {
		return f.reads, nil
	}
	answer := f.episodes[0]
	if len(f.episodes) > 1 {
		f.episodes = f.episodes[1:]
	}
	return f.reads, answer
}

func (f *importEndServer) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requests = append(f.requests, r.Method+" "+r.URL.RequestURI())
	f.mu.Unlock()
	switch r.Method + " " + r.URL.Path {
	case "GET /api/v3/queue":
		current := *f.records.Load()
		body, _ := json.Marshal(QueueResponseScheme{
			Page: 1, PageSize: 200, TotalRecords: len(current), Records: current,
		})
		_, _ = w.Write(body)
	case "GET /api/v3/manualimport":
		body := *f.manualimport.Load()
		if body == "401" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(body))
	case "GET /api/v3/episode":
		n, answer := f.nextEpisodes()
		if hook := f.onEpisodeRead.Load(); hook != nil {
			(*hook)(n)
		}
		if answer == nil {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		type episode struct {
			ID            int `json:"id"`
			EpisodeFileID int `json:"episodeFileId"`
		}
		found := make([]episode, 0)
		for _, raw := range r.URL.Query()["episodeIds"] {
			for id, fileID := range answer {
				if raw == strconv.Itoa(id) {
					found = append(found, episode{ID: id, EpisodeFileID: fileID})
				}
			}
		}
		body, _ := json.Marshal(found)
		_, _ = w.Write(body)
	case "GET /api/v3/history":
		f.serveHistory(w, r)
	case "POST /api/v3/command":
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":1,"status":"queued"}`))
	case "GET /api/v3/command/1":
		_, _ = w.Write([]byte(`{"id":1,"status":"failed","message":"Import failed"}`))
	case "GET /api/v3/system/status":
		http.Error(w, "no", http.StatusUnauthorized)
	case "DELETE /api/v3/queue/bulk":
		var payload struct {
			IDs []int `json:"ids"`
		}
		_ = json.NewDecoder(r.Body).Decode(&payload)
		f.mu.Lock()
		f.removals = append(f.removals, deleteCall{query: r.URL.Query(), ids: payload.IDs})
		f.mu.Unlock()
		if status := f.bulkStatus.Load(); status != 0 {
			http.Error(w, "no", int(status))
			return
		}
		w.WriteHeader(http.StatusOK)
	default:
		http.NotFound(w, r)
	}
}

func (f *importEndServer) serveHistory(w http.ResponseWriter, r *http.Request) {
	if f.historyFails.Load() {
		http.Error(w, "no", http.StatusUnauthorized)
		return
	}
	records := make([]HistoryRecord, 0)
	if f.imported.Load() {
		records = append(records, HistoryRecord{
			ID: 7, DownloadID: r.URL.Query().Get("downloadId"), EventType: EventDownloadFolderImported,
		})
	}
	body, _ := json.Marshal(HistorySchema{Page: 1, PageSize: 100, TotalRecords: len(records), Records: records})
	_, _ = w.Write(body)
}

func (f *importEndServer) count(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, request := range f.requests {
		if strings.HasPrefix(request, prefix) {
			n++
		}
	}
	return n
}

func (f *importEndServer) deletes() []deleteCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.removals)
}

func (f *importEndServer) lastRequests(n int) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.requests[max(len(f.requests)-n, 0):])
}

func (f *importEndServer) lookups() int { return f.count("GET /api/v3/manualimport") }

func (f *importEndServer) episodeReads() int { return f.count("GET /api/v3/episode") }

func importEndService(
	t *testing.T, fake *importEndServer, typ Type, cfg config.QueueCleanup, clock *fakeClock,
) *Service {
	t.Helper()
	cfg.Rules = config.DefaultQueueCleanupRules()
	service := New(config.NewStore(&config.Config{QueueCleanup: cfg}), nil, zerolog.Nop())
	service.now = clock.Now
	service.arrs["arr"] = Arr{Name: "arr", Host: fake.URL, Token: "secret", Type: typ}
	return service
}

func (c *fakeClock) set(at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = at
}

func endRows(downloadID string, episodeIDs ...int) []QueueSchema {
	base := stuckRecords(matchedByIDTitle)[0]
	rows := make([]QueueSchema, len(episodeIDs))
	for i, episodeID := range episodeIDs {
		rows[i] = base
		rows[i].ID = i + 1
		rows[i].DownloadID = downloadID
		rows[i].EpisodeID = episodeID
	}
	return rows
}

func tick(t *testing.T, service *Service, clock *fakeClock) {
	t.Helper()
	runCleanup(t, service, "arr", 1)
	clock.advance(10 * time.Second)
}

func tickUntil(t *testing.T, service *Service, clock *fakeClock, until time.Time) {
	t.Helper()
	for clock.Now().Before(until) {
		tick(t, service, clock)
	}
}

func tickN(t *testing.T, service *Service, clock *fakeClock, n int) {
	t.Helper()
	for range n {
		tick(t, service, clock)
	}
}

func wantDelete(t *testing.T, fake *importEndServer, blocklist bool, ids ...int) {
	t.Helper()
	deletes := fake.deletes()
	if len(deletes) != 1 {
		t.Fatalf("DELETEs = %d, want 1; requests:\n%s", len(deletes), strings.Join(fake.lastRequests(40), "\n"))
	}
	flag := "false"
	if blocklist {
		flag = "true"
	}
	for _, key := range []string{"blocklist", "removeFromClient", "skipRedownload"} {
		if got := deletes[0].query.Get(key); got != flag {
			t.Errorf("%s = %q, want %q", key, got, flag)
		}
	}
	if !slices.Equal(deletes[0].ids, ids) {
		t.Errorf("DELETE ids = %v, want %v", deletes[0].ids, ids)
	}
}

func wantNoDelete(t *testing.T, fake *importEndServer) {
	t.Helper()
	if got := len(fake.deletes()); got != 0 {
		t.Fatalf("DELETEs = %d, want none", got)
	}
}

func wantCount(t *testing.T, what string, got, want int) {
	t.Helper()
	if got != want {
		t.Fatalf("%s = %d, want %d", what, got, want)
	}
}

func logTo(service *Service, level zerolog.Level) *bytes.Buffer {
	var logs bytes.Buffer
	service.logger = zerolog.New(&logs).Level(level)
	return &logs
}

func warnLines(logs *bytes.Buffer) int { return strings.Count(logs.String(), `"level":"warn"`) }

func runOnce(
	t *testing.T, rows []QueueSchema, body string, episodes ...map[int]int,
) (*importEndServer, *Service) {
	t.Helper()
	fake := newImportEndServer(t, rows, body, episodes...)
	clock := &fakeClock{now: time.Unix(1000, 0)}
	service := importEndService(t, fake, Sonarr, config.QueueCleanup{}, clock)
	runCleanup(t, service, "arr", 1)
	return fake, service
}

func TestImportEndRemovesNoFilesRowAfterAnHour(t *testing.T) {
	t.Parallel()
	fake := newImportEndServer(t, endRows("hash", 11, 12), `[]`, map[int]int{11: 5, 12: 6})
	t0 := time.Unix(1000, 0)
	clock := &fakeClock{now: t0}
	service := importEndService(t, fake, Sonarr, config.QueueCleanup{}, clock)
	logs := logTo(service, zerolog.InfoLevel)

	tickUntil(t, service, clock, t0.Add(importRetryDelay-time.Second))
	wantNoDelete(t, fake)
	tickUntil(t, service, clock, t0.Add(importRetryDelay+time.Minute))
	wantDelete(t, fake, false, 1, 2)
	if !strings.Contains(logs.String(), `"rule":"`+noFilesRule+`"`) {
		t.Fatalf("no INFO line names the rule %s; logs:\n%s", noFilesRule, logs.String())
	}
}

func TestImportEndBlocklistsWrongEpisode(t *testing.T) {
	t.Parallel()
	fake, service := runOnce(t, endRows("hash", 11), wrongEpisodeBody(), map[int]int{11: 5})
	wantDelete(t, fake, true, 1)
	want := []string{"GET /api/v3/episode", "GET /api/v3/history?", "GET /api/v3/episode", "DELETE /api/v3/queue/bulk?"}
	tail := fake.lastRequests(len(want))
	for i, prefix := range want {
		if !strings.HasPrefix(tail[i], prefix) {
			t.Fatalf("last requests = %v, want episode read, history read, episode read, DELETE", tail)
		}
	}
	service.guard.mu.Lock()
	defer service.guard.mu.Unlock()
	bucket := service.guard.buckets[guardKey{"arr", wrongEpisodeRule, "unknown"}]
	if bucket == nil {
		t.Fatalf("no guard bucket for %s", wrongEpisodeRule)
	}
	if _, ok := bucket.sent["hash"]; !ok {
		t.Fatalf("bucket sent = %v, want the download", bucket.sent)
	}
	if len(service.guard.pending) != 0 {
		t.Fatalf("pending reservations = %v, want none", service.guard.pending)
	}
}

func TestImportEndWrongEpisodeWithACreditedImportRemovesWithoutBlocklist(t *testing.T) {
	t.Parallel()
	fake := newImportEndServer(t, endRows("hash", 11, 12), wrongEpisodeBody(), map[int]int{11: 5, 12: 6})
	fake.imported.Store(true)
	service := importEndService(t, fake, Sonarr, config.QueueCleanup{}, &fakeClock{now: time.Unix(1000, 0)})
	logs := logTo(service, zerolog.InfoLevel)
	runCleanup(t, service, "arr", 1)
	wantDelete(t, fake, false, 1, 2)
	if !strings.Contains(logs.String(), `"rule":"`+rejectedRule+`"`) {
		t.Fatalf("no INFO line names the rule %s; logs:\n%s", rejectedRule, logs.String())
	}
	if !strings.Contains(logs.String(), "imported from this download") {
		t.Fatalf("no INFO line gives the reason; logs:\n%s", logs.String())
	}
	service.guard.mu.Lock()
	defer service.guard.mu.Unlock()
	if bucket := service.guard.buckets[guardKey{"arr", wrongEpisodeRule, "unknown"}]; bucket != nil {
		t.Fatalf("wrong-episode guard bucket = %+v, want none", bucket)
	}
	if len(service.guard.pending) != 0 {
		t.Fatalf("pending reservations = %v, want none", service.guard.pending)
	}
}

func TestImportEndWrongEpisodeWithUnreadHistorySendsNothing(t *testing.T) {
	t.Parallel()
	fake := newImportEndServer(t, endRows("hash", 11), wrongEpisodeBody(), map[int]int{11: 5})
	fake.historyFails.Store(true)
	service := importEndService(t, fake, Sonarr, config.QueueCleanup{}, &fakeClock{now: time.Unix(1000, 0)})
	runCleanup(t, service, "arr", 1)
	wantNoDelete(t, fake)
	wantCount(t, "history reads", fake.count("GET /api/v3/history"), 1)
	service.guard.mu.Lock()
	defer service.guard.mu.Unlock()
	if len(service.guard.pending) != 0 || len(service.guard.buckets) != 0 {
		t.Fatalf("guard pending = %v, buckets = %v, want none", service.guard.pending, service.guard.buckets)
	}
}

func TestImportEndRemovesNotUpgradeOnly(t *testing.T) {
	t.Parallel()
	fake := newImportEndServer(t, endRows("hash", 11), notUpgradeBody(), map[int]int{11: 5})
	service := importEndService(t, fake, Sonarr, config.QueueCleanup{}, &fakeClock{now: time.Unix(1000, 0)})
	logs := logTo(service, zerolog.InfoLevel)
	runCleanup(t, service, "arr", 1)
	wantDelete(t, fake, false, 1)
	if !strings.Contains(logs.String(), `"rule":"`+rejectedRule+`"`) {
		t.Fatalf("no INFO line names the rule %s; logs:\n%s", rejectedRule, logs.String())
	}
}

func TestImportEndMixedRejectionsRemoveWithoutBlocklist(t *testing.T) {
	t.Parallel()
	for name, body := range map[string]string{
		"wrong episode first": rejectedBody([]string{wrongEpisodeOne}, []string{notUpgradeReason}),
		"wrong episode last":  rejectedBody([]string{notUpgradeReason}, []string{wrongEpisodeOne}),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			fake, _ := runOnce(t, endRows("hash", 11), body, map[int]int{11: 5})
			wantDelete(t, fake, false, 1)
		})
	}
}

func TestImportEndBlocklistsPluralWrongEpisode(t *testing.T) {
	t.Parallel()
	fake, _ := runOnce(t, endRows("hash", 11), rejectedBody([]string{wrongEpisodeMany}), map[int]int{11: 5})
	wantDelete(t, fake, true, 1)
}

func TestImportEndNoFilesMissingEpisodeWarnsOnce(t *testing.T) {
	t.Parallel()
	fake := newImportEndServer(t, endRows("hash", 11, 12), `[]`, map[int]int{11: 5, 12: 0})
	clock := &fakeClock{now: time.Unix(1000, 0)}
	service := importEndService(t, fake, Sonarr, config.QueueCleanup{}, clock)
	logs := logTo(service, zerolog.InfoLevel)

	tickN(t, service, clock, 6)
	wantCount(t, "episode reads before the hour", fake.episodeReads(), 0)
	const retries = 24
	for range retries {
		clock.advance(importRetryDelay)
		tick(t, service, clock)
	}
	wantNoDelete(t, fake)
	wantCount(t, "warnings", warnLines(logs), 1)
	wantCount(t, "lookups", fake.lookups(), maxImportAttempts+retries)
	wantCount(t, "episode reads", fake.episodeReads(), retries)
}

func TestImportEndRejectedMissingEpisodeWarnsOnce(t *testing.T) {
	t.Parallel()
	fake := newImportEndServer(t, endRows("hash", 11), notUpgradeBody(), map[int]int{11: 0})
	clock := &fakeClock{now: time.Unix(1000, 0)}
	service := importEndService(t, fake, Sonarr, config.QueueCleanup{}, clock)
	logs := logTo(service, zerolog.InfoLevel)

	runCleanup(t, service, "arr", 1)
	for range 2 {
		clock.advance(importRejectedDelay)
		runCleanup(t, service, "arr", 1)
	}
	wantNoDelete(t, fake)
	wantCount(t, "warnings", warnLines(logs), 1)
	wantCount(t, "lookups", fake.lookups(), 3)
	wantCount(t, "episode reads", fake.episodeReads(), 3)
}

func TestImportEndRefusesUnownedDownloads(t *testing.T) {
	t.Parallel()
	sibling := func(rows []QueueSchema, before bool) []QueueSchema {
		other := endRows("hash", 12)[0]
		other.ID = 2
		other.TrackedDownloadStatus = "ok"
		if before {
			other.ID = 1
			rows[0].ID = 2
			return append([]QueueSchema{other}, rows...)
		}
		return append(rows, other)
	}
	for _, tc := range []struct {
		name     string
		rows     []QueueSchema
		wantRead int
	}{
		{name: "episode id zero", rows: endRows("hash", 0), wantRead: 0},
		{name: "sibling after", rows: sibling(endRows("hash", 11), false), wantRead: 0},
		{name: "sibling before", rows: sibling(endRows("hash", 11), true), wantRead: 0},
		{name: "no download id", rows: endRows("", 11), wantRead: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fake, _ := runOnce(t, tc.rows, notUpgradeBody(), map[int]int{11: 5, 12: 6})
			wantNoDelete(t, fake)
			wantCount(t, "lookups", fake.lookups(), 1)
			wantCount(t, "episode reads", fake.episodeReads(), tc.wantRead)
		})
	}
	for _, tc := range []struct {
		name string
		rows []QueueSchema
		ids  []int
	}{
		{name: "episode id set", rows: endRows("hash", 11), ids: []int{1}},
		{name: "sibling also stuck", rows: endRows("hash", 11, 12), ids: []int{1, 2}},
		{name: "download id set", rows: endRows("hash", 11), ids: []int{1}},
	} {
		t.Run("control "+tc.name, func(t *testing.T) {
			t.Parallel()
			fake, _ := runOnce(t, tc.rows, notUpgradeBody(), map[int]int{11: 5, 12: 6})
			wantDelete(t, fake, false, tc.ids...)
		})
	}
}

func TestImportEndEpisodeReadFailureKeepsRow(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		episodes []map[int]int
	}{
		{name: "read fails", episodes: []map[int]int{nil}},
		{name: "id omitted", episodes: []map[int]int{{11: 5}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fake := newImportEndServer(t, endRows("hash", 11, 12), notUpgradeBody(), tc.episodes...)
			service := importEndService(t, fake, Sonarr, config.QueueCleanup{}, &fakeClock{now: time.Unix(1000, 0)})
			logs := logTo(service, zerolog.DebugLevel)
			runCleanup(t, service, "arr", 1)
			wantNoDelete(t, fake)
			wantCount(t, "episode reads", fake.episodeReads(), 1)
			if !strings.Contains(logs.String(), "import end skipped") {
				t.Fatalf("no DEBUG line about the skipped end; logs:\n%s", logs.String())
			}
		})
	}
}

func TestImportEndIgnoresQueueEpisodeHasFile(t *testing.T) {
	t.Parallel()
	rows := endRows("hash", 11)
	rows[0].EpisodeHasFile = true
	fake, _ := runOnce(t, rows, notUpgradeBody(), map[int]int{11: 0})
	wantNoDelete(t, fake)
	wantCount(t, "episode reads", fake.episodeReads(), 1)
}

func TestImportEndNeedsAnHourOfEmptyLookups(t *testing.T) {
	t.Parallel()
	fake := newImportEndServer(t, endRows("hash", 11), `401`, map[int]int{11: 5})
	clock := &fakeClock{now: time.Unix(1000, 0)}
	service := importEndService(t, fake, Sonarr, config.QueueCleanup{}, clock)

	tickN(t, service, clock, maxImportAttempts)
	fake.setManualImport(`[]`)
	clock.advance(importRetryDelay)
	tick(t, service, clock)
	wantNoDelete(t, fake)
	clock.advance(importRetryDelay)
	tick(t, service, clock)
	wantDelete(t, fake, false, 1)
}

func TestImportEndFilesFoundRestartTheClock(t *testing.T) {
	t.Parallel()
	fake := newImportEndServer(t, endRows("hash", 11), `[]`, map[int]int{11: 5})
	t0 := time.Unix(1000, 0)
	clock := &fakeClock{now: t0}
	service := importEndService(t, fake, Sonarr, config.QueueCleanup{}, clock)

	tickN(t, service, clock, maxImportAttempts-1)
	fake.setManualImport(`[` + cleanCandidate + `]`)
	tickN(t, service, clock, 2)
	wantCount(t, "command submissions", fake.count("POST /api/v3/command"), 1)
	wantCount(t, "command status reads", fake.count("GET /api/v3/command/1"), 1)
	fake.setManualImport(`[]`)

	clock.set(t0.Add(importRetryDelay + time.Minute))
	lookups := fake.lookups()
	tick(t, service, clock)
	wantCount(t, "lookups", fake.lookups(), lookups+1)
	wantNoDelete(t, fake)
	clock.advance(importRetryDelay)
	tick(t, service, clock)
	wantDelete(t, fake, false, 1)
}

func TestImportEndNullOrEmptyBodyRestartsTheClock(t *testing.T) {
	t.Parallel()
	for _, body := range []string{`null`, ``} {
		t.Run("body "+body, func(t *testing.T) {
			t.Parallel()
			fake := newImportEndServer(t, endRows("hash", 11), `[]`, map[int]int{11: 5})
			t0 := time.Unix(1000, 0)
			clock := &fakeClock{now: t0}
			service := importEndService(t, fake, Sonarr, config.QueueCleanup{}, clock)

			tickN(t, service, clock, maxImportAttempts-1)
			fake.setManualImport(body)
			tick(t, service, clock)
			fake.setManualImport(`[]`)
			clock.set(t0.Add(importRetryDelay + 40*time.Second))
			lookups := fake.lookups()
			tick(t, service, clock)
			wantCount(t, "lookups", fake.lookups(), lookups+1)
			wantNoDelete(t, fake)
			clock.advance(importRetryDelay)
			tick(t, service, clock)
			wantDelete(t, fake, false, 1)
		})
	}
}

func TestImportEndHeldByGuardWaitsForRetry(t *testing.T) {
	t.Parallel()
	fake := newImportEndServer(t, endRows("hash", 11), wrongEpisodeBody(), map[int]int{11: 5})
	t0 := time.Unix(1000, 0)
	clock := &fakeClock{now: t0}
	service := importEndService(t, fake, Sonarr, config.QueueCleanup{BlocklistPerBucket: 1}, clock)
	logs := logTo(service, zerolog.InfoLevel)
	service.guard.commit("arr", []QueueSchema{{
		ID: 99, DownloadID: "other", rule: wrongEpisodeRule, provider: "unknown",
	}}, clock.Now())

	runCleanup(t, service, "arr", 1)
	wantNoDelete(t, fake)
	service.guard.mu.Lock()
	_, heldUnit := service.guard.heldSince[wrongEpisodeUnit]
	held := len(service.guard.heldSince)
	service.guard.mu.Unlock()
	wantCount(t, "held downloads", held, 1)
	if !heldUnit {
		t.Fatalf("heldSince has no entry for %q", wrongEpisodeUnit)
	}
	if strings.Contains(logs.String(), "rejects every file") {
		t.Fatalf("a held end logged the rejection warning:\n%s", logs.String())
	}
	lookups, reads := fake.lookups(), fake.episodeReads()

	clock.set(t0.Add(2 * time.Hour))
	runCleanup(t, service, "arr", 1)
	clock.set(t0.Add(importRejectedDelay - time.Minute))
	runCleanup(t, service, "arr", 1)
	wantCount(t, "lookups while held", fake.lookups(), lookups)
	wantCount(t, "episode reads while held", fake.episodeReads(), reads)
	wantNoDelete(t, fake)

	clock.set(t0.Add(importRejectedDelay))
	runCleanup(t, service, "arr", 1)
	wantCount(t, "lookups at the retry", fake.lookups(), lookups+1)
	wantDelete(t, fake, true, 1)
}

func TestImportEndRefusedRemovalWaitsForRetry(t *testing.T) {
	t.Parallel()
	fake := newImportEndServer(t, endRows("hash", 11), `[]`, map[int]int{11: 5})
	fake.bulkStatus.Store(http.StatusBadRequest)
	clock := &fakeClock{now: time.Unix(1000, 0)}
	service := importEndService(t, fake, Sonarr, config.QueueCleanup{}, clock)
	logs := logTo(service, zerolog.InfoLevel)

	tick(t, service, clock)
	clock.advance(importRetryDelay)
	attemptAt := clock.Now()
	tick(t, service, clock)
	wantCount(t, "DELETEs", len(fake.deletes()), 1)
	wantCount(t, "removal errors", strings.Count(logs.String(), "leftover removal failed"), 1)
	lookups, reads := fake.lookups(), fake.episodeReads()

	tickN(t, service, clock, 20)
	wantCount(t, "lookups while waiting", fake.lookups(), lookups)
	wantCount(t, "episode reads while waiting", fake.episodeReads(), reads)
	wantCount(t, "DELETEs while waiting", len(fake.deletes()), 1)
	wantCount(t, "removal errors while waiting", strings.Count(logs.String(), "leftover removal failed"), 1)

	clock.set(attemptAt.Add(importRetryDelay))
	tick(t, service, clock)
	wantCount(t, "lookups at the retry", fake.lookups(), lookups+1)
	wantCount(t, "episode reads at the retry", fake.episodeReads(), reads+1)
	wantCount(t, "DELETEs at the retry", len(fake.deletes()), 2)
	wantCount(t, "removal errors at the retry", strings.Count(logs.String(), "leftover removal failed"), 2)
}

func TestImportEndNeedsSonarr(t *testing.T) {
	t.Parallel()
	for _, typ := range []Type{Radarr, Others} {
		t.Run(string(typ), func(t *testing.T) {
			t.Parallel()
			fake := newImportEndServer(t, endRows("hash", 11), notUpgradeBody(), map[int]int{11: 5})
			service := importEndService(t, fake, typ, config.QueueCleanup{}, &fakeClock{now: time.Unix(1000, 0)})
			runCleanup(t, service, "arr", 1)
			wantNoDelete(t, fake)
			wantCount(t, "episode reads", fake.episodeReads(), 0)
			if typ == Others && fake.count("GET /api/v3/system/status") == 0 {
				t.Fatal("the instance type was never probed")
			}
		})
	}
	t.Run("control Sonarr", func(t *testing.T) {
		t.Parallel()
		fake, _ := runOnce(t, endRows("hash", 11), notUpgradeBody(), map[int]int{11: 5})
		wantDelete(t, fake, false, 1)
	})
}

type guardObservation struct {
	mu      sync.Mutex
	pending bool
	sent    bool
}

func TestImportEndReleasedWhenEpisodeLosesFile(t *testing.T) {
	t.Parallel()
	key := guardKey{"arr", wrongEpisodeRule, "unknown"}
	for _, tc := range []struct {
		name     string
		ids      []int
		episodes []map[int]int
	}{
		{name: "file removed", ids: []int{11}, episodes: []map[int]int{{11: 5}, {11: 0}}},
		{name: "read fails", ids: []int{11}, episodes: []map[int]int{{11: 5}, nil}},
		{name: "id omitted", ids: []int{11}, episodes: []map[int]int{{11: 5}, {}}},
		{name: "second row file removed", ids: []int{11, 12}, episodes: []map[int]int{{11: 5, 12: 6}, {11: 5, 12: 0}}},
		{name: "second row omitted", ids: []int{11, 12}, episodes: []map[int]int{{11: 5, 12: 6}, {11: 5}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fake := newImportEndServer(t, endRows("hash", tc.ids...), wrongEpisodeBody(), tc.episodes...)
			t0 := time.Unix(1000, 0)
			clock := &fakeClock{now: t0}
			service := importEndService(t, fake, Sonarr, config.QueueCleanup{}, clock)
			var seen guardObservation
			hook := func(n int) {
				if n != 2 {
					return
				}
				service.guard.mu.Lock()
				defer service.guard.mu.Unlock()
				_, pending := service.guard.pending[wrongEpisodeUnit]
				bucket := service.guard.buckets[key]
				if bucket == nil {
					t.Errorf("no guard bucket at the second read")
					return
				}
				_, sent := bucket.sent["hash"]
				seen.mu.Lock()
				seen.pending, seen.sent = pending, sent
				seen.mu.Unlock()
			}
			fake.onEpisodeRead.Store(&hook)

			runCleanup(t, service, "arr", 1)
			wantNoDelete(t, fake)
			wantCount(t, "episode reads", fake.episodeReads(), 2)
			seen.mu.Lock()
			if !seen.pending || !seen.sent {
				t.Errorf("at the second read pending = %v, sent = %v, want both reserved", seen.pending, seen.sent)
			}
			seen.mu.Unlock()
			service.guard.mu.Lock()
			pending := len(service.guard.pending)
			sent := 0
			if bucket := service.guard.buckets[key]; bucket != nil {
				sent = len(bucket.sent)
			}
			service.guard.mu.Unlock()
			wantCount(t, "pending reservations", pending, 0)
			wantCount(t, "bucket sent", sent, 0)

			clock.set(t0.Add(importRejectedDelay - time.Second))
			runCleanup(t, service, "arr", 1)
			wantCount(t, "lookups before the retry", fake.lookups(), 1)
			clock.set(t0.Add(importRejectedDelay))
			runCleanup(t, service, "arr", 1)
			wantCount(t, "lookups at the retry", fake.lookups(), 2)
		})
	}
}

func TestImportEndNoFilesBoundaryIsInclusive(t *testing.T) {
	t.Parallel()
	fake := newImportEndServer(t, endRows("hash", 11), `[]`, map[int]int{11: 5})
	t0 := time.Unix(1000, 0)
	clock := &fakeClock{now: t0}
	service := importEndService(t, fake, Sonarr, config.QueueCleanup{}, clock)
	retryNow := func() {
		service.mu.Lock()
		failure := service.importFailures["arr"]["hash"]
		failure.retryAt = time.Time{}
		service.importFailures["arr"]["hash"] = failure
		service.mu.Unlock()
	}

	runCleanup(t, service, "arr", 1)
	retryNow()
	clock.set(t0.Add(importRetryDelay - time.Second))
	runCleanup(t, service, "arr", 1)
	wantCount(t, "lookups", fake.lookups(), 2)
	wantNoDelete(t, fake)
	retryNow()
	clock.set(t0.Add(importRetryDelay))
	runCleanup(t, service, "arr", 1)
	wantCount(t, "lookups", fake.lookups(), 3)
	wantDelete(t, fake, false, 1)
}

func TestImportEndErrorRestartsTheClock(t *testing.T) {
	t.Parallel()
	fake := newImportEndServer(t, endRows("hash", 11), `[]`, map[int]int{11: 5})
	t0 := time.Unix(1000, 0)
	clock := &fakeClock{now: t0}
	service := importEndService(t, fake, Sonarr, config.QueueCleanup{}, clock)

	tickN(t, service, clock, maxImportAttempts-1)
	fake.setManualImport(`401`)
	tick(t, service, clock)
	fake.setManualImport(`[]`)
	clock.set(t0.Add(importRetryDelay + 40*time.Second))
	lookups := fake.lookups()
	tick(t, service, clock)
	wantCount(t, "lookups", fake.lookups(), lookups+1)
	wantNoDelete(t, fake)
	clock.advance(importRetryDelay)
	tick(t, service, clock)
	wantDelete(t, fake, false, 1)
}

func TestImportEndStatusChangeRestartsTheClock(t *testing.T) {
	t.Parallel()
	fake := newImportEndServer(t, endRows("hash", 11), `[]`, map[int]int{11: 5})
	t0 := time.Unix(1000, 0)
	clock := &fakeClock{now: t0}
	service := importEndService(t, fake, Sonarr, config.QueueCleanup{}, clock)

	change := t0.Add(importRetryDelay - 20*time.Second)
	tickUntil(t, service, clock, change)
	wantNoDelete(t, fake)
	changed := endRows("hash", 11)
	changed[0].StatusMessages[0].Title = changedStatus
	fake.records.Store(&changed)
	tickUntil(t, service, clock, change.Add(importRetryDelay-time.Second))
	wantNoDelete(t, fake)
	tickUntil(t, service, clock, change.Add(importRetryDelay+time.Minute))
	wantDelete(t, fake, false, 1)
}

func TestImportEndNeedsTheWholeDownloadAdmitted(t *testing.T) {
	t.Parallel()
	fake := newImportEndServer(t, endRows("hash", 11, 12), wrongEpisodeBody(), map[int]int{11: 5, 12: 6})
	service := importEndService(t, fake, Sonarr, config.QueueCleanup{}, &fakeClock{now: time.Unix(1000, 0)})
	var calls atomic.Int64
	service.SetQueueGuardHooks(QueueGuardHooks{Provider: func(string) string {
		if calls.Add(1) == 1 {
			return "rd"
		}
		return "torrent"
	}})

	runCleanup(t, service, "arr", 1)
	wantDelete(t, fake, true, 1, 2)
	service.guard.mu.Lock()
	defer service.guard.mu.Unlock()
	if len(service.guard.buckets) != 1 {
		t.Fatalf("guard buckets = %d, want 1", len(service.guard.buckets))
	}
	bucket := service.guard.buckets[guardKey{"arr", wrongEpisodeRule, "rd"}]
	if bucket == nil {
		t.Fatalf("no bucket %s/rd; buckets: %v", wrongEpisodeRule, service.guard.buckets)
	}
	if _, ok := bucket.sent["hash"]; !ok {
		t.Fatalf("bucket sent = %v, want the download", bucket.sent)
	}
}

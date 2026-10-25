package arr

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
)

type seasonKey struct{ series, season int }

// researchArr is a Sonarr that serves a mutable queue, its download client settings, grab history and season
// episode lists, and records the bulk deletes per skipRedownload value and the search commands it is sent.
type researchArr struct {
	appName string // what api/v3/system/status reports; "" answers 404

	mu             sync.Mutex
	records        []QueueSchema
	settings       *DownloadClientConfig // nil answers 401
	interactive    map[string]bool       // downloadID → grabbed by interactive search
	historyFails   map[string]bool
	noGrab         map[string]bool
	episodeFails   bool
	searchHangsUp  bool // the connection drops after the command is read
	searchLost     bool // with searchHangsUp, the arr never queues it
	received       []Command
	commandsFail   bool
	commandReads   int
	clock          *fakeClock // when set, stamps received commands and is advanced by replyDelay before a hang-up
	replyDelay     time.Duration
	seasons        map[seasonKey][]int
	blocklistFails bool
	removeBefore   int // ids a failing bulk delete still removes
	searchFails    bool
	commands       int
	deletes        map[string][]int
	leftovers      []int    // ids removed with blocklist=false
	leftoverFlags  []string // removeFromClient,skipRedownload of each leftover delete
	searches       [][]int
	seasonSearches []seasonKey
}

func newResearchArr(records ...QueueSchema) *researchArr {
	return &researchArr{
		records:     slices.Clone(records),
		settings:    &DownloadClientConfig{AutoRedownloadFailed: true, AutoRedownloadFailedFromInteractiveSearch: true},
		interactive: make(map[string]bool), historyFails: make(map[string]bool), noGrab: make(map[string]bool),
		seasons: make(map[seasonKey][]int), deletes: make(map[string][]int),
	}
}

func (a *researchArr) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	query := r.URL.Query()
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/api/v3/queue":
		body, _ := json.Marshal(QueueResponseScheme{
			Page: 1, PageSize: 200, TotalRecords: len(a.records), Records: a.records,
		})
		_, _ = w.Write(body)
	case r.Method == http.MethodGet && r.URL.Path == "/api/v3/system/status" && a.appName != "":
		_, _ = fmt.Fprintf(w, `{"appName":%q}`, a.appName)
	case r.Method == http.MethodGet && r.URL.Path == "/api/v3/config/downloadclient":
		if a.settings == nil {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		body, _ := json.Marshal(a.settings)
		_, _ = w.Write(body)
	case r.Method == http.MethodGet && r.URL.Path == "/api/v3/history":
		a.history(w, query)
	case r.Method == http.MethodGet && r.URL.Path == "/api/v3/episode":
		a.episodes(w, query)
	case r.Method == http.MethodDelete && r.URL.Path == "/api/v3/queue/bulk":
		a.bulkDelete(w, r)
	case r.Method == http.MethodGet && r.URL.Path == "/api/v3/command":
		a.commandReads++
		if a.commandsFail {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		body, _ := json.Marshal(a.received)
		_, _ = w.Write(body)
	case r.Method == http.MethodPost && r.URL.Path == "/api/v3/command":
		a.command(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (a *researchArr) history(w http.ResponseWriter, query url.Values) {
	downloadID := query.Get("downloadId")
	if a.historyFails[downloadID] {
		http.Error(w, "no", http.StatusUnauthorized)
		return
	}
	if a.noGrab[downloadID] {
		_, _ = w.Write([]byte(`{"page":1,"totalRecords":0,"records":[]}`))
		return
	}
	source := "Search"
	if a.interactive[downloadID] {
		source = "InteractiveSearch"
	}
	_, _ = fmt.Fprintf(w, `{"page":1,"totalRecords":1,"records":[`+
		`{"id":1,"downloadId":%q,"eventType":"grabbed","data":{"releaseSource":%q}}]}`, downloadID, source)
}

func (a *researchArr) episodes(w http.ResponseWriter, query url.Values) {
	if a.episodeFails {
		http.Error(w, "no", http.StatusUnauthorized)
		return
	}
	series, _ := strconv.Atoi(query.Get("seriesId"))
	season, _ := strconv.Atoi(query.Get("seasonNumber"))
	episodes := make([]map[string]int, 0)
	for _, id := range a.seasons[seasonKey{series, season}] {
		episodes = append(episodes, map[string]int{"id": id, "seasonNumber": season})
	}
	body, _ := json.Marshal(episodes)
	_, _ = w.Write(body)
}

func (a *researchArr) bulkDelete(w http.ResponseWriter, r *http.Request) {
	var body struct {
		IDs []int `json:"ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	query := r.URL.Query()
	skip := query.Get("skipRedownload")
	if query.Get("blocklist") == "false" {
		a.records = slices.DeleteFunc(
			a.records,
			func(record QueueSchema) bool { return slices.Contains(body.IDs, record.ID) },
		)
		a.leftovers = append(a.leftovers, body.IDs...)
		a.leftoverFlags = append(a.leftoverFlags, query.Get("removeFromClient")+","+skip)
		w.WriteHeader(http.StatusOK)
		return
	}
	removed := body.IDs
	if a.blocklistFails && skip == "true" {
		removed = body.IDs[:min(a.removeBefore, len(body.IDs))]
	}
	a.records = slices.DeleteFunc(a.records, func(record QueueSchema) bool {
		return slices.Contains(removed, record.ID)
	})
	if a.blocklistFails && skip == "true" {
		http.Error(w, "no", http.StatusInternalServerError)
		return
	}
	a.deletes[skip] = append(a.deletes[skip], body.IDs...)
	w.WriteHeader(http.StatusOK)
}

func (a *researchArr) command(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name         string `json:"name"`
		EpisodeIDs   []int  `json:"episodeIds"`
		SeriesID     int    `json:"seriesId"`
		SeasonNumber int    `json:"seasonNumber"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	a.commands++
	if !a.searchFails && !a.searchLost {
		queued := time.Now()
		if a.clock != nil {
			queued = a.clock.Now()
		}
		a.received = append(a.received, Command{Queued: queued, Body: CommandBody{
			Name: body.Name, EpisodeIDs: body.EpisodeIDs, SeriesID: body.SeriesID, SeasonNumber: body.SeasonNumber,
		}})
	}
	if a.searchHangsUp {
		if a.clock != nil {
			a.clock.advance(a.replyDelay)
		}
		hijacked, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = hijacked.Close()
		}
		return
	}
	if a.searchFails {
		http.Error(w, "no", http.StatusServiceUnavailable)
		return
	}
	switch body.Name {
	case "EpisodeSearch":
		a.searches = append(a.searches, body.EpisodeIDs)
	case "SeasonSearch":
		a.seasonSearches = append(a.seasonSearches, seasonKey{body.SeriesID, body.SeasonNumber})
	default:
		http.Error(w, "unexpected command "+body.Name, http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusCreated)
	_, _ = w.Write([]byte(`{"id":1,"status":"queued"}`))
}

func (a *researchArr) update(change func(a *researchArr)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	change(a)
}

// missingRecord is a queue record of series 7 season 1 that the episodes_missing rule blocklists and re-searches.
func missingRecord(id, episodeID int, downloadID string, hasFile bool) QueueSchema {
	record := QueueSchema{
		ID: id, EpisodeID: episodeID, SeriesID: 7, SeasonNumber: 1, DownloadID: downloadID, EpisodeHasFile: hasFile,
		Status: "completed", TrackedDownloadStatus: "warning", TrackedDownloadState: "importBlocked",
	}
	record.StatusMessages = append(record.StatusMessages, struct {
		Title    string   `json:"title"`
		Messages []string `json:"messages"`
	}{Title: "One or more episodes expected in this release were not imported or missing from the release"})
	return record
}

func researchService(t *testing.T, fake *researchArr, kind Type) *Service {
	t.Helper()
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)
	cfg := config.NewStore(&config.Config{QueueCleanup: config.QueueCleanup{Rules: config.DefaultQueueCleanupRules()}})
	service := New(cfg, nil, zerolog.Nop())
	service.arrs["tv"] = Arr{Name: "tv", Host: server.URL, Token: "secret", Type: kind}
	return service
}

type researchWant struct {
	leftovers      []int
	arrSearch      []int
	skipped        []int
	searches       [][]int
	seasonSearches []seasonKey
}

func (a *researchArr) check(t *testing.T, want researchWant) {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	if !slices.Equal(a.leftovers, want.leftovers) {
		t.Errorf("leftover ids = %v, want %v", a.leftovers, want.leftovers)
	}
	for _, flags := range a.leftoverFlags {
		if flags != "false,false" {
			t.Errorf("leftover delete removeFromClient,skipRedownload = %s, want false,false", flags)
		}
	}
	if got := a.deletes["false"]; !slices.Equal(got, want.arrSearch) {
		t.Errorf("skipRedownload=false ids = %v, want %v", got, want.arrSearch)
	}
	if got := a.deletes["true"]; !slices.Equal(got, want.skipped) {
		t.Errorf("skipRedownload=true ids = %v, want %v", got, want.skipped)
	}
	if !slices.EqualFunc(a.searches, want.searches, slices.Equal) {
		t.Errorf("episode searches = %v, want %v", a.searches, want.searches)
	}
	if !slices.Equal(a.seasonSearches, want.seasonSearches) {
		t.Errorf("season searches = %v, want %v", a.seasonSearches, want.seasonSearches)
	}
}

func mixedQueue() []QueueSchema {
	unmapped := missingRecord(5, 0, "mixed", false)
	unmapped.SeriesID = 0
	return []QueueSchema{
		missingRecord(1, 11, "onDisk", true), missingRecord(2, 12, "onDisk", true),
		missingRecord(3, 13, "partial", true), missingRecord(4, 14, "partial", false),
		unmapped, missingRecord(6, 16, "mixed", false),
	}
}

// A mapped Sonarr download is blocklisted without the arr's re-search, which would cover every episode of
// the download; decypharr then searches only the episodes without a file. A download with an unmapped record,
// or an instance not known to be Sonarr, keeps the arr's.
func TestCleanupQueueSearchesOnlyEpisodesWithoutFiles(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		kind    Type
		appName string
		want    researchWant
	}{
		{name: "sonarr", kind: Sonarr, want: researchWant{
			leftovers: []int{1, 2}, arrSearch: []int{5, 6}, skipped: []int{3, 4}, searches: [][]int{{14}},
		}},
		{name: "probed sonarr", kind: Others, appName: "Sonarr", want: researchWant{
			leftovers: []int{1, 2}, arrSearch: []int{5, 6}, skipped: []int{3, 4}, searches: [][]int{{14}},
		}},
		{name: "unknown type", kind: Others, want: researchWant{leftovers: []int{1, 2}, arrSearch: []int{3, 4, 5, 6}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fake := newResearchArr(mixedQueue()...)
			fake.appName = tt.appName
			runCleanup(t, researchService(t, fake, tt.kind), "tv", 1)
			fake.check(t, tt.want)
		})
	}
}

// decypharr's search follows Sonarr's own failed-download settings; what it cannot read, Sonarr decides.
func TestCleanupQueueSearchFollowsSonarrRedownloadSettings(t *testing.T) {
	t.Parallel()
	queue := []QueueSchema{
		missingRecord(1, 11, "auto", false), missingRecord(2, 12, "picked", false), missingRecord(3, 13, "lost", false),
	}
	tests := []struct {
		name   string
		change func(a *researchArr)
		want   researchWant
	}{
		{name: "redownload off", change: func(a *researchArr) { a.settings.AutoRedownloadFailed = false },
			want: researchWant{skipped: []int{1, 2, 3}}},
		{name: "interactive grab not redownloaded", change: func(a *researchArr) {
			a.settings.AutoRedownloadFailedFromInteractiveSearch = false
			a.interactive["picked"] = true
		}, want: researchWant{skipped: []int{1, 2, 3}, searches: [][]int{{11, 13}}}},
		{name: "interactive grab redownloaded", change: func(a *researchArr) { a.interactive["picked"] = true },
			want: researchWant{skipped: []int{1, 2, 3}, searches: [][]int{{11, 12, 13}}}},
		{name: "no grab history", change: func(a *researchArr) {
			a.settings.AutoRedownloadFailedFromInteractiveSearch = false
			a.noGrab["picked"] = true
		}, want: researchWant{skipped: []int{1, 2, 3}, searches: [][]int{{11, 12, 13}}}},
		{name: "settings unreadable", change: func(a *researchArr) { a.settings = nil },
			want: researchWant{arrSearch: []int{1, 2, 3}}},
		{name: "grab history unreadable", change: func(a *researchArr) {
			a.settings.AutoRedownloadFailedFromInteractiveSearch = false
			a.historyFails["lost"] = true
		}, want: researchWant{arrSearch: []int{3}, skipped: []int{1, 2}, searches: [][]int{{11, 12}}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fake := newResearchArr(queue...)
			fake.update(tt.change)
			runCleanup(t, researchService(t, fake, Sonarr), "tv", 1)
			fake.check(t, tt.want)
		})
	}
}

// A pack whose every episode has a file did its job: its leftover row is removed without a blocklist or a search.
func TestCleanupQueueRemovesCompletePackLeftovers(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		records []QueueSchema
		want    researchWant
	}{
		{name: "complete pack across two records",
			records: []QueueSchema{missingRecord(1, 11, "p", true), missingRecord(2, 12, "p", true)},
			want:    researchWant{leftovers: []int{1, 2}}},
		{name: "one episode missing",
			records: []QueueSchema{missingRecord(1, 11, "p", true), missingRecord(2, 12, "p", false)},
			want:    researchWant{skipped: []int{1, 2}, searches: [][]int{{12}}}},
		{name: "missing episode listed first",
			records: []QueueSchema{missingRecord(1, 11, "p", false), missingRecord(2, 12, "p", true)},
			want:    researchWant{skipped: []int{1, 2}, searches: [][]int{{11}}}},
		{name: "empty download id",
			records: []QueueSchema{missingRecord(1, 11, "", true)},
			want:    researchWant{skipped: []int{1}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fake := newResearchArr(tt.records...)
			runCleanup(t, researchService(t, fake, Sonarr), "tv", 1)
			fake.check(t, tt.want)
		})
	}
}

// A search the arr refused after the blocklist went through is retried a minute later, once.
func TestCleanupQueueRetriesAFailedSearch(t *testing.T) {
	t.Parallel()
	fake := newResearchArr(missingRecord(1, 11, "a", false), missingRecord(2, 12, "a", true))
	fake.searchFails = true
	service := researchService(t, fake, Sonarr)
	clock := &fakeClock{now: time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)}
	service.now = clock.Now
	runCleanup(t, service, "tv", 1)
	fake.check(t, researchWant{skipped: []int{1, 2}})

	fake.update(func(a *researchArr) { a.searchFails = false })
	clock.advance(searchRetryDelay)
	runCleanup(t, service, "tv", 2)
	fake.check(t, researchWant{skipped: []int{1, 2}, searches: [][]int{{11}}})
}

// When a bulk delete fails part way, the downloads it removed are searched next tick; those still queued go
// through the cleanup rules again.
func TestCleanupQueueSearchesDownloadsAPartialBlocklistRemoved(t *testing.T) {
	t.Parallel()
	fake := newResearchArr(missingRecord(1, 11, "gone", false), missingRecord(2, 12, "stays", false))
	fake.blocklistFails, fake.removeBefore = true, 1
	service := researchService(t, fake, Sonarr)
	runCleanup(t, service, "tv", 1)
	fake.check(t, researchWant{})

	fake.update(func(a *researchArr) { a.blocklistFails = false })
	runCleanup(t, service, "tv", 1)
	fake.check(t, researchWant{skipped: []int{2}, searches: [][]int{{11, 12}}})
}

// A pending search whose record is back in the queue untouched by the rules is dropped, not searched.
func TestCleanupQueueDropsPendingSearchForARecordStillQueued(t *testing.T) {
	t.Parallel()
	fake := newResearchArr(missingRecord(1, 11, "a", false))
	fake.blocklistFails = true
	service := researchService(t, fake, Sonarr)
	runCleanup(t, service, "tv", 1)

	fake.update(func(a *researchArr) {
		a.blocklistFails = false
		a.records[0].TrackedDownloadState = "importing"
	})
	runCleanup(t, service, "tv", 1)
	fake.update(func(a *researchArr) { a.records = nil })
	runCleanup(t, service, "tv", 1)
	fake.check(t, researchWant{})
}

// Every episode of a season missing gets one SeasonSearch, as Sonarr's own re-search would send; a partly missing
// season, or one whose episodes the arr can't list, keeps the EpisodeSearch.
func TestCleanupQueueSearchesAWhollyMissingSeasonAsOne(t *testing.T) {
	t.Parallel()
	inSeason := func(number int, record QueueSchema) QueueSchema {
		record.SeasonNumber = number
		return record
	}
	queue := []QueueSchema{
		missingRecord(1, 11, "pack1", false), missingRecord(2, 12, "pack1", false),
		inSeason(2, missingRecord(3, 21, "pack2", false)), inSeason(2, missingRecord(4, 22, "pack2", true)),
		inSeason(3, missingRecord(5, 31, "pack3", false)), inSeason(3, missingRecord(6, 32, "pack3", false)),
	}
	tests := []struct {
		name         string
		episodeFails bool
		want         researchWant
	}{
		{name: "season listed", want: researchWant{
			skipped: []int{1, 2, 3, 4, 5, 6}, searches: [][]int{{21, 31, 32}}, seasonSearches: []seasonKey{{7, 1}},
		}},
		{name: "season unlisted", episodeFails: true, want: researchWant{
			skipped: []int{1, 2, 3, 4, 5, 6}, searches: [][]int{{11, 12, 21, 31, 32}},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fake := newResearchArr(queue...)
			fake.episodeFails = tt.episodeFails
			fake.seasons[seasonKey{7, 1}] = []int{11, 12}
			fake.seasons[seasonKey{7, 2}] = []int{21, 22}
			fake.seasons[seasonKey{7, 3}] = []int{31, 32, 33}
			runCleanup(t, researchService(t, fake, Sonarr), "tv", 1)
			fake.check(t, tt.want)
		})
	}
}

// A search whose response never came is sent again only if the arr's command list shows no search for it queued
// since it was sent.
func TestCleanupQueueResendsASearchOfUnknownOutcomeOnlyIfLost(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name         string
		season       bool
		lost         bool
		commandsFail bool
		replyDelay   time.Duration
		wantResent   bool
	}{
		{name: "queued", wantResent: false},
		{name: "queued, reply timed out", replyDelay: 30 * time.Second, wantResent: false},
		{name: "lost", lost: true, wantResent: true},
		{name: "command list unreadable", lost: true, commandsFail: true, wantResent: false},
		{name: "season queued", season: true, wantResent: false},
		{name: "season lost", season: true, lost: true, wantResent: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			records := []QueueSchema{missingRecord(1, 11, "a", false)}
			want := researchWant{skipped: []int{1}}
			if tc.season {
				records = append(records, missingRecord(2, 12, "a", false))
				want.skipped = []int{1, 2}
			}
			fake := newResearchArr(records...)
			fake.seasons[seasonKey{7, 1}] = []int{11, 12}
			clock := &fakeClock{now: time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)}
			fake.clock, fake.replyDelay = clock, tc.replyDelay
			earlier := clock.Now().Add(-time.Minute)
			fake.received = []Command{
				{Queued: clock.Now(), Body: CommandBody{Name: "EpisodeSearch", EpisodeIDs: []int{99}}},
				{Queued: earlier, Body: CommandBody{Name: "EpisodeSearch", EpisodeIDs: []int{11, 12}}},
				{Queued: earlier, Body: CommandBody{Name: "SeasonSearch", SeriesID: 7, SeasonNumber: 1}},
			}
			fake.searchHangsUp, fake.searchLost = true, tc.lost
			service := researchService(t, fake, Sonarr)
			service.now = clock.Now
			runCleanup(t, service, "tv", 1)
			fake.update(func(a *researchArr) {
				a.searchHangsUp, a.searchLost, a.commandsFail = false, false, tc.commandsFail
			})
			clock.advance(searchRetryDelay)
			runCleanup(t, service, "tv", 2)

			wantCommands := 1
			if tc.wantResent {
				wantCommands = 2
				if tc.season {
					want.seasonSearches = []seasonKey{{7, 1}}
				} else {
					want.searches = [][]int{{11}}
				}
			}
			fake.check(t, want)
			fake.update(func(a *researchArr) {
				if a.commands != wantCommands {
					t.Errorf("search commands sent = %d, want %d", a.commands, wantCommands)
				}
			})
		})
	}
}

// A search the arr keeps refusing is given up after an hour instead of being retried every tick.
func TestCleanupQueueGivesUpOnASearchRefusedForAnHour(t *testing.T) {
	t.Parallel()
	fake := newResearchArr(missingRecord(1, 11, "a", false))
	fake.searchFails = true
	service := researchService(t, fake, Sonarr)
	clock := &fakeClock{now: time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)}
	service.now = clock.Now
	runCleanup(t, service, "tv", 1)
	clock.advance(time.Hour - time.Second)
	runCleanup(t, service, "tv", 1)
	clock.advance(time.Second)
	runCleanup(t, service, "tv", 1)
	fake.update(func(a *researchArr) { a.searchFails = false })
	runCleanup(t, service, "tv", 1)

	fake.check(t, researchWant{skipped: []int{1}})
	fake.update(func(a *researchArr) {
		if a.commands != 2 {
			t.Errorf("search commands sent = %d, want 2", a.commands)
		}
	})
}

// A refused search waits a minute before it is sent again, not every tick.
func TestCleanupQueueBacksOffARefusedSearch(t *testing.T) {
	t.Parallel()
	fake := newResearchArr(missingRecord(1, 11, "a", false))
	fake.searchFails = true
	service := researchService(t, fake, Sonarr)
	clock := &fakeClock{now: time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)}
	service.now = clock.Now
	runCleanup(t, service, "tv", 1)
	clock.advance(10 * time.Second)
	runCleanup(t, service, "tv", 1)
	clock.advance(50 * time.Second)
	runCleanup(t, service, "tv", 1)

	fake.update(func(a *researchArr) {
		if a.commands != 2 {
			t.Errorf("search commands sent = %d, want 2", a.commands)
		}
	})
}

// A search held for an hour because the arr's command list stayed unreadable is given up without claiming the
// arr refused it.
func TestCleanupQueueGivesUpOnAHeldSearchNeutrally(t *testing.T) {
	t.Parallel()
	fake := newResearchArr(missingRecord(1, 11, "a", false))
	fake.searchHangsUp, fake.searchLost = true, true
	service := researchService(t, fake, Sonarr)
	clock := &fakeClock{now: time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)}
	service.now = clock.Now
	var logs bytes.Buffer
	service.logger = zerolog.New(&logs).Level(zerolog.WarnLevel)
	runCleanup(t, service, "tv", 1)
	fake.update(func(a *researchArr) { a.searchHangsUp, a.searchLost, a.commandsFail = false, false, true })
	clock.advance(30 * time.Minute)
	runCleanup(t, service, "tv", 1)
	clock.advance(30 * time.Minute)
	runCleanup(t, service, "tv", 1)

	got := logs.String()
	if !strings.Contains(got, "episode search not done after 1h") || strings.Contains(got, "still refused") {
		t.Fatalf("give-up log = %s, want a neutral give-up", got)
	}
}

// A search of unknown outcome held while the arr's command list is unreadable reads that list at most once per
// retry delay, not every tick.
func TestCleanupQueueReadsCommandsForAHeldSearchOncePerRetryDelay(t *testing.T) {
	t.Parallel()
	fake := newResearchArr(missingRecord(1, 11, "a", false))
	fake.searchHangsUp, fake.searchLost = true, true
	service := researchService(t, fake, Sonarr)
	clock := &fakeClock{now: time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)}
	service.now = clock.Now
	runCleanup(t, service, "tv", 1)
	fake.update(func(a *researchArr) { a.searchHangsUp, a.searchLost, a.commandsFail = false, false, true })
	for _, step := range []time.Duration{20 * time.Second, 20 * time.Second, 20 * time.Second, 30 * time.Second, 30 * time.Second} {
		clock.advance(step)
		runCleanup(t, service, "tv", 2)
	}

	fake.update(func(a *researchArr) {
		if a.commandReads != 2 {
			t.Errorf("command list reads = %d, want 2", a.commandReads)
		}
		if a.commands != 1 {
			t.Errorf("search commands sent = %d, want 1", a.commands)
		}
	})
}

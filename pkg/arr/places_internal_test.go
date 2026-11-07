package arr

import (
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
)

// cappedService is a Sonarr service whose arr may have only perArr distinct downloads blocklisted per window, so
// the cap is the boundary: a place that is not given back shows as a download held that should have gone.
func cappedService(t *testing.T, fake *researchArr, perArr int) *Service {
	t.Helper()
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)
	cfg := config.NewStore(&config.Config{QueueCleanup: config.QueueCleanup{
		Rules: config.DefaultQueueCleanupRules(), BlocklistPerArr: perArr,
	}})
	service := New(cfg, nil, zerolog.Nop())
	service.arrs["tv"] = Arr{Name: "tv", Host: server.URL, Token: "secret", Type: Sonarr}
	clock := &fakeClock{now: time.Unix(1_000_000, 0)}
	service.now = clock.Now
	return service
}

// failedPlainDownloads are failed downloads the arr could not map to episodes: blocklisted without any owed search,
// so they need neither the ledger nor the arr's re-search.
func failedPlainDownloads(prefix string, count, firstID int) []QueueSchema {
	var rows []QueueSchema
	for i := range count {
		rows = append(rows, failedRows(fmt.Sprintf("%s-%02d", prefix, i), firstID+i, 1)...)
	}
	return rows
}

func missingDownloads(prefix string, count, firstID int) []QueueSchema {
	var rows []QueueSchema
	for i := range count {
		rows = append(rows, missingRecord(firstID+i, 1000+firstID+i, fmt.Sprintf("%s-%02d", prefix, i), false))
	}
	return rows
}

func replaceQueue(fake *researchArr, rows []QueueSchema) {
	fake.update(func(a *researchArr) { a.records = rows })
}

// A blocklist the arr refused was never sent, so its downloads give their places back: at the arr's cap, a
// different download is admitted afterwards. (Retrying the same download proves nothing: it passes anyway.)
func TestRefusedCleanupBlocklistGivesItsPlacesBack(t *testing.T) {
	t.Parallel()
	fake := newResearchArr(missingDownloads("first", 5, 1)...)
	fake.deleteRefused = true
	service := cappedService(t, fake, 5)

	runCleanup(t, service, "tv", 2)
	if got := len(blocklisted(fake)); got != 0 {
		t.Fatalf("setup: %d rows were blocklisted despite the refusal", got)
	}
	replaceQueue(fake, failedPlainDownloads("other", 5, 100))
	runCleanup(t, service, "tv", 1)
	if left := len(queueIDs(fake)); left != 0 {
		t.Fatalf("%d of 5 different downloads still queued: the refused ones kept the arr's places", left)
	}
}

// If the owed searches cannot be recorded nothing is removed, so the downloads give their places back as well.
func TestUnrecordedOwedSearchesGiveTheirPlacesBack(t *testing.T) {
	t.Parallel()
	fake := newResearchArr(missingDownloads("first", 5, 1)...)
	service := cappedService(t, fake, 5)
	if err := service.OpenSearchLedger(filepath.Join(t.TempDir(), "db", "searches.db")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close() })
	// The ledger's file is gone from under it: every write now fails.
	if err := service.ledger.close(); err != nil {
		t.Fatal(err)
	}

	runCleanup(t, service, "tv", 2)
	if got := len(blocklisted(fake)); got != 0 {
		t.Fatalf("%d rows were removed although their searches could not be recorded", got)
	}
	replaceQueue(fake, failedPlainDownloads("other", 5, 100))
	runCleanup(t, service, "tv", 1)
	if left := len(queueIDs(fake)); left != 0 {
		t.Fatalf("%d of 5 different downloads still queued: the unrecorded ones kept the arr's places", left)
	}
}

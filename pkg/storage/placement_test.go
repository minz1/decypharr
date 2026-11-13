package storage_test

import (
	"math/rand"
	"testing"
	"time"

	debridTypes "github.com/sirrobot01/decypharr/pkg/debrid/types"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

func downloaded(provider string, at time.Time) *storage.ProviderEntry {
	return &storage.ProviderEntry{
		Provider: provider, ID: provider + "-id", Status: debridTypes.TorrentStatusDownloaded, DownloadedAt: &at,
	}
}

func shuffledPlacements(rng *rand.Rand, src map[string]*storage.ProviderEntry) map[string]*storage.ProviderEntry {
	keys := make([]string, 0, len(src))
	for k := range src {
		keys = append(keys, k)
	}
	rng.Shuffle(len(keys), func(i, j int) { keys[i], keys[j] = keys[j], keys[i] })
	out := make(map[string]*storage.ProviderEntry, len(src))
	for _, k := range keys {
		out[k] = src[k]
	}
	return out
}

func TestBestPlacementIsDeterministicAndOrdered(t *testing.T) {
	t.Parallel()
	now := time.Now()
	src := map[string]*storage.ProviderEntry{
		"alldebrid":  downloaded("alldebrid", now),
		"realdebrid": downloaded("realdebrid", now.Add(-time.Hour)),
		"torbox":     downloaded("torbox", now.Add(time.Hour)),
		"premiumize": {Provider: "premiumize", Status: debridTypes.TorrentStatusDownloading},
	}
	cases := []struct {
		name  string
		order []string
		want  string
	}{
		{"config order wins", []string{"realdebrid", "torbox"}, "realdebrid"},
		{"unlisted lose to listed", []string{"alldebrid"}, "alldebrid"},
		{"newest download without order", nil, "torbox"},
	}
	for _, c := range cases {
		rng := rand.New(rand.NewSource(1))
		for range 100 {
			if got := storage.BestPlacement(shuffledPlacements(rng, src), c.order); got != c.want {
				t.Fatalf("%s: got %q want %q", c.name, got, c.want)
			}
		}
	}
	tied := map[string]*storage.ProviderEntry{"b": downloaded("b", now), "a": downloaded("a", now)}
	if got := storage.BestPlacement(tied, nil); got != "a" {
		t.Fatalf("name tiebreak: got %q", got)
	}
	if got := storage.BestPlacement(map[string]*storage.ProviderEntry{"p": src["premiumize"]}, nil); got != "" {
		t.Fatalf("no downloaded placement: got %q", got)
	}
}

func TestSetActiveRules(t *testing.T) {
	t.Parallel()
	now := time.Now()
	e := &storage.Entry{Providers: map[string]*storage.ProviderEntry{
		"a": downloaded("a", now),
		"b": {Provider: "b", Status: debridTypes.TorrentStatusDownloading},
	}}
	if !e.SetActive("b", false) || e.ActiveProvider != "b" {
		t.Fatalf("first placement must be accepted, active=%q", e.ActiveProvider)
	}
	if e.SetActive("zzz", false) || e.ActiveProvider != "b" {
		t.Fatalf("switch to a missing placement must be refused, active=%q", e.ActiveProvider)
	}
	e.Providers["c"] = &storage.ProviderEntry{Provider: "c", Status: debridTypes.TorrentStatusDownloading}
	if e.SetActive("c", false) || e.ActiveProvider != "b" {
		t.Fatalf("switch to a non-Downloaded placement must be refused, active=%q", e.ActiveProvider)
	}
	if !e.SetActive("c", true) || e.ActiveProvider != "c" {
		t.Fatalf("submit path must be accepted, active=%q", e.ActiveProvider)
	}
	if !e.SetActive("a", false) || e.ActiveProvider != "a" {
		t.Fatalf("switch to Downloaded must be accepted, active=%q", e.ActiveProvider)
	}
	e.ClearActive()
	if e.ActiveProvider != "" {
		t.Fatalf("ClearActive left %q", e.ActiveProvider)
	}
}

func TestRemoveProviderPicksBestOrClears(t *testing.T) {
	t.Parallel()
	now := time.Now()
	e := &storage.Entry{Providers: map[string]*storage.ProviderEntry{
		"a": downloaded("a", now),
		"b": downloaded("b", now),
		"c": downloaded("c", now),
	}}
	e.SetActive("a", true)
	e.RemoveProvider("a", []string{"c", "b"}, nil)
	if e.ActiveProvider != "c" {
		t.Fatalf("active=%q want c", e.ActiveProvider)
	}
	e.Providers["d"] = &storage.ProviderEntry{Provider: "d", Status: debridTypes.TorrentStatusDownloading}
	e.RemoveProvider("b", nil, nil)
	e.RemoveProvider("c", nil, nil)
	if e.ActiveProvider != "" {
		t.Fatalf("a dropped active with no Downloaded left must clear, got %q", e.ActiveProvider)
	}
}

func TestAddTorrentProviderKeepsTimestampsForTheSameID(t *testing.T) {
	t.Parallel()
	e := &storage.Entry{}
	first := e.AddTorrentProvider(
		&debridTypes.Torrent{Debrid: "torbox", ID: "one", Status: debridTypes.TorrentStatusDownloaded},
	)
	added := time.Now().Add(-48 * time.Hour)
	done := added.Add(time.Hour)
	first.AddedAt, first.DownloadedAt = added, &done

	same := e.AddTorrentProvider(
		&debridTypes.Torrent{Debrid: "torbox", ID: "one", Status: debridTypes.TorrentStatusDownloaded},
	)
	if !same.AddedAt.Equal(added) || same.DownloadedAt == nil || !same.DownloadedAt.Equal(done) {
		t.Fatalf("same id lost timestamps: %v %v", same.AddedAt, same.DownloadedAt)
	}
	other := e.AddTorrentProvider(
		&debridTypes.Torrent{Debrid: "torbox", ID: "two", Status: debridTypes.TorrentStatusDownloading},
	)
	if other.AddedAt.Equal(added) || other.DownloadedAt != nil {
		t.Fatalf("new id kept timestamps: %v %v", other.AddedAt, other.DownloadedAt)
	}
}

func TestInvariantsFlagTheTwoViolations(t *testing.T) {
	t.Parallel()
	now := time.Now()
	ok := &storage.Entry{
		ActiveProvider: "a", IsComplete: true,
		Providers: map[string]*storage.ProviderEntry{"a": downloaded("a", now)},
	}
	if err := ok.Invariants(); err != nil {
		t.Fatalf("valid entry: %v", err)
	}
	dangling := &storage.Entry{ActiveProvider: "gone", Providers: map[string]*storage.ProviderEntry{}}
	if dangling.Invariants() == nil {
		t.Fatal("dangling active must violate")
	}
	noStamp := &storage.Entry{IsComplete: true, Providers: map[string]*storage.ProviderEntry{
		"a": {Provider: "a", Status: debridTypes.TorrentStatusDownloaded}}}
	if noStamp.Invariants() == nil {
		t.Fatal("complete entry with an unstamped Downloaded placement must violate")
	}
	noStamp.IsComplete = false
	if err := noStamp.Invariants(); err != nil {
		t.Fatalf("incomplete entry may lack the stamp: %v", err)
	}
}

func mergeActive(existingActive, incomingActive string, incoming map[string]*storage.ProviderEntry) string {
	existing := &storage.Entry{ActiveProvider: existingActive}
	in := &storage.Entry{ActiveProvider: incomingActive, Providers: incoming}
	return storage.HandleExistingEntryMerge(existing, in).ActiveProvider
}

func TestMergeRepairsOnlyAnEmptyIncomingActive(t *testing.T) {
	t.Parallel()
	now := time.Now()
	withY := map[string]*storage.ProviderEntry{"y": downloaded("y", now)}
	if got := mergeActive("x", "", withY); got != "y" {
		t.Fatalf("dangling inherited active with a Downloaded placement: got %q want y", got)
	}
	noneLeft := map[string]*storage.ProviderEntry{"z": {Provider: "z", Status: debridTypes.TorrentStatusDownloading}}
	if got := mergeActive("x", "", noneLeft); got != "" {
		t.Fatalf("dangling inherited active with no Downloaded placement: got %q want empty", got)
	}
	if got := mergeActive("x", "w", withY); got != "w" {
		t.Fatalf("non-empty incoming active must not change: got %q want w", got)
	}
}

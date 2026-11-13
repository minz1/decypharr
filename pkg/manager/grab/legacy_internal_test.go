package grab

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sirrobot01/decypharr/internal/config"
	debridTypes "github.com/sirrobot01/decypharr/pkg/debrid/types"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

const (
	packHash  = "0123456789abcdef0123456789abcdef01234567"
	otherHash = "89abcdef0123456789abcdef0123456789abcdef"
)

func addedOn() time.Time { return time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC) }
func updated() time.Time { return time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC) }

func legacy(state storage.TorrentState, status storage.EntryStatus) *storage.Entry {
	return &storage.Entry{
		Protocol: config.ProtocolTorrent, InfoHash: packHash, State: state, Status: status,
		AddedOn: addedOn(), UpdatedAt: updated(),
	}
}

// placedAt gives e an active placement with status and one file whose provider file is pf (nil: none).
func placedAt(e *storage.Entry, status debridTypes.TorrentStatus, pf *storage.ProviderFile) *storage.Entry {
	e.ActiveProvider = "rd"
	files := map[string]*storage.ProviderFile{}
	if pf != nil {
		files["a.mkv"] = pf
	}
	e.Providers = map[string]*storage.ProviderEntry{"rd": {Provider: "rd", Status: status, Files: files}}
	e.Files = map[string]*storage.File{"a.mkv": {Name: "a.mkv"}}
	return e
}

func TestSeasonHashMatchesTheManager(t *testing.T) {
	t.Parallel()
	sum := sha256.Sum256([]byte("abc-3"))
	want := hex.EncodeToString(sum[:16])
	if got := SeasonHash("abc", 3); got != want || len(got) != 32 {
		t.Errorf("SeasonHash = %q, want %q (32 hex digits)", got, want)
	}
}

func TestSeasonIndexMatchesAnUpperCasePack(t *testing.T) {
	t.Parallel()
	upper := strings.ToUpper(packHash)
	idx := SeasonIndex([]string{upper})
	if got := idx[SeasonHash(upper, 2)]; got != packHash {
		t.Errorf("index[child] = %q, want the lower-case pack %q", got, packHash)
	}
	if len(idx) != 99 {
		t.Errorf("index has %d children, want 99", len(idx))
	}
}

type legacyCase struct {
	name     string
	e        *storage.Entry
	packOf   map[string]string
	noLib    bool
	want     Phase
	pack     string
	reason   string
	noSubmit string
}

func legacyCases() []legacyCase {
	child := SeasonHash(packHash, 2)
	packOf := SeasonIndex([]string{packHash, child})
	season := func(state storage.TorrentState, complete bool) *storage.Entry {
		e := legacy(state, storage.EntryStatusDownloaded)
		e.InfoHash, e.IsComplete = child, complete
		return e
	}
	errComplete := legacy(storage.EntryStateError, storage.EntryStatusError)
	errComplete.IsComplete, errComplete.LastError = true, "boom"
	orphanDone := legacy("", "")
	orphanDone.IsComplete = true
	otherMagnet := legacy(storage.EntryStateError, storage.EntryStatusError)
	otherMagnet.Magnet = "magnet:?xt=urn:btih:" + otherHash
	badMagnet := legacy(storage.EntryStateError, storage.EntryStatusError)
	badMagnet.Magnet = "not a magnet"
	nzb := legacy(storage.EntryStateDownloading, storage.EntryStatusDownloading)
	nzb.Protocol, nzb.Magnet = config.ProtocolNZB, "/data/nzbs/x.nzb"
	deletedOnly := placedAt(legacy(storage.EntryStateDownloading, storage.EntryStatusDownloading),
		debridTypes.TorrentStatusDownloaded, &storage.ProviderFile{Link: "https://x"})
	deletedOnly.Files["a.mkv"].Deleted = true

	return []legacyCase{
		{name: "season error", e: season(storage.EntryStateError, false), packOf: packOf, want: PhaseFailed,
			pack: packHash, reason: "season failed"},
		{name: "season pausedUP", e: season(storage.EntryStatePausedUP, false), packOf: packOf, want: PhaseComplete,
			pack: packHash},
		{name: "season empty state", e: season("", false), packOf: packOf, want: PhaseActing, pack: packHash},
		{name: "season complete without library", e: season("", true), packOf: packOf, noLib: true,
			want: PhaseComplete, pack: packHash},
		{name: "orphan season complete", e: orphanDone, want: PhaseComplete},
		{name: "orphan season", e: legacy("", ""), want: PhaseFailed, reason: "orphan season"},
		{name: "error wins over complete", e: errComplete, want: PhaseFailed, reason: "boom"},
		{name: "magnet of another hash", e: otherMagnet, want: PhaseFailed, noSubmit: "unmatched season"},
		{name: "unparseable magnet", e: badMagnet, want: PhaseFailed},
		{name: "nzb file path in magnet", e: nzb, want: PhaseQueued},
		{name: "pausedUP", e: legacy(storage.EntryStatePausedUP, storage.EntryStatusDownloaded),
			want: PhaseComplete},
		{name: "pausedUP without library", e: legacy(storage.EntryStatePausedUP, storage.EntryStatusDownloaded),
			noLib: true, want: PhaseFailed, reason: "library entry missing"},
		{name: "queued", e: legacy(storage.EntryStateDownloading, storage.EntryStatusQueued), want: PhaseQueued},
		{name: "status downloaded", e: legacy(storage.EntryStateDownloading, storage.EntryStatusDownloaded),
			want: PhaseActing},
		{name: "acting without library stays", e: legacy(storage.EntryStateDownloading,
			storage.EntryStatusDownloaded), noLib: true, want: PhaseActing},
		{name: "placement downloaded with link", e: placedAt(legacy(storage.EntryStateDownloading,
			storage.EntryStatusDownloading), debridTypes.TorrentStatusDownloaded,
			&storage.ProviderFile{Link: "https://x"}), want: PhaseActing},
		{name: "placement downloaded without provider file", e: placedAt(legacy(storage.EntryStateDownloading,
			storage.EntryStatusDownloading), debridTypes.TorrentStatusDownloaded, nil), want: PhaseAwaitingLinks},
		{name: "placement downloaded with only deleted files", e: deletedOnly, want: PhaseAwaitingLinks},
		{name: "placement downloading", e: placedAt(legacy(storage.EntryStateDownloading,
			storage.EntryStatusDownloading), debridTypes.TorrentStatusDownloading, nil), want: PhaseProvider},
		{name: "downloading without placement", e: legacy(storage.EntryStateDownloading,
			storage.EntryStatusDownloading), want: PhaseQueued},
		{name: "pausedDL", e: legacy(storage.EntryStatePausedDL, storage.EntryStatusDownloading),
			want: PhaseFailed, reason: "unreadable legacy state pausedDL/downloading"},
		{name: "downloading with error status", e: legacy(storage.EntryStateDownloading,
			storage.EntryStatusError), want: PhaseFailed, reason: "unreadable legacy state downloading/error"},
	}
}

func TestFromLegacy(t *testing.T) {
	t.Parallel()
	for _, c := range legacyCases() {
		got := FromLegacy(c.e, c.packOf, !c.noLib)
		if got.Phase != c.want || got.PackKey != c.pack || got.NoSubmit != c.noSubmit {
			t.Errorf("%s: phase %v pack %q noSubmit %q, want %v %q %q",
				c.name, got.Phase, got.PackKey, got.NoSubmit, c.want, c.pack, c.noSubmit)
		}
		if c.reason != "" && got.Failure.Reason != c.reason {
			t.Errorf("%s: reason %q, want %q", c.name, got.Failure.Reason, c.reason)
		}
		if got.Key != strings.ToLower(c.e.InfoHash) || !got.PhaseSince.Equal(updated()) || got.NZB != c.e.IsNZB() {
			t.Errorf("%s: key %q since %v nzb %v", c.name, got.Key, got.PhaseSince, got.NZB)
		}
		if err := got.Validate(); err != nil {
			t.Errorf("%s: %v", c.name, err)
		}
	}
}

func TestPrepopulatedFilesWithoutLinkAwaitLinks(t *testing.T) {
	t.Parallel()
	// Real-Debrid fills a file's ID before its link.
	e := placedAt(legacy(storage.EntryStateDownloading, storage.EntryStatusDownloading),
		debridTypes.TorrentStatusDownloaded, &storage.ProviderFile{ID: "1", Path: "/a.mkv"})
	if !LacksLink(e) {
		t.Error("LacksLink = false for a file with an ID and no link")
	}
	if got := FromLegacy(e, nil, true); got.Phase != PhaseAwaitingLinks {
		t.Errorf("phase %v, want awaiting_links", got.Phase)
	}
}

func TestCompleteSeasonChildWithoutLibraryStaysComplete(t *testing.T) {
	t.Parallel()
	child := SeasonHash(packHash, 1)
	e := legacy(storage.EntryStatePausedUP, storage.EntryStatusDownloaded)
	e.InfoHash = child
	if got := FromLegacy(e, SeasonIndex([]string{packHash}), false); got.Phase != PhaseComplete {
		t.Errorf("phase %v, want complete: a season child never has a library record", got.Phase)
	}
}

func TestLegacyFailuresCarryATime(t *testing.T) {
	t.Parallel()
	lastErr := time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)
	child := SeasonHash(packHash, 2)
	failing := map[string]func() *storage.Entry{
		"season error": func() *storage.Entry {
			e := legacy(storage.EntryStateError, storage.EntryStatusError)
			e.InfoHash = child
			return e
		},
		"orphan":     func() *storage.Entry { return legacy("", "") },
		"unreadable": func() *storage.Entry { return legacy(storage.EntryStatePausedDL, "") },
		"error":      func() *storage.Entry { return legacy(storage.EntryStateError, storage.EntryStatusError) },
	}
	for name, mk := range failing {
		for _, tc := range []struct {
			set  func(*storage.Entry)
			want time.Time
		}{
			{func(e *storage.Entry) { e.LastErrorTime, e.ErrorCount = &lastErr, 3 }, lastErr},
			{func(*storage.Entry) {}, addedOn()},
			{func(e *storage.Entry) { e.AddedOn = time.Time{} }, updated()},
		} {
			e := mk()
			tc.set(e)
			got := FromLegacy(e, SeasonIndex([]string{packHash}), true)
			if got.Phase != PhaseFailed || !got.Failure.At.Equal(tc.want) || got.Failure.Count != e.ErrorCount {
				t.Errorf("%s: %v at %v count %d, want failed at %v count %d",
					name, got.Phase, got.Failure.At, got.Failure.Count, tc.want, e.ErrorCount)
			}
		}
	}
}

func ExampleSeasonHash() {
	fmt.Println(len(SeasonHash(packHash, 1)))
	// Output: 32
}

package grab

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/sirrobot01/decypharr/internal/utils"
	debridTypes "github.com/sirrobot01/decypharr/pkg/debrid/types"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

// seasonHashBytes truncates season IDs to 32 hex digits. IDs are persisted
// and reported to the arrs, so the length must not change.
const seasonHashBytes = 16

// maxSeason bounds the season numbers SeasonIndex derives children for.
const maxSeason = 99

// SeasonHash is the key of season season of the pack packHash.
func SeasonHash(packHash string, season int) string {
	sum := sha256.Sum256(fmt.Appendf(nil, "%s-%d", packHash, season))
	return hex.EncodeToString(sum[:seasonHashBytes])
}

// SeasonIndex maps each possible season child key of hashes to its pack key, both lower-case.
// The child is hashed from the pack hash as stored, since that is what wrote the child.
func SeasonIndex(hashes []string) map[string]string {
	idx := make(map[string]string, len(hashes)*maxSeason)
	for _, h := range hashes {
		for n := 1; n <= maxSeason; n++ {
			idx[strings.ToLower(SeasonHash(h, n))] = strings.ToLower(h)
		}
	}
	return idx
}

// LacksLink reports whether e has no live file, or a live file its active placement has no link for.
// A file ID is not a link: Real-Debrid fills IDs before links.
func LacksLink(e *storage.Entry) bool {
	var files map[string]*storage.ProviderFile
	if p := e.Providers[e.ActiveProvider]; p != nil {
		files = p.Files
	}
	live := false
	for name, f := range e.Files {
		if f == nil || f.Deleted {
			continue
		}
		live = true
		if pf := files[name]; pf == nil || pf.Link == "" {
			return true
		}
	}
	return !live
}

// FromLegacy maps a queue record written before grab phases existed to a grab state.
// packOf is SeasonIndex over every stored record; inLibrary says whether the entry has a library record.
func FromLegacy(e *storage.Entry, packOf map[string]string, inLibrary bool) State {
	st := State{Key: strings.ToLower(e.InfoHash), PhaseSince: e.UpdatedAt, NZB: e.IsNZB()}
	failed := func(reason string) State {
		var last time.Time
		if e.LastErrorTime != nil {
			last = *e.LastErrorTime
		}
		st.Phase = PhaseFailed
		st.Failure = Failure{Reason: reason, At: cmp.Or(last, e.AddedOn, e.UpdatedAt), Count: e.ErrorCount}
		return st
	}
	complete := e.State == storage.EntryStatePausedUP || e.IsComplete

	if pack, ok := packOf[st.Key]; ok {
		st.PackKey = pack
		switch {
		case e.State == storage.EntryStateError:
			return failed("season failed")
		case complete:
			st.Phase = PhaseComplete
		default:
			st.Phase = PhaseActing
		}
		return st
	}
	if e.State == "" {
		if e.IsComplete {
			st.Phase = PhaseComplete
			return st
		}
		return failed("orphan season")
	}
	if !st.NZB && e.Magnet != "" {
		if m, err := utils.GetMagnetInfo(e.Magnet, false); err == nil && strings.ToLower(m.InfoHash) != st.Key {
			st.NoSubmit = "unmatched season"
		}
	}
	switch {
	case e.State == storage.EntryStateError:
		return failed(cmp.Or(e.LastError, "failed"))
	case complete:
		if !inLibrary {
			return failed("library entry missing")
		}
		st.Phase = PhaseComplete
		return st
	case e.State == storage.EntryStateDownloading:
		if p, ok := downloadingPhase(e); ok {
			st.Phase = p
			return st
		}
	}
	return failed(fmt.Sprintf("unreadable legacy state %s/%s", e.State, e.Status))
}

// downloadingPhase maps a downloading record by its status and its active placement.
func downloadingPhase(e *storage.Entry) (Phase, bool) {
	p := e.Providers[e.ActiveProvider]
	switch {
	case e.Status == debridTypes.TorrentStatusQueued:
		return PhaseQueued, true
	case e.Status == debridTypes.TorrentStatusDownloaded:
		return PhaseActing, true
	case e.Status != debridTypes.TorrentStatusDownloading:
		return PhaseAbsent, false
	case e.ActiveProvider == "" || p == nil:
		return PhaseQueued, true
	case p.Status == debridTypes.TorrentStatusDownloaded && LacksLink(e):
		return PhaseAwaitingLinks, true
	case p.Status == debridTypes.TorrentStatusDownloaded:
		return PhaseActing, true
	default:
		return PhaseProvider, true
	}
}

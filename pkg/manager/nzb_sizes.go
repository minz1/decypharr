package manager

import (
	"context"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

func (m *Manager) fixNZBFileSizes(ctx context.Context) {
	if m.usenet == nil {
		return
	}

	ids, err := m.usenet.NZBStorage().GetAllNZBIDs()
	if err != nil {
		m.logger.Warn().Err(err).Msg("Failed to list NZB IDs for size correction")
		return
	}

	updated := 0
	for _, id := range ids {
		select {
		case <-ctx.Done():
			return
		default:
		}

		nzb, getNZBErr := m.usenet.GetNZB(id)
		if getNZBErr != nil || nzb == nil {
			continue
		}

		changed, total := normalizeNZBFileSizes(nzb)
		if !changed {
			continue
		}

		if addNZBErr := m.usenet.NZBStorage().AddNZB(nzb); addNZBErr != nil {
			m.logger.Warn().
				Err(addNZBErr).
				Str("nzb_id", nzb.ID).
				Msg("Failed to update NZB metadata during size correction")
			continue
		}

		m.applyNZBSizesToEntry(nzb, total)

		updated++
	}

	if updated > 0 {
		m.logger.Info().Int("updated", updated).Msg("Corrected NZB file sizes")
	}
}

// applyNZBSizesToEntry copies corrected NZB sizes onto the managed entry.
func (m *Manager) applyNZBSizesToEntry(nzb *storage.NZB, total int64) {
	entry, err := m.storage.Get(nzb.ID)
	if err != nil || entry == nil || entry.Protocol != config.ProtocolNZB {
		return
	}
	changed := entry.Size != total || entry.Bytes != total
	entry.Size = total
	entry.Bytes = total
	for _, nzbFile := range nzb.Files {
		if file, ok := entry.Files[nzbFile.Name]; ok && file.Size != nzbFile.Size {
			file.Size = nzbFile.Size
			changed = true
		}
	}
	if !changed {
		return
	}
	// Add usenet placement to update it
	_ = entry.AddUsenetProvider(nzb)
	if addErr := m.storage.AddOrUpdate(entry); addErr != nil {
		m.logger.Warn().
			Err(addErr).
			Str("nzb_id", nzb.ID).
			Msg("Failed to update entry during NZB size correction")
	}
}

func normalizeNZBFileSizes(nzb *storage.NZB) (bool, int64) {
	if nzb == nil {
		return false, 0
	}

	changed := false
	var total int64

	for i := range nzb.Files {
		file := &nzb.Files[i]
		streamSize := streamSizeFromSegments(file.Segments)
		if streamSize > 0 && (file.Size <= 0 || file.Size > streamSize) {
			file.Size = streamSize
			changed = true
		}
		total += file.Size
	}

	if nzb.TotalSize != total {
		nzb.TotalSize = total
		changed = true
	}

	return changed, total
}

func streamSizeFromSegments(segments []storage.NZBSegment) int64 {
	if len(segments) == 0 {
		return 0
	}

	var maxEnd int64
	var sum int64
	for _, seg := range segments {
		if seg.Bytes > 0 {
			sum += seg.Bytes
		}
		if seg.EndOffset+1 > maxEnd {
			maxEnd = seg.EndOffset + 1
		}
	}

	if maxEnd > 0 {
		return maxEnd
	}
	return sum
}

package external

import (
	"context"
)

// Stats returns mount metadata and RC statistics. RC statistics are only asked
// of a server that answered the probe.
func (m *Manager) Stats() map[string]any {
	current := m.state.Load()
	var stats map[string]any
	if g := m.group.Load(); g != nil && current == extReady {
		ctx, cancel := context.WithTimeout(g.Context(), rcCallTimeout)
		stats = m.client.Stats(ctx)
		cancel()
	}
	if stats == nil {
		stats = make(map[string]any)
	}
	stats["ready"] = current == extReady
	stats["state"] = current.String()
	stats["enabled"] = true
	stats["type"] = m.Type()
	return stats
}

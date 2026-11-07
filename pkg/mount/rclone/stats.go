package rclone

import "context"

// Stats returns mount metadata and RC statistics. Unless rcd is Ready it
// reports the owner's state and makes no RC call.
func (m *Manager) Stats() map[string]any {
	current := m.owner.state.Load()
	var stats map[string]any
	if current == rcdReady {
		ctx, cancel := context.WithTimeout(m.owner.group.Context(), rcCallTimeout)
		stats = m.client.Stats(ctx)
		cancel()
	}
	if stats == nil {
		stats = make(map[string]any)
	}
	stats["ready"] = current == rcdReady
	stats["state"] = current.String()
	stats["restarts"] = m.owner.restarts.Load()
	stats["enabled"] = true
	stats["type"] = m.Type()
	stats["mounts"] = m.getMountInfo()
	return stats
}

package manager

import (
	"context"
	"strings"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/pkg/arr"
	"github.com/sirrobot01/decypharr/pkg/notifications"
)

// queueGuardHooks lends queue cleanup's blocklist guard what pkg/arr cannot reach itself: decypharr's own entries,
// the repair canary and the notifications.
func (m *Manager) queueGuardHooks() arr.QueueGuardHooks {
	return arr.QueueGuardHooks{
		Provider: m.queueProvider,
		Canary: func(ctx context.Context, provider string) arr.CanaryVerdict {
			return m.repair.QueueCanary(ctx, provider)
		},
		Notify: func(subject, message string) {
			if m.Notifications == nil {
				return
			}
			m.Notifications.Notify(notifications.Event{
				Type: config.EventRepairBreaker, Status: "warning", Message: "\n**" + subject + "**" + message,
			})
		},
	}
}

// queueProvider is the provider of decypharr's entry for an arr download ID, "" when decypharr has none. Arrs
// uppercase the IDs of torrents.
func (m *Manager) queueProvider(downloadID string) string {
	if m.repair == nil {
		return ""
	}
	provider := m.repair.ProviderFor(strings.ToLower(downloadID))
	if provider == "" || provider == "unknown" {
		return ""
	}
	return provider
}

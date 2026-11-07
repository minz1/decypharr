package manager

import (
	"fmt"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/pkg/arr/reacquire"
	"github.com/sirrobot01/decypharr/pkg/notifications"
)

// reacquireAttentionEvent is the notification for a reacquisition job that
// stopped for an operator.
func reacquireAttentionEvent(notice reacquire.NeedsAttention) notifications.Event {
	return notifications.Event{
		Type:   config.EventReacquireNeedsAttention,
		Status: "warning",
		Message: fmt.Sprintf(
			"\n**Job**: %s\n**Arr**: %s\n**File**: %s\n**Why**: %s\n**Episodes**: %s\n"+
				"Check the action in the Arr, then acknowledge the job on the reacquire page.",
			notice.JobID, notice.ArrName, notice.File, notice.Why, notice.Remonitor,
		),
	}
}

// announceReacquireAttention sends the notification; the notification service
// applies the configured event filter.
func (m *Manager) announceReacquireAttention(notice reacquire.NeedsAttention) {
	if m.Notifications != nil {
		m.Notifications.Notify(reacquireAttentionEvent(notice))
	}
}

package arr

import (
	"testing"

	"github.com/sirrobot01/decypharr/internal/config"
)

func TestResolveQueueActionFailedDownloadLeavesArrHandledItems(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		state string
		want  QueueAction
	}{
		{name: "failed pending", state: "failedPending", want: QueueActionNone},
		{name: "failed", state: "failed", want: QueueActionNone},
		{name: "not grabbed by the arr", state: "downloading", want: QueueActionBlocklistResearch},
		{name: "arr without tracked state", state: "", want: QueueActionBlocklistResearch},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			item := QueueSchema{Status: "failed", TrackedDownloadState: test.state}
			if got := resolveQueueAction(item, config.DefaultQueueCleanupRules()); got != test.want {
				t.Fatalf("resolveQueueAction() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestResolveQueueActionCustomRuleStillMatchesArrHandledItems(t *testing.T) {
	t.Parallel()

	item := QueueSchema{Status: "failed", TrackedDownloadState: "failed"}
	item.StatusMessages = append(item.StatusMessages, struct {
		Title    string   `json:"title"`
		Messages []string `json:"messages"`
	}{Title: "Failed download detected"})
	rules := []config.QueueCleanupRule{{Match: "failed download", Action: string(QueueActionBlocklist)}}

	if got := resolveQueueAction(item, rules); got != QueueActionBlocklist {
		t.Fatalf("resolveQueueAction() = %q, want %q", got, QueueActionBlocklist)
	}
}

func TestResolveQueueActionCatalogLeavesArrHandledItems(t *testing.T) {
	t.Parallel()

	for _, title := range []string{"Unable to parse download", "Title mismatch; automatic import is not possible"} {
		t.Run(title, func(t *testing.T) {
			t.Parallel()
			item := QueueSchema{Status: "failed", TrackedDownloadState: "FailedPending"}
			item.StatusMessages = append(item.StatusMessages, struct {
				Title    string   `json:"title"`
				Messages []string `json:"messages"`
			}{Title: title})
			if got := resolveQueueAction(item, config.DefaultQueueCleanupRules()); got != QueueActionNone {
				t.Fatalf("resolveQueueAction() = %q, want none", got)
			}
		})
	}
}

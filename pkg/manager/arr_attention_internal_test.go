package manager

import (
	"strings"
	"testing"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/pkg/arr/reacquire"
)

func TestReacquireAttentionEventCarriesWhatTheOperatorNeeds(t *testing.T) {
	t.Parallel()
	event := reacquireAttentionEvent(reacquire.NeedsAttention{
		JobID:     "job-1",
		ArrName:   "sonarr-main",
		File:      "/library/Show/S01E01.mkv",
		Why:       "search releases: arr returned 400; a compensating search was sent",
		Remonitor: "episodes are monitored again",
	})
	if event.Type != config.EventReacquireNeedsAttention || event.Status != "warning" {
		t.Fatalf("event = %s / %s, want %s / warning", event.Type, event.Status, config.EventReacquireNeedsAttention)
	}
	for _, want := range []string{
		"job-1", "sonarr-main", "/library/Show/S01E01.mkv", "a compensating search was sent",
		"episodes are monitored again",
	} {
		if !strings.Contains(event.Message, want) {
			t.Errorf("message %q does not carry %q", event.Message, want)
		}
	}
}

func TestReacquireAttentionIsOnWhenNoEventsAreChosen(t *testing.T) {
	t.Parallel()
	all := &config.Notifications{Enabled: true}
	chosen := &config.Notifications{Enabled: true, Events: []config.NotificationEvent{config.EventDownloadFailed}}
	withIt := &config.Notifications{
		Enabled: true, Events: []config.NotificationEvent{config.EventReacquireNeedsAttention},
	}
	if !all.IsEventEnabled(config.EventReacquireNeedsAttention) ||
		chosen.IsEventEnabled(config.EventReacquireNeedsAttention) ||
		!withIt.IsEventEnabled(config.EventReacquireNeedsAttention) {
		t.Fatal("event filtering does not treat reacquire_needs_attention like the other events")
	}
}

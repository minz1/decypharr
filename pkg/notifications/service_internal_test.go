package notifications

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/task"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

// captureNotifier keeps every event it is sent.
type captureNotifier struct {
	mu     sync.Mutex
	events []Event
}

func (c *captureNotifier) Name() string { return "capture" }

func (c *captureNotifier) Send(_ context.Context, ev Event) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, ev)
	return nil
}

func (c *captureNotifier) got() []Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.events)
}

func newTestService(t *testing.T, cfg *config.Notifications, n Notifier) *Service {
	t.Helper()
	g := task.NewRoot(context.WithoutCancel(t.Context()), "test", zerolog.Nop())
	t.Cleanup(func() { stopGroup(t, g) })
	return newService(g, cfg, []Notifier{n}, zerolog.Nop(), nopObserver{})
}

func TestDisabledServiceNotifyNoop(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		g := task.NewRoot(context.WithoutCancel(t.Context()), "test", zerolog.Nop())
		defer stopGroup(t, g)
		s := New(g, &config.Notifications{Enabled: false, CallbackURL: "http://127.0.0.1:1"}, nil, zerolog.Nop())
		if s.IsEnabled() {
			t.Fatal("IsEnabled() = true for a disabled config")
		}
		s.Notify(Event{Type: config.EventDownloadComplete})
		synctest.Wait()
	})
}

func TestNotifyNilEntryAndError(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		c := &captureNotifier{}
		s := newTestService(t, &config.Notifications{Enabled: true}, c)
		s.Notify(Event{Type: config.EventDownloadComplete, Status: "success"})
		synctest.Wait()
		got := c.got()
		if len(got) != 1 || got[0].Entry != nil || got[0].Error != nil {
			t.Fatalf("sent %+v, want one event with no entry and no error", got)
		}
	})
}

func TestNotifySnapshotsEntry(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		c := &captureNotifier{}
		s := newTestService(t, &config.Notifications{Enabled: true}, c)
		entry := &storage.Entry{
			InfoHash: "abc", Name: "Show", Category: "tv", ActiveProvider: "rd", ContentPath: "/mnt/Show",
		}
		failure := errors.New("boom")
		s.Notify(Event{Type: config.EventDownloadFailed, Status: "error", Entry: entry, Error: failure})
		entry.Name = "changed after Notify"
		synctest.Wait()
		got := c.got()
		if len(got) != 1 {
			t.Fatalf("sent %d events, want 1", len(got))
		}
		e := got[0].Entry
		if e == entry {
			t.Fatal("the notifier got the caller's entry, want a copy")
		}
		if e.InfoHash != "abc" || e.Name != "Show" || e.Category != "tv" || e.ActiveProvider != "rd" ||
			e.ContentPath != "/mnt/Show" {
			t.Fatalf("entry snapshot = %+v, want the fields as they were at Notify", e)
		}
		if !errors.Is(got[0].Error, failure) {
			t.Fatalf("error = %v, want %v", got[0].Error, failure)
		}
	})
}

func TestNotifySkipsDisabledEvent(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		c := &captureNotifier{}
		cfg := &config.Notifications{Enabled: true, Events: []config.NotificationEvent{config.EventDownloadFailed}}
		s := newTestService(t, cfg, c)
		s.Notify(Event{Type: config.EventDownloadComplete})
		s.Notify(Event{Type: config.EventDownloadFailed, Message: "kept"})
		synctest.Wait()
		got := c.got()
		if len(got) != 1 || got[0].Message != "kept" {
			t.Fatalf("sent %+v, want only the enabled event", got)
		}
	})
}

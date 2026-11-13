package manager

import (
	"context"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/puzpuzpuz/xsync/v4"
	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	debrid "github.com/sirrobot01/decypharr/pkg/debrid/common"
	"github.com/sirrobot01/decypharr/pkg/debrid/types"
	"github.com/sirrobot01/decypharr/pkg/manager/grab"
)

type lookupAnswer struct {
	result types.LookupResult
	status types.TorrentStatus
}

type lookupProvider struct {
	debrid.Client

	name     string
	priority int
	answers  []lookupAnswer
	mu       sync.Mutex
	asked    []time.Time
	order    *[]string
}

func (p *lookupProvider) Config() config.Debrid {
	return config.Debrid{Name: p.name, Provider: p.name, Priority: p.priority}
}

func (p *lookupProvider) Lookup(ctx context.Context, hash string) (types.LookupResult, *types.Torrent) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.order != nil {
		*p.order = append(*p.order, p.name)
	}
	n := len(p.asked)
	p.asked = append(p.asked, time.Now())
	if ctx.Err() != nil {
		return types.LookupUnknown, nil
	}
	a := p.answers[min(n, len(p.answers)-1)]
	if a.result != types.LookupFound {
		return a.result, nil
	}
	return types.LookupFound, &types.Torrent{
		ID: "id-" + p.name, Debrid: p.name, InfoHash: hash, Name: "Name", Status: a.status,
	}
}

func (p *lookupProvider) times() []time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]time.Time(nil), p.asked...)
}

func lookupManager(providers ...*lookupProvider) *Manager {
	m := &Manager{logger: zerolog.Nop(), clients: xsync.NewMap[string, debrid.Client]()}
	for _, p := range providers {
		m.clients.Store(p.name, p)
	}
	return m
}

func lookupCall(provider string, settleFrom time.Time) entryChildCall {
	return entryChildCall{
		kind: grab.ChildLookup,
		grab: grab.Grab{
			State: grab.State{
				Key:           "a",
				Phase:         grab.PhaseSubmitting,
				SubmitAttempt: grab.SubmitAttempt{Provider: provider},
			},
			Entry: testEntry("a"),
		},
		settleFrom: settleFrom,
	}
}

func answers(results ...types.LookupResult) []lookupAnswer {
	out := make([]lookupAnswer, len(results))
	for i, r := range results {
		out[i] = lookupAnswer{result: r, status: types.TorrentStatusDownloading}
	}
	return out
}

func TestLookupFoundAfterAnAbsentListing(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		p := &lookupProvider{name: "rd", answers: answers(types.LookupAbsent, types.LookupFound)}
		start := time.Now()
		msg := lookupManager(p).lookupChild(t.Context(), lookupCall("rd", start))
		if msg.ev == nil || msg.ev.Kind != grab.EvLookup || msg.ev.Outcome != grab.OutcomeFound ||
			msg.ev.Provider != "rd" || msg.ev.TorrentID != "id-rd" {
			t.Fatalf("event %+v, want found on rd", msg.ev)
		}
		if msg.torrent == nil || msg.torrent.ID != "id-rd" {
			t.Fatalf("torrent %+v, want the found torrent", msg.torrent)
		}
		if got := p.times(); len(got) != 2 || got[1].Sub(start) < grab.LookupSettle {
			t.Fatalf("asked at %v, want a second listing after the settle", got)
		}
	})
}

func TestLookupAbsentNeedsTwoListingsASettleApart(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		a := &lookupProvider{name: "rd", answers: answers(types.LookupAbsent)}
		b := &lookupProvider{name: "tb", priority: 1, answers: answers(types.LookupAbsent)}
		start := time.Now()
		msg := lookupManager(a, b).lookupChild(t.Context(), lookupCall("rd", start))
		if msg.ev == nil || msg.ev.Kind != grab.EvLookup || msg.ev.Outcome != grab.OutcomeAbsent {
			t.Fatalf("event %+v, want absent", msg.ev)
		}
		for _, p := range []*lookupProvider{a, b} {
			got := p.times()
			last := got[len(got)-1]
			if len(got) < 2 || last.Sub(start) < 2*grab.LookupSettle {
				t.Fatalf("%s asked at %v, want two listings from settle on", p.name, got)
			}
		}
	})
}

func TestLookupUnknownSendsNoEvent(t *testing.T) {
	t.Parallel()
	for name, providers := range map[string]func() []*lookupProvider{
		"unknown": func() []*lookupProvider {
			return []*lookupProvider{{name: "rd", answers: answers(types.LookupUnknown)}}
		},
		"absent and unknown": func() []*lookupProvider {
			return []*lookupProvider{
				{name: "rd", answers: answers(types.LookupAbsent)},
				{name: "tb", priority: 1, answers: answers(types.LookupUnknown)},
			}
		},
		"unknown on the second listing": func() []*lookupProvider {
			return []*lookupProvider{{name: "rd", answers: answers(types.LookupAbsent, types.LookupAbsent,
				types.LookupUnknown)}}
		},
		"no provider": func() []*lookupProvider { return nil },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				msg := lookupManager(providers()...).lookupChild(t.Context(), lookupCall("rd", time.Now()))
				if msg.ev != nil || msg.torrent != nil {
					t.Fatalf("message %+v, want none", msg)
				}
			})
		})
	}
}

func TestLookupStopsInTheSettleWait(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		p := &lookupProvider{name: "rd", answers: answers(types.LookupAbsent)}
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan entryMsg)
		go func() { done <- lookupManager(p).lookupChild(ctx, lookupCall("rd", time.Now())) }()
		synctest.Wait()
		start := time.Now()
		cancel()
		msg := <-done
		if msg.ev != nil || !time.Now().Equal(start) {
			t.Fatalf("message %+v after %v, want none at once", msg.ev, time.Since(start))
		}
		if n := len(p.times()); n != 1 {
			t.Fatalf("%d listings, want 1", n)
		}
	})
}

func TestLookupAsksTheSubmitProviderFirst(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		var order []string
		a := &lookupProvider{name: "rd", answers: answers(types.LookupUnknown), order: &order}
		b := &lookupProvider{name: "tb", priority: 1, answers: answers(types.LookupFound), order: &order}
		msg := lookupManager(a, b).lookupChild(t.Context(), lookupCall("tb", time.Now()))
		if msg.ev == nil || msg.ev.Outcome != grab.OutcomeFound || msg.ev.Provider != "tb" {
			t.Fatalf("event %+v, want found on tb", msg.ev)
		}
		if len(order) != 1 || order[0] != "tb" {
			t.Fatalf("asked %v, want tb first and alone", order)
		}
	})
}

func TestLookupFoundDownloadedEntersProvider(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		p := &lookupProvider{name: "rd", answers: []lookupAnswer{
			{result: types.LookupFound, status: types.TorrentStatusDownloaded},
		}}
		call := lookupCall("rd", time.Now())
		msg := lookupManager(p).lookupChild(t.Context(), call)
		if msg.ev == nil || msg.ev.Downloaded || msg.ev.AllLinked {
			t.Fatalf("event %+v, want found with nothing downloaded or linked", msg.ev)
		}
		ev := *msg.ev
		ev.At = time.Now()
		next, _, err := grab.Transition(call.grab.State, ev)
		if err != nil || next.Phase != grab.PhaseProvider {
			t.Fatalf("stepped to %v (%v), want Provider", next.Phase, err)
		}
	})
}

package manager

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/customerror"
	"github.com/sirrobot01/decypharr/internal/utils"
	"github.com/sirrobot01/decypharr/pkg/debrid/types"
	"github.com/sirrobot01/decypharr/pkg/hearsay"
	"github.com/sirrobot01/decypharr/pkg/manager/grab"
)

const (
	hintHash      = "2c6b6858d61da9543d4231a71db4b1c9264b0685"
	bypassMessage = "Hearsay hint bypassed: the provider answers"
)

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (b *syncBuffer) records(t *testing.T) []map[string]any {
	t.Helper()
	var out []map[string]any
	for line := range strings.SplitSeq(b.String(), "\n") {
		if line == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}
		out = append(out, record)
	}
	return out
}

func (b *syncBuffer) bypasses(t *testing.T) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, record := range b.records(t) {
		if record["message"] == bypassMessage {
			out = append(out, record)
		}
	}
	return out
}

func hintService(t *testing.T, logger zerolog.Logger, vendor string) *hearsay.Service {
	t.Helper()
	cfg := &config.Config{Debrids: []config.Debrid{{Name: vendor, Provider: vendor}}}
	cfg.WithDir(t.TempDir())
	cfg.Hearsay.AdviceMode = "active"
	service, err := hearsay.New(testGroup(t), cfg, logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	return service
}

type hintProvider struct {
	throttlingProvider

	cached   bool
	uncached bool
	log      zerolog.Logger
}

func (p *hintProvider) Config() config.Debrid {
	return config.Debrid{Name: p.name, Provider: p.name, DownloadUncached: p.uncached}
}

func (p *hintProvider) Logger() zerolog.Logger { return p.log }

func (p *hintProvider) SubmitMagnet(_ context.Context, t *types.Torrent) (*types.Torrent, error) {
	p.submits.Add(1)
	return &types.Torrent{ID: "pid", Debrid: p.name, InfoHash: t.InfoHash, Name: t.Name}, nil
}

func (p *hintProvider) CheckStatus(_ context.Context, t *types.Torrent) (*types.Torrent, error) {
	if p.cached {
		t.Status = types.TorrentStatusDownloaded
		return t, nil
	}
	return t, fmt.Errorf("torrent %s: %w", t.Name, customerror.ErrTorrentNotCached)
}

func (*hintProvider) DeleteTorrent(context.Context, string) error { return nil }

type hintCase struct {
	manual            bool
	category          string
	cached            bool
	negative          bool
	recovered         bool
	perRequestUncache bool
	providerUncached  bool
}

type hintRun struct {
	m           *Manager
	g           grab.Grab
	msg         entryMsg
	fake        *fakeArrRecovery
	provider    *hintProvider
	service     *hearsay.Service
	providerLog *syncBuffer
	hearsayLog  *syncBuffer
	before      map[string]uint64
}

func adviceCounters(service *hearsay.Service) map[string]uint64 {
	advice := service.Status().Advice["rd"]
	return map[string]uint64{
		"evaluations": advice.Evaluations, "outcomes": advice.Outcomes,
		"correct": advice.Correct, "incorrect": advice.Incorrect,
	}
}

func (r *hintRun) delta(name string) uint64 {
	return adviceCounters(r.service)[name] - r.before[name]
}

func runHintSubmit(t *testing.T, c hintCase) *hintRun {
	t.Helper()
	fake := &fakeArrRecovery{}
	m, entry := newDroppedTestManager(t, fake)
	m = withTestConfig(t, m)
	entry.Protocol = config.ProtocolTorrent
	entry.InfoHash = hintHash
	entry.Manual = c.manual
	entry.Category = c.category
	entry.DownloadUncached = c.recovered && c.perRequestUncache

	providerLog, hearsayLog := &syncBuffer{}, &syncBuffer{}
	service := hintService(t, zerolog.New(hearsayLog).Level(zerolog.DebugLevel), "rd")
	if c.negative {
		service.ObserveTorrent("rd", hintHash, false)
	}
	provider := &hintProvider{
		throttlingProvider: throttlingProvider{name: "rd"},
		cached:             c.cached,
		uncached:           c.providerUncached,
		log:                zerolog.New(providerLog),
	}
	m.hearsay = service
	m.clients.Store("rd", provider)

	call := entryChildCall{
		kind: grab.ChildSubmit,
		grab: grab.Grab{State: grab.State{Key: hintHash, Phase: grab.PhaseSubmitting}, Entry: entry},
	}
	if !c.recovered {
		var override *bool
		if c.perRequestUncache {
			override = new(true)
		}
		call.req = NewTorrentRequest(
			"", "", utils.ConstructMagnet(hintHash, entry.Name), m.arr.GetOrCreate(entry.Category), "", override, "",
			ImportTypeAPI, false,
		)
	}
	run := &hintRun{
		m: m, g: call.grab, fake: fake, provider: provider, service: service,
		providerLog: providerLog, hearsayLog: hearsayLog, before: adviceCounters(service),
	}
	run.msg = m.submitChild(t.Context(), call)
	return run
}

func forEachRequestShape(t *testing.T, run func(t *testing.T, recovered bool)) {
	t.Helper()
	for name, recovered := range map[string]bool{"fresh request": false, "recovered record": true} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			run(t, recovered)
		})
	}
}

func TestAClientTorrentGrabAsksTheProviderPastANegativeHint(t *testing.T) {
	t.Parallel()
	forEachRequestShape(t, func(t *testing.T, recovered bool) {
		r := runHintSubmit(t, hintCase{category: "sonarr", negative: true, recovered: recovered})
		if got := r.provider.submits.Load(); got != 1 {
			t.Fatalf("provider submits = %d, want the provider asked once", got)
		}
		if r.msg.ev == nil || r.msg.ev.Fail != grab.FailBadRelease {
			t.Fatalf("submit result = %+v, want a bad release", r.msg.ev)
		}
		r.m.failEffects(failedGrab(r.g.Entry, r.msg.ev.Reason), r.msg.ev.Fail)
		if len(r.fake.fails) != 1 {
			t.Fatalf("FailDownload calls = %v, want one", r.fake.fails)
		}
	})
}

func TestAManualTorrentGrabKeepsTheHint(t *testing.T) {
	t.Parallel()
	forEachRequestShape(t, func(t *testing.T, recovered bool) {
		t.Run("manual grab keeps the hint", func(t *testing.T) {
			t.Parallel()
			r := runHintSubmit(t, hintCase{manual: true, category: "sonarr", negative: true, recovered: recovered})
			if got := r.provider.submits.Load(); got != 0 {
				t.Fatalf("provider submits = %d, want none", got)
			}
			if r.msg.ev == nil || r.msg.ev.Fail != grab.FailEntry ||
				!strings.Contains(r.msg.ev.Reason, "recently proven not cached") {
				t.Fatalf("submit result = %+v, want a hint failure", r.msg.ev)
			}
		})
		t.Run("unregistered category bypasses", func(t *testing.T) {
			t.Parallel()
			r := runHintSubmit(t, hintCase{category: "unknown", negative: true, recovered: recovered})
			if got := r.provider.submits.Load(); got != 1 {
				t.Fatalf("provider submits = %d, want the provider asked once", got)
			}
			if r.msg.ev == nil || r.msg.ev.Fail != grab.FailBadRelease {
				t.Fatalf("submit result = %+v, want a bad release", r.msg.ev)
			}
			if r.m.arrOwnsFailure(r.g) {
				t.Fatal("an unregistered category's failure is owned by an Arr")
			}
			r.m.failEffects(failedGrab(r.g.Entry, r.msg.ev.Reason), r.msg.ev.Fail)
			if len(r.fake.fails) != 0 {
				t.Fatalf("FailDownload calls = %v, want none", r.fake.fails)
			}
		})
	})
}

func TestABypassedHintScoresTheProviderAnswerOnce(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		cached  bool
		correct uint64
		wrong   uint64
	}{
		"provider answers cached":     {cached: true, wrong: 1},
		"provider answers not cached": {correct: 1},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			forEachRequestShape(t, func(t *testing.T, recovered bool) {
				r := runHintSubmit(t, hintCase{category: "sonarr", negative: true, cached: tc.cached, recovered: recovered})
				for counter, want := range map[string]uint64{
					"evaluations": 1, "outcomes": 1, "correct": tc.correct, "incorrect": tc.wrong,
				} {
					if got := r.delta(counter); got != want {
						t.Errorf("%s delta = %d, want %d", counter, got, want)
					}
				}
				if log := r.hearsayLog.String(); strings.Contains(log, "advice outcome failed") {
					t.Errorf("hearsay log reports a refused outcome:\n%s", log)
				}
			})
		})
	}
}

func TestTheTorrentBypassLineIsLoggedOnlyForABypassedNegative(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		c    hintCase
		want int
	}{
		"client grab with a negative":             {hintCase{category: "sonarr", negative: true}, 1},
		"manual grab with a negative":             {hintCase{manual: true, category: "sonarr", negative: true}, 0},
		"client grab with no negative":            {hintCase{category: "sonarr"}, 0},
		"client grab, request allows uncached":    {hintCase{category: "sonarr", negative: true, perRequestUncache: true}, 0},
		"client grab, provider allows uncached":   {hintCase{category: "sonarr", negative: true, providerUncached: true}, 0},
		"client grab, no negative, cached answer": {hintCase{category: "sonarr", cached: true}, 0},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			forEachRequestShape(t, func(t *testing.T, recovered bool) {
				c := tc.c
				c.recovered = recovered
				r := runHintSubmit(t, c)
				lines := r.providerLog.bypasses(t)
				if len(lines) != tc.want {
					t.Fatalf("bypass lines = %v, want %d", lines, tc.want)
				}
				if tc.want == 0 {
					return
				}
				line := lines[0]
				if line["level"] != "info" || line["Provider"] != "rd" || line["Hash"] != hintHash ||
					line["Arr"] != "sonarr" {
					t.Fatalf("bypass line = %v, want info with Provider rd, the hash and Arr sonarr", line)
				}
			})
		})
	}
}

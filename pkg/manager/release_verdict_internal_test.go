package manager

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/puzpuzpuz/xsync/v4"
	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/customerror"
	"github.com/sirrobot01/decypharr/internal/utils"
	debrid "github.com/sirrobot01/decypharr/pkg/debrid/common"
	"github.com/sirrobot01/decypharr/pkg/debrid/types"
	"github.com/sirrobot01/decypharr/pkg/manager/grab"
)

type deadStatusProvider struct {
	throttlingProvider
}

func (p *deadStatusProvider) SubmitMagnet(_ context.Context, t *types.Torrent) (*types.Torrent, error) {
	return &types.Torrent{ID: "pid", Debrid: p.name, InfoHash: t.InfoHash, Name: t.Name}, nil
}

func (p *deadStatusProvider) CheckStatus(_ context.Context, t *types.Torrent) (*types.Torrent, error) {
	t.Status = types.TorrentStatusError
	return t, errors.New("torrent: x has error status dead")
}

func (*deadStatusProvider) DeleteTorrent(context.Context, string) error { return nil }

func submitFailEffect(t *testing.T, providers ...debrid.Client) (grab.FailEffect, grab.Outcome) {
	t.Helper()
	m := withTestConfig(t, &Manager{logger: zerolog.Nop(), clients: xsync.NewMap[string, debrid.Client]()})
	for _, p := range providers {
		m.clients.Store(p.Config().Name, p)
	}
	_, err := m.SendToDebrid(&ImportRequest{Magnet: &utils.Magnet{InfoHash: "hash", Name: "name"}})
	if err == nil {
		t.Fatal("submit succeeded, want a failure")
	}
	msg := submitResult(testEntry("hash"), nil, err)
	return msg.ev.Fail, msg.ev.Outcome
}

func refusing(name string, err error) debrid.Client {
	return &throttlingProvider{name: name, err: err}
}

func dead(name string) debrid.Client {
	return &deadStatusProvider{throttlingProvider{name: name}}
}

func TestASubmitEveryProviderRefusedFailsAsABadRelease(t *testing.T) {
	t.Parallel()
	transient := errors.New("giving up after 4 attempt(s)")
	for name, tc := range map[string]struct {
		providers []debrid.Client
		want      grab.FailEffect
	}{
		"not cached everywhere": {
			[]debrid.Client{refusing("rd", customerror.ErrTorrentNotCached), refusing("tb", customerror.ErrTorrentNotCached)},
			grab.FailBadRelease,
		},
		"blocked and not cached": {
			[]debrid.Client{refusing("rd", customerror.ErrTorrentBlocked), refusing("tb", customerror.ErrTorrentNotCached)},
			grab.FailBadRelease,
		},
		"blocked on the only provider":     {[]debrid.Client{refusing("rd", customerror.ErrTorrentBlocked)}, grab.FailBadRelease},
		"dead status on the only provider": {[]debrid.Client{dead("rd")}, grab.FailEntry},
		"not cached and transient": {
			[]debrid.Client{refusing("rd", customerror.ErrTorrentNotCached), refusing("tb", transient)},
			grab.FailEntry,
		},
		"dead status and transient": {[]debrid.Client{dead("rd"), refusing("tb", transient)}, grab.FailEntry},
		"not found everywhere": {
			[]debrid.Client{refusing("rd", customerror.ErrTorrentNotFound), refusing("tb", customerror.ErrTorrentNotFound)},
			grab.FailEntry,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got, _ := submitFailEffect(t, tc.providers...); got != tc.want {
				t.Fatalf("submit fail effect = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestAThrottledSubmitIsStillThrottled(t *testing.T) {
	t.Parallel()
	_, outcome := submitFailEffect(t,
		refusing("rd", customerror.ErrTorrentBlocked), refusing("tb", customerror.ErrTooManyActiveDownloads))
	if outcome != grab.OutcomeThrottled {
		t.Fatalf("outcome = %v, want throttled", outcome)
	}
}

func TestTheHearsaySkipIsNoProviderVerdict(t *testing.T) {
	t.Parallel()
	if err := hearsayNotCached("rd", "hash"); submitVerdict(err) {
		t.Fatalf("hearsay skip %v counted as a provider verdict", err)
	}
}

func TestANotCachedPollFailsAsABadRelease(t *testing.T) {
	t.Parallel()
	provider := &uncachedProvider{
		phase: types.PhaseDownloading, cached: types.CachedNo,
	}
	m, entry := queuedEntryAddedAt(t, provider, time.Now())
	m.store = testConfigStore(t, nil)
	msg := m.pollChild(t.Context(), entryChildCall{kind: grab.ChildPoll, grab: grab.Grab{Entry: entry}}, false)
	closePlacements(t, m)
	if msg.ev == nil || msg.ev.Fail != grab.FailBadRelease {
		t.Fatalf("not-cached poll = %+v, want FailBadRelease", msg.ev)
	}
}

package manager

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/nntp"
	"github.com/sirrobot01/decypharr/pkg/manager/grab"
	"github.com/sirrobot01/decypharr/pkg/storage"
	"github.com/sirrobot01/decypharr/pkg/usenet/parser"
)

func causeNow() time.Time { return time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC) }

func causeManager() *Manager {
	return &Manager{logger: zerolog.Nop(), pruneNow: causeNow}
}

func nzbFile(subject string, ago time.Duration, id string) string {
	return fmt.Sprintf(
		`<file poster="p" date="%d" subject="%s"><groups><group>a.b</group></groups><segments>`+
			`<segment bytes="10" number="1">%s</segment></segments></file>`,
		causeNow().Add(-ago).Unix(), subject, id)
}

func nzbDoc(files ...string) []byte {
	return []byte(`<?xml version="1.0"?><nzb xmlns="http://www.newzbin.com/DTD/2003/nzb">` +
		strings.Join(files, "") + `</nzb>`)
}

func oneFile(ago time.Duration) []byte {
	return nzbDoc(nzbFile(`&quot;a.mkv&quot; yEnc (1/1)`, ago, "a@example"))
}

func undated() []byte {
	stamp := strconv.FormatInt(causeNow().Unix(), 10)
	return []byte(strings.Replace(string(oneFile(0)), stamp, "0", 1))
}

func missing() error { return &parser.ContentMissingError{Err: errors.New("430")} }

func holes() error { return &parser.SegmentHolesError{File: "a.mkv", Present: 1, Span: 2} }

func TestParseFailCause(t *testing.T) {
	t.Parallel()
	older := nzbFile(`&quot;old.mkv&quot; yEnc (1/1)`, 48*time.Hour, "o@example")
	younger := nzbFile(`&quot;young.mkv&quot; yEnc (1/1)`, time.Hour, "y@example")
	cases := []struct {
		name    string
		err     error
		content []byte
		want    string
	}{
		{"5h59m", missing(), oneFile(5*time.Hour + 59*time.Minute), ""},
		{"exactly 6h", missing(), oneFile(6 * time.Hour), grab.CauseArticlesMissing},
		{"now", missing(), oneFile(0), ""},
		{"no date", missing(), undated(), grab.CauseArticlesMissing},
		{"old", missing(), oneFile(48 * time.Hour), grab.CauseArticlesMissing},
		{"7h ahead", missing(), oneFile(-7 * time.Hour), grab.CauseArticlesMissing},
		{"1h ahead", missing(), oneFile(-time.Hour), ""},
		{"holes old", holes(), oneFile(48 * time.Hour), grab.CauseSegmentsMissing},
		{"holes fresh", holes(), oneFile(time.Hour), ""},
		{"merged by subject", missing(), nzbDoc(
			nzbFile(`&quot;a.mkv&quot; yEnc (1/1)`, 48*time.Hour, "o@example"),
			nzbFile(`&quot;a.mkv&quot; yEnc (1/1)`, time.Hour, "y@example")), ""},
		{"older then younger subject", missing(), nzbDoc(older, younger), ""},
		{"ampersand", missing(), nzbDoc(nzbFile("a", 48*time.Hour, "a&amp;b@x")), ""},
		{"angle open", missing(), nzbDoc(nzbFile("a", 48*time.Hour, "&lt;x&gt;")), ""},
		{"angle close", missing(), nzbDoc(nzbFile("a", 48*time.Hour, "x&gt;y")), ""},
		{"empty id", missing(), nzbDoc(nzbFile("a", 48*time.Hour, "")), ""},
		{"folded id", missing(), nzbDoc(nzbFile("a", 48*time.Hour, "book@\n example")), ""},
		{"tab id", missing(), nzbDoc(nzbFile("a", 48*time.Hour, "book@\texample")), ""},
		{"wrapped id", missing(), nzbDoc(nzbFile("a", 48*time.Hour, "\n a@example \n")), grab.CauseArticlesMissing},
		{"no files", missing(), nzbDoc(), ""},
		{"garbage", missing(), []byte("garbage"), ""},
		{"plain error", errors.New("x"), oneFile(48 * time.Hour), ""},
		{"nil", nil, oneFile(48 * time.Hour), ""},
		{"bare proof", &nntp.NotFoundEverywhereError{Err: errors.New("430")}, oneFile(48 * time.Hour), ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := causeManager().parseFailCause(c.err, c.content); got != c.want {
				t.Errorf("cause %q, want %q", got, c.want)
			}
		})
	}
}

func parseReturning(err error) parseFunc {
	return func(context.Context, string, string, []byte, string) (*storage.NZB, map[string]*parser.FileGroup, error) {
		return nil, nil, err
	}
}

func TestParseChildClassifiesAProvenMiss(t *testing.T) {
	t.Parallel()
	m, call := newParseChildCallWith(t, parseReturning(missing()), oneFile(48*time.Hour))
	m.pruneNow = causeNow

	msg := m.parseChild(t.Context(), call)

	if msg.ev == nil || msg.ev.Cause != grab.CauseArticlesMissing ||
		!strings.HasPrefix(msg.ev.Reason, "usenet parse failed: ") {
		t.Fatalf("parse result = %+v, want articles missing with the parse text", msg.ev)
	}
}

func TestParseChildClassifiesHolesAtProcess(t *testing.T) {
	t.Parallel()
	m, call := newParseChildCallWith(t, func(context.Context, string, string, []byte, string) (
		*storage.NZB, map[string]*parser.FileGroup, error,
	) {
		return &storage.NZB{ID: "nzb-1", Name: "Show.S01"}, parsedGroups(), nil
	}, oneFile(48*time.Hour))
	m.pruneNow = causeNow
	m.usenetTimeout = time.Minute
	m.processNZBFunc = func(context.Context, *storage.NZB, map[string]*parser.FileGroup) (*storage.NZB, error) {
		return nil, fmt.Errorf("analyze: %w", holes())
	}

	msg := m.parseChild(t.Context(), call)

	if msg.ev == nil || msg.ev.Cause != grab.CauseSegmentsMissing ||
		!strings.HasPrefix(msg.ev.Reason, "failed to process nzb: ") {
		t.Fatalf("parse result = %+v, want segments missing with the process text", msg.ev)
	}
}

func TestParseChildLeavesAProcessDeadlineRaw(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		m, call := newParseChildCallWith(t, func(context.Context, string, string, []byte, string) (
			*storage.NZB, map[string]*parser.FileGroup, error,
		) {
			return &storage.NZB{ID: "nzb-1", Name: "Show.S01"}, parsedGroups(), nil
		}, oneFile(48*time.Hour))
		m.pruneNow = causeNow
		m.usenetTimeout = time.Minute
		m.processNZBFunc = func(ctx context.Context, _ *storage.NZB, _ map[string]*parser.FileGroup) (*storage.NZB, error) {
			<-ctx.Done()
			unproven := &nntp.Error{Type: nntp.ErrorTypeArticleNotFound}
			return nil, errors.Join(fmt.Errorf("x: %w", ctx.Err()), unproven)
		}

		msg := m.parseChild(t.Context(), call)

		if msg.ev == nil || msg.ev.Cause != "" || !strings.HasPrefix(msg.ev.Reason, "failed to process nzb: ") {
			t.Fatalf("parse result = %+v, want a process failure with no cause", msg.ev)
		}
	})
}

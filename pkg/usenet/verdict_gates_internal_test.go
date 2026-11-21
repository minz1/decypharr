package usenet

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/customerror"
	"github.com/sirrobot01/decypharr/internal/nntp"
	"github.com/sirrobot01/decypharr/internal/task"
	"github.com/sirrobot01/decypharr/internal/testutil/nntpd"
	"github.com/sirrobot01/decypharr/pkg/storage"
	"github.com/sirrobot01/decypharr/pkg/usenet/internal/gate"
	"github.com/sirrobot01/decypharr/pkg/usenet/parser"
)

type unobservedSource struct{ parser.ArticleSource }

func (unobservedSource) IsAvailable(string) bool { return false }

func TestVerdictOfGateErrorsIsNone(t *testing.T) {
	t.Parallel()
	u, nzb, _ := parsingNZB(t, "gates")
	u.config = config.NewStore(config.New(t.TempDir()))
	u.analyzer = parser.NewParserWithSource(unobservedSource{}, 1, zerolog.Nop())

	srv, err := nntpd.New(nntpd.Config{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	g := task.NewRoot(t.Context(), "test", zerolog.Nop())
	t.Cleanup(func() {
		ctx, cancel := task.Detached(t.Context(), 30*time.Second)
		defer cancel()
		_ = g.Stop(ctx)
	})
	host, port := srv.Addr()
	u.nntp, err = nntp.NewClient(g.Child("nntp"), &config.Config{Usenet: config.Usenet{
		Providers: []config.UsenetProvider{{Host: host, Port: port, MaxConnections: 2}},
	}}, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	nzb.Files = []storage.NZBFile{{
		Name:     "Movie.mkv",
		Segments: []storage.NZBSegment{{Number: 1, MessageID: "<gone@nntpd>", Bytes: 1}},
	}}

	result, finishErr := u.finish(t.Context(), nzb, u.gateSteps(t.Context(), nzb))
	if !errors.Is(finishErr, customerror.ErrUsenetSegmentMissing) {
		t.Fatalf("finish = %v, want the availability gate to report missing segments", finishErr)
	}
	if got := parser.VerdictOf(finishErr); got != parser.VerdictNone {
		t.Fatalf("VerdictOf(finish) = %v, want none", got)
	}
	if result.Outcome.Verdict != parser.VerdictNone {
		t.Fatalf("ProcessResult.Outcome.Verdict = %v, want none", result.Outcome.Verdict)
	}

	proven := &nntp.NotFoundEverywhereError{Err: fmt.Errorf("%w: %w", nntp.ErrAllProvidersFailed,
		&nntp.Error{Type: nntp.ErrorTypeArticleNotFound, Code: 430, Message: "no such article"})}
	head := classifyHeadReadError("Movie.mkv", proven)
	if head.Kind() != gate.KindMissing {
		t.Fatalf("classifyHeadReadError kind = %v, want missing", head.Kind())
	}
	headErr := head.Err()
	if !errors.Is(headErr, customerror.ErrUsenetSegmentMissing) {
		t.Fatalf("classifyHeadReadError = %v, want missing segments", headErr)
	}
	if got := parser.VerdictOf(headErr); got != parser.VerdictNone {
		t.Fatalf("VerdictOf(classifyHeadReadError) = %v, want none", got)
	}
}

func gateHarness(t *testing.T, reply func(command, messageID string) string) (*Usenet, *nntpd.Server) {
	t.Helper()
	u, srv, _ := gateHarnessGroup(t, reply)
	return u, srv
}

func gateHarnessGroup(
	t *testing.T,
	reply func(command, messageID string) string,
) (*Usenet, *nntpd.Server, *task.Group) {
	t.Helper()
	srv, err := nntpd.New(nntpd.Config{Reply: reply})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	_, port := srv.Addr()
	u, root := newLifecycleUsenet(t, port, 2)
	return u, srv, root
}

func gateNZB(t *testing.T, u *Usenet, file storage.NZBFile) *storage.NZB {
	t.Helper()
	nzb := &storage.NZB{ID: "gate", Name: "Movie", Status: NZBStatusParsing, Files: []storage.NZBFile{file}}
	if err := u.nzbStorage.AddNZB(nzb); err != nil {
		t.Fatal(err)
	}
	return nzb
}

func gateFile(size int) storage.NZBFile {
	return storage.NZBFile{
		Name:     "Movie.mkv",
		FileType: storage.NZBFileTypeMedia,
		Size:     int64(size),
		Segments: []storage.NZBSegment{{
			Number: 1, MessageID: "<head@gate>", Bytes: int64(size), EndOffset: int64(size) - 1,
		}},
	}
}

func TestGateFactProducers(t *testing.T) {
	t.Parallel()
	statGone := func(command, _ string) string {
		if command == "STAT" {
			return "430 no such article"
		}
		return ""
	}
	bodyGone := func(command, _ string) string {
		if command == "BODY" {
			return "430 no such article"
		}
		return ""
	}
	var emptyBodies atomic.Int32
	bodyNoData := func(command, _ string) string {
		if command == "BODY" {
			emptyBodies.Add(1)
			return "222 0 <head@gate> body follows\r\n."
		}
		return ""
	}
	const (
		unavailable = `file "Movie.mkv" unavailable`
		headMissing = `head article of "Movie.mkv" missing`
		noSignature = `head of "Movie.mkv" matches no media container signature`
	)
	for _, tc := range []gateProducerCase{
		{name: "availability miss", reply: statGone, serve: true, want: true, wrapper: unavailable},
		{
			name: "head missing via the analyzer prefix read", reply: bodyGone, serve: true,
			want: true, wrapper: headMissing,
		},
		{
			name: "head missing via the serving cursor", reply: bodyGone, serve: true, viaCursor: true,
			want: true, wrapper: headMissing,
		},
		{name: "head served with no signature", serve: true, wrapper: noSignature},
		{
			name: "head served with no signature via the serving cursor", serve: true, viaCursor: true,
			wrapper: noSignature,
		},
		{
			name: "head 222 with no yEnc data counts as missing (HEAD behaviour)", reply: bodyNoData, serve: true,
			want: true, wrapper: headMissing, served: &emptyBodies,
		},
		{name: "context ended", serve: true, ended: true, wrapper: "processing interrupted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tc.run(t)
		})
	}
}

type gateProducerCase struct {
	name      string
	reply     func(command, messageID string) string
	serve     bool
	viaCursor bool
	ended     bool
	want      bool
	wrapper   string
	served    *atomic.Int32
}

func (tc gateProducerCase) run(t *testing.T) {
	payload := []byte(strings.Repeat("not a media container ", 40))
	u, srv := gateHarness(t, tc.reply)
	if tc.serve {
		srv.AddArticle("<head@gate>", nntpd.Encode(payload, "Movie.mkv", 1, int64(len(payload)), 0))
	}
	nzb := gateNZB(t, u, gateFile(len(payload)))
	ctx := t.Context()
	if tc.ended {
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(ctx)
		cancel()
	}
	if tc.viaCursor {
		u.analyzer = nil
	}
	got, err := u.finish(ctx, nzb, u.gateSteps(ctx, nzb))
	tc.check(t, got, err)
	if tc.served != nil && tc.served.Load() == 0 {
		t.Fatal("the fixture never served the empty body")
	}
}

func (tc gateProducerCase) check(t *testing.T, got ProcessResult, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("the gates passed, want a failure")
	}
	if !strings.Contains(err.Error(), tc.wrapper) {
		t.Fatalf("error = %v, want it to contain %q", err, tc.wrapper)
	}
	if got.GateMissing != tc.want {
		t.Fatalf("GateMissing = %v, want %v (error %v)", got.GateMissing, tc.want, err)
	}
	if got.Outcome != (parser.Outcome{}) {
		t.Fatalf("Outcome = %+v, want none", got.Outcome)
	}
	if tc.want && !errors.Is(err, customerror.ErrUsenetSegmentMissing) {
		t.Fatalf("error = %v, want missing segments", err)
	}
	if !tc.want && !tc.ended && !errors.Is(err, customerror.ErrUsenetCorruptContent) {
		t.Fatalf("error = %v, want corrupt content", err)
	}
	if tc.ended && !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want the cancellation", err)
	}
}

func TestProcessReportsGateMissing(t *testing.T) {
	t.Parallel()
	var gone atomic.Bool
	u, srv := gateHarness(t, func(command, messageID string) string {
		if gone.Load() && command == "STAT" && messageID == "<b@gate>" {
			return "430 no such article"
		}
		return ""
	})
	payload := []byte(strings.Repeat("not a media container ", 40))
	total := int64(len(payload))
	srv.AddArticle("<a@gate>", nntpd.Encode(payload[:100], "movie.mkv", 1, total, 0))
	srv.AddArticle("<b@gate>", nntpd.Encode(payload[100:], "movie.mkv", 2, total, 100))
	parsed, err := u.analyzer.Parse(t.Context(), "release.nzb", gateNZBXML("1:a@gate", "2:b@gate"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	gone.Store(true)
	parsed.NZB.Status = NZBStatusParsing
	if addErr := u.nzbStorage.AddNZB(parsed.NZB); addErr != nil {
		t.Fatal(addErr)
	}
	updated, result, err := u.Process(t.Context(), parsed)
	if !errors.Is(err, customerror.ErrUsenetSegmentMissing) ||
		!strings.Contains(err.Error(), `file "release.mkv" unavailable`) {
		t.Fatalf("Process error = %v, want the availability gate's missing segments", err)
	}
	if !result.GateMissing {
		t.Fatal("Process lost the gate's missing fact")
	}
	if updated == nil {
		t.Fatal("Process returned no NZB with its error")
	}
}

func TestParseNeverReturnsNilParsed(t *testing.T) {
	t.Parallel()
	valid := []byte(`<?xml version="1.0"?><nzb xmlns="http://www.newzbin.com/DTD/2003/nzb">` +
		`<file poster="p" date="1" subject="[1/1] - &quot;movie.mkv&quot; yEnc (1/1)">` +
		`<groups><group>alt.test</group></groups>` +
		`<segments><segment bytes="5" number="1">a@gate</segment></segments></file></nzb>`)
	for _, tc := range []struct {
		name    string
		id      string
		content []byte
		damage  func(t *testing.T, u *Usenet)
		want    string
	}{
		{name: "empty content", want: "NZB content is empty"},
		{name: "invalid content", content: []byte("not an nzb"), want: "invalid NZB content"},
		{name: "failing save", id: "no-dir/x", content: valid, want: "failed to save NZB file to disk"},
		{
			name: "failing mark", content: valid, want: "failed to mark NZB as processing",
			damage: func(t *testing.T, u *Usenet) {
				if err := u.markers.Close(); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "failing storage add", id: "blocked", content: valid, want: "failed to save NZB to storage",
			damage: func(t *testing.T, u *Usenet) {
				path, err := u.nzbStorage.metaFilePath("blocked")
				if err != nil {
					t.Fatal(err)
				}
				if err = os.Mkdir(path+".tmp", 0o750); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			u, srv := gateHarness(t, nil)
			srv.AddArticle("<a@gate>", nntpd.Encode([]byte("hello"), "movie.mkv", 1, 5, 0))
			if tc.damage != nil {
				tc.damage(t, u)
			}
			parsed, err := u.ParseWithID(t.Context(), tc.id, "release.nzb", tc.content, "cat")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Parse error = %v, want it to contain %q", err, tc.want)
			}
			if parsed == nil {
				t.Fatal("Parse returned a nil *Parsed with its error")
			}
		})
	}
}

func gateNZBXML(ids ...string) []byte {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0"?><nzb xmlns="http://www.newzbin.com/DTD/2003/nzb">`)
	fmt.Fprintf(&b, `<file poster="p" date="1" subject="[1/1] - &quot;movie.mkv&quot; yEnc (1/%d)">`+
		`<groups><group>alt.test</group></groups><segments>`, len(ids))
	for _, id := range ids {
		number, message, _ := strings.Cut(id, ":")
		fmt.Fprintf(&b, `<segment bytes="5" number="%s">%s</segment>`, number, message)
	}
	b.WriteString(`</segments></file></nzb>`)
	return []byte(b.String())
}

type realParserCase struct {
	name           string
	articles       map[string]int
	segments       []string
	holes          bool
	goneAfterParse bool
}

func (tc realParserCase) run(t *testing.T) {
	var bodiesGone atomic.Bool
	u, srv := gateHarness(t, func(command, _ string) string {
		if command == "BODY" && bodiesGone.Load() {
			return "430 no such article"
		}
		return ""
	})
	for id, n := range tc.articles {
		srv.AddArticle("<"+id+">", nntpd.Encode([]byte("hello"), "movie.mkv", n, 15, int64(n-1)*5))
	}
	parsed, err := u.analyzer.Parse(t.Context(), "release.nzb", gateNZBXML(tc.segments...))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	bodiesGone.Store(tc.goneAfterParse)
	parsed.NZB.Status = NZBStatusParsing
	if addErr := u.nzbStorage.AddNZB(parsed.NZB); addErr != nil {
		t.Fatal(addErr)
	}
	_, result, err := u.Process(t.Context(), parsed)
	if err == nil {
		t.Fatal("Process succeeded, want a failure")
	}
	if result.GateMissing {
		t.Fatalf("GateMissing = true for a parser failure (%v)", err)
	}
	if tc.holes && !strings.Contains(err.Error(), "not contiguous") {
		t.Fatalf("error = %v, want a segment hole", err)
	}
	if tc.holes && result.Outcome.Verdict != parser.VerdictSegmentsMissing {
		t.Fatalf("Outcome.Verdict = %v, want %v", result.Outcome.Verdict, parser.VerdictSegmentsMissing)
	}
	if tc.goneAfterParse {
		if !strings.Contains(err.Error(), "failed to fetch first segment header") ||
			!strings.Contains(err.Error(), "code 430") {
			t.Fatalf("error = %v, want the first-segment fetch failing with a 430", err)
		}
		if want := (parser.Outcome{Verdict: parser.VerdictArticlesMissing, Rule: "error chain"}); result.Outcome != want {
			t.Fatalf("Outcome = %+v, want %+v", result.Outcome, want)
		}
	}
	if got := parser.VerdictOf(errors.Unwrap(err)); got != result.Outcome.Verdict {
		t.Fatalf("Outcome.Verdict = %v, VerdictOf gives %v", result.Outcome.Verdict, got)
	}
}

func TestGateFactRealParserProcess(t *testing.T) {
	t.Parallel()
	for _, tc := range []realParserCase{
		{
			name:     "segment holes",
			articles: map[string]int{"a@gate": 1, "c@gate": 3},
			segments: []string{"1:a@gate", "3:c@gate"},
			holes:    true,
		},
		{
			name:           "enrich on 430s",
			articles:       map[string]int{"a@gate": 1, "b@gate": 2},
			segments:       []string{"1:a@gate", "2:b@gate"},
			goneAfterParse: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tc.run(t)
		})
	}
}

func TestParseKeepsGroupsAndOutcomeThroughAnalyzerFailure(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		parse func(u *Usenet) (*parser.Parsed, error)
	}{
		{"Parse", func(u *Usenet) (*parser.Parsed, error) {
			return u.Parse(t.Context(), "release.nzb", gateNZBXML("1:a@gate"), "cat")
		}},
		{"ParseWithID", func(u *Usenet) (*parser.Parsed, error) {
			return u.ParseWithID(t.Context(), "given-id", "release.nzb", gateNZBXML("1:a@gate"), "cat")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			u, _ := gateHarness(t, nil)
			parsed, err := tc.parse(u)
			if !errors.Is(err, customerror.ErrUsenetSegmentMissing) ||
				!strings.Contains(err.Error(), "failed to stat segment") {
				t.Fatalf("error = %v, want the STAT path's missing segments", err)
			}
			if parsed == nil || len(parsed.Groups) == 0 {
				t.Fatalf("parsed = %+v, want the groups kept with the error", parsed)
			}
			if parsed.Outcome.Verdict != parser.VerdictArticlesMissing {
				t.Fatalf("Outcome = %+v, want an articles-missing verdict", parsed.Outcome)
			}
		})
	}
}

func TestFinishReportsCompletionWriteFailureAsNotMissing(t *testing.T) {
	t.Parallel()
	u, _ := gateHarness(t, nil)
	nzb := &storage.NZB{ID: "gate", Name: "Movie", Status: NZBStatusParsing}
	blockMetaWrites(t, u.nzbStorage, nzb.ID)

	got, err := u.finish(t.Context(), nzb, u.gateSteps(t.Context(), nzb))
	if err == nil || !strings.Contains(err.Error(), "failed to mark NZB as completed") {
		t.Fatalf("finish error = %v, want the completion write failure", err)
	}
	if got.GateMissing {
		t.Fatal("a failed completion write was reported as a missing release")
	}
	if got.Outcome != (parser.Outcome{}) {
		t.Fatalf("Outcome = %+v, want none", got.Outcome)
	}
}

func TestFinishCompletesWhenBothGatesPass(t *testing.T) {
	t.Parallel()
	u, srv := gateHarness(t, nil)
	payload := append([]byte{0x1A, 0x45, 0xDF, 0xA3}, make([]byte, 600)...)
	srv.AddArticle("<head@gate>", nntpd.Encode(payload, "Movie.mkv", 1, int64(len(payload)), 0))
	nzb := gateNZB(t, u, gateFile(len(payload)))

	got, err := u.finish(t.Context(), nzb, u.gateSteps(t.Context(), nzb))
	if err != nil || got != (ProcessResult{}) {
		t.Fatalf("finish = %+v, %v, want a clean completion", got, err)
	}
	if nzb.Status != NZBStatusCompleted {
		t.Fatalf("Status = %q, want %q", nzb.Status, NZBStatusCompleted)
	}
}

func TestStatAvailabilityReturns(t *testing.T) {
	t.Parallel()
	connectionDown := func(command, _ string) string {
		if command == "STAT" {
			return "400 service unavailable"
		}
		return ""
	}
	for _, tc := range []struct {
		name    string
		reply   func(command, messageID string) string
		ids     []string
		serve   bool
		stopped bool
		want    gate.Kind
	}{
		{name: "no ids", want: gate.KindPass},
		{name: "every article available", ids: []string{"<head@gate>"}, serve: true, want: gate.KindPass},
		{
			name: "connection errors only", reply: connectionDown, ids: []string{"<head@gate>"}, serve: true,
			want: gate.KindUnchecked,
		},
		{name: "client stopped", ids: []string{"<head@gate>"}, serve: true, stopped: true, want: gate.KindUnchecked},
		{name: "an article is gone", ids: []string{"<head@gate>"}, want: gate.KindMissing},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			u, srv, root := gateHarnessGroup(t, tc.reply)
			if tc.serve {
				srv.AddArticle("<head@gate>", nntpd.Encode([]byte("hello"), "Movie.mkv", 1, 5, 0))
			}
			if tc.stopped {
				stopLifecycleGroup(t, root)
			}
			r := u.statAvailability(t.Context(), "Movie.mkv", tc.ids)
			if r.Kind() != tc.want {
				t.Fatalf("kind = %v, want %v (error %v)", r.Kind(), tc.want, r.Err())
			}
			if (tc.want == gate.KindMissing) != errors.Is(r.Err(), customerror.ErrUsenetSegmentMissing) {
				t.Fatalf("error = %v, want missing segments only when an article is gone", r.Err())
			}
		})
	}
}

func TestVerifyFileHeadReturns(t *testing.T) {
	t.Parallel()
	payload := []byte(strings.Repeat("not a media container ", 40))
	noSegments := gateFile(len(payload))
	noSegments.Size = 0
	noSegments.Segments = nil
	unsupported := gateFile(len(payload))
	unsupported.FileType = storage.NZBFileTypeUnknown
	for _, tc := range []struct {
		name     string
		file     storage.NZBFile
		serve    bool
		bare     bool
		stopWork bool
		want     gate.Kind
		wantErr  string
	}{
		{name: "too small to classify", file: gateFile(100), want: gate.KindPass},
		{name: "entry cannot be created", file: gateFile(len(payload)), bare: true, stopWork: true, want: gate.KindUnchecked, wantErr: "create entry for Movie.mkv"},
		{name: "reader cannot be created", file: noSegments, bare: true, want: gate.KindUnchecked, wantErr: "no segments in volume"},
		{name: "analyzer defers to the serving cursor", file: unsupported, serve: true, want: gate.KindFailed, wantErr: "matches no media container signature"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			u, srv := gateHarness(t, nil)
			if tc.serve {
				srv.AddArticle("<head@gate>", nntpd.Encode(payload, "Movie.mkv", 1, int64(len(payload)), 0))
			}
			if tc.bare {
				u.analyzer = nil
			}
			if tc.stopWork {
				stopLifecycleGroup(t, u.work)
			}
			r := u.verifyFileHead(t.Context(), &tc.file)
			if r.Kind() != tc.want {
				t.Fatalf("kind = %v, want %v (error %v)", r.Kind(), tc.want, r.Err())
			}
			if !errorMatches(r.Err(), tc.wantErr) {
				t.Fatalf("error = %v, want %q", r.Err(), tc.wantErr)
			}
		})
	}
}

func TestClassifyHeadReadErrorKeepsRawErrors(t *testing.T) {
	t.Parallel()
	raw := errors.New("connection reset")
	r := classifyHeadReadError("Movie.mkv", raw)
	if r.Kind() != gate.KindUnchecked || !errors.Is(r.Err(), raw) {
		t.Fatalf("classifyHeadReadError = %v, %v, want unchecked with the raw error", r.Kind(), r.Err())
	}
}

func TestVerifyNZBContentReturns(t *testing.T) {
	t.Parallel()
	payload := []byte(strings.Repeat("not a media container ", 40))
	for _, tc := range []struct {
		name  string
		reply func(command, messageID string) string
		ended bool
		want  gate.Kind
	}{
		{name: "context ended", ended: true, want: gate.KindSkipped},
		{name: "connection errors are not a content failure", want: gate.KindPass, reply: func(command, _ string) string {
			if command == "BODY" {
				return "400 service unavailable"
			}
			return ""
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			u, srv := gateHarness(t, tc.reply)
			srv.AddArticle("<head@gate>", nntpd.Encode(payload, "Movie.mkv", 1, int64(len(payload)), 0))
			nzb := gateNZB(t, u, gateFile(len(payload)))
			ctx := t.Context()
			if tc.ended {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			r := u.verifyNZBContent(ctx, nzb)
			if r.Kind() != tc.want || r.Err() != nil {
				t.Fatalf("verifyNZBContent = %v, %v, want %v", r.Kind(), r.Err(), tc.want)
			}
		})
	}
}

func TestProcessCompletesAReleaseThatPassesBothGates(t *testing.T) {
	t.Parallel()
	u, srv := gateHarness(t, nil)
	payload := append([]byte{0x1A, 0x45, 0xDF, 0xA3}, make([]byte, 600)...)
	srv.AddArticle("<a@gate>", nntpd.Encode(payload, "movie.mkv", 1, int64(len(payload)), 0))
	parsed, err := u.analyzer.Parse(t.Context(), "release.nzb", gateNZBXML("1:a@gate"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	parsed.NZB.Status = NZBStatusParsing
	if addErr := u.nzbStorage.AddNZB(parsed.NZB); addErr != nil {
		t.Fatal(addErr)
	}
	updated, result, err := u.Process(t.Context(), parsed)
	if err != nil || result != (ProcessResult{}) || updated == nil || updated.Status != NZBStatusCompleted {
		t.Fatalf("Process = %+v, %+v, %v, want a completed NZB and a clean result", updated, result, err)
	}
}

func errorMatches(err error, want string) bool {
	if want == "" {
		return err == nil
	}
	return err != nil && strings.Contains(err.Error(), want)
}

func TestParseWithIDReturnsTheParsedNZB(t *testing.T) {
	t.Parallel()
	u, srv := gateHarness(t, nil)
	srv.AddArticle("<a@gate>", nntpd.Encode([]byte("hello"), "movie.mkv", 1, 5, 0))

	parsed, err := u.ParseWithID(t.Context(), "given-id", "release.nzb", gateNZBXML("1:a@gate"), "cat")
	if err != nil {
		t.Fatalf("ParseWithID: %v", err)
	}
	if parsed.NZB == nil || parsed.NZB.ID != "given-id" || parsed.NZB.Category != "cat" ||
		parsed.NZB.Status != NZBStatusParsing || parsed.NZB.Path == "" {
		t.Fatalf("parsed NZB = %+v, want the given id, category, parsing status and a saved path", parsed.NZB)
	}
	if len(parsed.Groups) == 0 || parsed.Outcome != (parser.Outcome{}) {
		t.Fatalf("parsed = %+v, want groups and no outcome", parsed)
	}
	if _, headerErr := u.nzbStorage.GetNZBHeader("given-id"); headerErr != nil {
		t.Fatalf("the parsed NZB was not stored: %v", headerErr)
	}
}

func TestProcessRunsTheContentStep(t *testing.T) {
	t.Parallel()
	u, srv := gateHarness(t, nil)
	payload := []byte(strings.Repeat("not a media container ", 40))
	srv.AddArticle("<a@gate>", nntpd.Encode(payload, "movie.mkv", 1, int64(len(payload)), 0))
	parsed, err := u.analyzer.Parse(t.Context(), "release.nzb", gateNZBXML("1:a@gate"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	parsed.NZB.Status = NZBStatusParsing
	if addErr := u.nzbStorage.AddNZB(parsed.NZB); addErr != nil {
		t.Fatal(addErr)
	}
	updated, _, err := u.Process(t.Context(), parsed)
	if !errors.Is(err, customerror.ErrUsenetCorruptContent) {
		t.Fatalf("Process error = %v, want the content gate's corrupt content", err)
	}
	if updated == nil || updated.Status == NZBStatusCompleted {
		t.Fatalf("Process = %+v, want an NZB that is not completed", updated)
	}
}

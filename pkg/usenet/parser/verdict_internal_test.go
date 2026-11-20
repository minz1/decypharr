package parser

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/customerror"
	"github.com/sirrobot01/decypharr/internal/nntp"
	"github.com/sirrobot01/decypharr/pkg/storage"
	"github.com/sirrobot01/decypharr/pkg/usenet/manifest"
)

func bare430() error {
	return &nntp.Error{Type: nntp.ErrorTypeArticleNotFound, Code: 430, Message: "no such article"}
}

func proven430() error {
	return &nntp.NotFoundEverywhereError{Err: fmt.Errorf("%w: %w", nntp.ErrAllProvidersFailed, bare430())}
}

type scriptSource struct {
	headerSource

	header func(id string) (*nntp.YencMetadata, error)
	stat   func(id string) error
}

func (s scriptSource) Header(_ context.Context, id string, _ int) (*nntp.YencMetadata, error) {
	return s.header(id)
}

func (s scriptSource) Stat(_ context.Context, id string) error {
	if s.stat == nil {
		return nil
	}
	return s.stat(id)
}

func provenHeaders(_ string) (*nntp.YencMetadata, error) { return nil, proven430() }

func provenStat(string) error { return proven430() }

func segs(ids ...string) []manifest.Segment {
	out := make([]manifest.Segment, len(ids))
	for i, id := range ids {
		out[i] = manifest.Segment{Number: i + 1, MessageID: id}
	}
	return out
}

const fileOpen = `<file poster="p" date="1" subject="%s"><groups><group>alt.test</group></groups><segments>`

func fileXML(index, total int, name string, segments ...string) string {
	var b strings.Builder
	subject := fmt.Sprintf(`[%d/%d] - &quot;%s&quot; yEnc (1/%d)`, index, total, name, len(segments))
	fmt.Fprintf(&b, fileOpen, subject)
	for _, segment := range segments {
		number, id, _ := strings.Cut(segment, ":")
		fmt.Fprintf(&b, `<segment bytes="768000" number="%s">%s</segment>`, number, id)
	}
	b.WriteString(`</segments></file>`)
	return b.String()
}

func nzbOf(files ...string) string {
	return `<?xml version="1.0"?><nzb xmlns="http://www.newzbin.com/DTD/2003/nzb">` + strings.Join(files, "") + `</nzb>`
}

func parseVerdict(t *testing.T, p *NZBParser, nzb string) (Verdict, error) {
	t.Helper()
	_, _, err := p.Parse(t.Context(), "release.nzb", []byte(nzb))
	return VerdictOf(err), err
}

func isContentMissing(err error) bool {
	_, ok := errors.AsType[*ContentMissingError](err)
	return ok
}

func TestHeaderPrefixAllProvenIsContentMissing(t *testing.T) {
	t.Parallel()
	file := manifest.File{Segments: segs("a@x", "b@x", "c@x")}
	source := scriptSource{header: provenHeaders}
	_, err := fetchFileHeaderPrefix(t.Context(), source, file, metadataOnly)
	if !isContentMissing(err) || !nntp.IsArticleNotFoundError(err) {
		t.Fatalf("error = %v, want a ContentMissingError that is still not-found", err)
	}
}

func TestHeaderPrefixOneUnprovenIsNot(t *testing.T) {
	t.Parallel()
	file := manifest.File{Segments: segs("a@x", "b@x", "c@x")}
	source := scriptSource{header: func(id string) (*nntp.YencMetadata, error) {
		if id == "b@x" {
			return nil, bare430()
		}
		return nil, proven430()
	}}
	_, err := fetchFileHeaderPrefix(t.Context(), source, file, metadataOnly)
	if err == nil || isContentMissing(err) || !nntp.IsArticleNotFoundError(err) {
		t.Fatalf("error = %v, want a plain not-found error", err)
	}
}

func TestHeaderPrefixYencThenProvenIsNot(t *testing.T) {
	t.Parallel()
	file := manifest.File{Segments: segs("a@x", "b@x", "c@x")}
	source := scriptSource{header: func(id string) (*nntp.YencMetadata, error) {
		if id == "a@x" {
			return nil, &nntp.Error{Type: nntp.ErrorTypeYencDecode, Message: "bad crc"}
		}
		return nil, proven430()
	}}
	_, err := fetchFileHeaderPrefix(t.Context(), source, file, metadataOnly)
	if err == nil || isContentMissing(err) {
		t.Fatalf("error = %v, want no proof after a corrupt probe", err)
	}
}

func TestHeaderPrefixEmptyIDIsNot(t *testing.T) {
	t.Parallel()
	file := manifest.File{Segments: segs("a@x", "", "c@x")}
	source := scriptSource{header: provenHeaders}
	_, err := fetchFileHeaderPrefix(t.Context(), source, file, metadataOnly)
	if err == nil || isContentMissing(err) {
		t.Fatalf("error = %v, want no proof with an unaskable segment", err)
	}
}

func TestHeaderPrefixCtxErrorIsNot(t *testing.T) {
	t.Parallel()
	file := manifest.File{Segments: segs("a@x", "b@x")}
	source := scriptSource{header: func(id string) (*nntp.YencMetadata, error) {
		if id == "a@x" {
			return nil, proven430()
		}
		return nil, context.Canceled
	}}
	_, err := fetchFileHeaderPrefix(t.Context(), source, file, metadataOnly)
	if !errors.Is(err, context.Canceled) || isContentMissing(err) {
		t.Fatalf("error = %v, want the context error without proof", err)
	}
}

func TestHeaderPrefixDuplicateNumberIsNot(t *testing.T) {
	t.Parallel()
	file := manifest.File{Segments: []manifest.Segment{
		{Number: 1, MessageID: "a@x"}, {Number: 1, MessageID: "b@x"}, {Number: 2, MessageID: "c@x"},
	}}
	source := scriptSource{header: provenHeaders}
	_, err := fetchFileHeaderPrefix(t.Context(), source, file, metadataOnly)
	if err == nil || isContentMissing(err) {
		t.Fatalf("error = %v, want no proof when a probed number has an alternative", err)
	}
}

func mediaProbeGroups(files ...string) map[string]*FileGroup {
	groups := make(map[string]*FileGroup, len(files))
	for _, name := range files {
		groups[name] = &FileGroup{
			BaseName: name,
			Type:     storage.NZBFileTypeMedia,
			Files:    []manifest.File{{Filename: name + ".mkv", Segments: segs(name + "@x")}},
		}
	}
	return groups
}

func TestProbeAllProvenIsContentMissing(t *testing.T) {
	t.Parallel()
	p := NewParserWithSource(scriptSource{}, 1, zerolog.Nop())
	err := p.probeContentAvailability(t.Context(), mediaProbeGroups("a", "b"),
		func(context.Context, string) error { return proven430() }, false)
	if !errors.Is(err, customerror.ErrUsenetSegmentMissing) || VerdictOf(err) != VerdictArticlesMissing {
		t.Fatalf("error = %v verdict %v, want missing with articles verdict", err, VerdictOf(err))
	}
}

func TestProbeOneUnprovenIsNot(t *testing.T) {
	t.Parallel()
	p := NewParserWithSource(scriptSource{}, 1, zerolog.Nop())
	err := p.probeContentAvailability(t.Context(), mediaProbeGroups("a", "b"),
		func(_ context.Context, id string) error {
			if id == "b@x" {
				return bare430()
			}
			return proven430()
		}, false)
	if !errors.Is(err, customerror.ErrUsenetSegmentMissing) || VerdictOf(err) != VerdictNone {
		t.Fatalf("error = %v verdict %v, want missing without a verdict", err, VerdictOf(err))
	}
}

func TestProbeWithDetectionDoubtIsNot(t *testing.T) {
	t.Parallel()
	p := NewParserWithSource(scriptSource{}, 1, zerolog.Nop())
	err := p.probeContentAvailability(t.Context(), mediaProbeGroups("a"),
		func(context.Context, string) error { return proven430() }, true)
	if !errors.Is(err, customerror.ErrUsenetSegmentMissing) || VerdictOf(err) != VerdictNone {
		t.Fatalf("error = %v verdict %v, want missing without a verdict", err, VerdictOf(err))
	}
}

func TestParseDuplicateNumberAlternativeIsNotProof(t *testing.T) {
	t.Parallel()
	source := scriptSource{header: provenHeaders, stat: func(id string) error {
		if id == "a@x" {
			return proven430()
		}
		return nil
	}}
	p := NewParserWithSource(source, 2, zerolog.Nop())
	verdict, err := parseVerdict(t, p, nzbOf(fileXML(1, 1, "movie.mkv", "1:a@x", "1:b@x", "2:c@x")))
	if !errors.Is(err, customerror.ErrUsenetSegmentMissing) || verdict != VerdictNone {
		t.Fatalf("error = %v verdict %v, want missing without a verdict", err, verdict)
	}
}

func TestParseDuplicateNumberUniqueControlIsProof(t *testing.T) {
	t.Parallel()
	source := scriptSource{header: provenHeaders, stat: provenStat}
	p := NewParserWithSource(source, 2, zerolog.Nop())
	verdict, err := parseVerdict(t, p, nzbOf(fileXML(1, 1, "movie.mkv", "1:a@x", "2:b@x", "3:c@x")))
	if !errors.Is(err, customerror.ErrUsenetSegmentMissing) || verdict != VerdictArticlesMissing {
		t.Fatalf("error = %v verdict %v, want an articles verdict", err, verdict)
	}
}

const (
	knownMedia = "release.mkv"
	deadOne    = "a8f3k2q9z7"
	deadTwo    = "q7z9w2e4r6"
)

func twoUnknownsNZB() string {
	return nzbOf(
		fileXML(1, 3, knownMedia, "1:media@x"),
		fileXML(2, 3, deadOne, "1:one@x"),
		fileXML(3, 3, deadTwo, "1:two@x"),
	)
}

func TestParseDetectionTimeoutBlocksProof(t *testing.T) {
	t.Parallel()
	source := scriptSource{stat: provenStat, header: func(id string) (*nntp.YencMetadata, error) {
		if id == "two@x" {
			return nil, context.DeadlineExceeded
		}
		return nil, proven430()
	}}
	p := NewParserWithSource(source, 2, zerolog.Nop())
	verdict, err := parseVerdict(t, p, twoUnknownsNZB())
	if !errors.Is(err, customerror.ErrUsenetSegmentMissing) || verdict != VerdictNone {
		t.Fatalf("error = %v verdict %v, want missing without a verdict", err, verdict)
	}
}

func TestParseObservedUnknownBlocksProof(t *testing.T) {
	t.Parallel()
	source := scriptSource{stat: provenStat, header: func(id string) (*nntp.YencMetadata, error) {
		if id == "two@x" {
			return &nntp.YencMetadata{Size: 10, PartSize: 10, Snippet: []byte("plain text")}, nil
		}
		return nil, proven430()
	}}
	p := NewParserWithSource(source, 2, zerolog.Nop())
	verdict, err := parseVerdict(t, p, twoUnknownsNZB())
	if !errors.Is(err, customerror.ErrUsenetSegmentMissing) || verdict != VerdictNone {
		t.Fatalf("error = %v verdict %v, want missing without a verdict", err, verdict)
	}
}

func TestParseAllProvenMissingIsArticles(t *testing.T) {
	t.Parallel()
	source := scriptSource{stat: provenStat, header: provenHeaders}
	p := NewParserWithSource(source, 2, zerolog.Nop())
	verdict, err := parseVerdict(t, p, twoUnknownsNZB())
	if !errors.Is(err, customerror.ErrUsenetSegmentMissing) || verdict != VerdictArticlesMissing {
		t.Fatalf("error = %v verdict %v, want an articles verdict", err, verdict)
	}
}

func TestAllMissingSiteProof(t *testing.T) {
	t.Parallel()
	nzb := nzbOf(fileXML(1, 2, deadOne, "1:one@x"), fileXML(2, 2, deadTwo, "1:two@x"))
	p := NewParserWithSource(scriptSource{header: provenHeaders}, 2, zerolog.Nop())
	verdict, err := parseVerdict(t, p, nzb)
	if !errors.Is(err, customerror.ErrUsenetSegmentMissing) || verdict != VerdictArticlesMissing {
		t.Fatalf("all proven: error = %v verdict %v, want an articles verdict", err, verdict)
	}

	mixed := scriptSource{header: func(id string) (*nntp.YencMetadata, error) {
		if id == "two@x" {
			return nil, bare430()
		}
		return nil, proven430()
	}}
	p = NewParserWithSource(mixed, 2, zerolog.Nop())
	verdict, err = parseVerdict(t, p, nzb)
	if !errors.Is(err, customerror.ErrUsenetSegmentMissing) || verdict != VerdictNone {
		t.Fatalf("one bare 430: error = %v verdict %v, want missing without a verdict", err, verdict)
	}
}

func TestDoubtCarriesIntoProcess(t *testing.T) {
	t.Parallel()
	named := fileXML(1, 2, "movie.part1.rar", "1:rar@x")
	for _, tc := range []struct {
		name string
		nzb  string
		want Verdict
	}{
		{"timeout beside the set", nzbOf(named, fileXML(2, 2, deadOne, "1:one@x")), VerdictNone},
		{"no doubt", nzbOf(named), VerdictArticlesMissing},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			source := scriptSource{header: func(id string) (*nntp.YencMetadata, error) {
				switch id {
				case "one@x":
					return nil, context.DeadlineExceeded
				default:
					return nil, proven430()
				}
			}}
			p := NewParserWithSource(source, 2, zerolog.Nop())
			nzb, groups, err := p.Parse(t.Context(), "release.nzb", []byte(tc.nzb))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			_, err = p.Process(t.Context(), nzb, groups)
			if err == nil || VerdictOf(err) != tc.want {
				t.Fatalf("Process error = %v verdict %v, want verdict %v", err, VerdictOf(err), tc.want)
			}
		})
	}
}

func refuseSamples(name string, _ int64) error {
	if strings.Contains(strings.ToLower(name), "sample") {
		return errors.New("sample refused")
	}
	return nil
}

func TestRefusedNamesWithholdProof(t *testing.T) {
	t.Parallel()
	source := scriptSource{stat: provenStat, header: provenHeaders}
	newParser := func() *NZBParser {
		return NewParserWithSource(source, 2, zerolog.Nop()).WithFileFilter(refuseSamples)
	}

	t.Run("refused media name at Parse", func(t *testing.T) {
		t.Parallel()
		verdict, err := parseVerdict(t, newParser(), nzbOf(fileXML(1, 1, "x.sample.mkv", "1:a@x")))
		if !errors.Is(err, customerror.ErrUsenetSegmentMissing) || verdict != VerdictNone {
			t.Fatalf("error = %v verdict %v, want missing without a verdict", err, verdict)
		}
	})

	t.Run("allowed media name at Parse", func(t *testing.T) {
		t.Parallel()
		verdict, err := parseVerdict(t, newParser(), nzbOf(fileXML(1, 1, "x.mkv", "1:a@x")))
		if !errors.Is(err, customerror.ErrUsenetSegmentMissing) || verdict != VerdictArticlesMissing {
			t.Fatalf("error = %v verdict %v, want an articles verdict", err, verdict)
		}
	})

	process := func(t *testing.T, nzbName string) Verdict {
		t.Helper()
		passing := scriptSource{header: provenHeaders}
		p := NewParserWithSource(passing, 2, zerolog.Nop()).WithFileFilter(refuseSamples)
		nzb, groups, err := p.Parse(t.Context(), nzbName, []byte(nzbOf(fileXML(1, 1, "movie.mkv", "1:a@x", "2:b@x"))))
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		_, err = p.Process(t.Context(), nzb, groups)
		if err == nil {
			t.Fatal("Process succeeded over missing headers")
		}
		return VerdictOf(err)
	}

	t.Run("refused nzb name at Process", func(t *testing.T) {
		t.Parallel()
		if got := process(t, "release.sample.nzb"); got != VerdictNone {
			t.Fatalf("verdict = %v, want none", got)
		}
	})

	t.Run("allowed nzb name at Process", func(t *testing.T) {
		t.Parallel()
		if got := process(t, "release.nzb"); got != VerdictArticlesMissing {
			t.Fatalf("verdict = %v, want an articles verdict", got)
		}
	})
}

func joinGroup(name, id string) *FileGroup {
	return &FileGroup{
		BaseName: name,
		Type:     storage.NZBFileTypeMedia,
		fileMeta: map[string]filePartMeta{},
		key:      name,
		Files:    []manifest.File{{Filename: name + ".mkv", Number: 1, Segments: segs(id)}},
	}
}

func TestProcessFileGroupsJoin(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	source := scriptSource{header: func(id string) (*nntp.YencMetadata, error) {
		if id == "bad@x" {
			return nil, boom
		}
		return nil, proven430()
	}}
	p := NewParserWithSource(source, 2, zerolog.Nop())
	for _, tc := range []struct {
		name   string
		groups map[string]*FileGroup
		want   Verdict
	}{
		{"both proven", map[string]*FileGroup{"a": joinGroup("a", "p1@x"), "b": joinGroup("b", "p2@x")}, VerdictArticlesMissing},
		{"proven first", map[string]*FileGroup{"a": joinGroup("a", "p1@x"), "b": joinGroup("b", "bad@x")}, VerdictNone},
		{"proven last", map[string]*FileGroup{"a": joinGroup("a", "bad@x"), "b": joinGroup("b", "p1@x")}, VerdictNone},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := p.processFileGroups(t.Context(), tc.groups, "")
			if err == nil || VerdictOf(err) != tc.want {
				t.Fatalf("error = %v verdict %v, want verdict %v", err, VerdictOf(err), tc.want)
			}
		})
	}
}

func TestVerdictOf(t *testing.T) {
	t.Parallel()
	cm := &ContentMissingError{Err: errors.New("gone")}
	holes := &SegmentHolesError{File: "f", Present: 1, Span: 2}
	for _, tc := range []struct {
		name string
		err  error
		want Verdict
	}{
		{"nil", nil, VerdictNone},
		{"canceled", context.Canceled, VerdictNone},
		{"bare 430", bare430(), VerdictNone},
		{"proven 430 alone", proven430(), VerdictNone},
		{"content missing", cm, VerdictArticlesMissing},
		{"wrapped twice", fmt.Errorf("a: %w", fmt.Errorf("b: %w", cm)), VerdictArticlesMissing},
		{"holes", holes, VerdictSegmentsMissing},
		{"sentinel beside proof", fmt.Errorf("%w: %w", customerror.ErrUsenetSegmentMissing, cm), VerdictNone},
		{"join without proof", errors.Join(errors.New("a"), errors.New("b")), VerdictNone},
		{"join of proofs", errors.Join(cm, fmt.Errorf("x: %w", cm)), VerdictArticlesMissing},
		{"join proof then boom", errors.Join(cm, errors.New("boom")), VerdictNone},
		{"join boom then proof", errors.Join(errors.New("boom"), cm), VerdictNone},
		{"join articles and holes", errors.Join(cm, holes), VerdictNone},
		{"join holes and articles", errors.Join(holes, cm), VerdictNone},
		{"withheld proof", &proofWithheldError{err: cm}, VerdictNone},
		{"withheld inside wrap", fmt.Errorf("x: %w", &proofWithheldError{err: cm}), VerdictNone},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := VerdictOf(tc.err); got != tc.want {
				t.Fatalf("VerdictOf = %v, want %v", got, tc.want)
			}
		})
	}
}

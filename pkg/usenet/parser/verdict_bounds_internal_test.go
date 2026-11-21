package parser

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/customerror"
	"github.com/sirrobot01/decypharr/internal/nntp"
	"github.com/sirrobot01/decypharr/pkg/storage"
	"github.com/sirrobot01/decypharr/pkg/usenet/manifest"
)

func healthyHeader(string) (*nntp.YencMetadata, error) {
	return &nntp.YencMetadata{Size: 15, PartSize: 5, Part: 1, Begin: 1, End: 5}, nil
}

func TestSizeFilterWithholdsProof(t *testing.T) {
	t.Parallel()
	sized := (&config.Config{AllowedExt: []string{"mkv"}, MinFileSize: "100MB"}).ValidateFileAllowed
	capped := (&config.Config{AllowedExt: []string{"mkv"}, MaxFileSize: "1MB"}).ValidateFileAllowed
	plain := (&config.Config{AllowedExt: []string{"mkv"}}).ValidateFileAllowed

	holes := func(t *testing.T, filter func(string, int64) error) Verdict {
		t.Helper()
		p := NewParserWithSource(scriptSource{header: healthyHeader}, 2, zerolog.Nop()).WithFileFilter(filter)
		nzb, groups, err := p.Parse(
			t.Context(),
			"release.nzb",
			[]byte(nzbOf(fileXML(1, 1, "preview.mkv", "1:a@x", "3:c@x"))),
		)
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		_, err = p.Process(t.Context(), nzb, groups)
		if err == nil || !strings.Contains(err.Error(), "not contiguous") {
			t.Fatalf("Process error = %v, want a hole", err)
		}
		return VerdictOf(err)
	}
	dead := func(t *testing.T, filter func(string, int64) error, file string) Verdict {
		t.Helper()
		source := scriptSource{stat: provenStat, header: provenHeaders}
		p := NewParserWithSource(source, 2, zerolog.Nop()).WithFileFilter(filter)
		verdict, err := parseVerdict(t, p, nzbOf(fileXML(1, 1, file, "1:a@x")))
		if !errors.Is(err, customerror.ErrUsenetSegmentMissing) {
			t.Fatalf("error = %v, want missing", err)
		}
		return verdict
	}
	for _, row := range []struct {
		name   string
		got    func(*testing.T) Verdict
		expect Verdict
	}{
		{"holes under a minimum size", func(t *testing.T) Verdict { return holes(t, sized) }, VerdictNone},
		{"holes under a maximum size", func(t *testing.T) Verdict { return holes(t, capped) }, VerdictNone},
		{"holes without a size filter", func(t *testing.T) Verdict { return holes(t, plain) }, VerdictSegmentsMissing},
		{"dead media under a minimum size", func(t *testing.T) Verdict { return dead(t, sized, "movie.mkv") }, VerdictNone},
		{"dead split under a minimum size", func(t *testing.T) Verdict { return dead(t, sized, "movie.mkv.001") }, VerdictNone},
		{"dead media without a size filter", func(t *testing.T) Verdict { return dead(t, plain, "movie.mkv") }, VerdictArticlesMissing},
		{"dead split without a size filter", func(t *testing.T) Verdict { return dead(t, plain, "movie.mkv.001") }, VerdictArticlesMissing},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			if got := row.got(t); got != row.expect {
				t.Fatalf("verdict %v, want %v", got, row.expect)
			}
		})
	}
}

type bodyScript struct {
	scriptSource

	body func(id string) ([]byte, error)
}

func (s bodyScript) Body(_ context.Context, id string) ([]byte, error) { return s.body(id) }

func TestBodySourceVerdictIsNotProof(t *testing.T) {
	t.Parallel()
	for name, sourceErr := range map[string]error{
		"articles": &ContentMissingError{Err: context.DeadlineExceeded},
		"segments": &SegmentHolesError{File: "movie.mkv", Present: 1, Span: 2},
		"proof":    proven430(),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			source := bodyScript{body: func(string) ([]byte, error) { return nil, sourceErr }}
			_, err := fetchSegmentData(t.Context(), source, storage.NZBSegment{MessageID: "a@x"})
			wrapped := fmt.Errorf("zip tail: %w", err)
			if !errors.Is(err, sourceErr) || VerdictOf(wrapped) != VerdictNone {
				t.Fatalf("error = %v verdict %v, want the source error with no verdict", err, VerdictOf(wrapped))
			}
		})
	}
}

type deadBackend struct{}

func (deadBackend) Fetch(context.Context, string) ([]byte, *nntp.YencMetadata, nntp.Answer, error) {
	return nil, nil, nntp.AnswerMissing, proven430()
}

func (deadBackend) Stat(context.Context, string) (nntp.Answer, error) {
	return nntp.AnswerMissing, proven430()
}

func TestBrokerAllMissingBodyIsArticles(t *testing.T) {
	t.Parallel()
	p := NewParserWithSource(newArticleBroker(deadBackend{}, 2, 1<<20), 2, zerolog.Nop())
	verdict, err := parseVerdict(t, p, nzbOf(fileXML(1, 1, deadOne, "1:a@x", "2:b@x")))
	if !errors.Is(err, customerror.ErrUsenetSegmentMissing) || verdict != VerdictArticlesMissing {
		t.Fatalf("error = %v verdict %v, want an articles verdict from the broker's BODY path", err, verdict)
	}
}

func zipProbeGroup(files ...manifest.File) map[string]*FileGroup {
	return map[string]*FileGroup{"movie": {BaseName: "movie", Type: storage.NZBFileTypeZip, Files: files}}
}

func TestZIPProofNeedsBothEnds(t *testing.T) {
	t.Parallel()
	single := []manifest.File{{Filename: "movie.zip", Segments: segs("first@x", "mid@x", "last@x")}}
	split := []manifest.File{
		{Filename: "movie.zip", Segments: segs("zfirst@x", "last@x")},
		{Filename: "movie.z01", Segments: segs("first@x", "mid@x")},
	}
	numbered := []manifest.File{
		{Filename: "movie.zip.002", Segments: segs("vfirst@x", "last@x")},
		{Filename: "movie.zip.001", Segments: segs("first@x", "mid@x")},
	}
	for _, row := range []struct {
		name  string
		files []manifest.File
		dead  map[string]error
		want  error
		proof Verdict
	}{
		{"dead first, intact tail", single, map[string]error{"first@x": proven430()}, nil, VerdictNone},
		{"both ends dead", single, map[string]error{"first@x": proven430(), "last@x": proven430()}, customerror.ErrUsenetSegmentMissing, VerdictArticlesMissing},
		{"dead first, tail unproven", single, map[string]error{"first@x": proven430(), "last@x": bare430()}, customerror.ErrUsenetSegmentMissing, VerdictNone},
		{"split dead first, intact tail", split, map[string]error{"first@x": proven430()}, nil, VerdictNone},
		{"split both ends dead", split, map[string]error{"first@x": proven430(), "last@x": proven430()}, customerror.ErrUsenetSegmentMissing, VerdictArticlesMissing},
		{"numbered dead first, intact tail", numbered, map[string]error{"first@x": proven430()}, nil, VerdictNone},
		{"tail with an alternate article", []manifest.File{
			{Filename: "movie.zip", Segments: []manifest.Segment{{Number: 1, MessageID: "zfirst@x"}, {Number: 2, MessageID: "alt1@x"}, {Number: 2, MessageID: "alt2@x"}}},
			{Filename: "movie.z01", Segments: segs("first@x", "mid@x")},
		}, map[string]error{"first@x": proven430(), "alt1@x": proven430(), "alt2@x": proven430()}, customerror.ErrUsenetSegmentMissing, VerdictNone},
		{"numbered both ends dead", numbered, map[string]error{"first@x": proven430(), "last@x": proven430()}, customerror.ErrUsenetSegmentMissing, VerdictArticlesMissing},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			p := NewParserWithSource(scriptSource{}, 1, zerolog.Nop())
			files := append([]manifest.File(nil), row.files...)
			err := p.probeContentAvailability(
				t.Context(),
				zipProbeGroup(files...),
				func(_ context.Context, id string) (nntp.Answer, error) {
					return answerFromError(row.dead[id]), row.dead[id]
				},
				false,
			)
			if row.want == nil {
				if err != nil {
					t.Fatalf("error = %v, want the ZIP available", err)
				}
				return
			}
			if !errors.Is(err, row.want) || VerdictOf(err) != row.proof {
				t.Fatalf("error = %v verdict %v, want %v", err, VerdictOf(err), row.proof)
			}
		})
	}
}

func TestMediaProofKeepsFirstArticleRule(t *testing.T) {
	t.Parallel()
	p := NewParserWithSource(scriptSource{}, 1, zerolog.Nop())
	groups := map[string]*FileGroup{"movie": {
		BaseName: "movie", Type: storage.NZBFileTypeMedia,
		Files: []manifest.File{{Filename: "movie.mkv", Segments: segs("first@x", "last@x")}},
	}}
	stats := 0
	err := p.probeContentAvailability(t.Context(), groups, statWith(func(_ context.Context, id string) error {
		stats++
		if id == "first@x" {
			return proven430()
		}
		return nil
	}), false)
	if VerdictOf(err) != VerdictArticlesMissing || stats != 1 {
		t.Fatalf("error = %v verdict %v after %d stats, want an articles verdict from the first article alone",
			err, VerdictOf(err), stats)
	}
}

func archiveJoinGroup(name, file string, kind storage.NZBFileType, id string) *FileGroup {
	group := joinGroup(name, id)
	group.Type = kind
	group.Files[0].Filename = file
	return group
}

func TestProcessFileGroupsHealthySiblingWithholds(t *testing.T) {
	t.Parallel()
	source := scriptSource{header: func(id string) (*nntp.YencMetadata, error) {
		if strings.HasPrefix(id, "ok") {
			return &nntp.YencMetadata{Size: 5, PartSize: 5, Part: 1, Begin: 1, End: 5}, nil
		}
		return nil, proven430()
	}}
	filter := (&config.Config{AllowedExt: []string{"mkv"}}).ValidateFileAllowed
	p := NewParserWithSource(source, 2, zerolog.Nop()).WithFileFilter(filter)
	for _, row := range []struct {
		name   string
		groups map[string]*FileGroup
		want   Verdict
	}{
		{"healthy sibling", map[string]*FileGroup{"a": joinGroup("a", "ok@x"), "b": joinGroup("b", "dead@x")}, VerdictNone},
		{
			"sample-named archive alone",
			map[string]*FileGroup{"s": archiveJoinGroup("sample-extra", "sample-extra.zip", storage.NZBFileTypeZip, "dead@x")},
			VerdictNone,
		},
		{
			"sample-named rar beside a dead feature",
			map[string]*FileGroup{
				"s": archiveJoinGroup("sample-extra", "sample-extra.rar", storage.NZBFileTypeRar, "dead1@x"),
				"f": archiveJoinGroup("feature", "feature.rar", storage.NZBFileTypeRar, "dead2@x"),
			},
			VerdictNone,
		},
		{
			"obfuscated archive whose yEnc name is a sample",
			map[string]*FileGroup{"o": func() *FileGroup {
				group := archiveJoinGroup("a1b2c3", "a1b2c3.zip", storage.NZBFileTypeZip, "dead@x")
				group.ActualFilename = "sample-extra.zip"
				return group
			}()},
			VerdictNone,
		},
		{
			"plain archive alone",
			map[string]*FileGroup{"e": archiveJoinGroup("extra", "extra.zip", storage.NZBFileTypeZip, "dead@x")},
			VerdictArticlesMissing,
		},
		{"all dead", map[string]*FileGroup{"a": joinGroup("a", "dead1@x"), "b": joinGroup("b", "dead2@x")}, VerdictArticlesMissing},
		{
			"split zip with one dead volume",
			map[string]*FileGroup{"z": func() *FileGroup {
				group := archiveJoinGroup("movie", "movie.z01", storage.NZBFileTypeZip, "dead1@x")
				group.Files[0].Segments = segs("dead1@x", "dead2@x")
				group.Files = append(group.Files, manifest.File{Filename: "movie.zip", Number: 2, Segments: segs("ok@x")})
				return group
			}()},
			VerdictNone,
		},
		{
			"split zip with a dead last volume",
			map[string]*FileGroup{"z": func() *FileGroup {
				group := archiveJoinGroup("movie", "movie.z01", storage.NZBFileTypeZip, "ok1@x")
				group.Files[0].Segments = segs("ok1@x", "ok2@x")
				group.Files = append(group.Files, manifest.File{Filename: "movie.zip", Number: 2, Segments: segs("dead@x")})
				return group
			}()},
			VerdictNone,
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			_, err := p.processFileGroups(t.Context(), row.groups, "")
			if err == nil || VerdictOf(err) != row.want {
				t.Fatalf("error = %v verdict %v, want %v", err, VerdictOf(err), row.want)
			}
		})
	}
}

func TestParseSampleArchiveWithholds(t *testing.T) {
	t.Parallel()
	filter := (&config.Config{AllowedExt: []string{"mkv"}}).ValidateFileAllowed
	source := scriptSource{stat: provenStat, header: provenHeaders}
	for _, row := range []struct {
		name string
		file string
		want Verdict
	}{
		{"sample-named zip", "sample-extra.zip", VerdictNone},
		{"sample-named rar", "sample-extra.rar", VerdictNone},
		{"plain zip", "extra.zip", VerdictArticlesMissing},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			p := NewParserWithSource(source, 2, zerolog.Nop()).WithFileFilter(filter)
			verdict, err := parseVerdict(t, p, nzbOf(fileXML(1, 1, row.file, "1:a@x", "2:b@x")))
			if !errors.Is(err, customerror.ErrUsenetSegmentMissing) || verdict != row.want {
				t.Fatalf("error = %v verdict %v, want %v", err, verdict, row.want)
			}
		})
	}
}

func TestFilterErrorIsNotProof(t *testing.T) {
	t.Parallel()
	filter := func(_ string, size int64) error {
		if size == 15 {
			return &ContentMissingError{Err: errors.New("filter unavailable")}
		}
		return nil
	}
	p := NewParserWithSource(scriptSource{header: healthyHeader}, 2, zerolog.Nop()).WithFileFilter(filter)
	nzb, groups, err := p.Parse(
		t.Context(),
		"release.nzb",
		[]byte(nzbOf(fileXML(1, 1, "movie.mkv", "1:a@x", "2:b@x", "3:c@x"))),
	)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	_, err = p.Process(t.Context(), nzb, groups)
	if err == nil || !strings.Contains(err.Error(), "skipped") || VerdictOf(err) != VerdictNone {
		t.Fatalf("error = %v verdict %v, want a skipped file with no verdict", err, VerdictOf(err))
	}
}

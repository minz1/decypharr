package manifest_test

import (
	"encoding/xml"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Tensai75/nzbparser"
	"golang.org/x/net/html/charset"

	"github.com/sirrobot01/decypharr/pkg/usenet/manifest"
)

func TestDecodePreservesCurrentNZBSemantics(t *testing.T) {
	t.Parallel()

	const source = `<?xml version="1.0" encoding="UTF-8"?>
<nzb xmlns="http://www.newzbin.com/DTD/2003/nzb">
  <!-- fixture comment -->
  <head>
    <meta type="title">Fixture title</meta>
    <meta type="password">secret</meta>
  </head>
  <file poster="poster-b" date="2" subject="&quot;second.r00&quot; [2/2] yEnc (1/3)">
    <groups><group>alt.binaries.b</group></groups>
    <segments>
      <segment bytes="200" number="2">second-2@example</segment>
      <segment bytes="100" number="1">second-1@example</segment>
    </segments>
  </file>
  <file poster="poster-b-duplicate" date="3" subject="&quot;second.r00&quot; [2/2] yEnc (1/3)">
    <groups><group>alt.binaries.duplicate</group></groups>
    <segments>
      <segment bytes="100" number="1">second-1@example</segment>
      <segment bytes="50" number="3">second-3@example</segment>
    </segments>
  </file>
  <file poster="poster-a" date="1" subject="&quot;first.rar&quot; [1/2] yEnc (1/2)">
    <groups><group>alt.binaries.a</group></groups>
    <segments>
      <segment bytes="200" number="2">first-2@example</segment>
      <segment bytes="100" number="1">first-1@example</segment>
    </segments>
  </file>
</nzb>`

	got, err := manifest.Decode(strings.NewReader(source))
	if err != nil {
		t.Fatal(err)
	}

	if got.Metadata["title"] != "Fixture title" || got.Metadata["password"] != "secret" {
		t.Fatalf("metadata = %#v", got.Metadata)
	}
	if got.Stats != (manifest.Statistics{TotalFiles: 2, AvailableSegments: 5, TotalSegments: 5, Bytes: 650}) {
		t.Fatalf("statistics = %#v", got.Stats)
	}
	if len(got.Files) != 2 {
		t.Fatalf("files = %d, want 2", len(got.Files))
	}

	first := got.Files[0]
	if first.Order != 0 || first.Number != 1 || first.Filename != "first.rar" || first.BaseFilename != "first" {
		t.Fatalf("first file identity = %#v", first)
	}
	if len(first.Segments) != 2 || first.Segments[0].Number != 1 || first.Segments[1].Number != 2 {
		t.Fatalf("first file segments = %#v", first.Segments)
	}

	second := got.Files[1]
	if second.Order != 1 || second.Number != 2 || second.Filename != "second.r00" || second.BaseFilename != "second" {
		t.Fatalf("second file identity = %#v", second)
	}
	if second.Bytes != 350 || len(second.Segments) != 3 {
		t.Fatalf("merged second file = %#v", second)
	}
	if len(second.Groups) != 1 || second.Groups[0] != "alt.binaries.b" {
		t.Fatalf("merged file groups = %#v", second.Groups)
	}
	for index, segment := range second.Segments {
		if segment.Number != index+1 {
			t.Fatalf("second segment %d = %#v", index, segment)
		}
	}
}

func TestDecodeRejectsInvalidInput(t *testing.T) {
	t.Parallel()

	if _, err := manifest.Decode(nil); err == nil {
		t.Fatal("nil reader succeeded")
	}
	if _, err := manifest.Decode(strings.NewReader("<nzb>")); err == nil {
		t.Fatal("malformed XML succeeded")
	}
}

func TestDecodeMatchesLegacyContract(t *testing.T) {
	t.Parallel()
	fixtures := map[string]string{
		"generated": generatedNZB(8, 16),
		"duplicates-and-inner-xml": `<?xml version="1.0" encoding="UTF-8"?>
<nzb xmlns="http://www.newzbin.com/DTD/2003/nzb">
<!--one--><!--two-->
<head><meta type="title"><![CDATA[A & B]]></meta><meta type="title">last</meta></head>
<file poster="first" date="1" subject="&quot;same.bin&quot; [1/2] yEnc (1/4)">
<groups><group>a</group></groups><segments>
<segment bytes="20" number="2">two@example</segment>
<segment bytes="10" number="1">one@example</segment>
</segments></file>
<file poster="ignored" date="2" subject="&quot;same.bin&quot; [1/2] yEnc (1/4)">
<groups><group>b</group></groups><segments>
<segment bytes="10" number="1">one@example</segment>
<segment bytes="30" number="4">four@example</segment>
</segments></file>
<file poster="second" date="3" subject="opaque-without-extension">
<groups><group>c</group></groups><segments><segment bytes="5" number="3">three@example</segment></segments>
</file></nzb>`,
	}
	for name, source := range fixtures {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := manifest.Decode(strings.NewReader(source))
			if err != nil {
				t.Fatal(err)
			}
			want := decodeLegacy(t, source)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("streaming manifest differs from compatibility contract\ngot:  %#v\nwant: %#v", got, want)
			}
		})
	}
}

// TestDecodeMatchesLegacyCommittedCorpus is the differential that always runs.
// The local-corpus test below reads a gitignored directory and skips when it is
// absent, so on every machine but the author's it proved nothing.
func TestDecodeMatchesLegacyCommittedCorpus(t *testing.T) {
	t.Parallel()
	paths, err := filepath.Glob(filepath.Join("testdata", "*.nzb"))
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) == 0 {
		t.Fatal("committed NZB corpus is missing from testdata")
	}
	for _, path := range paths {
		t.Run(filepath.Base(path), func(t *testing.T) {
			t.Parallel()
			source, readFileErr := os.ReadFile(path)
			if readFileErr != nil {
				t.Fatal(readFileErr)
			}
			got, readFileErr := manifest.Decode(strings.NewReader(string(source)))
			if readFileErr != nil {
				t.Fatal(readFileErr)
			}
			if want := decodeLegacy(t, string(source)); !reflect.DeepEqual(got, want) {
				t.Fatalf("streaming manifest differs from the compatibility parser\ngot:  %#v\nwant: %#v", got, want)
			}
		})
	}
}

// Subjects the subject parser cannot read leave Number at 0, so they all tie.
// The order they end up in becomes file.Order, which decides how the release is
// assembled, so it has to be document order every single time.
func TestDecodeOrdersUnparsedSubjectsDeterministically(t *testing.T) {
	t.Parallel()

	// A release where only some subjects are readable. The unreadable ones all
	// parse as file 1 of 1, so they tie with each other while the readable ones
	// carry distinct numbers - the mix that makes an unstable sort partition
	// and shuffle the tied group. All-equal keys alone would not: the sort
	// short-circuits on them and the bug would stay hidden.
	const fileCount = 64
	var builder strings.Builder
	builder.WriteString(`<?xml version="1.0" encoding="UTF-8"?><nzb xmlns="http://www.newzbin.com/DTD/2003/nzb">`)
	var opaque []string
	for index := range fileCount {
		id := fmt.Sprintf("file-%03d@example", index)
		subject := fmt.Sprintf(`&quot;readable-%03d.rar&quot; [%d/%d] yEnc (1/1)`, index, index+2, fileCount+2)
		if index%2 == 1 {
			subject = fmt.Sprintf("opaque-subject-%03d-without-any-parseable-part", index)
			opaque = append(opaque, id)
		}
		_, _ = fmt.Fprintf(
			&builder,
			`<file poster="p" date="%d" subject="%s"><groups><group>g</group></groups><segments><segment bytes="1" number="1">%s</segment></segments></file>`,
			index+1,
			subject,
			id,
		)
	}
	builder.WriteString(`</nzb>`)
	source := builder.String()

	for attempt := range 16 {
		decoded, err := manifest.Decode(strings.NewReader(source))
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for index, file := range decoded.Files {
			if file.Order != index {
				t.Fatalf("attempt %d: file %d has Order %d", attempt, index, file.Order)
			}
			if file.Filename == "" {
				got = append(got, file.Segments[0].MessageID)
			}
		}
		if len(got) != len(opaque) {
			t.Fatalf("attempt %d: found %d unreadable subjects, want %d", attempt, len(got), len(opaque))
		}
		if !reflect.DeepEqual(got, opaque) {
			t.Fatalf(
				"attempt %d: document order not preserved among tied files:\ngot:  %v\nwant: %v",
				attempt,
				got,
				opaque,
			)
		}
	}
}

// A pretty-printed NZB puts newlines and indentation inside <segment>, and
// innerxml keeps every byte of it. Untrimmed those ids reach NNTP verbatim and
// come back as missing articles.
func TestDecodeTrimsMessageIDWhitespace(t *testing.T) {
	t.Parallel()

	const source = `<?xml version="1.0" encoding="UTF-8"?>
<nzb xmlns="http://www.newzbin.com/DTD/2003/nzb">
  <file poster="p" date="1" subject="&quot;padded.mkv&quot; [1/1] yEnc (1/3)">
    <groups><group>g</group></groups>
    <segments>
      <segment bytes="10" number="1">
        padded-1@example
      </segment>
      <segment bytes="10" number="2">padded-2@example</segment>
      <segment bytes="10" number="3">
	padded-1@example
      </segment>
    </segments>
  </file>
</nzb>`

	decoded, err := manifest.Decode(strings.NewReader(source))
	if err != nil {
		t.Fatal(err)
	}
	if len(decoded.Files) != 1 {
		t.Fatalf("files = %d, want 1", len(decoded.Files))
	}
	// The third segment repeats the first id with different padding, so
	// trimming before the dedup key is taken must collapse the two.
	segments := decoded.Files[0].Segments
	if len(segments) != 2 {
		t.Fatalf("segments = %#v, want the whitespace duplicate removed", segments)
	}
	for _, segment := range segments {
		if strings.TrimSpace(segment.MessageID) != segment.MessageID {
			t.Fatalf("message id %q still carries whitespace", segment.MessageID)
		}
	}
	if segments[0].MessageID != "padded-1@example" || segments[1].MessageID != "padded-2@example" {
		t.Fatalf("message ids = %#v", segments)
	}
}

func TestDecodeMatchesLegacyLocalCorpus(t *testing.T) {
	t.Parallel()
	paths, err := filepath.Glob(filepath.Join("..", "..", "..", "data", "usenet", "nzbs", "*.nzb"))
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) == 0 {
		t.Skip("local NZB corpus is not present")
	}
	for _, path := range paths {
		t.Run(filepath.Base(path), func(t *testing.T) {
			t.Parallel()
			source, readFileErr := os.ReadFile(path)
			if readFileErr != nil {
				t.Fatal(readFileErr)
			}
			got, readFileErr := manifest.Decode(strings.NewReader(string(source)))
			if readFileErr != nil {
				t.Fatal(readFileErr)
			}
			want := decodeLegacy(t, string(source))
			if !reflect.DeepEqual(got, want) {
				t.Fatal("streaming manifest differs from the compatibility parser")
			}
		})
	}
}

func decodeLegacy(t *testing.T, source string) *manifest.Manifest {
	t.Helper()
	legacy, err := nzbparser.Parse(strings.NewReader(source))
	if err != nil {
		t.Fatal(err)
	}
	result := &manifest.Manifest{
		Comment:  legacy.Comment,
		Metadata: make(map[string]string, len(legacy.Meta)),
		Files:    make([]manifest.File, len(legacy.Files)),
		Stats: manifest.Statistics{
			TotalFiles:        legacy.TotalFiles,
			AvailableSegments: legacy.Segments,
			TotalSegments:     legacy.TotalSegments,
			Bytes:             legacy.Bytes,
		},
	}
	derived := deriveLegacy(t, source)
	result.NewestDate = derived.newest
	maps.Copy(result.Metadata, legacy.Meta)
	for fileIndex, legacyFile := range legacy.Files {
		segments := make([]manifest.Segment, len(legacyFile.Segments))
		for segmentIndex, legacySegment := range legacyFile.Segments {
			segments[segmentIndex] = manifest.Segment{
				Bytes:  int64(legacySegment.Bytes),
				Number: legacySegment.Number,
				// The one deliberate divergence from the legacy parser: it
				// hands back the raw innerxml, so a pretty-printed NZB gives
				// ids padded with newlines and indentation. Decode trims them.
				// Everything else must still match exactly.
				MessageID: strings.TrimSpace(legacySegment.Id),
			}
		}
		result.Files[fileIndex] = manifest.File{
			Order:         fileIndex,
			Groups:        append([]string(nil), legacyFile.Groups...),
			Segments:      segments,
			Poster:        legacyFile.Poster,
			Date:          legacyFile.Date,
			Subject:       legacyFile.Subject,
			Number:        legacyFile.Number,
			Filename:      legacyFile.Filename,
			BaseFilename:  legacyFile.Basefilename,
			TotalSegments: legacyFile.TotalSegments,
			Bytes:         legacyFile.Bytes,
			Rewritten:     derived.rewritten[legacyFile.Subject],
		}
	}
	return result
}

func BenchmarkDecode(b *testing.B) {
	source := generatedNZB(64, 128)
	b.ReportAllocs()
	b.SetBytes(int64(len(source)))

	for b.Loop() {
		// b.Loop keeps the result alive; no sink variable needed.
		if _, err := manifest.Decode(strings.NewReader(source)); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkLegacyDecode(b *testing.B) {
	source := generatedNZB(64, 128)
	b.ReportAllocs()
	b.SetBytes(int64(len(source)))

	for b.Loop() {
		if _, err := nzbparser.Parse(strings.NewReader(source)); err != nil {
			b.Fatal(err)
		}
	}
}

func generatedNZB(fileCount, segmentsPerFile int) string {
	var builder strings.Builder
	builder.Grow(fileCount * segmentsPerFile * 80)
	builder.WriteString(
		`<?xml version="1.0" encoding="UTF-8"?><nzb xmlns="http://www.newzbin.com/DTD/2003/nzb"><head><meta type="title">Benchmark</meta></head>`,
	)
	for fileIndex := range fileCount {
		_, _ = fmt.Fprintf(
			&builder,
			`<file poster="benchmark" date="1" subject="&quot;file-%03d.mkv&quot; [%d/%d] yEnc (1/%d)"><groups><group>alt.binaries.test</group></groups><segments>`,
			fileIndex,
			fileIndex+1,
			fileCount,
			segmentsPerFile,
		)
		for segmentIndex := range segmentsPerFile {
			_, _ = fmt.Fprintf(
				&builder,
				`<segment bytes="768000" number="%d">file-%03d-part-%05d@example</segment>`,
				segmentIndex+1,
				fileIndex,
				segmentIndex+1,
			)
		}
		builder.WriteString(`</segments></file>`)
	}
	builder.WriteString(`</nzb>`)
	return builder.String()
}

func nzbOf(files ...string) string {
	return `<?xml version="1.0" encoding="UTF-8"?><nzb xmlns="http://www.newzbin.com/DTD/2003/nzb">` +
		strings.Join(files, "") + `</nzb>`
}

func fileOf(date int, subject string, segments ...string) string {
	return fmt.Sprintf(
		`<file poster="p" date="%d" subject="%s"><groups><group>g</group></groups><segments>%s</segments></file>`,
		date, subject, strings.Join(segments, ""))
}

func segOf(number int, id string) string {
	return fmt.Sprintf(`<segment bytes="1" number="%d">%s</segment>`, number, id)
}

func mustDecode(t *testing.T, source string) *manifest.Manifest {
	t.Helper()
	got, err := manifest.Decode(strings.NewReader(source))
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestDecodeNewestDate(t *testing.T) {
	t.Parallel()
	merged := mustDecode(t, nzbOf(
		fileOf(100, "same", segOf(1, "a@x")),
		fileOf(200, "same", segOf(2, "b@x")),
	))
	if merged.NewestDate != 200 || merged.Files[0].Date != 100 || !merged.Files[0].Rewritten {
		t.Fatalf("merged: newest %d, first date %d, rewritten %v",
			merged.NewestDate, merged.Files[0].Date, merged.Files[0].Rewritten)
	}
	distinct := mustDecode(t, nzbOf(
		fileOf(100, "one", segOf(1, "a@x")),
		fileOf(300, "two", segOf(1, "b@x")),
	))
	if distinct.NewestDate != 300 {
		t.Fatalf("distinct: newest %d", distinct.NewestDate)
	}
}

func TestDecodeMergedContiguousRangesAreRewritten(t *testing.T) {
	t.Parallel()
	got := mustDecode(t, nzbOf(
		fileOf(1, "same", segOf(1, "a@x"), segOf(2, "b@x")),
		fileOf(1, "same", segOf(4, "d@x"), segOf(5, "e@x")),
	))
	file := got.Files[0]
	var numbers []int
	for _, segment := range file.Segments {
		numbers = append(numbers, segment.Number)
	}
	if !reflect.DeepEqual(numbers, []int{1, 2, 4, 5}) || !file.Rewritten {
		t.Fatalf("numbers %v, rewritten %v", numbers, file.Rewritten)
	}
}

func TestDecodeDroppedDuplicateIsRewritten(t *testing.T) {
	t.Parallel()
	got := mustDecode(t, nzbOf(fileOf(1, "f", segOf(1, "a@x"), segOf(2, "a@x"), segOf(3, "b@x"))))
	if len(got.Files[0].Segments) != 2 || !got.Files[0].Rewritten {
		t.Fatalf("segments %d, rewritten %v", len(got.Files[0].Segments), got.Files[0].Rewritten)
	}
	spaced := mustDecode(t, nzbOf(fileOf(1, "f", segOf(1, "a@x"), segOf(2, " a@x\n"))))
	if len(spaced.Files[0].Segments) != 1 || !spaced.Files[0].Rewritten {
		t.Fatalf("whitespace variant: segments %d, rewritten %v",
			len(spaced.Files[0].Segments), spaced.Files[0].Rewritten)
	}
}

func TestDecodePlainFileIsNotRewritten(t *testing.T) {
	t.Parallel()
	got := mustDecode(t, nzbOf(fileOf(1, "f", segOf(1, "a@x"), segOf(3, "c@x"))))
	if got.Files[0].Rewritten {
		t.Fatal("a plain file was marked rewritten")
	}
}

type legacyDerived struct {
	newest    int
	rewritten map[string]bool
}

func deriveLegacy(t *testing.T, source string) legacyDerived {
	t.Helper()
	var doc struct {
		Files []struct {
			Date     int    `xml:"date,attr"`
			Subject  string `xml:"subject,attr"`
			Segments []struct {
				ID string `xml:",innerxml"`
			} `xml:"segments>segment"`
		} `xml:"file"`
	}
	decoder := xml.NewDecoder(strings.NewReader(source))
	decoder.CharsetReader = charset.NewReaderLabel
	decoder.Strict = false
	if err := decoder.Decode(&doc); err != nil {
		t.Fatal(err)
	}
	derived := legacyDerived{rewritten: make(map[string]bool)}
	rawCount := make(map[string]int)
	seen := make(map[string]map[string]struct{})
	for _, file := range doc.Files {
		derived.newest = max(derived.newest, file.Date)
		rawCount[file.Subject]++
		if rawCount[file.Subject] > 1 {
			derived.rewritten[file.Subject] = true
		}
		if seen[file.Subject] == nil {
			seen[file.Subject] = make(map[string]struct{})
		}
		for _, segment := range file.Segments {
			id := strings.TrimSpace(segment.ID)
			if _, dup := seen[file.Subject][id]; dup {
				derived.rewritten[file.Subject] = true
			}
			seen[file.Subject][id] = struct{}{}
		}
	}
	return derived
}

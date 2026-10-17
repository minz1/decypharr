package parser

import (
	"context"
	"fmt"
	"testing"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/nntp"
	"github.com/sirrobot01/decypharr/pkg/storage"
	"github.com/sirrobot01/decypharr/pkg/usenet/manifest"
)

type headerSource map[string]*nntp.YencMetadata

func (s headerSource) Header(_ context.Context, id string, _ int) (*nntp.YencMetadata, error) {
	if metadata, ok := s[id]; ok {
		return metadata, nil
	}
	return nil, fmt.Errorf("no header for %s", id)
}
func (s headerSource) Body(context.Context, string) ([]byte, error) {
	return nil, fmt.Errorf("no body")
}
func (s headerSource) Stat(context.Context, string) error { return nil }
func (s headerSource) IsAvailable(string) bool            { return true }
func (s headerSource) Metrics() ArticleMetrics            { return ArticleMetrics{} }

const (
	sizingArticle = 768000
	sizingTail    = 563445
)

// scrambledArchive mirrors the NZBgeek failures: scrambled [n/N], decoded-size NZB bytes, short last volume.
func scrambledArchive(fileType storage.NZBFileType, names [3]string) (*FileGroup, headerSource) {
	source := headerSource{}
	full := []int64{sizingArticle, sizingArticle, sizingArticle, sizingArticle, sizingArticle}
	volumes := [][]int64{full, full, {sizingArticle, sizingTail}}
	numbers := []int{2, 3, 1}
	group := &FileGroup{BaseName: "x", Type: fileType, fileMeta: map[string]filePartMeta{}}
	for i, parts := range volumes {
		file := manifest.File{Filename: names[i], Number: numbers[i]}
		var size int64
		for _, part := range parts {
			size += part
		}
		for j, part := range parts {
			id := fmt.Sprintf("%s-%d", names[i], j+1)
			file.Segments = append(file.Segments, manifest.Segment{Number: j + 1, Bytes: part, MessageID: id})
			begin := int64(j)*sizingArticle + 1
			source[id] = &nntp.YencMetadata{
				Name: names[i], Size: size, Part: int64(j + 1),
				Begin: begin, End: begin + part - 1, PartSize: part,
			}
		}
		group.Files = append(group.Files, file)
	}
	return group, source
}

func TestEnrichSizesLastArchiveVolumeFromItsOwnHeader(t *testing.T) {
	t.Parallel()
	cases := []struct {
		fileType storage.NZBFileType
		names    [3]string
	}{
		{storage.NZBFileTypeRar, [3]string{"x.part01.rar", "x.part02.rar", "x.part03.rar"}},
		{storage.NZBFileTypeRar, [3]string{"x.rar", "x.r00", "x.r01"}},
		{storage.NZBFileTypeZip, [3]string{"x.z01", "x.z02", "x.zip"}},
		{storage.NZBFileTypeSevenZip, [3]string{"x.7z.001", "x.7z.002", "x.7z.003"}},
	}
	for _, tc := range cases {
		t.Run(tc.names[2], func(t *testing.T) {
			t.Parallel()
			group, source := scrambledArchive(tc.fileType, tc.names)
			p := NewParserWithSource(source, 2, zerolog.Nop())
			if err := p.enrichGroupWithFileInfo(context.Background(), group); err != nil {
				t.Fatal(err)
			}
			last := group.Files[len(group.Files)-1]
			if last.Filename != tc.names[2] {
				t.Fatalf("last volume is %s, want %s", last.Filename, tc.names[2])
			}
			got, _, _ := getNZBSegments(len(group.Files)-1, last, group)
			if want := int64(sizingArticle + sizingTail); got != want {
				t.Fatalf("last volume %s sized %d, yEnc says %d", last.Filename, got, want)
			}
		})
	}
}

package parser

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/nntp"
	"github.com/sirrobot01/decypharr/internal/task"
	"github.com/sirrobot01/decypharr/internal/testutil/nntpd"
	"github.com/sirrobot01/decypharr/pkg/storage"
	"github.com/sirrobot01/decypharr/pkg/usenet/manifest"
)

func bookFilter() func(string, int64) error {
	cfg := &config.Config{AllowedExt: []string{"mkv", "mp4", "epub", "azw3", "mobi", "pdf", "cbz", "m4b", "m4a"}}
	return cfg.ValidateFileAllowed
}

func epubPayload(size int) []byte {
	data := make([]byte, size)
	copy(data, "PK\x03\x04")
	copy(data[26:], "\x08\x00\x00\x00")
	copy(data[30:], "mimetypeapplication/epub+zip")
	return data
}

func TestAllowedExtensionsArePlainFiles(t *testing.T) {
	t.Parallel()
	p := (&NZBParser{}).WithFileFilter(bookFilter())
	for name, want := range map[string]storage.NZBFileType{
		"The Time Machine.epub": storage.NZBFileTypeMedia,
		"Edgedancer.AZW3":       storage.NZBFileTypeMedia,
		"comic.cbz":             storage.NZBFileTypeMedia,
		"audiobook.m4b":         storage.NZBFileTypeMedia,
		"archive.zip":           storage.NZBFileTypeZip,
		"archive.part01.rar":    storage.NZBFileTypeRar,
		"cover.jpg":             storage.NZBFileTypeIgnore,
		"setup.exe":             storage.NZBFileTypeUnknown,
		"no extension":          storage.NZBFileTypeUnknown,
	} {
		if got := p.detectFileType(name); got != want {
			t.Errorf("detectFileType(%q) = %q, want %q", name, got, want)
		}
	}
	if got := (&NZBParser{}).detectFileType("The Time Machine.epub"); got != storage.NZBFileTypeUnknown {
		t.Errorf("without a file filter an .epub = %q, want unknown", got)
	}
}

func TestContentDetectionNamesBooks(t *testing.T) {
	t.Parallel()
	mobi := make([]byte, 80)
	copy(mobi[60:], "BOOKMOBI")
	p := &NZBParser{}
	for name, tc := range map[string]struct {
		data      []byte
		fileType  storage.NZBFileType
		extension string
	}{
		"epub":     {epubPayload(64), storage.NZBFileTypeMedia, ".epub"},
		"mobi":     {mobi, storage.NZBFileTypeMedia, ".mobi"},
		"pdf":      {[]byte("%PDF-1.7\n"), storage.NZBFileTypeMedia, ".pdf"},
		"m4b":      {[]byte("\x00\x00\x00\x20ftypM4B \x00\x00"), storage.NZBFileTypeMedia, ".m4b"},
		"m4a":      {[]byte("\x00\x00\x00\x20ftypM4A \x00\x00"), storage.NZBFileTypeMedia, ".m4a"},
		"mp4":      {[]byte("\x00\x00\x00\x20ftypisom\x00\x00"), storage.NZBFileTypeMedia, ".mp4"},
		"zip":      {[]byte("PK\x03\x04\x14\x00\x00\x00\x08\x00"), storage.NZBFileTypeZip, ""},
		"zip+name": {append([]byte("PK\x03\x04"), make([]byte, 60)...), storage.NZBFileTypeZip, ""},
	} {
		fileType, extension := p.detectFileTypeAndExtensionFromContent(tc.data)
		if fileType != tc.fileType || extension != tc.extension {
			t.Errorf("%s: content classification = (%q, %q), want (%q, %q)",
				name, fileType, extension, tc.fileType, tc.extension)
		}
	}
}

func parseOnePost(t *testing.T, yencName string, payload []byte) *storage.NZB {
	t.Helper()
	server, err := nntpd.New(nntpd.Config{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(server.Close)
	server.AddArticle("<book@nntpd>", nntpd.Encode(payload, yencName, 1, int64(len(payload)), 0))
	root := task.NewRoot(t.Context(), "test", zerolog.Nop())
	t.Cleanup(func() {
		root.Close()
		<-root.Done()
	})
	host, port := server.Addr()
	client, err := nntp.NewClient(root.Child("nntp"), &config.Config{Usenet: config.Usenet{
		Providers: []config.UsenetProvider{{Host: host, Port: port, MaxConnections: 2}},
	}}, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	source := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<nzb xmlns="http://www.newzbin.com/DTD/2003/nzb">
  <file poster="poster" date="1" subject="&quot;%s&quot; [1/1] yEnc (1/1)">
    <groups><group>alt.test</group></groups>
    <segments><segment bytes="%d" number="1">book@nntpd</segment></segments>
  </file>
</nzb>`, yencName, len(payload)+100)
	p := NewParser(client, 2, zerolog.Nop()).WithFileFilter(bookFilter())
	const nzbName = "H G Wells - The Time Machine (illus) (retail) (epub)"
	parsed, err := p.Parse(t.Context(), nzbName+".nzb", []byte(source))
	if err != nil {
		t.Fatal(err)
	}
	parsed.NZB.Name = nzbName
	processed, _, err := p.Process(t.Context(), parsed)
	if err != nil {
		t.Fatal(err)
	}
	return processed
}

func TestBookPostsBecomeOnePlainFile(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		yencName, ext string
		payload       []byte
	}{
		"named epub":         {"The Time Machine.epub", ".epub", epubPayload(2048)},
		"extensionless epub": {"H G Wells - The Time Machine (illus) (retail) (epub)", ".epub", epubPayload(2048)},
		"named azw3":         {"The Time Machine.azw3", ".azw3", make([]byte, 2048)},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			nzb := parseOnePost(t, tc.yencName, tc.payload)
			if len(nzb.Files) != 1 {
				t.Fatalf("files = %d, want 1", len(nzb.Files))
			}
			file := nzb.Files[0]
			if filepath.Ext(file.Name) != tc.ext || !strings.HasPrefix(file.Name, "H G Wells") {
				t.Fatalf("file name = %q, want the NZB name with %s", file.Name, tc.ext)
			}
			if file.Size != 2048 {
				t.Fatalf("file size = %d, want the whole book (2048)", file.Size)
			}
		})
	}
}

func TestAllowedExtensionRuleLeavesNumericVolumesToContentDetection(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{AllowedExt: []string{"mkv", "001", "zip", "rar", "par2", "jpg"}}
	p := (&NZBParser{}).WithFileFilter(cfg.ValidateFileAllowed)
	for name, want := range map[string]storage.NZBFileType{
		"opaque.001":   storage.NZBFileTypeUnknown,
		"archive.zip":  storage.NZBFileTypeZip,
		"archive.rar":  storage.NZBFileTypeRar,
		"release.par2": storage.NZBFileTypeIgnore,
		"cover.jpg":    storage.NZBFileTypeIgnore,
		"movie.mkv":    storage.NZBFileTypeMedia,
	} {
		if got := p.detectFileType(name); got != want {
			t.Errorf("detectFileType(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestPlainFilesWithTheSameBaseNameStaySeparate(t *testing.T) {
	t.Parallel()
	item := func(name string, number int) contentResult {
		return contentResult{
			file: manifest.File{
				Filename: name, BaseFilename: strings.TrimSuffix(name, filepath.Ext(name)), Number: number,
				Groups: []string{"alt.test"},
			},
			fileType: storage.NZBFileTypeMedia,
		}
	}
	groups := (&NZBParser{}).groupProcessedFiles([]contentResult{
		item("Movie.mkv", 1), item("Movie.MKV", 2), item("Movie.pdf", 3), item("Book.epub", 4), item("Book.azw3", 5),
	})
	sizes := map[string]int{}
	for _, g := range groups {
		for _, f := range g.Files {
			sizes[strings.ToLower(f.Filename)] = len(g.Files)
		}
	}
	if len(groups) != 4 || sizes["movie.mkv"] != 2 || sizes["movie.pdf"] != 1 || sizes["book.epub"] != 1 {
		t.Fatalf(
			"groups = %d, files per name = %v; want 4 groups with both Movie.mkv parts together",
			len(groups),
			sizes,
		)
	}
}

func TestPlainFileKeysNeverMeetArchiveKeys(t *testing.T) {
	t.Parallel()
	p := &NZBParser{}
	media := contentResult{
		file:     manifest.File{Filename: "Movie.mkv", BaseFilename: "Movie", Number: 1},
		fileType: storage.NZBFileTypeMedia,
	}
	archive := func(name string, number int) contentResult {
		return contentResult{
			file:     manifest.File{Filename: name, BaseFilename: p.getBaseFilename(name), Number: number},
			fileType: storage.NZBFileTypeRar,
		}
	}
	for _, order := range [][]contentResult{
		{media, archive("Movie.mkv.part01.rar", 2), archive("Movie.mkv.part02.rar", 3)},
		{archive("Movie.mkv.part01.rar", 2), archive("Movie.mkv.part02.rar", 3), media},
	} {
		types := make(map[storage.NZBFileType]int)
		for _, g := range p.groupProcessedFiles(order) {
			types[g.Type] += len(g.Files)
		}
		if types[storage.NZBFileTypeMedia] != 1 || types[storage.NZBFileTypeRar] != 2 {
			t.Fatalf("files per group type = %v, want 1 media and 2 rar", types)
		}
	}
}

func TestObfuscatedRARMergeKeepsPlainFilesThatShareABaseName(t *testing.T) {
	t.Parallel()
	p := &NZBParser{logger: zerolog.Nop()}
	rar := func(name string, number int) contentResult {
		return contentResult{
			file:     manifest.File{Filename: name, BaseFilename: name, Number: number},
			fileType: storage.NZBFileTypeRar,
		}
	}
	plain := func(name string, number int) contentResult {
		return contentResult{
			file:     manifest.File{Filename: name, BaseFilename: "Movie", Number: number},
			fileType: storage.NZBFileTypeMedia,
		}
	}
	groups := p.mergeObfuscatedRarGroups(p.groupProcessedFiles([]contentResult{
		rar("yXIBWWn7qKVUVpS6", 1), rar("Qm3pZr8tLk2wVn5c", 2), plain("Movie.mkv", 3), plain("Movie.pdf", 4),
	}))
	if len(groups) != 3 {
		t.Fatalf("groups after the RAR merge = %d, want 3 (merged RAR, Movie.mkv, Movie.pdf)", len(groups))
	}
}

func TestGroupsWithTheSameBaseNameSortByKey(t *testing.T) {
	t.Parallel()
	mkv := &FileGroup{BaseName: "Movie", key: "Movie\x00.mkv", Type: storage.NZBFileTypeMedia}
	pdf := &FileGroup{BaseName: "Movie", key: "Movie\x00.pdf", Type: storage.NZBFileTypeMedia}
	for _, in := range [][]*FileGroup{{mkv, pdf}, {pdf, mkv}} {
		sortFileGroups(in)
		if in[0] != mkv {
			t.Fatalf("sorted first = %q, want the .mkv group", in[0].key)
		}
	}
}

package parser

import (
	"testing"
)

func TestDecodeRAR4Name(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		field []byte
		want  string
	}{
		"utf-8 without encoding": {[]byte("Café.mkv"), "Café.mkv"},
		// Opcode 2 (wide) for every char: flag bytes 0xAA carry four each.
		"wide": {
			append([]byte("Caf?.mkv\x00"),
				0x00, 0xAA, 'C', 0, 'a', 0, 'f', 0, 0xE9, 0x00,
				0xAA, '.', 0, 'm', 0, 'k', 0, 'v', 0),
			"Café.mkv",
		},
		// Opcode 1 (high byte from header 0x04) gives Cyrillic; opcode 3
		// copies the ASCII ".mkv" run (length 4 = 2+2) from the OEM name.
		"high byte and copy run": {
			append([]byte("??.mkv\x00"), 0x04, 0x5C, 0x1F, 0x40, 0x02),
			"Пр.mkv",
		},
		"truncated encoding keeps the decoded prefix": {
			append([]byte("Caf?.mkv\x00"), 0x00, 0xAA, 'C', 0, 'a'),
			"C",
		},
		"empty encoding falls back to the OEM name": {[]byte("movie.mkv\x00"), "movie.mkv"},
	}
	for name, tt := range tests {
		if got := decodeRAR4Name(tt.field); got != tt.want {
			t.Errorf("%s: decodeRAR4Name = %q, want %q", name, got, tt.want)
		}
	}
}

// A RAR4 file header with the Unicode flag yields the decoded name, not the
// OEM name with the encoded bytes appended.
func TestRAR4UnicodeFileHeaderName(t *testing.T) {
	t.Parallel()
	field := append([]byte("Caf?.mkv\x00"),
		0x00, 0xAA, 'C', 0, 'a', 0, 'f', 0, 0xE9, 0x00,
		0xAA, '.', 0, 'm', 0, 'k', 0, 'v', 0)
	archive := rar4Block(RAR4HeaderTypeArchive, 0, make([]byte, 6))
	file := rar4Block(RAR4HeaderTypeFile, RAR4HeaderFlagLongBlock|RAR4FileFlagUnicode,
		rar4FileBody(string(field), 4, 4))
	data := append([]byte(RAR4Signature), archive...)
	data = append(data, file...)
	data = append(data, make([]byte, 4)...)

	files := (&RARParser{}).parseRAR4Headers(data, 0, "a.rar")
	if len(files) != 1 || files[0].Name != "Café.mkv" {
		t.Fatalf("files = %+v, want Café.mkv", files)
	}
}

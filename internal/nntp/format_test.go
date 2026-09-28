package nntp_test

import (
	"testing"

	"github.com/sirrobot01/decypharr/internal/nntp"
)

func TestFormatMessageID(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"a@b":         "<a@b>",
		" <a@b> ":     "<a@b>",
		"<a@b":        "<a@b>",
		"a@b>":        "<a@b>",
		"a@b\r\nQUIT": "<a@bQUIT>", // must not end the command line
		"a\n@b\r":     "<a@b>",
	}
	for in, want := range cases {
		if got := nntp.FormatMessageID(in); got != want {
			t.Errorf("FormatMessageID(%q) = %q, want %q", in, got, want)
		}
	}
}

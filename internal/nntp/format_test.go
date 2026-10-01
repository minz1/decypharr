package nntp_test

import (
	"testing"

	"github.com/sirrobot01/decypharr/internal/nntp"
)

func TestFormatMessageID(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ in, want string }{
		{"a@b", "<a@b>"},
		{" <a@b> ", "<a@b>"},
		{"<a@b", "<a@b>"},
		{"a@b>", "<a@b>"},
		{"a@b\r\nQUIT", "<a@bQUIT>"}, // must not end the command line
		{"a\n@b\r", "<a@b>"},
	} {
		if got := nntp.FormatMessageID(tc.in); got != tc.want {
			t.Errorf("FormatMessageID(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

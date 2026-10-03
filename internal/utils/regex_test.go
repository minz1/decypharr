package utils_test

import (
	"strings"
	"testing"

	"github.com/sirrobot01/decypharr/internal/utils"
)

func TestSafeFolderName(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		value    string
		fallback string
		want     string
	}{
		{
			name:     "removes path separators and invalid characters",
			value:    "../bad:name?",
			fallback: "hash",
			want:     "badname",
		},
		{
			name:     "falls back for empty names",
			value:    " . ",
			fallback: "hash",
			want:     "hash",
		},
		{
			name:     "falls back for reserved device names",
			value:    "CON",
			fallback: "hash",
			want:     "hash",
		},
		{
			name:     "falls back for reserved device names with multiple extensions",
			value:    "CON.foo.bar",
			fallback: "hash",
			want:     "hash",
		},
		{
			name:     "truncates long components",
			value:    strings.Repeat("a", 300),
			fallback: "hash",
			want:     strings.Repeat("a", 255),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := utils.SafeFolderName(tt.value, tt.fallback); got != tt.want {
				t.Fatalf("expected %q, got %q", tt.want, got)
			}
		})
	}
}

func TestPathElement(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		"Movie: Part 1 (2023)": "Movie: Part 1 (2023)",
		"a..b":                 "a..b",
		"../escape":            ".._escape",
		"sub/dir":              "sub_dir",
		"tab\there":            "tab_here",
		"":                     "fallback",
		".":                    "fallback",
		"..":                   "fallback",
	}
	for in, want := range tests {
		if got := utils.PathElement(in, "fallback"); got != want {
			t.Errorf("PathElement(%q) = %q, want %q", in, got, want)
		}
	}
}

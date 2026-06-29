package config

import (
	"testing"
)

func TestParseSize(t *testing.T) {
	const (
		_K = 1024
		_M = 1024 * _K
		_G = 1024 * _M
		_T = 1024 * _G
		_P = 1024 * _T
	)

	cases := []struct {
		input   string
		want    int64
		wantErr bool
	}{
		// Single-letter units (the previously broken cases)
		{"85G", 85 * _G, false},
		{"512M", 512 * _M, false},
		{"10K", 10 * _K, false},
		{"2T", 2 * _T, false},
		{"1P", 1 * _P, false},
		{"1.5G", int64(1.5 * float64(_G)), false},

		// Two-letter units (must still work)
		{"500MB", 500 * _M, false},
		{"1GB", 1 * _G, false},
		{"2TB", 2 * _T, false},
		{"4KB", 4 * _K, false},

		// Bare bytes
		{"1024", 1024, false},
		{"1024B", 1024, false},

		// Case insensitive
		{"85g", 85 * _G, false},
		{"512m", 512 * _M, false},
		{"500mb", 500 * _M, false},

		// Whitespace trimmed
		{" 85G ", 85 * _G, false},

		// Invalid
		{"bad", 0, true},
		{"85Gigs", 0, true},
		{"", 0, true},
	}

	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			got, err := ParseSize(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseSize(%q) = %d, want error", tc.input, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseSize(%q) error: %v", tc.input, err)
			}
			if got != tc.want {
				t.Fatalf("ParseSize(%q) = %d, want %d", tc.input, got, tc.want)
			}
		})
	}
}

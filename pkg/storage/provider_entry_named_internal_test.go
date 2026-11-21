package storage

import "testing"

func TestProviderEntryNamed(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		p    *ProviderEntry
		want bool
	}{
		"nil":         {nil, false},
		"empty":       {&ProviderEntry{ID: ""}, false},
		"zero":        {&ProviderEntry{ID: "0"}, false},
		"one":         {&ProviderEntry{ID: "1"}, true},
		"double zero": {&ProviderEntry{ID: "00"}, true},
		"opaque":      {&ProviderEntry{ID: "abc"}, true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := tc.p.Named(); got != tc.want {
				t.Fatalf("Named() = %v, want %v", got, tc.want)
			}
		})
	}
}

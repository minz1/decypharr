package nntp_test

import (
	"slices"
	"testing"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/nntp"
)

// NewClient must not reorder or rewrite the caller's (shared) config slice.
func TestNewClientLeavesConfigProvidersUntouched(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{}
	cfg.Usenet.Providers = []config.UsenetProvider{
		{Host: "b.example", Port: 563, MaxConnections: 1, Priority: 2, Backbone: " Omicron "},
		{Host: "a.example", Port: 563, MaxConnections: 1, Priority: 1},
	}
	want := slices.Clone(cfg.Usenet.Providers)
	c, err := nntp.NewClient(cfg, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if !slices.Equal(cfg.Usenet.Providers, want) {
		t.Fatalf("config providers mutated:\n got %+v\nwant %+v", cfg.Usenet.Providers, want)
	}
}

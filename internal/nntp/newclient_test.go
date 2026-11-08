package nntp_test

import (
	"slices"
	"testing"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/nntp"
	"github.com/sirrobot01/decypharr/internal/task"
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
	root := task.NewRoot(t.Context(), "test", zerolog.Nop())
	t.Cleanup(func() {
		root.Close()
		<-root.Done()
	})
	if _, err := nntp.NewClient(root.Child("nntp"), cfg, zerolog.Nop()); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(cfg.Usenet.Providers, want) {
		t.Fatalf("config providers mutated:\n got %+v\nwant %+v", cfg.Usenet.Providers, want)
	}
}

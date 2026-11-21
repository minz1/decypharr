package manager_test

import (
	"testing"

	"github.com/sirrobot01/decypharr/internal/config"
	debrid "github.com/sirrobot01/decypharr/pkg/debrid/common"
	"github.com/sirrobot01/decypharr/pkg/manager"
	"github.com/sirrobot01/decypharr/pkg/manager/managertest"
)

type namedClient struct {
	debrid.Client
	name string
}

func (c *namedClient) Config() config.Debrid { return config.Debrid{Name: c.name} }

func TestWithClientsReplacesOnlyConfiguredProviders(t *testing.T) {
	t.Parallel()
	store := managertest.Store(t, func(cfg *config.Config) {
		cfg.Debrids = []config.Debrid{{Name: "realdebrid", Provider: "realdebrid", APIKey: "test"}}
	})
	configured, ghost := &namedClient{name: "realdebrid"}, &namedClient{name: "ghost"}
	mgr := managertest.NewOn(t, store, manager.WithClients(map[string]debrid.Client{
		"realdebrid": configured,
		"ghost":      ghost,
	}))
	if got := mgr.ProviderClient("realdebrid"); got != configured {
		t.Fatalf("realdebrid client = %v, want the replacement", got)
	}
	if got := mgr.ProviderClient("ghost"); got != nil {
		t.Fatalf("ghost client = %v, want none for a name the configuration does not list", got)
	}
}

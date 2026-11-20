package arr

import (
	"slices"
	"sync"
	"testing"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
)

type addedNames struct {
	mu    sync.Mutex
	names []string
}

func (a *addedNames) add(name string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.names = append(a.names, name)
}

func (a *addedNames) take() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	taken := a.names
	a.names = nil
	return taken
}

func TestAddedHookFiresOnNewNameOnly(t *testing.T) {
	t.Parallel()
	service := New(config.NewStore(&config.Config{}), nil, zerolog.Nop())
	var added addedNames
	service.SetAddedHook(added.add)
	sonarr := Arr{Name: "sonarr", Host: "http://sonarr.example", Token: "t", Source: SourceAuto}

	service.AddOrUpdate(sonarr)
	if got := added.take(); !slices.Equal(got, []string{"sonarr"}) {
		t.Fatalf("after a new name: %v", got)
	}
	service.AddOrUpdate(sonarr)
	if got := added.take(); len(got) != 0 {
		t.Fatalf("after the same name: %v", got)
	}

	service.SyncFromConfig(nil, []config.Arr{
		{Name: "sonarr", Host: "http://sonarr.example", Token: "t", Source: string(SourceAuto)},
		{Name: "radarr", Host: "http://radarr.example", Token: "t", Source: string(SourceAuto)},
	})
	if got := added.take(); !slices.Equal(got, []string{"radarr"}) {
		t.Fatalf("after a sync adding one and keeping one: %v", got)
	}

	service.Clear()
	service.AddOrUpdate(sonarr)
	if got := added.take(); !slices.Equal(got, []string{"sonarr"}) {
		t.Fatalf("after Clear and AddOrUpdate: %v", got)
	}
}

func TestAddedHookMayCallBackIntoTheService(t *testing.T) {
	t.Parallel()
	service := New(config.NewStore(&config.Config{}), nil, zerolog.Nop())
	seen := make(chan bool, 1)
	service.SetAddedHook(func(name string) {
		_, ok := service.Get(name)
		seen <- ok
	})
	service.AddOrUpdate(Arr{Name: "sonarr", Host: "http://sonarr.example", Token: "t", Source: SourceAuto})
	if !<-seen {
		t.Fatal("hook did not see the new Arr")
	}
}

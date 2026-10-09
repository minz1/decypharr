package arr

import (
	"testing"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
)

func TestSyncFromConfigAppliesValidHost(t *testing.T) {
	t.Parallel()

	arrs := New(config.NewStore(&config.Config{}), nil, zerolog.Nop())
	arrs.AddOrUpdate(Arr{Name: "whisparr", Host: "http://old.example", Token: "old-token", Source: SourceAuto})
	arrs.SyncFromConfig([]config.Arr{{
		Name:   "whisparr",
		Host:   "http://new.example",
		Token:  "new-token",
		Source: string(SourceAuto),
	}})

	got, ok := arrs.Get("whisparr")
	if !ok || got.Host != "http://new.example" || got.Token != "new-token" {
		t.Fatalf("synced Arr = %#v", got)
	}
}

func TestSyncFromConfigPreservesResolvedHostForInvalidUpdate(t *testing.T) {
	t.Parallel()

	arrs := New(config.NewStore(&config.Config{}), nil, zerolog.Nop())
	arrs.AddOrUpdate(Arr{Name: "whisparr", Host: "http://resolved.example", Token: "old-token", Source: SourceAuto})
	arrs.SyncFromConfig([]config.Arr{{
		Name:   "whisparr",
		Host:   "not-a-url",
		Token:  "new-token",
		Source: string(SourceAuto),
	}})

	got, ok := arrs.Get("whisparr")
	if !ok || got.Host != "http://resolved.example" || got.Token != "new-token" {
		t.Fatalf("synced Arr = %#v", got)
	}
}

func TestArrInstanceFingerprintCanonicalizesHost(t *testing.T) {
	t.Parallel()
	first := Arr{Type: Sonarr, Host: "HTTP://Example.COM:80/sonarr/", Token: "first"}.Fingerprint()
	second := Arr{Type: Sonarr, Host: "http://example.com/sonarr", Token: "second"}.Fingerprint()
	if first == "" || first != second {
		t.Fatalf("equivalent Arr hosts produced fingerprints %q and %q", first, second)
	}

	differentPath := Arr{Type: Sonarr, Host: "http://example.com/other"}.Fingerprint()
	differentType := Arr{Type: Radarr, Host: "http://example.com/sonarr"}.Fingerprint()
	if first == differentPath || first == differentType {
		t.Fatal("different Arr instances produced the same fingerprint")
	}
}

// An Arr registered from client credentials is trusted only when the config
// saves the same name, host and token; a different saved token is no match.
func TestMatchCredentialsTrustsAutoArrOnlyWithSavedToken(t *testing.T) {
	t.Parallel()
	saved := config.Arr{Name: "sonarr", Host: "http://sonarr.invalid", Token: "saved-token", Source: string(SourceAuto)}
	arrs := New(config.NewStore(&config.Config{Arrs: []config.Arr{saved}}), nil, zerolog.Nop())

	arrs.AddOrUpdate(Arr{Name: "sonarr", Host: saved.Host, Token: "client-token", Source: SourceAuto})
	if _, ok := arrs.MatchCredentials("sonarr", saved.Host, "client-token"); ok {
		t.Fatal("matched an auto Arr whose token differs from the saved one")
	}

	arrs.AddOrUpdate(Arr{Name: "sonarr", Host: saved.Host, Token: "saved-token", Source: SourceAuto})
	if _, ok := arrs.MatchCredentials("sonarr", saved.Host, "saved-token"); !ok {
		t.Fatal("did not match an auto Arr whose credentials are saved")
	}
}

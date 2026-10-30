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
	arrs.SyncFromConfig(nil, []config.Arr{{
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
	arrs.SyncFromConfig(nil, []config.Arr{{
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

func TestSyncFromConfigDropsArrRemovedFromConfig(t *testing.T) {
	t.Parallel()

	saved := []config.Arr{{Name: "sonarr", Host: "http://sonarr.example", Token: "sonarr-token"}}
	arrs := New(config.NewStore(&config.Config{Arrs: saved}), nil, zerolog.Nop())
	arrs.AddOrUpdate(Arr{Name: "whisparr", Host: "http://whisparr.example", Token: "w-token", Source: SourceAuto})
	arrs.SyncFromConfig(saved, nil)

	if got, ok := arrs.Get("sonarr"); ok {
		t.Fatalf("removed Arr still registered: %#v", got)
	}
	if _, ok := arrs.MatchCredentials("sonarr", "", "sonarr-token"); ok {
		t.Fatal("removed Arr's credentials still match")
	}
	if _, ok := arrs.Get("whisparr"); !ok {
		t.Fatal("auto-registered Arr that was never saved was dropped")
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

// The import-webhook opt-in must survive the config round trip, or saving settings silently turns it off.
func TestImportWebhookSurvivesConfigRoundTrip(t *testing.T) {
	t.Parallel()
	instance := fromConfig(config.Arr{Name: "sonarr", Host: "http://sonarr", Token: "t", ImportWebhook: true})
	if !instance.ImportWebhook || !instance.toConfig().ImportWebhook {
		t.Fatalf("import_webhook lost: instance %v, config %v",
			instance.ImportWebhook, instance.toConfig().ImportWebhook)
	}
	if (config.Arr{ImportWebhook: true}).IsZero() {
		t.Fatal("an arr with only import_webhook set counts as zero")
	}
}

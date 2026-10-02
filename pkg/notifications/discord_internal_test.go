package notifications

import (
	"testing"

	"github.com/sirrobot01/decypharr/internal/config"
)

func TestDiscordHeaderTitleCasesUnknownEvents(t *testing.T) {
	config.SetConfigPath(t.TempDir())
	t.Cleanup(config.Reset)
	got := (&DiscordNotifier{}).getHeader(config.NotificationEvent("download_started"))
	if want := "[Decypharr] Download Started"; got != want {
		t.Fatalf("getHeader() = %q, want %q", got, want)
	}
}

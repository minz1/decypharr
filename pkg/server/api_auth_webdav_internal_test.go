package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/pkg/manager/managertest"
)

const (
	tokenOnlyWebdavWarning = "WebDAV auth is on but token-only auth has no credential WebDAV accepts — turn it off, or WebDAV clients will be rejected"
	rcloneWebdavWarning    = "WebDAV auth is on but the built-in rclone mount connects without credentials — it will be rejected"
)

func withCredentials(c *config.Config) {
	if err := c.SetCredentials("u", "pw1234567"); err != nil {
		panic(err)
	}
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestUpdateConfigSavesWebdavAuth(t *testing.T) {
	t.Parallel()
	mgr, store := managertest.New(t, withCredentials)
	if err := store.Get().Save(); err != nil {
		t.Fatal(err)
	}
	authBefore := readFile(t, store.Get().AuthFile())
	server := &Server{manager: mgr, config: store, logger: zerolog.Nop()}

	check := func(step string, want bool) {
		t.Helper()
		if got := store.Get().EnableWebdavAuth; got != want {
			t.Errorf("%s: live enable_webdav_auth = %v, want %v", step, got, want)
		}
		loaded, err := config.Load(store.Get().Dir(), config.MapEnv(nil))
		if err != nil {
			t.Fatal(err)
		}
		if loaded.EnableWebdavAuth != want {
			t.Errorf("%s: saved enable_webdav_auth = %v, want %v", step, loaded.EnableWebdavAuth, want)
		}
		if !store.Get().UseAuth {
			t.Errorf("%s: use_auth turned off", step)
		}
		if !bytes.Equal(authBefore, readFile(t, store.Get().AuthFile())) {
			t.Errorf("%s: auth.json changed", step)
		}
	}

	if postConfig(t, server, []byte(`{"enable_webdav_auth":true,"use_auth":false}`)) {
		t.Error("turning WebDAV auth on restarted services")
	}
	check("on", true)
	postConfig(t, server, []byte(`{"enable_webdav_auth":false}`))
	check("off", false)
	postConfig(t, server, []byte(`{"enable_webdav_auth":true}`))
	postConfig(t, server, []byte(`{"retries":3}`))
	check("save without the key", true)
}

func TestUpdateConfigWarnsAboutWebdavAuth(t *testing.T) {
	t.Parallel()
	rclone := func(c *config.Config) { c.Mount.Type = config.MountTypeRclone }
	for name, tc := range map[string]struct {
		seed []func(*config.Config)
		on   bool
		want []string
	}{
		"token-only": {
			seed: []func(*config.Config){func(c *config.Config) {
				c.UseAuth = true
				c.Auth = &config.Auth{APIToken: "t", TokenOnly: true}
			}},
			on:   true,
			want: []string{tokenOnlyWebdavWarning},
		},
		"rclone mount":           {seed: []func(*config.Config){withCredentials, rclone}, on: true, want: []string{rcloneWebdavWarning}},
		"web auth off":           {seed: []func(*config.Config){rclone, func(c *config.Config) { c.UseAuth = false }}, on: true},
		"webdav auth off":        {seed: []func(*config.Config){withCredentials, rclone}},
		"credentials, no rclone": {seed: []func(*config.Config){withCredentials}, on: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			mgr, store := managertest.New(t, func(c *config.Config) {
				for _, s := range tc.seed {
					s(c)
				}
			})
			if err := store.Get().Save(); err != nil {
				t.Fatal(err)
			}
			server := &Server{manager: mgr, config: store, logger: zerolog.Nop()}
			if got := postConfigWarnings(t, server, tc.on); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("warnings = %q, want %q", got, tc.want)
			}
		})
	}
}

func postConfigWarnings(t *testing.T, server *Server, enableWebdavAuth bool) []string {
	t.Helper()
	body, _ := json.Marshal(map[string]bool{"enable_webdav_auth": enableWebdavAuth})
	response := httptest.NewRecorder()
	server.handleUpdateConfig(response, httptest.NewRequest(http.MethodPost, "/api/config", bytes.NewReader(body)))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", response.Code, response.Body.String())
	}
	var result struct {
		Warnings []string `json:"warnings"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	return result.Warnings
}

func TestUpdateAuthWarnsAboutWebdavAuth(t *testing.T) {
	t.Parallel()
	rclone := func(c *config.Config) { c.Mount.Type = config.MountTypeRclone }
	const tokenOnly = `{"token_only":true}`
	const credentials = `{"username":"u","password":"pw1234567","confirm_password":"pw1234567"}`
	for name, tc := range map[string]struct {
		seed func(*config.Config)
		body string
		want []string
	}{
		"token-only":                       {body: tokenOnly, want: []string{tokenOnlyWebdavWarning}},
		"credentials with an rclone mount": {seed: rclone, body: credentials, want: []string{rcloneWebdavWarning}},
		"token-only with an rclone mount": {
			seed: rclone,
			body: tokenOnly,
			want: []string{tokenOnlyWebdavWarning, rcloneWebdavWarning},
		},
		"webdav auth off": {
			seed: func(c *config.Config) { c.EnableWebdavAuth = false; rclone(c) },
			body: tokenOnly,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			store := managertest.Store(t, func(c *config.Config) {
				c.EnableWebdavAuth = true
				if tc.seed != nil {
					tc.seed(c)
				}
			})
			message, warnings := postUpdateAuth(t, &Server{config: store, logger: zerolog.Nop()}, tc.body)
			if !reflect.DeepEqual(warnings, tc.want) {
				t.Errorf("warnings = %q, want %q", warnings, tc.want)
			}
			wantSuffix := ""
			if tc.want != nil {
				wantSuffix = ". " + strings.Join(tc.want, ". ")
			}
			if !strings.HasSuffix(message, wantSuffix) ||
				strings.Contains(strings.TrimSuffix(message, wantSuffix), "WebDAV") {
				t.Errorf(
					"message = %q, want it to end with %q and mention WebDAV nowhere else",
					message,
					wantSuffix,
				)
			}
		})
	}
}

func postUpdateAuth(t *testing.T, server *Server, body string) (string, []string) {
	t.Helper()
	response := httptest.NewRecorder()
	server.handleUpdateAuth(response, httptest.NewRequest(http.MethodPost, "/api/update-auth", strings.NewReader(body)))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", response.Code, response.Body.String())
	}
	var result struct {
		Message  string   `json:"message"`
		Warnings []string `json:"warnings"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	return result.Message, result.Warnings
}

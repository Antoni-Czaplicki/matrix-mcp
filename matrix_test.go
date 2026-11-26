package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/id"
)

func TestNewAppWithMatrixAccessToken(t *testing.T) {
	const user = "@bot:example.test"
	const device = "MCPDEVICE"
	var mu sync.Mutex
	var keys *mautrix.DeviceKeys
	var uploads, queries int

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		var deviceID id.DeviceID = device
		var userID id.UserID = user
		switch r.Header.Get("Authorization") {
		case "Bearer test-original-session", "Bearer test-renewed-session", "Bearer test-rejected-upload":
		case "Bearer test-other-device":
			deviceID = "OTHERDEVICE"
		case "Bearer test-other-account":
			userID = "@other:example.test"
		default:
			t.Error("request did not use the configured Matrix token")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"errcode":"M_UNKNOWN_TOKEN"}`)
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/_matrix/client/v3/account/whoami":
			_ = json.NewEncoder(w).Encode(mautrix.RespWhoami{UserID: userID, DeviceID: deviceID})
		case r.Method == http.MethodPost && r.URL.Path == "/_matrix/client/v3/keys/query":
			queries++
			resp := mautrix.RespQueryKeys{DeviceKeys: map[id.UserID]map[id.DeviceID]mautrix.DeviceKeys{}}
			if keys != nil {
				resp.DeviceKeys[user] = map[id.DeviceID]mautrix.DeviceKeys{device: *keys}
			}
			_ = json.NewEncoder(w).Encode(resp)
		case r.Method == http.MethodPost && r.URL.Path == "/_matrix/client/v3/keys/upload":
			var upload mautrix.ReqUploadKeys
			if err := json.NewDecoder(r.Body).Decode(&upload); err != nil || upload.DeviceKeys == nil {
				t.Errorf("expected a device key upload, decode error: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if upload.DeviceKeys.UserID != user || upload.DeviceKeys.DeviceID != device {
				t.Error("uploaded keys for an unexpected Matrix identity")
			}
			if r.Header.Get("Authorization") == "Bearer test-rejected-upload" {
				w.WriteHeader(http.StatusForbidden)
				_, _ = io.WriteString(w, `{"errcode":"M_FORBIDDEN"}`)
				return
			}
			keys = upload.DeviceKeys
			uploads++
			_, _ = io.WriteString(w, `{"one_time_key_counts":{"signed_curve25519":50}}`)
		default:
			t.Errorf("unexpected Matrix request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)

	cfg := Config{Homeserver: server.URL, UserID: user, PickleKey: "test-pickle", DataDir: t.TempDir(), AccessToken: "test-rejected-upload"}
	if app, err := NewApp(t.Context(), cfg, zerolog.Nop()); err == nil {
		app.Close()
		t.Fatal("application accepted a failed key upload")
	} else if !strings.Contains(err.Error(), "failed to share keys") {
		t.Fatalf("expected key upload to fail, got %v", err)
	}
	cfg.UserID, cfg.AccessToken = "@other:example.test", "test-other-account"
	if app, err := NewApp(t.Context(), cfg, zerolog.Nop()); err == nil {
		app.Close()
		t.Fatal("application reused the crypto store for a different user")
	} else if !strings.Contains(err.Error(), "store belongs to a different Matrix account or device") {
		t.Fatalf("expected a data directory user mismatch, got %v", err)
	}

	cfg.UserID, cfg.DataDir = user, filepath.Join(t.TempDir(), "with?query#fragment%25")
	if err := os.Mkdir(cfg.DataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	legacyDB := filepath.Join(cfg.DataDir, "matrix-mcp.db")
	const legacyContents = "existing password-mode data"
	if err := os.WriteFile(legacyDB, []byte(legacyContents), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{"test-original-session", "test-renewed-session"} {
		cfg.AccessToken = token
		app, err := NewApp(t.Context(), cfg, zerolog.Nop())
		if err != nil {
			t.Fatal(err)
		}
		if app.cli.UserID != user || app.cli.DeviceID != device {
			t.Error("application did not bind the Matrix session to the whoami identity")
		}
		app.Close()
	}
	cfg.AccessToken = "test-other-device"
	if app, err := NewApp(t.Context(), cfg, zerolog.Nop()); err == nil {
		app.Close()
		t.Fatal("application reused the crypto store for a different device")
	} else if !strings.Contains(err.Error(), "store belongs to a different Matrix account or device") {
		t.Fatalf("expected a crypto store device mismatch, got %v", err)
	}
	if contents, err := os.ReadFile(legacyDB); err != nil || string(contents) != legacyContents {
		t.Fatal("token mode touched the existing password-mode database")
	}
	if _, err := os.Stat(filepath.Join(cfg.DataDir, "matrix-mcp-token.db")); err != nil {
		t.Fatalf("token-mode database not created at the literal filesystem path: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if queries != 3 || uploads != 1 {
		t.Errorf("expected three key queries and one successful upload, got %d queries and %d successful uploads", queries, uploads)
	}
}

func TestMatrixTokenIdentityValidation(t *testing.T) {
	for _, tc := range []struct {
		name, response, wantError string
		status                    int
	}{
		{name: "different user", response: `{"user_id":"@other:example.test","device_id":"DEVICE"}`, status: 200, wantError: "does not belong"},
		{name: "no device", response: `{"user_id":"@bot:example.test"}`, status: 200, wantError: "associated with a device"},
		{name: "empty response", response: `null`, status: 200, wantError: "does not belong"},
		{name: "invalid token", response: `{"errcode":"M_UNKNOWN_TOKEN"}`, status: 401, wantError: "verify Matrix session"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.response)
			}))
			t.Cleanup(server.Close)
			_, err := newMatrixClient(t.Context(), Config{
				Homeserver: server.URL, UserID: "@bot:example.test", AccessToken: "test-session",
			})
			if err == nil || !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("expected error containing %q, got %v", tc.wantError, err)
			}
		})
	}
}

package main

import (
	"strings"
	"testing"
)

func TestLoadMatrixCredentials(t *testing.T) {
	const bearer = "test-bearer-value-for-the-mcp-only"
	t.Setenv("MATRIX_USER_ID", "@bot:example.test")
	t.Setenv("PICKLE_KEY", "test-pickle")
	t.Setenv("MCP_BEARER_TOKEN", bearer)

	for _, tc := range []struct {
		name, password, token, publicURL, wantError string
	}{
		{name: "password and OAuth", password: "test-password", publicURL: "https://mcp.example.test"},
		{name: "Matrix token", token: "test-matrix-session"},
		{name: "missing both", wantError: "set exactly one"},
		{name: "both set", password: "test-password", token: "test-matrix-session", wantError: "set exactly one"},
		{name: "token with OAuth", token: "test-matrix-session", publicURL: "https://mcp.example.test", wantError: "PUBLIC_URL requires MATRIX_PASSWORD"},
		{name: "same token for Matrix and MCP", token: bearer, wantError: "must be different"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("MATRIX_PASSWORD", tc.password)
			t.Setenv("MATRIX_ACCESS_TOKEN", tc.token)
			t.Setenv("PUBLIC_URL", tc.publicURL)
			cfg, err := loadConfig()
			if tc.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantError) {
					t.Fatalf("expected error containing %q, got %v", tc.wantError, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Password != tc.password || cfg.AccessToken != tc.token {
				t.Fatal("configuration did not retain the selected Matrix credentials")
			}
		})
	}
}

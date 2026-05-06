package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadConfigValidatesGroupsAndUnknownFields(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		content string
		wantErr string
	}{
		{
			name: "valid",
			content: `
groups:
  - name: alpha
    ws_auth_keys:
      - ws-alpha
    api_keys:
      - api-alpha
`,
		},
		{
			name: "unknown field",
			content: `
groups:
  - name: alpha
    ws_auth_keys:
      - ws-alpha
    api_keys:
      - api-alpha
    unknown: true
`,
			wantErr: "field unknown",
		},
		{
			name: "missing ws auth key",
			content: `
groups:
  - name: alpha
    api_keys:
      - api-alpha
`,
			wantErr: "ws_auth_keys",
		},
		{
			name: "missing api credential",
			content: `
groups:
  - name: alpha
    ws_auth_keys:
      - ws-alpha
`,
			wantErr: "api_keys",
		},
		{
			name: "legacy api bearer token field",
			content: `
groups:
  - name: alpha
    ws_auth_keys:
      - ws-alpha
    api_bearer_tokens:
      - bearer-alpha
`,
			wantErr: "field api_bearer_tokens",
		},
		{
			name: "legacy x-api-key field",
			content: `
groups:
  - name: alpha
    ws_auth_keys:
      - ws-alpha
    x_api_keys:
      - x-alpha
`,
			wantErr: "field x_api_keys",
		},
		{
			name: "duplicate api key across groups",
			content: `
groups:
  - name: alpha
    ws_auth_keys:
      - ws-alpha
    api_keys:
      - shared
  - name: beta
    ws_auth_keys:
      - ws-beta
    api_keys:
      - shared
`,
			wantErr: "configured in both group",
		},
		{
			name: "duplicate ws auth key across groups",
			content: `
groups:
  - name: alpha
    ws_auth_keys:
      - shared
    api_keys:
      - api-alpha
  - name: beta
    ws_auth_keys:
      - shared
    api_keys:
      - api-beta
`,
			wantErr: "configured in both group",
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			path := writeConfigFile(t, tc.content)
			cfg, err := LoadConfig(path)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("LoadConfig returned error: %v", err)
				}
				if cfg == nil || len(cfg.Groups) != 1 {
					t.Fatalf("expected 1 group, got %#v", cfg)
				}
				return
			}
			if err == nil {
				t.Fatal("expected LoadConfig to fail")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("expected error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

func TestLoadConfigSupportsJSON(t *testing.T) {
	t.Parallel()

	path := writeNamedConfigFile(t, "synapse-config.json", `{
		"groups": [
			{
				"name": "alpha",
				"ws_auth_keys": ["ws-alpha"],
				"api_keys": ["api-alpha"]
			}
		]
	}`)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig returned error: %v", err)
	}
	if len(cfg.Groups) != 1 || cfg.Groups[0].Name != "alpha" {
		t.Fatalf("unexpected config: %#v", cfg)
	}
}

func TestLoadConfigRejectsUnknownJSONFields(t *testing.T) {
	t.Parallel()

	path := writeNamedConfigFile(t, "synapse-config.json", `{
		"groups": [
			{
				"name": "alpha",
				"ws_auth_keys": ["ws-alpha"],
				"api_keys": ["api-alpha"],
				"unknown": true
			}
		]
	}`)

	err := loadConfigError(path)
	if err == nil {
		t.Fatal("expected LoadConfig to fail")
	}
	if !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("expected unknown field error, got %v", err)
	}
}

func TestAuthConfigMatchAPIGroupDetectsConflicts(t *testing.T) {
	t.Parallel()

	auth, err := newAuthConfigFromConfig(&Config{
		Groups: []ConfigGroup{
			{
				Name:       "alpha",
				WSAuthKeys: []string{"ws-alpha"},
				APIKeys:    []string{"api-alpha"},
			},
			{
				Name:       "beta",
				WSAuthKeys: []string{"ws-beta"},
				APIKeys:    []string{"api-beta"},
			},
		},
	})
	if err != nil {
		t.Fatalf("newAuthConfigFromConfig returned error: %v", err)
	}

	groupName, err := auth.matchAPIGroup(map[string][]string{
		"Authorization": {"Bearer api-alpha"},
	})
	if err != nil {
		t.Fatalf("expected bearer auth to match, got error: %v", err)
	}
	if groupName != "alpha" {
		t.Fatalf("expected alpha, got %q", groupName)
	}

	groupName, err = auth.matchAPIGroup(map[string][]string{
		"authorization": {"Bearer api-alpha"},
	})
	if err != nil {
		t.Fatalf("expected lowercase authorization header to match, got error: %v", err)
	}
	if groupName != "alpha" {
		t.Fatalf("expected alpha, got %q", groupName)
	}

	groupName, err = auth.matchAPIGroup(map[string][]string{
		"X-API-Key": {"api-beta"},
	})
	if err != nil {
		t.Fatalf("expected x-api-key auth to match, got error: %v", err)
	}
	if groupName != "beta" {
		t.Fatalf("expected beta, got %q", groupName)
	}

	groupName, err = auth.matchAPIGroup(map[string][]string{
		"x-api-key": {"api-beta"},
	})
	if err != nil {
		t.Fatalf("expected lowercase x-api-key header to match, got error: %v", err)
	}
	if groupName != "beta" {
		t.Fatalf("expected beta, got %q", groupName)
	}

	_, err = auth.matchAPIGroup(map[string][]string{
		"Authorization": {"Bearer api-alpha"},
		"X-API-Key":     {"api-beta"},
	})
	if err == nil || !strings.Contains(err.Error(), "conflicting") {
		t.Fatalf("expected conflicting credentials error, got %v", err)
	}

	_, err = auth.matchAPIGroup(map[string][]string{
		"Authorization": {"Bearer api-alpha"},
		"x-api-key":     {"api-beta"},
	})
	if err == nil || !strings.Contains(err.Error(), "conflicting") {
		t.Fatalf("expected conflicting credentials error for mixed-case headers, got %v", err)
	}
}

func TestServerReloadConfigKeepsPreviousConfigOnFailure(t *testing.T) {
	t.Parallel()

	srv, err := NewServerWithConfig(&Config{
		Groups: []ConfigGroup{
			{
				Name:       "alpha",
				WSAuthKeys: []string{"ws-alpha"},
				APIKeys:    []string{"api-alpha"},
			},
		},
	}, "test-version", "")
	if err != nil {
		t.Fatalf("NewServerWithConfig returned error: %v", err)
	}

	if err := srv.ReloadConfig(&Config{
		Groups: []ConfigGroup{
			{
				Name:       "beta",
				WSAuthKeys: []string{"ws-beta"},
			},
		},
	}); err == nil {
		t.Fatal("expected invalid reload to fail")
	}

	groupName, err := srv.currentAuthConfig().matchAPIGroup(map[string][]string{
		"Authorization": {"Bearer api-alpha"},
	})
	if err != nil {
		t.Fatalf("expected previous config to remain active, got error: %v", err)
	}
	if groupName != "alpha" {
		t.Fatalf("expected alpha, got %q", groupName)
	}
}

func writeConfigFile(t *testing.T, content string) string {
	t.Helper()

	return writeNamedConfigFile(t, "synapse-config.yaml", content)
}

func writeNamedConfigFile(t *testing.T, name, content string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}
	return path
}

func loadConfigError(path string) error {
	_, err := LoadConfig(path)
	return err
}

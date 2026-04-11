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
			content: `{
				"groups": [
					{
						"name": "alpha",
						"ws_auth_keys": ["ws-alpha"],
						"api_bearer_tokens": ["bearer-alpha"],
						"x_api_keys": ["x-alpha"]
					}
				]
			}`,
		},
		{
			name: "unknown field",
			content: `{
				"groups": [
					{
						"name": "alpha",
						"ws_auth_keys": ["ws-alpha"],
						"api_bearer_tokens": ["bearer-alpha"],
						"unknown": true
					}
				]
			}`,
			wantErr: "unknown field",
		},
		{
			name: "missing ws auth key",
			content: `{
				"groups": [
					{
						"name": "alpha",
						"api_bearer_tokens": ["bearer-alpha"]
					}
				]
			}`,
			wantErr: "ws_auth_keys",
		},
		{
			name: "missing api credential",
			content: `{
				"groups": [
					{
						"name": "alpha",
						"ws_auth_keys": ["ws-alpha"]
					}
				]
			}`,
			wantErr: "API credential",
		},
		{
			name: "duplicate bearer across groups",
			content: `{
				"groups": [
					{
						"name": "alpha",
						"ws_auth_keys": ["ws-alpha"],
						"api_bearer_tokens": ["shared"]
					},
					{
						"name": "beta",
						"ws_auth_keys": ["ws-beta"],
						"api_bearer_tokens": ["shared"]
					}
				]
			}`,
			wantErr: "configured in both group",
		},
		{
			name: "duplicate x-api-key across groups",
			content: `{
				"groups": [
					{
						"name": "alpha",
						"ws_auth_keys": ["ws-alpha"],
						"x_api_keys": ["shared"]
					},
					{
						"name": "beta",
						"ws_auth_keys": ["ws-beta"],
						"x_api_keys": ["shared"]
					}
				]
			}`,
			wantErr: "configured in both group",
		},
		{
			name: "duplicate ws auth key across groups",
			content: `{
				"groups": [
					{
						"name": "alpha",
						"ws_auth_keys": ["shared"],
						"api_bearer_tokens": ["bearer-alpha"]
					},
					{
						"name": "beta",
						"ws_auth_keys": ["shared"],
						"api_bearer_tokens": ["bearer-beta"]
					}
				]
			}`,
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

func TestAuthConfigMatchAPIGroupDetectsConflicts(t *testing.T) {
	t.Parallel()

	auth, err := newAuthConfigFromConfig(&Config{
		Groups: []ConfigGroup{
			{
				Name:            "alpha",
				WSAuthKeys:      []string{"ws-alpha"},
				APIBearerTokens: []string{"bearer-alpha"},
			},
			{
				Name:       "beta",
				WSAuthKeys: []string{"ws-beta"},
				XAPIKeys:   []string{"x-beta"},
			},
		},
	})
	if err != nil {
		t.Fatalf("newAuthConfigFromConfig returned error: %v", err)
	}

	groupName, err := auth.matchAPIGroup(map[string][]string{
		"Authorization": {"Bearer bearer-alpha"},
	})
	if err != nil {
		t.Fatalf("expected bearer auth to match, got error: %v", err)
	}
	if groupName != "alpha" {
		t.Fatalf("expected alpha, got %q", groupName)
	}

	groupName, err = auth.matchAPIGroup(map[string][]string{
		"X-API-Key": {"x-beta"},
	})
	if err != nil {
		t.Fatalf("expected x-api-key auth to match, got error: %v", err)
	}
	if groupName != "beta" {
		t.Fatalf("expected beta, got %q", groupName)
	}

	_, err = auth.matchAPIGroup(map[string][]string{
		"Authorization": {"Bearer bearer-alpha"},
		"X-API-Key":     {"x-beta"},
	})
	if err == nil || !strings.Contains(err.Error(), "conflicting") {
		t.Fatalf("expected conflicting credentials error, got %v", err)
	}
}

func writeConfigFile(t *testing.T, content string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "synapse-config.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}
	return path
}

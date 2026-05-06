package server

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

const defaultGroupName = "default"

type Config struct {
	Groups []ConfigGroup `json:"groups" yaml:"groups"`
}

type ConfigGroup struct {
	Name       string   `json:"name" yaml:"name"`
	WSAuthKeys []string `json:"ws_auth_keys" yaml:"ws_auth_keys"`
	APIKeys    []string `json:"api_keys" yaml:"api_keys"`
}

type authConfig struct {
	groups                  map[string]struct{}
	groupNames              []string
	wsAuthToGroup           map[string]string
	apiKeyToGroup           map[string]string
	defaultGroup            string
	allowUnauthenticatedWS  bool
	allowUnauthenticatedAPI bool
}

func LoadConfig(path string) (*Config, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open config %q: %w", path, err)
	}
	defer file.Close()

	var cfg Config
	if err := decodeConfig(path, file, &cfg); err != nil {
		return nil, fmt.Errorf("decode config %q: %w", path, err)
	}
	if _, err := newAuthConfigFromConfig(&cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func decodeConfig(path string, reader io.Reader, cfg *Config) error {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".json":
		decoder := json.NewDecoder(reader)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(cfg); err != nil {
			return err
		}
		return ensureJSONEOF(decoder)
	default:
		decoder := yaml.NewDecoder(reader)
		decoder.KnownFields(true)
		if err := decoder.Decode(cfg); err != nil {
			return err
		}
		return ensureYAMLEOF(decoder)
	}
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("unexpected trailing JSON content")
		}
		return err
	}
	return nil
}

func ensureYAMLEOF(decoder *yaml.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("unexpected trailing YAML content")
		}
		return err
	}
	return nil
}

func newLegacyAuthConfig(apiAuthKey, wsAuthKey string) *authConfig {
	auth := &authConfig{
		groups:        map[string]struct{}{defaultGroupName: {}},
		groupNames:    []string{defaultGroupName},
		wsAuthToGroup: make(map[string]string),
		apiKeyToGroup: make(map[string]string),
		defaultGroup:  defaultGroupName,
	}

	apiAuthKey = strings.TrimSpace(apiAuthKey)
	wsAuthKey = strings.TrimSpace(wsAuthKey)

	if wsAuthKey == "" {
		auth.allowUnauthenticatedWS = true
	} else {
		auth.wsAuthToGroup[wsAuthKey] = defaultGroupName
	}

	if apiAuthKey == "" {
		auth.allowUnauthenticatedAPI = true
	} else {
		auth.apiKeyToGroup[apiAuthKey] = defaultGroupName
	}

	return auth
}

func newAuthConfigFromConfig(cfg *Config) (*authConfig, error) {
	if cfg == nil {
		return nil, fmt.Errorf("config is required")
	}
	if len(cfg.Groups) == 0 {
		return nil, fmt.Errorf("config must define at least one group")
	}

	auth := &authConfig{
		groups:        make(map[string]struct{}, len(cfg.Groups)),
		wsAuthToGroup: make(map[string]string),
		apiKeyToGroup: make(map[string]string),
	}

	for _, rawGroup := range cfg.Groups {
		groupName := strings.TrimSpace(rawGroup.Name)
		if groupName == "" {
			return nil, fmt.Errorf("group name cannot be empty")
		}
		if _, exists := auth.groups[groupName]; exists {
			return nil, fmt.Errorf("duplicate group name %q", groupName)
		}

		wsKeys := normalizeUniqueStrings(rawGroup.WSAuthKeys)
		apiKeys := normalizeUniqueStrings(rawGroup.APIKeys)

		if len(wsKeys) == 0 {
			return nil, fmt.Errorf("group %q must define at least one ws_auth_keys entry", groupName)
		}
		if len(apiKeys) == 0 {
			return nil, fmt.Errorf("group %q must define at least one api_keys entry", groupName)
		}

		auth.groups[groupName] = struct{}{}
		auth.groupNames = append(auth.groupNames, groupName)

		if err := registerCredentialSet(auth.wsAuthToGroup, wsKeys, groupName, "ws_auth_key"); err != nil {
			return nil, err
		}
		if err := registerCredentialSet(auth.apiKeyToGroup, apiKeys, groupName, "api_key"); err != nil {
			return nil, err
		}
	}

	sort.Strings(auth.groupNames)
	return auth, nil
}

func normalizeUniqueStrings(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func registerCredentialSet(index map[string]string, values []string, groupName, credentialType string) error {
	for _, value := range values {
		if existingGroup, exists := index[value]; exists {
			return fmt.Errorf("%s %q is configured in both group %q and %q", credentialType, value, existingGroup, groupName)
		}
		index[value] = groupName
	}
	return nil
}

func (c *authConfig) matchWebSocketGroup(authKey string) (string, bool) {
	authKey = strings.TrimSpace(authKey)
	if groupName, exists := c.wsAuthToGroup[authKey]; exists {
		return groupName, true
	}
	if c.allowUnauthenticatedWS {
		return c.defaultGroup, true
	}
	return "", false
}

func (c *authConfig) matchAPIGroup(headers map[string][]string) (string, error) {
	matchedGroups := make(map[string]struct{}, 2)

	for _, value := range headerValues(headers, "Authorization") {
		if token, ok := bearerTokenFromHeader(value); ok {
			if groupName, exists := c.apiKeyToGroup[token]; exists {
				matchedGroups[groupName] = struct{}{}
			}
		}
	}

	for _, value := range headerValues(headers, "X-API-Key") {
		if xAPIKey := strings.TrimSpace(value); xAPIKey != "" {
			if groupName, exists := c.apiKeyToGroup[xAPIKey]; exists {
				matchedGroups[groupName] = struct{}{}
			}
		}
	}

	switch len(matchedGroups) {
	case 0:
		if c.allowUnauthenticatedAPI {
			return c.defaultGroup, nil
		}
		return "", fmt.Errorf("unauthorized")
	case 1:
		for groupName := range matchedGroups {
			return groupName, nil
		}
	default:
		return "", fmt.Errorf("conflicting credentials belong to different groups")
	}

	return "", fmt.Errorf("unauthorized")
}

func bearerTokenFromHeader(value string) (string, bool) {
	parts := strings.Fields(strings.TrimSpace(value))
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || parts[1] == "" {
		return "", false
	}
	return parts[1], true
}

func headerValues(headers map[string][]string, key string) []string {
	var matchedValues []string
	for headerKey, values := range headers {
		if !strings.EqualFold(headerKey, key) || len(values) == 0 {
			continue
		}
		matchedValues = append(matchedValues, values...)
	}
	return matchedValues
}

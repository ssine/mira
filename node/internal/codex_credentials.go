package node

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// This file belongs to one Node identity and is never sent to the Server.
// A single atomic replacement commits provider and secret changes together.
type managedAccountProfile struct {
	Provider    managedAccountProvider `json:"provider"`
	APIKey      string                 `json:"apiKey,omitempty"`
	Environment map[string]string      `json:"environment,omitempty"`
}

type managedAccountProvider struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	BaseURL string `json:"baseUrl"`
	EnvKey  string `json:"envKey"`
}

func managedAccountProfilePath(configuration config) (string, error) {
	if configuration.CodexAccountID == "" {
		return "", nil
	}
	home, err := accountProfileHome(configuration.IdentityFile, configuration.CodexAccountID)
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(home), "profile.json"), nil
}

func readManagedAccountProfile(configuration config) (managedAccountProfile, error) {
	profile := managedAccountProfile{}
	path, err := managedAccountProfilePath(configuration)
	if err != nil || path == "" {
		return profile, err
	}
	data, err := readAccountFile(path)
	if os.IsNotExist(err) {
		return profile, nil
	}
	if err != nil {
		return profile, fmt.Errorf("cannot read protected account profile")
	}
	if json.Unmarshal(data, &profile) != nil {
		return profile, fmt.Errorf("invalid protected account profile")
	}
	return profile, nil
}

func managedAccountOverrides(configuration config) ([]string, error) {
	profile, err := readManagedAccountProfile(configuration)
	if err != nil {
		return nil, err
	}
	p := profile.Provider
	if p.ID == "" {
		return nil, nil
	}
	// Codex splits override keys on dots without unquoting each component.
	// Provider IDs are restricted to bare TOML identifiers during validation.
	key := "model_providers." + p.ID + "."
	return []string{"model_provider=" + strconv.Quote(p.ID), key + "name=" + strconv.Quote(p.Name), key + "base_url=" + strconv.Quote(p.BaseURL), key + "wire_api=\"responses\"", key + "env_key=" + strconv.Quote(p.EnvKey), key + "requires_openai_auth=false"}, nil
}

func validateAccountEnvironment(values map[string]string) error {
	if len(values) > 128 {
		return fmt.Errorf("account environment has more than 128 variables")
	}
	for key, value := range values {
		if !accountEnvName.MatchString(key) || strings.EqualFold(key, "CODEX_HOME") || strings.HasPrefix(strings.ToUpper(key), "MIRA_") {
			return fmt.Errorf("invalid or reserved account environment name")
		}
		if strings.ContainsRune(value, 0) {
			return fmt.Errorf("account environment contains a NUL value")
		}
	}
	return nil
}

// Configure accepts secrets only while the runtime is stopped. Adopted homes
// are preserved; provider edits are allowed only in a Mira-owned profile.
func (manager *appServerManager) configureAccount(raw json.RawMessage) (map[string]any, error) {
	if len(raw) > maxAccountConfigBytes {
		return nil, fmt.Errorf("account configuration exceeds 256 KiB")
	}
	var input struct {
		Provider    *managedAccountProvider `json:"provider"`
		APIKey      *string                 `json:"apiKey"`
		Environment map[string]string       `json:"environment"`
	}
	if json.Unmarshal(raw, &input) != nil {
		return nil, fmt.Errorf("invalid account configuration")
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.runtimePreparing || (manager.instance != nil && !channelClosed(manager.instance.done)) {
		return nil, fmt.Errorf("请先停止此账号的运行实例，再修改配置或凭据")
	}
	path, err := managedAccountProfilePath(manager.configuration)
	if err != nil {
		return nil, err
	}
	if path == "" {
		return nil, fmt.Errorf("默认账号使用已有配置；请创建独立账号以管理凭据")
	}
	profile, err := readManagedAccountProfile(manager.configuration)
	if err != nil {
		return nil, err
	}
	if input.Provider != nil {
		p := *input.Provider
		if p.ID == "" {
			p.ID = "mira_account"
		}
		if p.Name == "" {
			p.Name = p.ID
		}
		if p.EnvKey == "" {
			p.EnvKey = "MIRA_NODE_CODEX_API_KEY"
		}
		endpoint, err := url.Parse(p.BaseURL)
		if err != nil || endpoint.Host == "" || (endpoint.Scheme != "https" && endpoint.Scheme != "http") || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
			return nil, fmt.Errorf("provider base URL must be HTTP(S) without credentials, query or fragment")
		}
		if !accountEnvName.MatchString(p.ID) || p.ID == "openai" || len(p.Name) > 128 || strings.ContainsAny(p.Name, "\r\n\x00") || !accountEnvName.MatchString(p.EnvKey) {
			return nil, fmt.Errorf("invalid provider name or environment reference")
		}
		if p.EnvKey != "MIRA_NODE_CODEX_API_KEY" {
			if err := validateAccountEnvironment(map[string]string{p.EnvKey: ""}); err != nil {
				return nil, err
			}
		}
		profile.Provider = p
	}
	if input.APIKey != nil {
		if profile.Provider.ID == "" {
			return nil, fmt.Errorf("请先配置自定义服务商；OpenAI API Key 请使用账号登录接口")
		}
		if len(*input.APIKey) > 32768 || strings.ContainsAny(*input.APIKey, "\r\n\x00") {
			return nil, fmt.Errorf("invalid API key")
		}
		profile.APIKey = *input.APIKey
	}
	if input.Environment != nil {
		if err := validateAccountEnvironment(input.Environment); err != nil {
			return nil, err
		}
		profile.Environment = input.Environment
	}
	encoded, err := json.Marshal(profile)
	if err != nil || len(encoded) > maxAccountConfigBytes {
		return nil, fmt.Errorf("account configuration exceeds 256 KiB")
	}
	if err := writeProtectedAccountFile(path, encoded); err != nil {
		return nil, fmt.Errorf("cannot save protected account configuration")
	}
	return map[string]any{"configured": true, "credentialPresent": profile.APIKey != ""}, nil
}

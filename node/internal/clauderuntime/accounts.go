package clauderuntime

import (
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// AccountProvider is public metadata. Keys belong only to the Node-local file.
type AccountProvider struct {
	ID      string `json:"id"`
	BaseURL string `json:"baseUrl"`
	Region  string `json:"region,omitempty"`
	Model   string `json:"model"`
	// Effort is the default reasoning effort for turns that use Model; empty
	// leaves the SDK's own default.
	Effort string `json:"effort,omitempty"`
}

// ValidEffort reports whether value is empty or an SDK reasoning effort level.
func ValidEffort(value string) bool {
	switch value {
	case "", "low", "medium", "high", "xhigh", "max":
		return true
	}
	return false
}

func (p AccountProvider) Validate() error {
	u, err := url.Parse(p.BaseURL)
	if err != nil || u.Hostname() == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || len(p.BaseURL) > 2048 {
		return errors.New("Claude provider requires an HTTP(S) base URL without credentials or query parameters")
	}
	if (p.ID != "anthropic" && p.ID != "bedrock") || p.Model == "" || len(p.Model) > 200 || strings.ContainsAny(p.Model+p.Region, "\r\n\x00") || len(p.Region) > 64 || (p.ID == "bedrock" && p.Region == "") {
		return errors.New("Choose a Claude Messages or Bedrock provider, model and Bedrock region")
	}
	if !ValidEffort(p.Effort) {
		return errors.New("Choose a Claude reasoning effort: low, medium, high, xhigh or max")
	}
	return nil
}

type localAccount struct {
	Provider AccountProvider `json:"provider"`
	APIKey   string          `json:"apiKey"`
}

var accountIDPattern = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func (m *Manager) accountHome(id string) (string, error) {
	if !accountIDPattern.MatchString(id) {
		return "", errors.New("Invalid Claude Node account ID")
	}
	return filepath.Join(m.accountsRoot, id, "claude"), nil
}

func (m *Manager) readAccount(id string) (localAccount, error) {
	var account localAccount
	home, err := m.accountHome(id)
	if err != nil {
		return account, err
	}
	f, err := os.Open(filepath.Join(home, "mira-account.json"))
	if err != nil {
		return account, errors.New("Configure this Claude account on the execution Node first")
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 256*1024+1))
	if err != nil || len(data) > 256*1024 || json.Unmarshal(data, &account) != nil {
		return account, errors.New("Invalid Node-local Claude account configuration")
	}
	return account, account.Provider.Validate()
}

// Called under the manager lock, shared with worker starts and completion.
func (m *Manager) configureAccount(params map[string]any) (any, error) {
	id, _ := params["nodeAccountId"].(string)
	home, err := m.accountHome(id)
	if err != nil {
		return nil, err
	}
	for _, p := range m.processes {
		if p.accountID == id {
			return nil, errors.New("Claude account has an active turn; wait or interrupt it before changing credentials")
		}
	}
	account, _ := m.readAccount(id)
	if raw, ok := params["provider"]; ok {
		encoded, _ := json.Marshal(raw)
		if json.Unmarshal(encoded, &account.Provider) != nil {
			return nil, errors.New("Invalid Claude provider")
		}
	}
	if err = account.Provider.Validate(); err != nil {
		return nil, err
	}
	if raw, ok := params["apiKey"]; ok {
		key, valid := raw.(string)
		if !valid || len(key) > 32768 || strings.ContainsAny(key, "\r\n\x00") {
			return nil, errors.New("Invalid Claude API key")
		}
		account.APIKey = key
	}
	if err = os.MkdirAll(home, 0700); err != nil {
		return nil, errors.New("Cannot create the private Claude account directory")
	}
	f, err := os.CreateTemp(home, ".account-")
	if err != nil {
		return nil, errors.New("Cannot write Claude account configuration")
	}
	defer os.Remove(f.Name())
	err = json.NewEncoder(f).Encode(account)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(f.Name(), filepath.Join(home, "mira-account.json"))
	}
	if err != nil {
		return nil, errors.New("Cannot save private Claude account configuration")
	}
	return map[string]any{"configured": account.APIKey != "", "provider": account.Provider}, nil
}

func (m *Manager) accountEnvironment(id string) ([]string, error) {
	if id == "" {
		return nil, nil
	} // Existing Node-native Claude login/config remains supported.
	account, err := m.readAccount(id)
	if err != nil {
		return nil, err
	}
	if account.APIKey == "" {
		return nil, errors.New("Claude account has no API key")
	}
	home, _ := m.accountHome(id)
	env := map[string]string{}
	for _, entry := range os.Environ() {
		key, value, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "ANTHROPIC_") || strings.HasPrefix(key, "CLAUDE_CODE_USE_") {
			continue
		}
		switch key {
		case "CLAUDE_CODE_OAUTH_TOKEN", "AWS_BEARER_TOKEN_BEDROCK", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN", "AWS_PROFILE", "AWS_DEFAULT_PROFILE":
			continue
		}
		env[key] = value
	}
	env["CLAUDE_CONFIG_DIR"] = home
	env["MIRA_NODE_CLAUDE_CONFIG_DIR"] = home
	for _, key := range []string{"ANTHROPIC_MODEL", "ANTHROPIC_DEFAULT_OPUS_MODEL", "ANTHROPIC_DEFAULT_SONNET_MODEL", "ANTHROPIC_DEFAULT_HAIKU_MODEL"} {
		env[key] = account.Provider.Model
	}
	env["CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC"] = "1"
	if account.Provider.ID == "bedrock" {
		env["CLAUDE_CODE_USE_BEDROCK"] = "1"
		env["ANTHROPIC_BEDROCK_BASE_URL"] = account.Provider.BaseURL
		env["AWS_REGION"] = account.Provider.Region
		env["AWS_BEARER_TOKEN_BEDROCK"] = account.APIKey
		env["AWS_EC2_METADATA_DISABLED"] = "true"
	} else {
		env["ANTHROPIC_BASE_URL"] = account.Provider.BaseURL
		env["ANTHROPIC_API_KEY"] = account.APIKey
	}
	result := make([]string, 0, len(env))
	for key, value := range env {
		result = append(result, key+"="+value)
	}
	return result, nil
}

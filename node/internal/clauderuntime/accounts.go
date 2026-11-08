package clauderuntime

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/ssine/mira/node/internal/accountegress"
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
	Provider AccountProvider     `json:"provider"`
	APIKey   string              `json:"apiKey"`
	AuthType string              `json:"authType,omitempty"`
	Egress   *accountegress.Spec `json:"egress,omitempty"`
}

type oauthCredentials struct {
	ClaudeAIOAuth struct {
		AccessToken           string   `json:"accessToken"`
		RefreshToken          string   `json:"refreshToken"`
		ExpiresAt             int64    `json:"expiresAt"`
		RefreshTokenExpiresAt int64    `json:"refreshTokenExpiresAt"`
		Scopes                []string `json:"scopes"`
	} `json:"claudeAiOauth"`
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
	if err := account.Provider.Validate(); err != nil {
		return account, err
	}
	if account.AuthType == "" {
		account.AuthType = "api_key"
	}
	if account.AuthType != "api_key" && account.AuthType != "subscription" {
		return account, errors.New("Invalid Claude account authentication type")
	}
	if account.Egress != nil {
		if err := account.Egress.Validate(); err != nil {
			return account, err
		}
	}
	return account, nil
}

func (m *Manager) accountConfigured(id string, account localAccount) bool {
	switch account.AuthType {
	case "api_key":
		return account.APIKey != ""
	case "subscription":
		home, err := m.accountHome(id)
		if err != nil {
			return false
		}
		return validOAuthCredentials(filepath.Join(home, ".credentials.json"))
	default:
		return false
	}
}

func validOAuthCredentials(path string) bool {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 1024*1024 {
		return false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var credentials oauthCredentials
	if json.Unmarshal(data, &credentials) != nil {
		return false
	}
	oauth := credentials.ClaudeAIOAuth
	if oauth.AccessToken == "" || oauth.RefreshToken == "" {
		return false
	}
	now := time.Now().UnixMilli()
	if oauth.RefreshTokenExpiresAt > 0 && oauth.RefreshTokenExpiresAt <= now {
		return false
	}
	if oauth.ExpiresAt > 0 && oauth.ExpiresAt <= now && oauth.RefreshToken == "" {
		return false
	}
	for _, scope := range oauth.Scopes {
		if scope == "user:inference" {
			return true
		}
	}
	return len(oauth.Scopes) == 0
}

func secureCredentialFile(path string) error {
	if runtime.GOOS != "windows" {
		if err := os.Chmod(path, 0600); err != nil {
			return err
		}
	}
	return nil
}

// Called under the manager lock, shared with worker starts and completion.
func (m *Manager) configureAccount(params map[string]any) (any, error) {
	id, _ := params["nodeAccountId"].(string)
	home, err := m.accountHome(id)
	if err != nil {
		return nil, err
	}
	if m.accountBusyLocked(id) {
		return nil, errors.New("Claude account has an active turn; wait or interrupt it before changing credentials")
	}
	account, _ := m.readAccount(id)
	if account.AuthType == "" {
		account.AuthType = "api_key"
	}
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
	if raw, ok := params["authType"]; ok {
		authType, valid := raw.(string)
		if !valid || authType != "api_key" && authType != "subscription" {
			return nil, errors.New("Invalid Claude account authentication type")
		}
		account.AuthType = authType
	}
	if raw, ok := params["egress"]; ok {
		if raw == nil {
			if err := m.egress.Delete(id); err != nil {
				return nil, fmt.Errorf("Cannot delete Claude account egress: %w", err)
			}
			account.Egress = nil
		} else {
			encoded, _ := json.Marshal(raw)
			var spec accountegress.Spec
			if json.Unmarshal(encoded, &spec) != nil {
				return nil, errors.New("Invalid Claude account egress")
			}
			if spec.Password == "" && account.Egress != nil && account.Egress.Type == spec.Type && account.Egress.Server == spec.Server && account.Egress.Port == spec.Port && account.Egress.Username == spec.Username {
				spec.Password = account.Egress.Password
			}
			if err := spec.Validate(); err != nil {
				return nil, err
			}
			if _, err := m.egress.Configure(id, spec); err != nil {
				return nil, fmt.Errorf("Cannot configure Claude account egress: %w", err)
			}
			account.Egress = &spec
		}
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
	return map[string]any{"configured": m.accountConfigured(id, account), "provider": account.Provider, "authType": account.AuthType}, nil
}

func (m *Manager) accountEnvironment(id string) ([]string, error) {
	values, release, err := m.accountEnvironmentAcquired(id, false)
	if release != nil {
		defer release()
	}
	return values, err
}

func (m *Manager) accountEnvironmentAcquired(id string, allowUnloggedSubscription bool) ([]string, func(), error) {
	if id == "" {
		return nil, func() {}, nil
	} // Existing Node-native Claude login/config remains supported.
	account, err := m.readAccount(id)
	if err != nil {
		return nil, nil, err
	}
	if account.AuthType == "" {
		account.AuthType = "api_key"
	}
	if account.AuthType == "api_key" && account.APIKey == "" {
		return nil, nil, errors.New("Claude account has no API key")
	}
	if account.AuthType == "subscription" && !allowUnloggedSubscription && !m.accountConfigured(id, account) {
		return nil, nil, errors.New("Claude subscription account is not logged in")
	}
	egressEnv, release, err := m.egress.Acquire(id)
	if err != nil {
		return nil, nil, fmt.Errorf("Claude account egress unavailable: %w", err)
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
	env["DISABLE_TELEMETRY"] = "1"
	env["DISABLE_ERROR_REPORTING"] = "1"
	env["DISABLE_FEEDBACK_COMMAND"] = "1"
	env["CLAUDE_CODE_DISABLE_FEEDBACK_SURVEY"] = "1"
	env["CLAUDE_CODE_DISABLE_FEATURE_FLAGS"] = "1"
	env["CLAUDE_CODE_DISABLE_OFFICIAL_MARKETPLACE_AUTOINSTALL"] = "1"
	if account.Provider.ID == "bedrock" {
		env["CLAUDE_CODE_USE_BEDROCK"] = "1"
		env["ANTHROPIC_BEDROCK_BASE_URL"] = account.Provider.BaseURL
		env["AWS_REGION"] = account.Provider.Region
		env["AWS_BEARER_TOKEN_BEDROCK"] = account.APIKey
		env["AWS_EC2_METADATA_DISABLED"] = "true"
	} else if account.AuthType != "subscription" {
		env["ANTHROPIC_BASE_URL"] = account.Provider.BaseURL
		env["ANTHROPIC_API_KEY"] = account.APIKey
	}
	for key, value := range egressEnv {
		env[key] = value
	}
	// New commits use an account-local identity without changing the host's Git config.
	env["GIT_AUTHOR_NAME"] = "Claude"
	env["GIT_COMMITTER_NAME"] = "Claude"
	env["GIT_AUTHOR_EMAIL"] = "claude@local.invalid"
	env["GIT_COMMITTER_EMAIL"] = "claude@local.invalid"
	result := make([]string, 0, len(env))
	for key, value := range env {
		result = append(result, key+"="+value)
	}
	return result, release, nil
}

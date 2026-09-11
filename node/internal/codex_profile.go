package node

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

const maxAccountConfigBytes = 256 * 1024

var accountEnvName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)

func readAccountFile(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > maxAccountConfigBytes {
		return nil, fmt.Errorf("account configuration must be a regular file of at most 256 KiB")
	}
	data, err := io.ReadAll(io.LimitReader(file, maxAccountConfigBytes+1))
	if len(data) > maxAccountConfigBytes {
		return nil, fmt.Errorf("account configuration exceeds 256 KiB")
	}
	return data, err
}

// Environment files are data, never shell scripts. Values are not interpolated
// or evaluated; error messages contain line numbers or variable names only.
func parseAccountEnvironment(data []byte) (map[string]string, error) {
	values := map[string]string{}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 4096), maxAccountConfigBytes)
	line := 0
	for scanner.Scan() {
		line++
		text := strings.TrimSpace(scanner.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		text = strings.TrimPrefix(text, "export ")
		key, value, ok := strings.Cut(text, "=")
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if !ok || !accountEnvName.MatchString(key) {
			return nil, fmt.Errorf("invalid environment declaration at line %d", line)
		}
		if strings.EqualFold(key, "CODEX_HOME") || strings.HasPrefix(strings.ToUpper(key), "MIRA_") {
			return nil, fmt.Errorf("environment file cannot override %s", key)
		}
		if strings.HasPrefix(value, "\"") {
			decoded, err := strconv.Unquote(value)
			if err != nil {
				return nil, fmt.Errorf("invalid quoted environment value at line %d", line)
			}
			value = decoded
		} else if strings.HasPrefix(value, "'") {
			if len(value) < 2 || !strings.HasSuffix(value, "'") {
				return nil, fmt.Errorf("invalid quoted environment value at line %d", line)
			}
			value = value[1 : len(value)-1]
		}
		if strings.ContainsRune(value, 0) {
			return nil, fmt.Errorf("invalid environment value at line %d", line)
		}
		values[key] = value
		if len(values) > 128 {
			return nil, fmt.Errorf("environment file has more than 128 variables")
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("environment file cannot be parsed")
	}
	return values, nil
}

type accountProviderConfig struct {
	Name       string            `toml:"name"`
	BaseURL    string            `toml:"base_url"`
	WireAPI    string            `toml:"wire_api"`
	EnvKey     string            `toml:"env_key"`
	EnvHeaders map[string]string `toml:"env_http_headers"`
	Bearer     string            `toml:"experimental_bearer_token"`
}

type accountHomeConfig struct {
	Provider  string                           `toml:"model_provider"`
	Profile   string                           `toml:"profile"`
	Providers map[string]accountProviderConfig `toml:"model_providers"`
	Profiles  map[string]struct {
		Provider string `toml:"model_provider"`
	} `toml:"profiles"`
}

func (configuration accountHomeConfig) provider() (string, accountProviderConfig) {
	id := configuration.Provider
	if profile, ok := configuration.Profiles[configuration.Profile]; ok && profile.Provider != "" {
		id = profile.Provider
	}
	if id == "" {
		id = "openai"
	}
	return id, configuration.Providers[id]
}

func prepareAccountEnvironment(configuration config, desired desiredAppServer) ([]string, map[string]any, error) {
	home, err := effectiveAccountHome(desired.CodexHome)
	if err != nil {
		return nil, nil, err
	}
	stored := accountHomeConfig{}
	data, err := readAccountFile(filepath.Join(home, "config.toml"))
	if err == nil {
		if toml.Unmarshal(data, &stored) != nil {
			return nil, nil, fmt.Errorf("Codex config.toml cannot be parsed")
		}
	} else if !os.IsNotExist(err) {
		return nil, nil, fmt.Errorf("cannot read Codex config.toml")
	}
	// Parse the same dotted provider overrides passed to Codex, including
	// env_key and env_http_headers. Never copy their values into reports.
	var merged map[string]any
	_ = toml.Unmarshal(data, &merged)
	if merged == nil {
		merged = map[string]any{}
	}
	for _, override := range desired.ConfigOverrides {
		var values map[string]any
		if toml.Unmarshal([]byte(override), &values) != nil {
			return nil, nil, fmt.Errorf("invalid account config override")
		}
		mergeAccountConfig(merged, values)
	}
	encoded, err := toml.Marshal(merged)
	if err != nil || toml.Unmarshal(encoded, &stored) != nil {
		return nil, nil, fmt.Errorf("cannot resolve account configuration")
	}
	id, provider := stored.provider()
	required := map[string]bool{}
	if provider.EnvKey != "" {
		required[provider.EnvKey] = true
	}
	for _, key := range provider.EnvHeaders {
		required[key] = true
	}
	values := map[string]string{}
	for _, pair := range os.Environ() {
		key, value, _ := strings.Cut(pair, "=")
		values[accountEnvKey(key)] = value
	}
	if configuration.CodexAccountID != "" {
		allowed := map[string]bool{}
		for key := range required {
			allowed[accountEnvKey(key)] = true
		}
		for _, key := range desired.InheritEnv {
			if !accountEnvName.MatchString(key) || strings.EqualFold(key, "CODEX_HOME") || strings.HasPrefix(strings.ToUpper(key), "MIRA_") {
				return nil, nil, fmt.Errorf("invalid or reserved inherited environment name")
			}
			allowed[accountEnvKey(key)] = true
		}
		for key := range values {
			if !baseAccountEnv(key) && !allowed[key] {
				delete(values, key)
			}
		}
	}
	sourceNames := []string{}
	if len(desired.EnvironmentFiles) > 8 || len(desired.InheritEnv) > 128 {
		return nil, nil, fmt.Errorf("account environment sources exceed bounds")
	}
	for _, path := range desired.EnvironmentFiles {
		if !filepath.IsAbs(path) {
			return nil, nil, fmt.Errorf("account environment file must be an absolute Node-local path")
		}
		data, err := readAccountFile(path)
		if err != nil {
			return nil, nil, fmt.Errorf("cannot read account environment file")
		}
		loaded, err := parseAccountEnvironment(data)
		if err != nil {
			return nil, nil, err
		}
		for key, value := range loaded {
			values[accountEnvKey(key)] = value
			sourceNames = append(sourceNames, key)
		}
	}
	profile, err := readManagedAccountProfile(configuration)
	if err != nil {
		return nil, nil, err
	}
	for key, value := range profile.Environment {
		values[accountEnvKey(key)] = value
		sourceNames = append(sourceNames, key)
	}
	if profile.APIKey != "" {
		values[accountEnvKey(profile.Provider.EnvKey)] = profile.APIKey
	}
	missing := []string{}
	for key := range required {
		if !accountEnvName.MatchString(key) {
			return nil, nil, fmt.Errorf("provider contains an invalid environment reference")
		}
		if values[accountEnvKey(key)] == "" {
			missing = append(missing, key)
		}
	}
	sort.Strings(missing)
	sort.Strings(sourceNames)
	view := map[string]any{"id": id, "name": provider.Name, "wireApi": provider.WireAPI, "environmentKeys": sourceNames, "missingEnvironment": missing}
	if endpoint, err := url.Parse(provider.BaseURL); err == nil && endpoint.Host != "" && endpoint.User == nil && endpoint.RawQuery == "" && endpoint.Fragment == "" {
		view["baseUrl"] = provider.BaseURL
	}
	source := "chatgpt"
	if id != "openai" {
		source = "providerConfig"
	}
	if provider.EnvKey != "" {
		source = "environment"
	}
	if provider.Bearer != "" {
		source = "providerConfig"
	}
	if profile.APIKey != "" {
		source = "managedEnvironment"
	}
	view["requiredEnvironment"] = sortedAccountKeys(required)
	view["credentialSource"] = source
	if len(missing) > 0 {
		return nil, view, fmt.Errorf("provider requires missing environment variables: %s", strings.Join(missing, ", "))
	}
	values["CODEX_HOME"] = home
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	environment := make([]string, 0, len(keys))
	for _, key := range keys {
		environment = append(environment, key+"="+values[key])
	}
	return environment, view, nil
}

func mergeAccountConfig(target, source map[string]any) {
	for key, value := range source {
		if nested, ok := value.(map[string]any); ok {
			current, _ := target[key].(map[string]any)
			if current == nil {
				current = map[string]any{}
				target[key] = current
			}
			mergeAccountConfig(current, nested)
		} else {
			target[key] = value
		}
	}
}

func accountEnvKey(key string) string {
	if runtime.GOOS == "windows" {
		return strings.ToUpper(key)
	}
	return key
}

func sortedAccountKeys(values map[string]bool) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func baseAccountEnv(key string) bool {
	key = strings.ToUpper(key)
	if strings.HasPrefix(key, "LC_") || strings.HasPrefix(key, "XDG_") {
		return true
	}
	switch key {
	case "PATH", "HOME", "USER", "LOGNAME", "SHELL", "LANG", "LANGUAGE", "TERM", "COLORTERM", "TMP", "TEMP", "TMPDIR", "USERPROFILE", "HOMEDRIVE", "HOMEPATH", "SYSTEMROOT", "SYSTEMDRIVE", "WINDIR", "COMSPEC", "PATHEXT", "APPDATA", "LOCALAPPDATA", "PROGRAMDATA", "PROGRAMFILES", "PROGRAMFILES(X86)", "SSH_AUTH_SOCK", "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY", "SSL_CERT_FILE", "SSL_CERT_DIR", "REQUESTS_CA_BUNDLE", "CURL_CA_BUNDLE", "NODE_EXTRA_CA_CERTS", "CODEX_SQLITE_HOME":
		return true
	}
	return false
}

func writeProtectedAccountFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".account-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if err = protectIdentityFile(file); err == nil {
		_, err = file.Write(data)
	}
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(file.Name(), path)
}

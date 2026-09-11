package node

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const testAccountBinding = "00000000-0000-4000-8000-000000000001"

func TestAccountProviderAdoptionAndEnvironment(t *testing.T) {
	home := t.TempDir()
	configuration := config{IdentityFile: filepath.Join(t.TempDir(), "identity.json"), CodexAccountID: testAccountBinding}
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(`model_provider = "custom"
[model_providers.custom]
name = "Custom"
base_url = "https://provider.example.test/v1"
wire_api = "responses"
experimental_bearer_token = "config-secret"
env_http_headers = {"x-tenant" = "CUSTOM_TENANT"}
`), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPENAI_API_KEY", "unrelated-secret")
	t.Setenv("UNRELATED_SECRET", "another-secret")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:9999")
	envPath := filepath.Join(t.TempDir(), "provider.env")
	if err := os.WriteFile(envPath, []byte("CUSTOM_TENANT='tenant-secret'\nLITERAL=$(do-not-run)\n"), 0600); err != nil {
		t.Fatal(err)
	}
	env, view, err := prepareAccountEnvironment(configuration, desiredAppServer{CodexHome: home, EnvironmentFiles: []string{envPath}})
	if err != nil {
		t.Fatal(err)
	}
	if view["credentialSource"] != "providerConfig" || view["id"] != "custom" {
		t.Fatalf("wrong provider detection: %#v", view)
	}
	values := map[string]string{}
	for _, entry := range env {
		key, value, _ := strings.Cut(entry, "=")
		values[key] = value
	}
	if values["CUSTOM_TENANT"] != "tenant-secret" || values["LITERAL"] != "$(do-not-run)" || values["HTTPS_PROXY"] == "" {
		t.Fatal("provider environment was not preserved")
	}
	if values["OPENAI_API_KEY"] != "" || values["UNRELATED_SECRET"] != "" {
		t.Fatal("unrelated credentials inherited")
	}
	encoded, _ := json.Marshal(view)
	if strings.Contains(string(encoded), "-secret") {
		t.Fatal("provider report leaked credentials")
	}
	_, missing, err := prepareAccountEnvironment(configuration, desiredAppServer{CodexHome: home})
	if err == nil || !reflect.DeepEqual(missing["missingEnvironment"], []string{"CUSTOM_TENANT"}) {
		t.Fatalf("missing env not reported: %#v %v", missing, err)
	}
}

func TestManagedAccountCredentialsAndOverride(t *testing.T) {
	configuration := config{IdentityFile: filepath.Join(t.TempDir(), "identity.json"), CodexAccountID: testAccountBinding}
	manager := newAppServerManager(configuration)
	_, err := manager.configureAccount(json.RawMessage(`{"provider":{"id":"custom","baseUrl":"https://provider.example.test/v1"},"apiKey":"managed-secret","environment":{"LOCAL_SETTING":"literal"}}`))
	if err != nil {
		t.Fatal(err)
	}
	overrides, err := managedAccountOverrides(configuration)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(overrides, " "), "managed-secret") {
		t.Fatal("secret appeared in arguments")
	}
	home, err := accountProfileHome(configuration.IdentityFile, testAccountBinding)
	if err != nil {
		t.Fatal(err)
	}
	env, view, err := prepareAccountEnvironment(configuration, desiredAppServer{CodexHome: home, ConfigOverrides: overrides})
	if err != nil {
		t.Fatal(err)
	}
	if view["id"] != "custom" || view["credentialSource"] != "managedEnvironment" {
		t.Fatalf("wrong effective provider: %#v", view)
	}
	found := false
	for _, entry := range env {
		if entry == "MIRA_NODE_CODEX_API_KEY=managed-secret" {
			found = true
		}
	}
	if !found {
		t.Fatal("managed credential missing from child environment")
	}
	_, err = manager.configureAccount(json.RawMessage(`{"environment":{"NEW_SETTING":"updated"}}`))
	if err != nil {
		t.Fatal(err)
	}
	stored, err := readManagedAccountProfile(configuration)
	if err != nil {
		t.Fatal(err)
	}
	if stored.APIKey != "managed-secret" || len(stored.Environment) != 1 {
		t.Fatal("partial update lost credential or failed to replace environment")
	}
	path, _ := managedAccountProfilePath(configuration)
	before, _ := os.ReadFile(path)
	_, err = manager.configureAccount(json.RawMessage(`{"environment":{"MIRA_NODE_TOKEN":"bad"}}`))
	if err == nil {
		t.Fatal("reserved variable accepted")
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("failed update changed credential file")
	}
}

func TestAccountCredentialGateAndPendingTurn(t *testing.T) {
	manager := newAppServerManager(config{})
	request := []byte(`{"id":1,"method":"turn/start","params":{"threadId":"thread-one"}}`)
	if err := manager.reserveAccountRequest("client", request); err != nil {
		t.Fatal(err)
	}
	if err := manager.beginAccountManagement("login"); err == nil {
		t.Fatal("login overlapped pending turn")
	}
	manager.observeAccountResponse("client", []byte(`{"id":1,"error":{"message":"rejected"}}`))
	if err := manager.beginAccountManagement("login"); err != nil {
		t.Fatal(err)
	}
	if err := manager.reserveAccountRequest("client", request); err == nil {
		t.Fatal("turn overlapped login")
	}
	manager.endAccountManagement("login")
	if err := manager.reserveAccountRequest("client", request); err != nil {
		t.Fatal(err)
	}
	manager.observeAccountResponse("client", []byte(`{"id":1,"result":{"turn":{"id":"turn-one"}}}`))
	if err := manager.beginAccountManagement("login"); err == nil {
		t.Fatal("login overlapped running turn")
	}
	manager.observeAccountThread([]byte(`{"method":"turn/completed","params":{"threadId":"thread-one"}}`))
	if err := manager.beginAccountManagement("login"); err != nil {
		t.Fatal(err)
	}
}

func TestAccountEnvironmentNeverEvaluatesShellOrReportsValues(t *testing.T) {
	values, err := parseAccountEnvironment([]byte("export TOKEN='$(touch /tmp/never-execute)'\nTEXT=\"two\\nlines\"\n"))
	if err != nil || values["TOKEN"] != "$(touch /tmp/never-execute)" || values["TEXT"] != "two\nlines" {
		t.Fatalf("parse failed: %v", err)
	}
	_, err = parseAccountEnvironment([]byte("TOKEN=\"private-unclosed"))
	if err == nil || strings.Contains(err.Error(), "private-unclosed") {
		t.Fatal("invalid value was accepted or disclosed")
	}
}

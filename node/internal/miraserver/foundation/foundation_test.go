package foundation

import (
	"bytes"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestMigrationChecksumsMatchReleasedServer(t *testing.T) {
	if err := validateMigrations(); err != nil {
		t.Fatal(err)
	}
	if CurrentSchemaVersion() != 29 {
		t.Fatalf("CurrentSchemaVersion() = %d, want 29", CurrentSchemaVersion())
	}
	copyOfMigrations := Migrations()
	copyOfMigrations[0].Name = "changed"
	if Migrations()[0].Name != "event-log-and-node-registry" {
		t.Fatal("Migrations returned mutable package storage")
	}
}

func TestConfigCompatibility(t *testing.T) {
	config, err := ConfigFromLookup(func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	if config.ListenHost != DefaultListenHost || config.ListenPort != DefaultListenPort ||
		config.DatabaseURL != DefaultDatabaseURL || config.CodexStoreEndpoint != "http://127.0.0.1:8787" ||
		!config.SecureCookies || config.TrustProxyHeaders {
		t.Fatalf("unexpected defaults: %#v", config)
	}

	environment := map[string]string{
		"LISTEN_HOST": "::1", "LISTEN_PORT": "9000", "DATABASE_URL": "postgres:///test",
		"MIRA_CODEX_STORE_ENDPOINT": "https://mira.example/", "MIRA_SECURE_COOKIES": "false",
		"MIRA_TRUST_PROXY_HEADERS": "true",
	}
	config, err = ConfigFromLookup(func(name string) (string, bool) {
		value, ok := environment[name]
		return value, ok
	})
	if err != nil {
		t.Fatal(err)
	}
	if config.ListenHost != "::1" || config.ListenPort != 9000 || config.CodexStoreEndpoint != "https://mira.example" ||
		config.SecureCookies || !config.TrustProxyHeaders {
		t.Fatalf("unexpected configured values: %#v", config)
	}
}

func TestReleasedArgon2HashCompatibility(t *testing.T) {
	const encoded = "$argon2id$v=19$m=19456,t=2,p=1$AAECAwQFBgcICQoLDA0ODw$K2tptPGUEusIVh2REEmgLh2EVCW8PIKKc2Q8YLkd4pU"
	if !VerifyPassword("Ａdministrator-passphrase", encoded) {
		t.Fatal("Go verifier rejected node-argon2 password hash")
	}
	if !VerifyPassword("Administrator-passphrase", encoded) {
		t.Fatal("NFKC-equivalent password did not verify")
	}
	if VerifyPassword("wrong-password", encoded) {
		t.Fatal("incorrect password verified")
	}
	for _, malformed := range []string{"", "$argon2id$v=19$m=19456,t=2$bad$bad", "$argon2i$v=19$m=19456,t=2,p=1$bad$bad"} {
		if VerifyPassword("anything", malformed) {
			t.Fatalf("malformed hash verified: %q", malformed)
		}
	}
	generated, err := HashPassword("a sufficiently long password")
	if err != nil {
		t.Fatal(err)
	}
	if !VerifyPassword("a sufficiently long password", generated) {
		t.Fatal("generated password hash did not verify")
	}
}

func TestTokenCompatibility(t *testing.T) {
	secretBytes := make([]byte, 32)
	for index := range secretBytes {
		secretBytes[index] = byte(index)
	}
	secret := base64.RawURLEncoding.EncodeToString(secretBytes)
	hash, ok := NodeSecretHash(secret)
	if !ok || hash != "630dcd2966c4336691125448bbb25b4ff412a49c732db2c8abc1b8581bd710dd" {
		t.Fatalf("NodeSecretHash() = %q, %v", hash, ok)
	}
	token := "mira_node_123e4567-e89b-12d3-a456-426614174000_" + secret
	parsed, ok := ParseNodeToken(token)
	if !ok || parsed.CredentialID != "123e4567-e89b-12d3-a456-426614174000" || parsed.SecretHash != hash {
		t.Fatalf("ParseNodeToken() = %#v, %v", parsed, ok)
	}
	if _, ok := ParseNodeToken(token + "x"); ok {
		t.Fatal("invalid Node token parsed")
	}
	const session = "mas_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	if got := SessionCSRFToken(session); got != "mcsrf_8DfuWotolZuiNa5xi1eV_EGcp2Fdbptf-0gQfDNQY5Y" {
		t.Fatalf("SessionCSRFToken() = %q", got)
	}
}

func TestRequestAddressCompatibility(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "http://mira.test", nil)
	request.RemoteAddr = "172.18.0.1:42100"
	request.Header.Set("X-Forwarded-For", "198.51.100.12, 203.0.113.9")
	if got := RequestAddress(request, false); got != "172.18.0.1" {
		t.Fatalf("untrusted proxy address = %q", got)
	}
	if got := RequestAddress(request, true); got != "203.0.113.9" {
		t.Fatalf("trusted proxy address = %q", got)
	}
}

func TestJSONHelpers(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "http://mira.test", strings.NewReader(""))
	var value map[string]any
	if err := ReadJSON(request, &value, 64); err != nil || !reflect.DeepEqual(value, map[string]any{}) {
		t.Fatalf("ReadJSON(empty) = %#v, %v", value, err)
	}
	request = httptest.NewRequest(http.MethodPost, "http://mira.test", bytes.NewBufferString(`{"value":true}`))
	if err := ReadJSON(request, &value, 4); err == nil {
		t.Fatal("oversized body accepted")
	} else if typed, ok := err.(*HTTPError); !ok || typed.Status != http.StatusRequestEntityTooLarge || typed.Code != "body_too_large" {
		t.Fatalf("oversized body error = %#v", err)
	}

	recorder := httptest.NewRecorder()
	if err := WriteJSON(recorder, http.StatusCreated, map[string]bool{"ok": true}, http.Header{"X-Test": []string{"yes"}}); err != nil {
		t.Fatal(err)
	}
	if recorder.Code != http.StatusCreated || recorder.Body.String() != `{"ok":true}` ||
		recorder.Header().Get("Cache-Control") != "no-store" || recorder.Header().Get("X-Test") != "yes" {
		t.Fatalf("unexpected response: status=%d headers=%v body=%q", recorder.Code, recorder.Header(), recorder.Body.String())
	}
}

package accountegress

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestConfigureAcquireReleaseAndDelete(t *testing.T) {
	manager, id := testManager(t)
	spec := Spec{Type: "socks5", Server: "proxy.example", Port: 8022, Username: "user", Password: "secret", Profile: ProfileClaude}
	public, err := manager.Configure(id, spec)
	if err != nil {
		t.Fatal(err)
	}
	if public.HasPassword != true || public.Profile != ProfileClaude {
		t.Fatalf("unexpected public spec: %#v", public)
	}
	withoutPassword := spec
	withoutPassword.Password = ""
	if _, err := manager.Configure(id, withoutPassword); err != nil {
		t.Fatal(err)
	}
	root, _ := manager.accountRoot(id)
	info, err := os.Stat(filepath.Join(root, "config.json"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("private config permissions: %v %v", info, err)
	}
	stored, err := readStored(filepath.Join(root, "config.json"))
	if err != nil || stored.Spec.Password != "secret" {
		t.Fatalf("password was not preserved: %#v %v", stored, err)
	}
	env, release, err := manager.Acquire(id)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(env["HTTPS_PROXY"], "http://127.0.0.1:") || !strings.Contains(env["NO_PROXY"], ".woa.com") {
		t.Fatalf("unexpected environment: %#v", env)
	}
	status, err := manager.Status(id)
	if err != nil || status["running"] != true {
		t.Fatalf("status: %#v %v", status, err)
	}
	release()
	status, err = manager.Status(id)
	if err != nil || status["running"] != false {
		t.Fatalf("released status: %#v %v", status, err)
	}
	if err := manager.Delete(id); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Status(id); !os.IsNotExist(err) {
		t.Fatalf("deleted config remains: %v", err)
	}
}

func TestGeneratedConfigRoutesOnlyProfileAndFallsBackDirect(t *testing.T) {
	manager, id := testManager(t)
	spec := Spec{Type: "http", Server: "proxy.example", Port: 9000, Profile: ProfileMafia}
	if _, err := manager.Configure(id, spec); err != nil {
		t.Fatal(err)
	}
	root, _ := manager.accountRoot(id)
	data, err := os.ReadFile(filepath.Join(root, "sing-box.json"))
	if err != nil {
		t.Fatal(err)
	}
	var config map[string]any
	if err := json.Unmarshal(data, &config); err != nil {
		t.Fatal(err)
	}
	route := config["route"].(map[string]any)
	if route["final"] != "direct" {
		t.Fatalf("final route = %#v", route["final"])
	}
	rules := route["rules"].([]any)
	first := rules[0].(map[string]any)
	if first["domain"].([]any)[0] != "api.appintheloop.com" || first["outbound"] != "remote" {
		t.Fatalf("unexpected first rule: %#v", first)
	}
}

func TestValidation(t *testing.T) {
	base := Spec{Type: "socks5", Server: "proxy.example", Port: 1, Username: "u", Password: "p", Profile: ProfileClaude}
	if err := base.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*Spec){
		func(value *Spec) { value.Type = "socks" },
		func(value *Spec) { value.Port = 0 },
		func(value *Spec) { value.Username = "" },
		func(value *Spec) { value.Profile = "unknown" },
	} {
		value := base
		mutate(&value)
		if value.Validate() == nil {
			t.Fatalf("accepted %#v", value)
		}
	}
}

func testManager(t *testing.T) (*Manager, string) {
	t.Helper()
	script := filepath.Join(t.TempDir(), "sing-box")
	body := "#!/bin/sh\nexec \"$EGRESS_TEST_BINARY\" -test.run TestEgressHelper -- \"$@\"\n"
	if err := os.WriteFile(script, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MIRA_NODE_SING_BOX", script)
	t.Setenv("EGRESS_TEST_BINARY", os.Args[0])
	t.Setenv("GO_WANT_EGRESS_HELPER", "1")
	manager := New(t.TempDir())
	t.Cleanup(manager.Close)
	return manager, "00000000-0000-4000-8000-000000000001"
}

func TestEgressHelper(t *testing.T) {
	if os.Getenv("GO_WANT_EGRESS_HELPER") != "1" {
		return
	}
	configPath := ""
	for index, value := range os.Args {
		if value == "-c" && index+1 < len(os.Args) {
			configPath = os.Args[index+1]
		}
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		os.Exit(2)
	}
	var config struct {
		Inbounds []struct {
			ListenPort int `json:"listen_port"`
		} `json:"inbounds"`
	}
	if json.Unmarshal(data, &config) != nil || len(config.Inbounds) == 0 {
		os.Exit(2)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(config.Inbounds[0].ListenPort))
	if err != nil {
		os.Exit(2)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodConnect {
			writer.WriteHeader(http.StatusNotFound)
			return
		}
		upstream, err := net.Dial("tcp", request.Host)
		if err != nil {
			writer.WriteHeader(http.StatusBadGateway)
			return
		}
		defer upstream.Close()
		hijacker := writer.(http.Hijacker)
		client, buffered, err := hijacker.Hijack()
		if err != nil {
			return
		}
		defer client.Close()
		_, _ = buffered.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
		_ = buffered.Flush()
		done := make(chan struct{}, 1)
		go func() {
			_, _ = io.Copy(upstream, client)
			done <- struct{}{}
		}()
		_, _ = io.Copy(client, upstream)
		<-done
	})}
	_ = server.Serve(listener)
}

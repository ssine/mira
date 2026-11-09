package accountegress

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	if os.Getenv("GO_WANT_EGRESS_HELPER") == "1" && len(os.Args) > 1 && os.Args[1] == "run" {
		runEgressHelper()
		return
	}
	os.Exit(m.Run())
}

func TestAcquireSharesReadyGateway(t *testing.T) {
	manager, id := testManager(t)
	spec := Spec{Type: "http", Server: "proxy.example", Port: 9000, Profile: ProfileClaude}
	if _, err := manager.Configure(id, spec); err != nil {
		t.Fatal(err)
	}
	first, releaseFirst, err := manager.Acquire(id)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseFirst()
	started := time.Now()
	second, releaseSecond, err := manager.Acquire(id)
	if err != nil {
		t.Fatalf("reusing a healthy account gateway failed after %s: %v", time.Since(started), err)
	}
	defer releaseSecond()
	if second["HTTPS_PROXY"] != first["HTTPS_PROXY"] {
		t.Fatalf("acquisitions did not share a gateway: %v != %v", second, first)
	}
	status, err := manager.Status(id)
	if err != nil || status["refs"] != 2 || status["running"] != true {
		t.Fatalf("shared gateway status: %#v %v", status, err)
	}
	releaseSecond()
	releaseSecond() // Releasing one acquisition is idempotent.
	status, err = manager.Status(id)
	if err != nil || status["refs"] != 1 || status["running"] != true {
		t.Fatalf("releasing one caller stopped another caller's gateway: %#v %v", status, err)
	}
}

func TestConcurrentAcquireSharesGateway(t *testing.T) {
	manager, id := testManager(t)
	spec := Spec{Type: "http", Server: "proxy.example", Port: 9000, Profile: ProfileClaude}
	if _, err := manager.Configure(id, spec); err != nil {
		t.Fatal(err)
	}
	const callers = 8
	type result struct {
		env     map[string]string
		release func()
		err     error
	}
	results := make(chan result, callers)
	start := make(chan struct{})
	for range callers {
		go func() {
			<-start
			env, release, err := manager.Acquire(id)
			results <- result{env, release, err}
		}()
	}
	close(start)
	proxy := ""
	for range callers {
		value := <-results
		if value.err != nil {
			t.Errorf("concurrent acquire: %v", value.err)
			continue
		}
		defer value.release()
		if proxy == "" {
			proxy = value.env["HTTPS_PROXY"]
		} else if value.env["HTTPS_PROXY"] != proxy {
			t.Errorf("concurrent callers received different gateways")
		}
	}
	status, err := manager.Status(id)
	if err != nil || status["refs"] != callers || status["running"] != true {
		t.Fatalf("concurrent gateway status: %#v %v", status, err)
	}
}

func TestAcquireCanRetryAfterStartFailure(t *testing.T) {
	manager, id := testManager(t)
	spec := Spec{Type: "http", Server: "proxy.example", Port: 9000, Profile: ProfileClaude}
	binary := manager.binary
	manager.binary = filepath.Join(t.TempDir(), "missing-sing-box")
	if _, err := manager.Configure(id, spec); err == nil || !strings.Contains(err.Error(), "start account egress:") {
		t.Fatalf("missing executable error: %v", err)
	}
	status, err := manager.Status(id)
	if err != nil || status["refs"] != 0 || status["running"] != false {
		t.Fatalf("failed startup left an active gateway: %#v %v", status, err)
	}
	manager.binary = binary
	_, release, err := manager.Acquire(id)
	if err != nil {
		t.Fatalf("retry after restoring executable: %v", err)
	}
	defer release()
}

func TestConcurrentAcquireReceivesStartupFailure(t *testing.T) {
	manager, id := testManager(t)
	if _, err := manager.Configure(id, Spec{Type: "http", Server: "proxy.example", Port: 9000, Profile: ProfileClaude}); err != nil {
		t.Fatal(err)
	}
	probe := manager.checkHealth
	failure := errors.New("upstream health failed")
	gate := make(chan struct{})
	var unblock sync.Once
	defer unblock.Do(func() { close(gate) })
	manager.checkHealth = func(ctx context.Context, _ storedConfig) error {
		select {
		case <-gate:
			return failure
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	const callers = 8
	results := make(chan error, callers)
	for range callers {
		go func() {
			_, release, err := manager.Acquire(id)
			if release != nil {
				release()
			}
			results <- err
		}()
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		status, err := manager.Status(id)
		if err != nil {
			t.Fatal(err)
		}
		if status["refs"] == callers {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("callers did not all wait for startup: %#v", status)
		}
		time.Sleep(time.Millisecond)
	}
	unblock.Do(func() { close(gate) })
	for range callers {
		if err := <-results; !errors.Is(err, failure) {
			t.Errorf("startup failure was not shared: %v", err)
		}
	}
	manager.checkHealth = probe
	_, release, err := manager.Acquire(id)
	if err != nil {
		t.Fatalf("retry after failed health check: %v", err)
	}
	defer release()
}

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
	if err != nil || runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
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

func TestGeneratedOpenAIConfigRoutesChatGPTAndFallsBackDirect(t *testing.T) {
	manager, id := testManager(t)
	spec := Spec{Type: "http", Server: "proxy.example", Port: 9000, Profile: ProfileOpenAI}
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
	suffixes := first["domain_suffix"].([]any)
	if !containsString(suffixes, "openai.com") || !containsString(suffixes, "chatgpt.com") || first["outbound"] != "remote" {
		t.Fatalf("unexpected first rule: %#v", first)
	}
}

func containsString(values []any, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
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
	// Run a native helper on every platform, including Windows without a shell.
	t.Setenv("MIRA_NODE_SING_BOX", os.Args[0])
	t.Setenv("GO_WANT_EGRESS_HELPER", "1")
	manager := New(t.TempDir())
	manager.checkHealth = func(ctx context.Context, config storedConfig) error {
		// Exercise the real proxy health loop without depending on public APIs.
		return waitHealthy(ctx, config, &routeProfile{
			healthURL: "http://account-egress.test/health",
			expected:  map[int]bool{http.StatusNotFound: true},
		})
	}
	t.Cleanup(manager.Close)
	return manager, "00000000-0000-4000-8000-000000000001"
}

func runEgressHelper() {
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
		writer.WriteHeader(http.StatusNotFound)
	})}
	_ = server.Serve(listener)
}

// Package accountegress owns account-scoped local proxy gateways.
package accountegress

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	ProfileClaude = "claude"
	ProfileMafia  = "mafia"
)

var (
	accountIDPattern = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	usernamePattern  = regexp.MustCompile(`^[^\s:@]{1,256}$`)
)

// Spec is the secret account-local remote proxy configuration.
type Spec struct {
	Type     string `json:"type"`
	Server   string `json:"server"`
	Port     int    `json:"port"`
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
	Profile  string `json:"profile"`
}

// PublicSpec is safe to persist in Server metadata and render in the Web UI.
type PublicSpec struct {
	Type        string `json:"type"`
	Server      string `json:"server"`
	Port        int    `json:"port"`
	Username    string `json:"username,omitempty"`
	Profile     string `json:"profile"`
	HasPassword bool   `json:"hasPassword"`
}

func (spec Spec) Validate() error {
	if spec.Type != "socks5" && spec.Type != "http" {
		return errors.New("egress type must be socks5 or http")
	}
	if spec.Server == "" || len(spec.Server) > 253 || strings.ContainsAny(spec.Server, "\r\n\t /") {
		return errors.New("egress server is invalid")
	}
	if spec.Port < 1 || spec.Port > 65535 {
		return errors.New("egress port is invalid")
	}
	if spec.Username != "" && !usernamePattern.MatchString(spec.Username) {
		return errors.New("egress username is invalid")
	}
	if spec.Username == "" && spec.Password != "" {
		return errors.New("egress password requires a username")
	}
	if len(spec.Username) > 256 || len(spec.Password) > 1024 || strings.ContainsAny(spec.Password, "\r\n\x00") {
		return errors.New("egress credentials are invalid")
	}
	if profileFor(spec.Profile) == nil {
		return errors.New("unknown egress route profile")
	}
	return nil
}

func (spec Spec) Public() PublicSpec {
	return PublicSpec{
		Type: spec.Type, Server: spec.Server, Port: spec.Port,
		Username: spec.Username, Profile: spec.Profile, HasPassword: spec.Password != "",
	}
}

type routeProfile struct {
	exactDomains   []string
	domainSuffixes []string
	domainKeywords []string
	ipCIDRs        []string
	healthURL      string
	expected       map[int]bool
	noProxy        string
}

func profileFor(name string) *routeProfile {
	switch name {
	case ProfileClaude:
		return &routeProfile{
			exactDomains: []string{
				"anthropic-com.ghost.io",
				"anthropic.com.cdn.cloudflare.net",
				"servd-anthropic-website.b-cdn.net",
			},
			domainSuffixes: []string{
				"anthropic.com",
				"claude.ai",
				"claude.com",
				"claude.app",
				"clau.de",
				"claudeusercontent.com",
				"claudemcpcontent.com",
				"claudemcpclient.com",
				"challenges.cloudflare.com",
				"sentry.io",
				"statsigapi.net",
				"intercom.io",
				"intercomcdn.com",
			},
			domainKeywords: []string{"datadog", "sentry", "sift"},
			ipCIDRs: []string{
				"160.79.104.0/23",
				"153.61.224.0/22",
				"2607:6bc0::/48",
			},
			healthURL: "https://api.anthropic.com/",
			expected:  map[int]bool{http.StatusNotFound: true},
			noProxy:   defaultNoProxy,
		}
	case ProfileMafia:
		return &routeProfile{
			exactDomains: []string{"api.appintheloop.com"},
			healthURL:    "https://api.appintheloop.com/v1/models",
			expected:     map[int]bool{http.StatusOK: true, http.StatusUnauthorized: true, http.StatusForbidden: true},
			noProxy:      defaultNoProxy,
		}
	default:
		return nil
	}
}

const defaultNoProxy = "localhost,127.0.0.1,::1,mira.ssine.cc,direct.mira.ssine.cc,.woa.com,.oa.com,.local,.lan,.home.arpa,.ssine.cc,.ts.net,100.64.0.0/10,10.0.0.0/8,172.16.0.0/12,192.168.0.0/16,9.0.0.0/8,11.0.0.0/8"

type storedConfig struct {
	Spec       Spec `json:"spec"`
	ListenPort int  `json:"listenPort"`
}

type entry struct {
	config   storedConfig
	refs     int
	stopping bool
	lastErr  string
	ready    chan error
	done     chan struct{}
	cancel   context.CancelFunc
}

// Manager owns the local proxy process for every configured account.
type Manager struct {
	root, binary string
	mu           sync.Mutex
	entries      map[string]*entry
	closed       bool
	closeOnce    sync.Once
}

// New creates a manager rooted in one Node identity directory.
func New(identityDir string) *Manager {
	binary := os.Getenv("MIRA_NODE_SING_BOX")
	if binary == "" {
		binary = "sing-box"
	}
	return &Manager{root: filepath.Join(identityDir, "account-egress"), binary: binary, entries: map[string]*entry{}}
}

func (manager *Manager) accountRoot(id string) (string, error) {
	if !accountIDPattern.MatchString(id) {
		return "", errors.New("invalid account ID")
	}
	return filepath.Join(manager.root, id), nil
}

func readStored(path string) (storedConfig, error) {
	var value storedConfig
	data, err := os.ReadFile(path)
	if err != nil {
		return value, err
	}
	if len(data) > 32*1024 || json.Unmarshal(data, &value) != nil {
		return value, errors.New("invalid account egress config")
	}
	if err := value.Spec.Validate(); err != nil {
		return value, err
	}
	if value.ListenPort < 1 || value.ListenPort > 65535 {
		return value, errors.New("invalid account egress listener")
	}
	return value, nil
}

func (manager *Manager) loadLocked(id string) (*entry, error) {
	if value := manager.entries[id]; value != nil {
		return value, nil
	}
	root, err := manager.accountRoot(id)
	if err != nil {
		return nil, err
	}
	stored, err := readStored(filepath.Join(root, "config.json"))
	if err != nil {
		return nil, err
	}
	value := &entry{config: stored}
	manager.entries[id] = value
	return value, nil
}

// Configure atomically replaces an account's remote proxy and verifies it.
func (manager *Manager) Configure(id string, spec Spec) (PublicSpec, error) {
	if err := spec.Validate(); err != nil {
		return PublicSpec{}, err
	}
	manager.mu.Lock()
	if manager.closed {
		manager.mu.Unlock()
		return PublicSpec{}, errors.New("account egress manager is closed")
	}
	value := manager.entries[id]
	if value != nil && value.refs > 0 {
		manager.mu.Unlock()
		return PublicSpec{}, errors.New("account egress is in use")
	}
	root, err := manager.accountRoot(id)
	if err != nil {
		manager.mu.Unlock()
		return PublicSpec{}, err
	}
	manager.mu.Unlock()

	if err := os.MkdirAll(root, 0700); err != nil {
		return PublicSpec{}, err
	}
	if previous, err := readStored(filepath.Join(root, "config.json")); err == nil && spec.Password == "" && previous.Spec.Type == spec.Type && previous.Spec.Server == spec.Server && previous.Spec.Port == spec.Port && previous.Spec.Username == spec.Username {
		spec.Password = previous.Spec.Password
	}
	if spec.Username == "" && spec.Password != "" || spec.Username != "" && spec.Password == "" {
		return PublicSpec{}, errors.New("egress username and password must be supplied together")
	}
	port, err := freeLoopbackPort()
	if err != nil {
		return PublicSpec{}, err
	}
	stored := storedConfig{Spec: spec, ListenPort: port}
	encoded, _ := json.Marshal(stored)
	if err := writeAtomic(filepath.Join(root, "config.json"), encoded, 0600); err != nil {
		return PublicSpec{}, err
	}
	if err := writeAtomic(filepath.Join(root, "sing-box.json"), manager.singBoxConfig(stored), 0600); err != nil {
		return PublicSpec{}, err
	}
	environment := strings.Join(EnvList(manager.environment(valueFor(stored))), "\n") + "\n"
	if err := writeAtomic(filepath.Join(root, "environment"), []byte(environment), 0600); err != nil {
		return PublicSpec{}, err
	}

	manager.mu.Lock()
	if manager.entries[id] != nil {
		manager.entries[id].config = stored
	}
	manager.mu.Unlock()
	_, release, err := manager.Acquire(id)
	if err != nil {
		return PublicSpec{}, err
	}
	release()
	return spec.Public(), nil
}

func valueFor(value storedConfig) *entry { return &entry{config: value} }

// Delete removes one account's configuration and stops it when idle.
func (manager *Manager) Delete(id string) error {
	manager.mu.Lock()
	value := manager.entries[id]
	if value != nil && value.refs > 0 {
		manager.mu.Unlock()
		return errors.New("account egress is in use")
	}
	delete(manager.entries, id)
	manager.mu.Unlock()
	root, err := manager.accountRoot(id)
	if err != nil {
		return err
	}
	return os.RemoveAll(root)
}

// Acquire starts or references one account's local gateway.
func (manager *Manager) Acquire(id string) (map[string]string, func(), error) {
	manager.mu.Lock()
	if manager.closed {
		manager.mu.Unlock()
		return nil, nil, errors.New("account egress manager is closed")
	}
	root, err := manager.accountRoot(id)
	if err != nil {
		manager.mu.Unlock()
		return nil, nil, err
	}
	if _, err := os.Stat(filepath.Join(root, "config.json")); os.IsNotExist(err) {
		manager.mu.Unlock()
		return nil, func() {}, nil
	}
	value, err := manager.loadLocked(id)
	if err != nil {
		manager.mu.Unlock()
		return nil, nil, err
	}
	value.refs++
	if value.cancel == nil {
		if err := manager.startLocked(id, value); err != nil {
			value.refs--
			manager.mu.Unlock()
			return nil, nil, err
		}
	}
	ready := value.ready
	manager.mu.Unlock()
	if ready != nil {
		select {
		case err := <-ready:
			if err != nil {
				manager.Release(id)
				return nil, nil, err
			}
		case <-time.After(15 * time.Second):
			manager.Release(id)
			return nil, nil, errors.New("account egress did not become ready")
		}
	}
	manager.mu.Lock()
	value = manager.entries[id]
	if value == nil {
		manager.mu.Unlock()
		return nil, nil, errors.New("account egress disappeared")
	}
	env := manager.environment(value)
	manager.mu.Unlock()
	var once sync.Once
	return env, func() { once.Do(func() { manager.Release(id) }) }, nil
}

func (manager *Manager) startLocked(id string, value *entry) error {
	root, _ := manager.accountRoot(id)
	ctx, cancel := context.WithCancel(context.Background())
	value.cancel = cancel
	value.ready = make(chan error, 1)
	value.done = make(chan struct{})
	value.stopping = false
	value.lastErr = ""
	go manager.run(ctx, id, value, root)
	return nil
}

func (manager *Manager) run(ctx context.Context, id string, value *entry, root string) {
	defer close(value.done)
	for {
		manager.mu.Lock()
		if manager.closed || value.stopping || value.refs == 0 {
			value.cancel = nil
			manager.mu.Unlock()
			return
		}
		ready := value.ready
		manager.mu.Unlock()

		attemptCtx, attemptCancel := context.WithCancel(ctx)
		command := exec.CommandContext(attemptCtx, manager.binary, "run", "-c", filepath.Join(root, "sing-box.json"))
		command.WaitDelay = 2 * time.Second
		var stderr boundedBuffer
		command.Stderr = &stderr
		if err := command.Start(); err != nil {
			attemptCancel()
			manager.mu.Lock()
			value.lastErr = fmt.Sprintf("start account egress: %v", err)
			manager.mu.Unlock()
			deliver(ready, errors.New(value.lastErr))
			if !manager.retry(id, value, ctx) {
				return
			}
			continue
		}
		healthErr := manager.waitHealthy(ctx, value)
		if healthErr == nil {
			deliver(ready, nil)
		} else {
			deliver(ready, healthErr)
			attemptCancel()
		}
		_ = command.Wait()
		attemptCancel()
		manager.mu.Lock()
		if manager.entries[id] == value && value.refs > 0 && !value.stopping && !manager.closed {
			value.lastErr = healthErrString(healthErr, stderr.String())
			value.ready = make(chan error, 1)
		}
		manager.mu.Unlock()
		if !manager.retry(id, value, ctx) {
			manager.mu.Lock()
			if manager.entries[id] == value {
				value.cancel = nil
			}
			manager.mu.Unlock()
			return
		}
	}
}

func healthErrString(err error, stderr string) string {
	if err != nil {
		return err.Error()
	}
	line := strings.TrimSpace(stderr)
	if len(line) > 2048 {
		line = line[len(line)-2048:]
	}
	if line == "" {
		line = "account egress exited"
	}
	return line
}

func deliver(ch chan error, err error) {
	if ch == nil {
		return
	}
	select {
	case ch <- err:
	default:
	}
}

func (manager *Manager) retry(id string, value *entry, ctx context.Context) bool {
	manager.mu.Lock()
	ok := manager.entries[id] == value && value.refs > 0 && !value.stopping && !manager.closed
	manager.mu.Unlock()
	if !ok {
		return false
	}
	select {
	case <-ctx.Done():
		return false
	case <-time.After(3 * time.Second):
		return true
	}
}

func (manager *Manager) waitHealthy(ctx context.Context, value *entry) error {
	profile := profileFor(value.config.Spec.Profile)
	proxy, _ := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", value.config.ListenPort))
	client := &http.Client{
		Timeout: 3 * time.Second,
		Transport: &http.Transport{
			Proxy: http.ProxyURL(proxy),
		},
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
	}
	deadline := time.NewTimer(12 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	var last error
	for {
		request, _ := http.NewRequestWithContext(ctx, http.MethodGet, profile.healthURL, nil)
		response, err := client.Do(request)
		if err == nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
			response.Body.Close()
			if profile.expected[response.StatusCode] {
				return nil
			}
			last = fmt.Errorf("account egress health returned HTTP %d", response.StatusCode)
		} else {
			last = err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			if last == nil {
				last = errors.New("account egress health timed out")
			}
			return last
		case <-ticker.C:
		}
	}
}

// Release drops one reference and stops the gateway at zero.
func (manager *Manager) Release(id string) {
	manager.mu.Lock()
	value := manager.entries[id]
	if value == nil || value.refs == 0 {
		manager.mu.Unlock()
		return
	}
	value.refs--
	if value.refs > 0 {
		manager.mu.Unlock()
		return
	}
	value.stopping = true
	cancel := value.cancel
	done := value.done
	manager.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		select {
		case <-done:
		case <-time.After(3 * time.Second):
		}
	}
}

// Status reports public configuration and current process health.
func (manager *Manager) Status(id string) (map[string]any, error) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	value, err := manager.loadLocked(id)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"configured": true,
		"running":    value.cancel != nil && !value.stopping,
		"refs":       value.refs,
		"error":      value.lastErr,
		"spec":       value.config.Spec.Public(),
	}, nil
}

// Close stops every gateway.
func (manager *Manager) Close() {
	manager.closeOnce.Do(func() {
		manager.mu.Lock()
		manager.closed = true
		values := make([]*entry, 0, len(manager.entries))
		for _, value := range manager.entries {
			values = append(values, value)
			value.stopping = true
			if value.cancel != nil {
				value.cancel()
			}
		}
		manager.mu.Unlock()
		for _, value := range values {
			if value.done != nil {
				select {
				case <-value.done:
				case <-time.After(3 * time.Second):
				}
			}
		}
	})
}

func (manager *Manager) environment(value *entry) map[string]string {
	proxy := fmt.Sprintf("http://127.0.0.1:%d", value.config.ListenPort)
	profile := profileFor(value.config.Spec.Profile)
	return map[string]string{
		"HTTP_PROXY": proxy, "http_proxy": proxy,
		"HTTPS_PROXY": proxy, "https_proxy": proxy,
		"NO_PROXY": profile.noProxy, "no_proxy": profile.noProxy,
	}
}

func (manager *Manager) singBoxConfig(value storedConfig) []byte {
	profile := profileFor(value.Spec.Profile)
	outbound := map[string]any{
		"type": value.Spec.Type, "tag": "remote",
		"server": value.Spec.Server, "server_port": value.Spec.Port,
	}
	if value.Spec.Type == "socks5" {
		outbound["type"] = "socks"
	}
	if value.Spec.Username != "" {
		outbound["username"] = value.Spec.Username
		outbound["password"] = value.Spec.Password
	}
	rules := []any{}
	if len(profile.exactDomains) > 0 {
		rules = append(rules, map[string]any{"domain": profile.exactDomains, "outbound": "remote"})
	}
	if len(profile.domainSuffixes) > 0 {
		rules = append(rules, map[string]any{"domain_suffix": profile.domainSuffixes, "outbound": "remote"})
	}
	if len(profile.domainKeywords) > 0 {
		rules = append(rules, map[string]any{"domain_keyword": profile.domainKeywords, "outbound": "remote"})
	}
	if len(profile.ipCIDRs) > 0 {
		rules = append(rules, map[string]any{"ip_cidr": profile.ipCIDRs, "outbound": "remote"})
	}
	rules = append(rules, map[string]any{"network": "udp", "port": 123, "outbound": "remote"})
	config := map[string]any{
		"log": map[string]any{"level": "error", "timestamp": true},
		"inbounds": []any{map[string]any{
			"type": "mixed", "tag": "local", "listen": "127.0.0.1", "listen_port": value.ListenPort,
		}},
		"outbounds": []any{outbound, map[string]any{"type": "direct", "tag": "direct"}},
		"route":     map[string]any{"default_domain_resolver": "local", "rules": rules, "final": "direct"},
		"dns":       map[string]any{"servers": []any{map[string]any{"type": "local", "tag": "local"}}},
	}
	data, _ := json.MarshalIndent(config, "", "  ")
	return data
}

func freeLoopbackPort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port, nil
}

func writeAtomic(path string, data []byte, mode os.FileMode) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".tmp-")
	if err != nil {
		return err
	}
	name := file.Name()
	defer os.Remove(name)
	if err := file.Chmod(mode); err == nil {
		_, err = file.Write(data)
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(name, path)
}

type boundedBuffer struct {
	bytes.Buffer
}

func (buffer *boundedBuffer) Write(value []byte) (int, error) {
	_, _ = buffer.Buffer.Write(value)
	if buffer.Len() > 8192 {
		data := append([]byte(nil), buffer.Bytes()[buffer.Len()-8192:]...)
		buffer.Reset()
		_, _ = buffer.Buffer.Write(data)
	}
	return len(value), nil
}

// ParsePublic validates and redacts one Web request.
func ParsePublic(raw any) (*Spec, *PublicSpec, error) {
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil, nil, err
	}
	var spec Spec
	if json.Unmarshal(encoded, &spec) != nil {
		return nil, nil, errors.New("invalid account egress")
	}
	if err := spec.Validate(); err != nil {
		return nil, nil, err
	}
	public := spec.Public()
	return &spec, &public, nil
}

// EnvList converts an environment map into deterministic KEY=VALUE pairs.
func EnvList(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]string, 0, len(keys))
	for _, key := range keys {
		result = append(result, key+"="+values[key])
	}
	return result
}

// PortString is useful in tests and diagnostics.
func (value PublicSpec) PortString() string { return strconv.Itoa(value.Port) }

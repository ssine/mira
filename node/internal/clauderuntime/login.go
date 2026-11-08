package clauderuntime

import (
	"context"
	"crypto/rand"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
)

var oauthURLPattern = regexp.MustCompile(`https://claude\.com/cai/oauth/authorize\?[^\s\x1b\x07]+`)
var terminalEscapePattern = regexp.MustCompile(`\x1b(?:\][^\x07]*(?:\x07|\x1b\\)|\[[0-?]*[ -/]*[@-~])`)

type loginProcess struct {
	mu        sync.Mutex
	sessionID string
	accountID string
	status    string
	url       string
	output    string
	command   *exec.Cmd
	input     io.WriteCloser
	release   func()
	stop      func() bool
	stopOnce  sync.Once
	done      chan struct{}
	ready     chan struct{}
	readyOnce sync.Once
	timeout   *time.Timer
}

func (p *loginProcess) Write(data []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.output) < 128*1024 {
		remaining := 128*1024 - len(p.output)
		if len(data) > remaining {
			data = data[:remaining]
		}
		p.output += string(data)
	}
	if p.url == "" {
		if match := oauthURLPattern.FindString(p.output); match != "" {
			p.url = strings.TrimRight(match, "\r\n")
			if p.status == "starting" {
				p.status = "pending"
			}
			p.readyOnce.Do(func() { close(p.ready) })
		}
	}
	return len(data), nil
}

func (p *loginProcess) view() map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	result := map[string]any{"sessionId": p.sessionID, "status": p.status}
	if p.url != "" {
		result["verificationUrl"] = p.url
	}
	if p.status == "failed" {
		message := strings.TrimSpace(terminalEscapePattern.ReplaceAllString(p.output, ""))
		if len(message) > 4096 {
			message = message[len(message)-4096:]
		}
		if message != "" {
			result["error"] = message
		}
	}
	return result
}

func (p *loginProcess) setStatus(status string) {
	p.mu.Lock()
	if p.status != "cancelled" {
		p.status = status
	}
	p.mu.Unlock()
	p.readyOnce.Do(func() { close(p.ready) })
}

func (p *loginProcess) finish(waitErr error, credentialsValid bool) {
	if waitErr == nil && credentialsValid {
		p.setStatus("completed")
	} else {
		p.setStatus("failed")
	}
	if p.timeout != nil {
		p.timeout.Stop()
	}
}

func (p *loginProcess) cancel() {
	p.stopOnce.Do(func() {
		p.mu.Lock()
		if p.status != "completed" {
			p.status = "cancelled"
		}
		p.mu.Unlock()
		if p.input != nil {
			_ = p.input.Close()
		}
		if p.stop != nil {
			p.stop()
		}
	})
	p.readyOnce.Do(func() { close(p.ready) })
}

func (m *Manager) claudeBinary() (string, error) {
	if binary := os.Getenv("MIRA_NODE_CLAUDE_BINARY"); binary != "" {
		return binary, nil
	}
	name := "claude"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	path := filepath.Join(m.dir, "node_modules", "@anthropic-ai", "claude-agent-sdk-"+runtime.GOOS+"-"+runtime.GOARCH, name)
	if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
		return path, nil
	}
	return "", errors.New("Claude Code executable is unavailable; prepare the runtime first")
}

func (m *Manager) loginCall(params map[string]any) (any, error) {
	action, _ := params["action"].(string)
	switch action {
	case "account/login/start":
		return m.startLogin(params)
	case "account/login/status":
		return m.loginStatus(params)
	case "account/login/complete":
		return m.completeLogin(params)
	case "account/login/cancel":
		return m.cancelLogin(params)
	case "account/logout":
		return m.logoutAccount(params)
	default:
		return nil, errors.New("unknown Claude account login action")
	}
}

func (m *Manager) startLogin(params map[string]any) (any, error) {
	id, _ := params["nodeAccountId"].(string)
	email, _ := params["email"].(string)
	if !accountIDPattern.MatchString(id) {
		return nil, errors.New("Invalid Claude Node account ID")
	}
	if len(email) > 320 || strings.ContainsAny(email, "\r\n\x00") {
		return nil, errors.New("Invalid Claude account email")
	}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, errors.New("Claude manager is shutting down")
	}
	if m.state != "ready" {
		m.mu.Unlock()
		return nil, errors.New("Claude runtime is not ready")
	}
	if existing := m.logins[id]; existing != nil {
		value := existing.view()
		if value["status"] == "starting" || value["status"] == "pending" {
			m.mu.Unlock()
			return value, nil
		}
		delete(m.logins, id)
	}
	activeLogins := 0
	for _, login := range m.logins {
		if status := login.view()["status"]; status == "starting" || status == "pending" {
			activeLogins++
		}
	}
	if activeLogins >= 4 {
		m.mu.Unlock()
		return nil, errors.New("Too many Claude account logins are waiting")
	}
	for _, process := range m.processes {
		if process.accountID == id {
			m.mu.Unlock()
			return nil, errors.New("Claude account has an active turn; wait or interrupt it before logging in")
		}
	}
	account, err := m.readAccount(id)
	if err != nil {
		m.mu.Unlock()
		return nil, err
	}
	if account.AuthType != "subscription" {
		m.mu.Unlock()
		return nil, errors.New("Claude account does not use subscription authentication")
	}
	env, release, err := m.accountEnvironmentAcquired(id, true)
	if err != nil {
		m.mu.Unlock()
		return nil, err
	}
	binary, err := m.claudeBinary()
	if err != nil {
		release()
		m.mu.Unlock()
		return nil, err
	}
	command := exec.Command(binary, "auth", "login", "--claudeai")
	if email != "" {
		command.Args = append(command.Args, "--email", email)
	}
	command.Env = env
	command.Dir = m.dir
	configureCommand(command)
	input, err := command.StdinPipe()
	if err != nil {
		release()
		m.mu.Unlock()
		return nil, err
	}
	login := &loginProcess{
		sessionID: rand.Text(), accountID: id, status: "starting", command: command,
		input: input, release: release, done: make(chan struct{}), ready: make(chan struct{}),
	}
	command.Stdout = login
	command.Stderr = login
	command.WaitDelay = 2 * time.Second
	if err = command.Start(); err != nil {
		_ = input.Close()
		release()
		m.mu.Unlock()
		return nil, err
	}
	cleanup, err := guardProcessTree(command)
	if err != nil {
		killCommand(command)
		_ = input.Close()
		_ = command.Wait()
		release()
		m.mu.Unlock()
		return nil, err
	}
	var stopped bool
	login.stop = func() bool {
		login.stopOnce.Do(func() { stopped = cleanup() })
		return stopped
	}
	m.logins[id] = login
	login.timeout = time.AfterFunc(10*time.Minute, login.cancel)
	m.mu.Unlock()

	go func() {
		waitErr := command.Wait()
		stopped := login.stop()
		_ = input.Close()
		home, _ := m.accountHome(id)
		credentials := filepath.Join(home, ".credentials.json")
		valid := validOAuthCredentials(credentials)
		if valid {
			_ = secureCredentialFile(credentials)
		}
		if !stopped && waitErr != nil {
			login.setStatus("failed")
		}
		login.finish(waitErr, valid)
		release()
		close(login.done)
	}()

	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	select {
	case <-login.ready:
	case <-login.done:
	case <-timer.C:
		login.cancel()
		value := login.view()
		if value["error"] == nil {
			value["error"] = "Claude Code did not emit an OAuth URL"
		}
		return value, errors.New("Claude Code did not emit an OAuth URL")
	}
	return login.view(), nil
}

func (m *Manager) findLogin(params map[string]any) (*loginProcess, error) {
	id, _ := params["nodeAccountId"].(string)
	sessionID, _ := params["sessionId"].(string)
	m.mu.Lock()
	login := m.logins[id]
	m.mu.Unlock()
	if login == nil {
		return nil, errors.New("Claude login session not found")
	}
	if sessionID == "" || sessionID != login.sessionID {
		return nil, errors.New("Claude login session does not match")
	}
	return login, nil
}

func (m *Manager) loginStatus(params map[string]any) (any, error) {
	login, err := m.findLogin(params)
	if err != nil {
		return nil, err
	}
	return login.view(), nil
}

func (m *Manager) completeLogin(params map[string]any) (any, error) {
	login, err := m.findLogin(params)
	if err != nil {
		return nil, err
	}
	code, _ := params["code"].(string)
	code = strings.TrimSpace(code)
	if code == "" || len(code) > 4096 || strings.ContainsAny(code, "\r\n\x00") {
		return nil, errors.New("Invalid Claude authorization code")
	}
	value := login.view()
	if value["status"] != "pending" {
		return value, nil
	}
	written := make(chan error, 1)
	go func() {
		_, writeErr := login.input.Write([]byte(code + "\n"))
		written <- writeErr
	}()
	select {
	case err = <-written:
		if err != nil {
			return nil, err
		}
	case <-time.After(2 * time.Second):
		return nil, errors.New("Claude login input timed out")
	}
	timer := time.NewTimer(30 * time.Second)
	defer timer.Stop()
	select {
	case <-login.done:
		return login.view(), nil
	case <-timer.C:
		return login.view(), nil
	}
}

func (m *Manager) cancelLogin(params map[string]any) (any, error) {
	login, err := m.findLogin(params)
	if err != nil {
		return nil, err
	}
	login.cancel()
	select {
	case <-login.done:
	case <-time.After(5 * time.Second):
	}
	return login.view(), nil
}

func (m *Manager) logoutAccount(params map[string]any) (any, error) {
	id, _ := params["nodeAccountId"].(string)
	if !accountIDPattern.MatchString(id) {
		return nil, errors.New("Invalid Claude Node account ID")
	}
	m.mu.Lock()
	for _, process := range m.processes {
		if process.accountID == id {
			m.mu.Unlock()
			return nil, errors.New("Claude account has an active turn; wait or interrupt it before logging out")
		}
	}
	login := m.logins[id]
	m.mu.Unlock()
	if login != nil {
		login.cancel()
	}
	account, err := m.readAccount(id)
	if err != nil {
		return nil, err
	}
	if account.AuthType != "subscription" {
		return nil, errors.New("Claude account does not use subscription authentication")
	}
	home, _ := m.accountHome(id)
	credentials := filepath.Join(home, ".credentials.json")
	env, release, err := m.accountEnvironmentAcquired(id, true)
	if err != nil {
		return nil, err
	}
	defer release()
	binary, binaryErr := m.claudeBinary()
	if binaryErr == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		command := exec.CommandContext(ctx, binary, "auth", "logout")
		command.Env = env
		command.Dir = m.dir
		configureCommand(command)
		output := &boundedOutput{limit: 256 * 1024}
		command.Stdout = output
		command.Stderr = output
		command.Cancel = func() error { killCommand(command); return nil }
		command.WaitDelay = 2 * time.Second
		if err = command.Run(); err != nil {
			return nil, errors.New("Claude logout failed: " + strings.TrimSpace(terminalEscapePattern.ReplaceAllString(string(output.Bytes()), "")))
		}
	}
	if err = os.Remove(credentials); err != nil && !os.IsNotExist(err) {
		return nil, errors.New("Claude logout succeeded but credentials could not be removed")
	}
	return map[string]any{"configured": false, "authType": "subscription"}, nil
}

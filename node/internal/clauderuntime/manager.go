// Package clauderuntime owns optional, Node-local Claude SDK processes.
package clauderuntime

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/ssine/mira/node/internal/accountegress"
)

//go:embed assets/*.mjs assets/package*.json
var assets embed.FS

type process struct {
	accountID string
	command   *exec.Cmd
	input     io.WriteCloser
	done      chan struct{}
	write     sync.Mutex
	acks      ackReader
	stop      func() bool
	release   func()
}
type Manager struct {
	mu                          sync.Mutex
	root, dir, state, lastError string
	accountsRoot                string
	cacheRoot, cacheConfig      string
	egress                      *accountegress.Manager
	ownsEgress                  bool
	runtimeID                   string
	preparing                   bool
	closed                      bool
	unconfirmedExit             bool
	cancel                      context.CancelFunc
	processes                   map[string]*process
	logins                      map[string]*loginProcess
	// Completed IDs prevent an ambiguous control reply from rerunning tools.
	accepted  map[string]bool
	sealed    map[string]bool
	starting  map[string]int
	cacheMu   sync.Mutex
	closeOnce sync.Once
}

func New(identityDir string) *Manager {
	return NewWithEgress(identityDir, nil)
}

// NewWithEgress uses a shared account egress manager.
func NewWithEgress(identityDir string, egress *accountegress.Manager) *Manager {
	ownsEgress := egress == nil
	if egress == nil {
		egress = accountegress.New(identityDir)
	}
	m := &Manager{runtimeID: rand.Text(), root: filepath.Join(identityDir, "runtimes", "claude"), accountsRoot: filepath.Join(identityDir, "accounts"), state: "stopped", processes: map[string]*process{}, logins: map[string]*loginProcess{}, accepted: map[string]bool{}, sealed: map[string]bool{}, starting: map[string]int{}}
	m.egress = egress
	m.ownsEgress = ownsEgress
	m.cachePaths(identityDir)
	return m
}
func nodeBinary() string {
	if s := os.Getenv("MIRA_NODE_CLAUDE_NODE"); s != "" {
		return s
	}
	return "node"
}
func (m *Manager) prepare() {
	if m.preparing || m.state == "ready" || m.closed {
		return
	}
	m.preparing = true
	m.state = "preparing"
	m.lastError = ""
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	m.cancel = cancel
	go func() {
		defer cancel()
		dir, err := m.install(ctx)
		m.mu.Lock()
		defer m.mu.Unlock()
		m.preparing = false
		if m.closed {
			return
		}
		if err != nil {
			m.state = "error"
			m.lastError = err.Error()
		} else {
			m.dir = dir
			m.state = "ready"
		}
	}()
}
func (m *Manager) install(ctx context.Context) (string, error) {
	if runtime.GOOS == "android" {
		return "", errors.New("Claude runtime is unavailable on Android")
	}
	cmd := exec.CommandContext(ctx, nodeBinary(), "-e", "if(Number(process.versions.node.split('.')[0])<22)process.exit(1)")
	configureCommand(cmd)
	if err := cmd.Run(); err != nil {
		return "", errors.New("Claude requires Node.js 22 or newer on this execution Node (MIRA_NODE_CLAUDE_NODE may select it)")
	}
	names, _ := assets.ReadDir("assets")
	hash := sha256.New()
	for _, name := range names {
		data, _ := assets.ReadFile("assets/" + name.Name())
		hash.Write([]byte(name.Name()))
		hash.Write(data)
	}
	dir := filepath.Join(m.root, hex.EncodeToString(hash.Sum(nil))[:24])
	if _, err := os.Stat(filepath.Join(dir, "ready")); err == nil {
		return dir, nil
	}
	if err := os.MkdirAll(m.root, 0700); err != nil {
		return "", err
	}
	stage, err := os.MkdirTemp(m.root, ".prepare-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(stage)
	for _, name := range names {
		data, _ := assets.ReadFile("assets/" + name.Name())
		if err := os.WriteFile(filepath.Join(stage, name.Name()), data, 0600); err != nil {
			return "", err
		}
	}
	npmName := "npm"
	if runtime.GOOS == "windows" {
		npmName = "npm.cmd"
	}
	npmPath, err := exec.LookPath(npmName)
	if err != nil {
		return "", errors.New("Claude SDK preparation requires npm on this Node")
	}
	npmScript, err := filepath.EvalSymlinks(npmPath)
	if err != nil {
		return "", err
	}
	if runtime.GOOS == "windows" {
		npmScript = filepath.Join(filepath.Dir(npmPath), "node_modules", "npm", "bin", "npm-cli.js")
	}
	cmd = exec.CommandContext(ctx, nodeBinary(), npmScript, "ci", "--ignore-scripts", "--no-audit", "--no-fund")
	configureCommand(cmd)
	cmd.Cancel = func() error { killCommand(cmd); return nil }
	cmd.WaitDelay = 5 * time.Second

	cmd.Dir = stage
	// No provider config or Node credential is passed to package-manager arguments.
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("prepare pinned Claude SDK with npm ci: %w", err)
	}
	if err := os.WriteFile(filepath.Join(stage, "ready"), []byte("1\n"), 0600); err != nil {
		return "", err
	}
	if err := os.Rename(stage, dir); err != nil {
		return "", err
	}
	return dir, nil
}
func (m *Manager) Call(params map[string]any) (any, error) {
	if action, _ := params["action"].(string); strings.HasPrefix(action, "account/login/") || action == "account/logout" {
		return m.loginCall(params)
	}
	if params["action"] == "steer" {
		return m.steer(params)
	}
	if params["action"] == "cache-session" {
		return m.sessionCache(params)
	}
	if params["action"] == "interrupt" || params["action"] == "answer" {
		return m.writeControl(params)
	}
	if params["action"] == "title" {
		// A model request can take a minute; never hold the manager lock for it.
		m.mu.Lock()
		closed, ready := m.closed, m.state == "ready"
		m.mu.Unlock()
		if closed {
			return nil, errors.New("Claude manager is shutting down")
		}
		if !ready {
			return nil, errors.New("Claude runtime is not ready")
		}
		return m.oneShot(params)
	}
	action, _ := params["action"].(string)
	id, _ := params["turnId"].(string)
	accountID, _ := params["nodeAccountId"].(string)
	// Reconciliation probes share m.mu and have a short Server budget. Child
	// processes and account egress readiness therefore run outside the lock.
	switch action {
	case "describe":
		return m.describe(accountID)
	case "start":
		return m.start(params)
	case "cache-status", "cache-configure":
		m.mu.Lock()
		closed := m.closed
		m.mu.Unlock()
		if closed {
			return nil, errors.New("Claude manager is shutting down")
		}
		return m.cacheCall(action, params["maxBytes"])
	case "prepare", "status":
		if accountID != "" {
			if _, err := m.accountEnvironment(accountID); err != nil {
				return nil, err
			}
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, errors.New("Claude manager is shutting down")
	}
	switch action {
	case "reconcile":
		return m.reconcile(params)
	case "account/configure":
		return m.configureAccount(params)
	case "prepare":
		m.prepare()
		fallthrough
	case "status":
		return map[string]any{"runtimeId": m.runtimeID, "status": m.state, "error": m.lastError, "active": m.processes[id] != nil, "accepted": m.accepted[id]}, nil
	default:
		return nil, errors.New("unknown Claude runtime action")
	}
}

func (m *Manager) describe(accountID string) (any, error) {
	m.mu.Lock()
	closed, ready, dir := m.closed, m.state == "ready", m.dir
	m.mu.Unlock()
	if closed {
		return nil, errors.New("Claude manager is shutting down")
	}
	if !ready {
		return nil, errors.New("Claude runtime is not ready")
	}
	env, release, err := m.accountEnvironmentAcquired(accountID, false)
	if err != nil {
		return nil, err
	}
	defer release()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, nodeBinary(), filepath.Join(dir, "worker.mjs"))
	cmd.Env = env
	cmd.Dir = dir
	configureCommand(cmd)
	cmd.Stdin = bytes.NewReader([]byte("{\"action\":\"describe\"}\n"))
	cmd.Cancel = func() error { killCommand(cmd); return nil }
	cmd.WaitDelay = 5 * time.Second
	output := &boundedOutput{limit: 4 * 1024 * 1024}
	cmd.Stdout = output
	if err = cmd.Run(); err != nil {
		return nil, errors.New("Could not read Claude models or Node-local authentication")
	}
	var value any
	err = json.Unmarshal(output.Bytes(), &value)
	return value, err
}

// startRejectedLocked holds every fencing and admission check for a start. A
// start runs them before and again after its unlocked preparation, so a
// reconciliation that fenced the turn meanwhile still prevents the worker.
func (m *Manager) startRejectedLocked(params map[string]any, id string) (any, error) {
	if m.closed {
		return nil, errors.New("Claude manager is shutting down")
	}
	if params["runtimeId"] != m.runtimeID {
		return nil, errors.New("Claude runtime changed before start; execution state is unknown")
	}
	if m.sealed[id] {
		return nil, errors.New("Claude turn was fenced after its worker stopped")
	}
	if m.accepted[id] {
		return map[string]any{"accepted": true, "active": m.processes[id] != nil}, nil
	}
	if m.state != "ready" {
		return nil, errors.New("Claude runtime is not ready")
	}
	if len(m.processes) >= 8 {
		return nil, errors.New("Claude runtime has reached its 8 active turn limit")
	}
	if id == "" {
		return nil, errors.New("turnId is required")
	}
	return nil, nil
}

func (m *Manager) start(params map[string]any) (any, error) {
	id, _ := params["turnId"].(string)
	accountID, _ := params["nodeAccountId"].(string)
	m.mu.Lock()
	if result, err := m.startRejectedLocked(params, id); result != nil || err != nil {
		m.mu.Unlock()
		return result, err
	}
	// Account changes wait for this start as they wait for a running turn.
	m.starting[accountID]++
	m.mu.Unlock()
	// Cache failures never prevent native execution. Maintenance repairs locks
	// left by a crashed SDK worker before the next turn starts.
	_, _ = m.cacheCall("cache-status", nil)
	env, release, err := m.accountEnvironmentAcquired(accountID, false)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.starting[accountID]--; m.starting[accountID] <= 0 {
		delete(m.starting, accountID)
	}
	if err != nil {
		return nil, err
	}
	if result, err := m.startRejectedLocked(params, id); result != nil || err != nil {
		release()
		return result, err
	}
	params["cache"] = map[string]string{"root": m.cacheRoot, "config": m.cacheConfig}
	cmd := exec.Command(nodeBinary(), filepath.Join(m.dir, "worker.mjs"))
	cmd.Env = env
	cmd.Dir = m.dir
	configureCommand(cmd)
	input, err := cmd.StdinPipe()
	if err != nil {
		release()
		return nil, err
	}
	// Transcripts flow directly to Server; stdout carries only steer
	// acknowledgements and stderr is not an unbounded log.
	p := &process{accountID: accountID, command: cmd, input: input, done: make(chan struct{}), release: release}
	cmd.Stdout = &p.acks
	cmd.Stderr = io.Discard
	cmd.WaitDelay = 2 * time.Second
	if err = cmd.Start(); err != nil {
		input.Close()
		release()
		return nil, err
	}
	cleanup, err := guardProcessTree(cmd)
	if err != nil {
		killCommand(cmd)
		_ = input.Close()
		_ = cmd.Wait()
		release()
		return nil, err
	}
	var stopOnce sync.Once
	var stopped bool
	p.stop = func() bool { stopOnce.Do(func() { stopped = cleanup() }); return stopped }
	m.processes[id] = p
	m.accepted[id] = true
	if len(m.accepted) > 4096 {
		for key := range m.accepted {
			if m.processes[key] == nil && key != id {
				delete(m.accepted, key)
				break
			}
		}
	}
	encoded, _ := json.Marshal(params)
	// Startup stdin is bounded too: a wedged worker must not hold m.mu and
	// prevent shutdown or reconciliation. Failure retains the accepted ID.
	err = writeProcessControl(p, append(encoded, '\n'))
	if err != nil {
		p.stop()
	}
	go func() {
		_ = cmd.Wait()
		stopped := p.stop()
		_ = input.Close()
		m.mu.Lock()
		m.unconfirmedExit = m.unconfirmedExit || !stopped
		delete(m.processes, id)
		m.mu.Unlock()
		p.release()
		close(p.done)
	}()
	return map[string]any{"accepted": true}, err
}

// accountBusyLocked reports a running or starting turn for the account.
func (m *Manager) accountBusyLocked(id string) bool {
	if m.starting[id] > 0 {
		return true
	}
	for _, p := range m.processes {
		if p.accountID == id {
			return true
		}
	}
	return false
}

// oneShot runs a short-lived worker for a single native title request.
func (m *Manager) oneShot(params map[string]any) (any, error) {
	prompt, _ := params["prompt"].(string)
	model, _ := params["model"].(string)
	accountID, _ := params["nodeAccountId"].(string)
	if prompt == "" || len(prompt) > 64*1024 {
		return nil, errors.New("A title prompt of at most 64 KiB is required")
	}
	command, _ := json.Marshal(map[string]any{"action": "title", "prompt": prompt, "model": model})
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Second)
	defer cancel()
	m.mu.Lock()
	dir := m.dir
	env, release, err := m.accountEnvironmentAcquired(accountID, false)
	m.mu.Unlock()
	if err != nil {
		return nil, err
	}
	defer release()
	cmd := exec.CommandContext(ctx, nodeBinary(), filepath.Join(dir, "worker.mjs"))
	cmd.Env = env
	cmd.Dir = dir
	configureCommand(cmd)
	cmd.Stdin = bytes.NewReader(append(command, '\n'))
	cmd.Cancel = func() error { killCommand(cmd); return nil }
	cmd.WaitDelay = 5 * time.Second
	output := &boundedOutput{limit: 64 * 1024}
	cmd.Stdout = output
	if err = cmd.Run(); err != nil {
		return nil, errors.New("Claude could not generate a title")
	}
	var value map[string]any
	if err = json.Unmarshal(output.Bytes(), &value); err != nil {
		return nil, errors.New("Claude returned an invalid title response")
	}
	if text, _ := value["error"].(string); text != "" {
		return nil, errors.New("Claude title request failed: " + text)
	}
	return value, nil
}

func writeProcessControl(p *process, body []byte) error {
	written := make(chan error, 1)
	go func() {
		p.write.Lock()
		defer p.write.Unlock()
		_, err := p.input.Write(body)
		written <- err
	}()
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	select {
	case err := <-written:
		return err
	case <-timer.C:
		// Closing stdin also releases the blocked writer. Never resend the
		// command: a partial write has an ambiguous outcome.
		_ = p.input.Close()
		return errors.New("Claude worker control input timed out")
	}
}

func (m *Manager) writeControl(params map[string]any) (any, error) {
	id, _ := params["turnId"].(string)
	m.mu.Lock()
	p, closed := m.processes[id], m.closed
	m.mu.Unlock()
	if closed || p == nil {
		return nil, errors.New("Claude turn is no longer running on this Node")
	}
	body, _ := json.Marshal(params)
	err := writeProcessControl(p, append(body, '\n'))
	return map[string]any{"accepted": err == nil}, err
}

// steer queues a message into a running turn. It waits for the worker's
// verdict without holding the manager lock: a worker records the message
// before accepting it, which is a Server round trip.
func (m *Manager) steer(params map[string]any) (any, error) {
	id, _ := params["turnId"].(string)
	steerID, _ := params["steerId"].(string)
	if steerID == "" {
		return nil, errors.New("steerId is required")
	}
	m.mu.Lock()
	p := m.processes[id]
	closed := m.closed
	m.mu.Unlock()
	if closed {
		return nil, errors.New("Claude manager is shutting down")
	}
	if p == nil {
		return map[string]any{"accepted": false, "reason": "turn_finishing"}, nil
	}
	ack := p.acks.wait(steerID)
	body, _ := json.Marshal(params)
	err := writeProcessControl(p, append(body, '\n'))
	if err != nil {
		p.acks.cancel(steerID, ack)
		return nil, err // An ambiguous write must not cause a new-turn replay.
	}
	timeout := time.NewTimer(25 * time.Second)
	defer timeout.Stop()
	select {
	case result := <-ack:
		return result, nil
	case <-p.done:
		// Wait delivers all worker output before done closes.
		select {
		case result := <-ack:
			return result, nil
		default:
			return map[string]any{"accepted": false, "reason": "turn_finishing"}, nil
		}
	case <-timeout.C:
		p.acks.cancel(steerID, ack)
		return nil, errors.New("Claude 未及时确认这条消息，它仍可能加入本轮；重试不会重复发送")
	}
}

func (m *Manager) Close() {
	m.closeOnce.Do(m.close)
}

func (m *Manager) close() {
	m.mu.Lock()
	m.closed = true
	if m.cancel != nil {
		m.cancel()
	}
	ps := make([]*process, 0, len(m.processes))
	for _, p := range m.processes {
		ps = append(ps, p)
	}
	logins := make([]*loginProcess, 0, len(m.logins))
	for _, login := range m.logins {
		logins = append(logins, login)
	}
	m.mu.Unlock()
	for _, login := range logins {
		login.cancel()
	}
	for _, p := range ps {
		// A wedged stdin or a concurrent steer must not consume the Supervisor's
		// entire stop deadline before we can terminate and reap the worker.
		go func(p *process) {
			_ = writeProcessControl(p, []byte("{\"action\":\"interrupt\"}\n"))
		}(p)
	}
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	forced := false
	for _, p := range ps {
		select {
		case <-p.done:
		case <-deadline.C:
			forced = true
			for _, q := range ps {
				q.stop()
			}
		}
		if forced {
			break
		}
	}
	// Receipt creation requires an actual Wait acknowledgement for every worker.
	// A kill request alone (or a Node restart with an empty map) is not evidence.
	reap := time.NewTimer(3 * time.Second)
	defer reap.Stop()
	for _, p := range ps {
		select {
		case <-p.done:
		case <-reap.C:
			return
		}
	}
	m.mu.Lock()
	uncertain := m.unconfirmedExit
	m.mu.Unlock()
	if uncertain {
		return
	}
	if m.ownsEgress && m.egress != nil {
		m.egress.Close()
	}
	_ = m.recordClosedRuntime()
}

// Bounded metadata IPC; worker transcripts never use stdout.
type boundedOutput struct {
	buffer bytes.Buffer
	limit  int
}

func (b *boundedOutput) Write(p []byte) (int, error) {
	if len(p) > b.limit-b.buffer.Len() {
		return 0, io.ErrShortBuffer
	}
	return b.buffer.Write(p)
}

func (b *boundedOutput) Bytes() []byte { return b.buffer.Bytes() }

// ackReader receives a turn worker's stdout: one JSON acknowledgement per steer
// command, delivered to the oldest request waiting for that steer ID.
type ackReader struct {
	mu      sync.Mutex
	line    []byte
	waiters map[string][]chan map[string]any
}

func (a *ackReader) wait(id string) chan map[string]any {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.waiters == nil {
		a.waiters = map[string][]chan map[string]any{}
	}
	ch := make(chan map[string]any, 1)
	a.waiters[id] = append(a.waiters[id], ch)
	return ch
}

func (a *ackReader) cancel(id string, ch chan map[string]any) {
	a.mu.Lock()
	defer a.mu.Unlock()
	waiting := a.waiters[id]
	for i, candidate := range waiting {
		if candidate == ch {
			waiting = append(waiting[:i], waiting[i+1:]...)
			break
		}
	}
	if len(waiting) == 0 {
		delete(a.waiters, id)
	} else {
		a.waiters[id] = waiting
	}
}

// Write never fails, so a malformed or oversized line cannot block the worker.
func (a *ackReader) Write(p []byte) (int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, b := range p {
		if b != '\n' {
			if len(a.line) <= 64*1024 {
				a.line = append(a.line, b)
			}
			continue
		}
		var ack map[string]any
		if len(a.line) <= 64*1024 && json.Unmarshal(a.line, &ack) == nil {
			id, _ := ack["steerId"].(string)
			if waiting := a.waiters[id]; len(waiting) > 0 {
				waiting[0] <- ack
				if len(waiting) == 1 {
					delete(a.waiters, id)
				} else {
					a.waiters[id] = waiting[1:]
				}
			}
		}
		a.line = a.line[:0]
	}
	return len(p), nil
}

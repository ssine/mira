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
)

//go:embed assets/*.mjs assets/package*.json
var assets embed.FS

type process struct {
	command *exec.Cmd
	input   io.WriteCloser
	done    chan struct{}
	write   sync.Mutex
}
type Manager struct {
	mu                          sync.Mutex
	root, dir, state, lastError string
	runtimeID                   string
	preparing                   bool
	closed                      bool
	cancel                      context.CancelFunc
	processes                   map[string]*process
	// Completed IDs prevent an ambiguous control reply from rerunning tools.
	accepted map[string]bool
}

func New(identityDir string) *Manager {
	return &Manager{runtimeID: rand.Text(), root: filepath.Join(identityDir, "runtimes", "claude"), state: "stopped", processes: map[string]*process{}, accepted: map[string]bool{}}
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
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, errors.New("Claude manager is shutting down")
	}
	action, _ := params["action"].(string)
	id, _ := params["turnId"].(string)
	switch action {
	case "prepare":
		m.prepare()
		fallthrough
	case "status":
		return map[string]any{"runtimeId": m.runtimeID, "status": m.state, "error": m.lastError, "active": m.processes[id] != nil, "accepted": m.accepted[id]}, nil
	case "describe":
		if m.state != "ready" {
			return nil, errors.New("Claude runtime is not ready")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, nodeBinary(), filepath.Join(m.dir, "worker.mjs"))
		cmd.Dir = m.dir
		configureCommand(cmd)
		cmd.Stdin = strings.NewReader("{\"action\":\"describe\"}\n")
		cmd.Cancel = func() error { killCommand(cmd); return nil }
		cmd.WaitDelay = 5 * time.Second
		output := &boundedOutput{limit: 4 * 1024 * 1024}
		cmd.Stdout = output
		err := cmd.Run()
		if err != nil {
			return nil, errors.New("Could not read Claude models or Node-local authentication")
		}
		var value any
		err = json.Unmarshal(output.Bytes(), &value)
		return value, err
	case "start":
		if params["runtimeId"] != m.runtimeID {
			return nil, errors.New("Claude runtime changed before start; execution state is unknown")
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
		cmd := exec.Command(nodeBinary(), filepath.Join(m.dir, "worker.mjs"))
		cmd.Dir = m.dir
		configureCommand(cmd)
		input, err := cmd.StdinPipe()
		if err != nil {
			return nil, err
		}
		// Transcripts flow directly to Server; stdout/stderr are not unbounded logs.
		cmd.Stdout = io.Discard
		cmd.Stderr = io.Discard
		if err = cmd.Start(); err != nil {
			input.Close()
			return nil, err
		}
		p := &process{command: cmd, input: input, done: make(chan struct{})}
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
		if _, err = input.Write(append(encoded, '\n')); err != nil {
			killCommand(cmd)
		}
		go func() {
			_ = cmd.Wait()
			_ = input.Close()
			m.mu.Lock()
			delete(m.processes, id)
			m.mu.Unlock()
			close(p.done)
		}()
		return map[string]any{"accepted": true}, err
	case "interrupt", "answer":
		p := m.processes[id]
		if p == nil {
			return nil, errors.New("Claude turn is no longer running on this Node")
		}
		body, _ := json.Marshal(params)
		p.write.Lock()
		_, err := p.input.Write(append(body, '\n'))
		p.write.Unlock()
		return map[string]any{"accepted": err == nil}, err
	default:
		return nil, errors.New("unknown Claude runtime action")
	}
}
func (m *Manager) Close() {
	m.mu.Lock()
	m.closed = true
	if m.cancel != nil {
		m.cancel()
	}
	ps := make([]*process, 0, len(m.processes))
	for _, p := range m.processes {
		ps = append(ps, p)
	}
	m.mu.Unlock()
	for _, p := range ps {
		p.write.Lock()
		_, _ = p.input.Write([]byte("{\"action\":\"interrupt\"}\n"))
		p.write.Unlock()
	}
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	for _, p := range ps {
		select {
		case <-p.done:
		case <-deadline.C:
			for _, q := range ps {
				killCommand(q.command)
			}
			return
		}
	}
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

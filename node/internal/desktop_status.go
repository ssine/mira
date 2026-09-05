package node

import (
	"net/url"
	"sync"
)

// A process-local projection for the Windows tray. It contains no credentials
// and is never a replacement for Server state or durable conversation history.
type desktopStatus struct {
	mu               sync.Mutex
	phase            string
	verificationCode string
	client           *controlClient
	cached           desktopView
}

func (status *desktopStatus) update(phase, code string) {
	if status == nil {
		return
	}
	status.mu.Lock()
	defer status.mu.Unlock()
	status.phase, status.verificationCode = phase, code
}

func (status *desktopStatus) attach(client *controlClient) {
	status.mu.Lock()
	defer status.mu.Unlock()
	status.client = client
}

func (client *controlClient) updateDesktopRegistration() {
	phase, code := "reconnecting", ""
	if client.state != nil {
		switch client.state.Enrollment.Status {
		case "pending", "rejected", "expired":
			phase, code = client.state.Enrollment.Status, client.state.Enrollment.VerificationCode
		}
	}
	client.desktop.update(phase, code)
}

type desktopView struct {
	Phase, VerificationCode, Codex string
	Processes, Terminals, SSH      int
}

func (status *desktopStatus) snapshot() desktopView {
	status.mu.Lock()
	defer status.mu.Unlock()
	view := status.cached
	view.Phase, view.VerificationCode = status.phase, status.verificationCode
	return view
}

func (status *desktopStatus) sample() {
	view := status.view()
	status.mu.Lock()
	status.cached = view
	status.mu.Unlock()
}

func (status *desktopStatus) view() desktopView {
	status.mu.Lock()
	view := desktopView{Phase: status.phase, VerificationCode: status.verificationCode, Codex: "starting"}
	client := status.client
	status.mu.Unlock()
	if client == nil {
		return view
	}
	report := client.appServer.report()
	view.Codex, _ = report["status"].(string)
	if message, _ := report["lastError"].(string); message != "" {
		view.Codex = "error"
	}
	client.runtime.processMu.Lock()
	for _, process := range client.runtime.processes {
		process.mu.Lock()
		if process.running {
			view.Processes++
		}
		process.mu.Unlock()
	}
	client.runtime.processMu.Unlock()
	client.runtime.ptyMu.Lock()
	for _, terminal := range client.runtime.ptys {
		terminal.mu.Lock()
		if terminal.running {
			view.Terminals++
		}
		terminal.mu.Unlock()
	}
	client.runtime.ptyMu.Unlock()
	client.sshMu.Lock()
	view.SSH = len(client.sshWorkers)
	client.sshMu.Unlock()
	return view
}

func desktopServerURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return ""
	}
	parsed.User, parsed.RawQuery, parsed.Fragment = nil, "", ""
	return parsed.String()
}

package node

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"
)

type sshWorkerConfig struct {
	Token           string   `json:"token"`
	ClientPublicKey string   `json:"clientPublicKey"`
	Roots           []string `json:"roots"`
}

// One bounded, independently supervised worker per SSH connection. Private
// bootstrap data uses an anonymous pipe, never argv, logs or central storage.
func (client *controlClient) startSSH(ctx context.Context, message controlMessage) {
	client.sshMu.Lock()
	if len(client.sshWorkers) >= 8 || client.sshWorkers[message.SessionID] != nil {
		client.sshMu.Unlock()
		return
	}
	workerCtx, cancel := context.WithCancel(ctx)
	client.sshWorkers[message.SessionID] = cancel
	client.sshMu.Unlock()
	go func() {
		defer cancel()
		defer func() { client.sshMu.Lock(); delete(client.sshWorkers, message.SessionID); client.sshMu.Unlock() }()
		if err := client.runSSHWorker(workerCtx, message); err != nil && workerCtx.Err() == nil {
			Log("SSH worker stopped", map[string]any{"sessionId": message.SessionID, "error": err.Error()})
		}
	}()
}

func (client *controlClient) runSSHWorker(ctx context.Context, message controlMessage) error {
	conn, err := dialSSHTransport(ctx, client.configuration.ServerURL, client.token, message.SessionID, "target")
	if err != nil {
		return err
	}
	defer conn.Close()
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	// Cancellation closes the transport first, allowing the worker to reap its
	// children. CommandContext's default immediate Kill would orphan shells.
	command := exec.Command(executable, "--internal-ssh-worker")
	command.Stderr = os.Stderr
	command.WaitDelay = 2 * time.Second
	input, err := command.StdinPipe()
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := command.StdoutPipe()
	if err != nil {
		return err
	}
	defer output.Close()
	if err := command.Start(); err != nil {
		return fmt.Errorf("start SSH worker: %w", err)
	}
	cleanupTree, err := guardSSHProcessTree(command.Process)
	if err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		return err
	}
	defer cleanupTree()
	var once sync.Once
	stop := func() { once.Do(func() { conn.Close(); input.Close(); output.Close() }) }
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			stop()
		case <-done:
		}
	}()
	defer close(done)
	if err := json.NewEncoder(input).Encode(sshWorkerConfig{Token: client.token, ClientPublicKey: message.ClientPublicKey, Roots: client.runtime.roots}); err != nil {
		stop()
		_ = command.Process.Kill()
		_ = command.Wait()
		return err
	}
	go func() { _, _ = io.Copy(input, conn); stop() }()
	_, copyErr := io.Copy(conn, output)
	stop()
	// EOF tells a healthy worker to cancel its sessions. Bound a broken worker's shutdown.
	timer := time.AfterFunc(3*time.Second, func() { _ = command.Process.Kill() })
	defer timer.Stop()
	err = command.Wait()
	if err != nil {
		return err
	}
	return copyErr
}

func (client *controlClient) stopSSH(sessionID string) {
	client.sshMu.Lock()
	cancel := client.sshWorkers[sessionID]
	client.sshMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

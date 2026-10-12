package node

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type cliFileTestPeer struct {
	client  *cliClient
	dir     string
	chunked bool
	delay   time.Duration
	hook    func(fileParams, int, map[string]any) error
	mu      sync.Mutex
	calls   []fileParams
}

// Exercise the real CLI HTTP adapter against the actual bounded Node file
// implementation, including offset/overwrite and allowed-root behavior.
func newCLIFileTestPeer(t *testing.T) *cliFileTestPeer {
	t.Helper()
	peer := &cliFileTestPeer{dir: t.TempDir(), chunked: true}
	runtime, err := newCapabilityRuntime(config{AllowedRoots: []string{peer.dir}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(runtime.close)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-only-token" {
			http.Error(w, "missing authentication", http.StatusUnauthorized)
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == "/v1/nodes/resolve" {
			_ = json.NewEncoder(w).Encode(map[string]any{"node": map[string]any{
				"nodeId": "file-test-node", "capabilities": map[string]any{"files": true, "fileChunkedWrite": peer.chunked},
			}})
			return
		}
		if r.Method != http.MethodPost || r.URL.Path != "/v1/nodes/file-test-node/invoke" {
			http.NotFound(w, r)
			return
		}
		var request struct {
			Capability string     `json:"capability"`
			Params     fileParams `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.Capability != "file" {
			http.Error(w, "invalid invocation", http.StatusBadRequest)
			return
		}
		peer.mu.Lock()
		// Keep metadata without retaining the large base64 payloads in tests.
		metadata := request.Params
		metadata.Content = ""
		peer.calls = append(peer.calls, metadata)
		index := len(peer.calls)
		peer.mu.Unlock()
		if peer.delay > 0 {
			time.Sleep(peer.delay)
		}
		result, err := runtime.file(request.Params)
		if err == nil && peer.hook != nil {
			err = peer.hook(request.Params, index, result.(map[string]any))
		}
		if err != nil {
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": err.Error(), "code": "file_failed"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"result": result})
	}))
	t.Cleanup(server.Close)
	peer.client = &cliClient{
		identity: &persistedNodeState{ServerURL: server.URL, Token: "test-only-token"},
		options:  cliOptions{Timeout: 5 * time.Second}, http: &http.Client{Timeout: 5 * time.Second},
	}
	return peer
}

func (peer *cliFileTestPeer) callMetadata() []fileParams {
	peer.mu.Lock()
	defer peer.mu.Unlock()
	return append([]fileParams(nil), peer.calls...)
}

func fileTransferTestBytes(size int) []byte {
	data := make([]byte, size)
	for index := range data {
		data[index] = byte(index * 37)
	}
	return data
}

func TestCLIFileTransferLargeInputAndStdin(t *testing.T) {
	for _, useStdin := range []bool{false, true} {
		t.Run(fmt.Sprintf("stdin=%v", useStdin), func(t *testing.T) {
			peer := newCLIFileTestPeer(t)
			data := fileTransferTestBytes(2*maxFileBytes + 123)
			remote := filepath.Join(peer.dir, "remote.bin")
			args := []string{"write", "--node", "file-test-node", "--path", remote}
			if useStdin {
				args = append(args, "--stdin")
			} else {
				input := filepath.Join(t.TempDir(), "input.bin")
				if err := os.WriteFile(input, data, 0600); err != nil {
					t.Fatal(err)
				}
				args = append(args, "--input", input)
			}
			var progress bytes.Buffer
			result, err := peer.client.runFile(context.Background(), args, bytes.NewReader(data), &progress)
			if err != nil || result.(map[string]any)["bytesWritten"] != int64(len(data)) {
				t.Fatalf("upload: %v, %v", result, err)
			}
			if !strings.Contains(progress.String(), fmt.Sprintf("Uploaded %d bytes", len(data))) {
				t.Fatalf("missing progress: %q", progress.String())
			}
			got, err := os.ReadFile(remote)
			if err != nil || !bytes.Equal(got, data) {
				t.Fatal("remote file differs from input")
			}
			calls := peer.callMetadata()
			if len(calls) != 3 || calls[0].Append || !calls[1].Append || !calls[2].Append || calls[1].Offset != maxFileBytes || calls[2].Offset != 2*maxFileBytes {
				t.Fatalf("unexpected chunk metadata: %#v", calls)
			}
			output := filepath.Join(t.TempDir(), "download.bin")
			result, err = peer.client.runFile(context.Background(), []string{"read", "--node", "file-test-node", "--path", remote, "--output", output}, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			view := result.(map[string]any)
			if view["bytesRead"] != int64(len(data)) || view["content"] != nil || view["localPath"] != output || view["eof"] != true {
				t.Fatalf("unexpected download metadata: %#v", view)
			}
			got, err = os.ReadFile(output)
			if err != nil || !bytes.Equal(got, data) {
				t.Fatal("download differs from input")
			}
		})
	}
}

func TestCLIFileUploadBoundariesAndOverwrite(t *testing.T) {
	for _, size := range []int{0, 31, maxFileBytes, maxFileBytes + 1} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			peer := newCLIFileTestPeer(t)
			data := fileTransferTestBytes(size)
			remote := filepath.Join(peer.dir, "remote.bin")
			args := []string{"write", "--node", "file-test-node", "--path", remote, "--stdin"}
			if _, err := peer.client.runFile(context.Background(), args, bytes.NewReader(data), nil); err != nil {
				t.Fatal(err)
			}
			if _, err := peer.client.runFile(context.Background(), args, bytes.NewReader([]byte("replace")), nil); err == nil {
				t.Fatal("overwrote an existing file without --overwrite")
			}
			got, err := os.ReadFile(remote)
			if err != nil || !bytes.Equal(got, data) {
				t.Fatal("failed overwrite changed the destination")
			}
			if _, err := peer.client.runFile(context.Background(), append(args, "--overwrite"), bytes.NewReader([]byte("replace")), nil); err != nil {
				t.Fatal(err)
			}
			got, err = os.ReadFile(remote)
			if err != nil || string(got) != "replace" {
				t.Fatal("explicit overwrite did not replace the whole file")
			}
		})
	}
}

func TestCLIFileUploadOldNodeAndRootBoundary(t *testing.T) {
	peer := newCLIFileTestPeer(t)
	peer.chunked = false
	remote := filepath.Join(peer.dir, "existing.bin")
	if err := os.WriteFile(remote, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	args := []string{"write", "--node", "file-test-node", "--path", remote, "--stdin", "--overwrite"}
	_, err := peer.client.runFile(context.Background(), args, bytes.NewReader(fileTransferTestBytes(maxFileBytes+1)), nil)
	if err == nil || !strings.Contains(err.Error(), "fileChunkedWrite") || len(peer.callMetadata()) != 0 {
		t.Fatalf("old Node should reject before writing: %v", err)
	}
	got, _ := os.ReadFile(remote)
	if string(got) != "keep" {
		t.Fatal("old Node rejection changed an existing file")
	}
	if _, err := peer.client.runFile(context.Background(), args, bytes.NewReader([]byte("small")), nil); err != nil {
		t.Fatalf("small uploads must work on older Nodes: %v", err)
	}
	outside := filepath.Join(t.TempDir(), "outside.bin")
	args = []string{"write", "--node", "file-test-node", "--path", outside, "--stdin"}
	if _, err := peer.client.runFile(context.Background(), args, strings.NewReader("data"), nil); err == nil {
		t.Fatal("upload escaped configured roots")
	}
}

func TestCLIFileDownloadRanges(t *testing.T) {
	peer := newCLIFileTestPeer(t)
	data := fileTransferTestBytes(2*maxFileBytes + 17)
	remote := filepath.Join(peer.dir, "source.bin")
	if err := os.WriteFile(remote, data, 0600); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct{ offset, length int }{{7, maxFileBytes + 3}, {maxFileBytes, 0}, {len(data) + 5, 0}} {
		output := filepath.Join(t.TempDir(), "range.bin")
		args := []string{"read", "--node", "file-test-node", "--path", remote, "--output", output,
			"--offset", fmt.Sprint(test.offset), "--length", fmt.Sprint(test.length)}
		if _, err := peer.client.runFile(context.Background(), args, nil, nil); err != nil {
			t.Fatal(err)
		}
		start := min(test.offset, len(data))
		end := len(data)
		if test.length > 0 {
			end = min(start+test.length, end)
		}
		got, err := os.ReadFile(output)
		if err != nil || !bytes.Equal(got, data[start:end]) {
			t.Fatalf("download range %v differs", test)
		}
	}
	// JSON reads without --output still expose one bounded chunk for inspection.
	result, err := peer.client.runFile(context.Background(), []string{"read", "--node", "file-test-node", "--path", remote}, nil, nil)
	if err != nil || result.(map[string]any)["bytesRead"] != float64(maxFileBytes) {
		t.Fatalf("bounded read: %v", err)
	}
}

func TestCLIFileUploadNeverReplaysUnacknowledgedWrite(t *testing.T) {
	peer := newCLIFileTestPeer(t)
	peer.hook = func(params fileParams, index int, result map[string]any) error {
		if index == 2 {
			return fmt.Errorf("lost acknowledgement")
		}
		return nil
	}
	data := fileTransferTestBytes(2*maxFileBytes + 1)
	remote := filepath.Join(peer.dir, "partial.bin")
	_, err := peer.client.runFile(context.Background(), []string{"write", "--node", "file-test-node", "--path", remote, "--stdin"}, bytes.NewReader(data), nil)
	if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("%d confirmed bytes", maxFileBytes)) || len(peer.callMetadata()) != 2 {
		t.Fatalf("failed write was retried or concealed: %v", err)
	}
	got, _ := os.ReadFile(remote)
	if !bytes.Equal(got, data[:2*maxFileBytes]) {
		t.Fatal("an unacknowledged write was duplicated")
	}
}

func TestCLIFileDownloadFailureAndCancellationPreserveOutput(t *testing.T) {
	for _, failure := range []string{"response", "cancel", "size", "empty_chunk"} {
		t.Run(failure, func(t *testing.T) {
			peer := newCLIFileTestPeer(t)
			remote := filepath.Join(peer.dir, "source.bin")
			if err := os.WriteFile(remote, fileTransferTestBytes(maxFileBytes+1), 0600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			peer.hook = func(params fileParams, index int, result map[string]any) error {
				if failure == "cancel" {
					cancel()
				}
				if index == 2 {
					switch failure {
					case "response":
						return fmt.Errorf("connection lost")
					case "size":
						result["size"] = int64(maxFileBytes + 2)
					case "empty_chunk":
						result["content"], result["bytesRead"], result["eof"] = "", 0, false
					}
				}
				return nil
			}
			output := filepath.Join(t.TempDir(), "existing.bin")
			if err := os.WriteFile(output, []byte("keep"), 0600); err != nil {
				t.Fatal(err)
			}
			_, err := peer.client.runFile(ctx, []string{"read", "--node", "file-test-node", "--path", remote, "--output", output}, nil, nil)
			if err == nil {
				t.Fatal("download should fail")
			}
			got, _ := os.ReadFile(output)
			if string(got) != "keep" {
				t.Fatal("failed download replaced existing output")
			}
			partials, _ := filepath.Glob(filepath.Join(filepath.Dir(output), ".mira-download-*"))
			if len(partials) != 0 {
				t.Fatal("failed download retained temporary files")
			}
		})
	}
}

func TestCLIFileUploadCancellationAndWindowsSession(t *testing.T) {
	peer := newCLIFileTestPeer(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	peer.hook = func(params fileParams, index int, result map[string]any) error {
		result["executionContext"], result["userSessionId"] = "user", 7
		if index == 2 {
			cancel()
		}
		return nil
	}
	remote := filepath.Join(peer.dir, "partial.bin")
	_, err := peer.client.runFile(ctx, []string{"write", "--node", "file-test-node", "--path", remote, "--stdin", "--execution-context", "user"}, bytes.NewReader(fileTransferTestBytes(2*maxFileBytes+1)), nil)
	if err == nil {
		t.Fatal("cancelled upload succeeded")
	}
	calls := peer.callMetadata()
	if len(calls) != 2 || calls[1].ExecutionContext != "user" || calls[1].UserSessionID == nil || *calls[1].UserSessionID != 7 {
		t.Fatalf("upload did not retain its Windows session: %#v", calls)
	}
}

func TestCLIFileTransferTimeoutIsPerRequest(t *testing.T) {
	peer := newCLIFileTestPeer(t)
	peer.delay = 150 * time.Millisecond
	remote := filepath.Join(peer.dir, "source.bin")
	if err := os.WriteFile(remote, fileTransferTestBytes(2*maxFileBytes+1), 0600); err != nil {
		t.Fatal(err)
	}
	// Run the public CLI entrypoint to catch accidental whole-command deadlines.
	credentialID, token, err := newNodeCredential()
	if err != nil {
		t.Fatal(err)
	}
	identity := *peer.client.identity
	identity.Version, identity.NodeID, identity.NodeKey = 1, "file-test-node", "file-test-node"
	identity.CredentialID, identity.Token, identity.Enrollment.Status = credentialID, token, "approved"
	// Authenticate this test CLI through a transport wrapper, without relaxing
	// the peer's credential check or involving any real Node identity.
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		request, _ := http.NewRequestWithContext(r.Context(), r.Method, peer.client.identity.ServerURL+r.URL.RequestURI(), r.Body)
		request.Header.Set("Authorization", "Bearer test-only-token")
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer response.Body.Close()
		w.WriteHeader(response.StatusCode)
		_, _ = io.Copy(w, response.Body)
	}))
	defer proxy.Close()
	identity.ServerURL = proxy.URL
	identityPath := filepath.Join(t.TempDir(), "test-identity.json")
	encoded, _ := json.Marshal(identity)
	if err := os.WriteFile(identityPath, encoded, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MIRA_IDENTITY_FILE", identityPath)
	output := filepath.Join(t.TempDir(), "download.bin")
	var stdout, stderr bytes.Buffer
	start := time.Now()
	exit := RunCLI(context.Background(), []string{"file", "read", "--node", "file-test-node", "--path", remote, "--output", output, "--timeout", "400ms", "--json"}, nil, &stdout, &stderr)
	if exit != 0 || stderr.Len() != 0 || time.Since(start) <= 400*time.Millisecond {
		t.Fatalf("transfer timeout should apply per request: exit=%d stderr=%q duration=%v", exit, stderr.String(), time.Since(start))
	}
	var response map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatalf("invalid CLI JSON: %v", err)
	}
	if response["data"].(map[string]any)["bytesRead"] != float64(2*maxFileBytes+1) {
		t.Fatal("CLI reported a truncated download")
	}
}

func TestCLIFileUploadRejectsShortWriteAcknowledgement(t *testing.T) {
	peer := newCLIFileTestPeer(t)
	peer.hook = func(params fileParams, index int, result map[string]any) error {
		decoded, err := base64.StdEncoding.DecodeString(params.Content)
		if err != nil || len(decoded) > maxFileBytes {
			return fmt.Errorf("invalid or unbounded chunk")
		}
		result["bytesWritten"] = len(decoded) - 1
		return nil
	}
	_, err := peer.client.runFile(context.Background(), []string{"write", "--node", "file-test-node", "--path", filepath.Join(peer.dir, "file.bin"), "--stdin"}, strings.NewReader("abc"), nil)
	if err == nil || len(peer.callMetadata()) != 1 {
		t.Fatal("short write acknowledgement was accepted")
	}
}

func TestCLIFileUploadCancellationUnblocksInput(t *testing.T) {
	peer := newCLIFileTestPeer(t)
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := peer.client.runFile(ctx, []string{"write", "--node", "file-test-node", "--path", filepath.Join(peer.dir, "cancel.bin"), "--stdin"}, reader, nil)
		done <- err
	}()
	// Synchronize with the blocked input read, then cancel without closing the
	// producer. Cancellation must release the command and avoid remote writes.
	if _, err := writer.Write([]byte("prefix")); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case err := <-done:
		if err == nil || len(peer.callMetadata()) != 0 {
			t.Fatal("cancelled stdin upload wrote remote data")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancellation did not unblock stdin")
	}
}

func TestCLIFileUploadInputFailure(t *testing.T) {
	peer := newCLIFileTestPeer(t)
	reader, writer := io.Pipe()
	defer reader.Close()
	failure := errors.New("input failed")
	go func() {
		_, _ = writer.Write([]byte("prefix"))
		_ = writer.CloseWithError(failure)
	}()
	_, err := peer.client.runFile(context.Background(), []string{"write", "--node", "file-test-node", "--path", filepath.Join(peer.dir, "failed.bin"), "--stdin"}, reader, nil)
	if !errors.Is(err, failure) || len(peer.callMetadata()) != 0 {
		t.Fatalf("failed input should not publish a partial chunk: %v", err)
	}
}

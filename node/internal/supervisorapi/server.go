package supervisorapi

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

type Server struct {
	config Config
	paths  paths
	token  string
	url    string

	listener net.Listener
	http     *http.Server

	mu       sync.Mutex
	randomMu sync.Mutex
	status   *OperationStatus
	active   bool

	updates   sync.WaitGroup
	close     sync.Once
	closed    chan struct{}
	closeDone chan struct{}
	closeMu   sync.Mutex
	closeErr  error
}

func Start(configuration Config) (*Server, error) {
	if configuration.Manager == nil {
		return nil, fmt.Errorf("Supervisor API requires an update manager")
	}
	paths, err := controlPaths(configuration.StateDir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(paths.stateDir, 0700); err != nil {
		return nil, err
	}
	if configuration.Listen == nil {
		configuration.Listen = net.Listen
	}
	if configuration.Random == nil {
		configuration.Random = rand.Reader
	}
	if configuration.Now == nil {
		configuration.Now = time.Now
	}
	if configuration.ClassifyFailure == nil {
		configuration.ClassifyFailure = classifyFailure
	}
	tokenBytes := make([]byte, 32)
	if _, err := io.ReadFull(configuration.Random, tokenBytes); err != nil {
		return nil, fmt.Errorf("generate Supervisor API token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(tokenBytes)
	listener, err := configuration.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	address, ok := listener.Addr().(*net.TCPAddr)
	if !ok || address.IP == nil || !address.IP.IsLoopback() {
		listener.Close()
		return nil, fmt.Errorf("Supervisor API listener is not loopback-only")
	}
	endpoint := "http://" + address.String()
	if err := atomicWrite(paths.token, []byte(token+"\n")); err != nil {
		listener.Close()
		return nil, err
	}
	if err := atomicWrite(paths.endpoint, []byte(endpoint+"\n")); err != nil {
		listener.Close()
		_ = os.Remove(paths.token)
		return nil, err
	}
	server := &Server{
		config:    configuration,
		paths:     paths,
		token:     token,
		url:       endpoint,
		listener:  listener,
		closed:    make(chan struct{}),
		closeDone: make(chan struct{}),
	}
	if err := server.recoverInterruptedStatus(); err != nil {
		listener.Close()
		_ = os.Remove(paths.endpoint)
		_ = os.Remove(paths.token)
		return nil, err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/update", server.handleUpdate)
	mux.HandleFunc("GET /v1/update/status", server.handleStatus)
	server.http = &http.Server{
		Handler:           server.authenticate(mux),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       30 * time.Second,
		MaxHeaderBytes:    16 * 1024,
	}
	go func() {
		defer close(server.closed)
		_ = server.http.Serve(listener)
	}()
	return server, nil
}

func (server *Server) URL() string { return server.url }

func (server *Server) recoverInterruptedStatus() error {
	status, err := readStatus(server.paths.status)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if status.Phase == PhaseStaging || status.Phase == PhaseSwitching {
		now := server.config.Now().UTC()
		status.Phase = PhaseFailed
		status.Error = "Supervisor restarted before the update operation completed"
		status.UpdatedAt = now
		status.FinishedAt = &now
		if err := writeStatus(server.paths.status, status); err != nil {
			return err
		}
	}
	server.status = &status
	return nil
}

func (server *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Cache-Control", "no-store")
		host, _, err := net.SplitHostPort(request.RemoteAddr)
		if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
			http.Error(response, "loopback access required", http.StatusForbidden)
			return
		}
		provided := ""
		if authorization := request.Header.Get("Authorization"); strings.HasPrefix(authorization, "Bearer ") {
			provided = strings.TrimPrefix(authorization, "Bearer ")
		}
		expectedHash := sha256.Sum256([]byte(server.token))
		providedHash := sha256.Sum256([]byte(provided))
		if subtle.ConstantTimeCompare(expectedHash[:], providedHash[:]) != 1 {
			response.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(response, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(response, request)
	})
}

func (server *Server) handleUpdate(response http.ResponseWriter, request *http.Request) {
	defer request.Body.Close()
	var body struct {
		Version string `json:"version"`
	}
	decoder := json.NewDecoder(io.LimitReader(request.Body, 64*1024))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil || !validVersion(body.Version) {
		http.Error(response, "invalid update request", http.StatusBadRequest)
		return
	}
	if err := ensureJSONEOF(decoder); err != nil {
		http.Error(response, "invalid update request", http.StatusBadRequest)
		return
	}
	now := server.config.Now().UTC()
	operationID, err := server.operationID()
	if err != nil {
		http.Error(response, "could not create update operation", http.StatusInternalServerError)
		return
	}
	status := OperationStatus{
		OperationID: operationID,
		Phase:       PhaseStaging,
		Version:     body.Version,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	server.mu.Lock()
	if server.active {
		current := *server.status
		server.mu.Unlock()
		writeJSON(response, http.StatusConflict, current)
		return
	}
	if err := writeStatus(server.paths.status, status); err != nil {
		server.mu.Unlock()
		http.Error(response, "could not persist update operation", http.StatusInternalServerError)
		return
	}
	server.status = &status
	server.active = true
	server.updates.Add(1)
	server.mu.Unlock()

	// Deliberately do not derive this context from request.Context. Closing the
	// CLI, SSH transport, or HTTP connection cannot cancel an in-flight update.
	go server.runUpdate(context.Background(), status)
	writeJSON(response, http.StatusAccepted, status)
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); errors.Is(err, io.EOF) {
		return nil
	} else if err == nil {
		return fmt.Errorf("multiple JSON values")
	} else {
		return err
	}
}

func randomID(random io.Reader) (string, error) {
	value := make([]byte, 16)
	if _, err := io.ReadFull(random, value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}

func (server *Server) operationID() (string, error) {
	server.randomMu.Lock()
	defer server.randomMu.Unlock()
	return randomID(server.config.Random)
}

func (server *Server) runUpdate(ctx context.Context, status OperationStatus) {
	defer server.updates.Done()
	status.Phase = PhaseSwitching
	status.UpdatedAt = server.config.Now().UTC()
	if err := server.setStatus(status, true); err != nil {
		finished := server.config.Now().UTC()
		status.Phase = PhaseFailed
		status.Error = fmt.Sprintf("persist switching status: %v", err)
		status.UpdatedAt = finished
		status.FinishedAt = &finished
		_ = server.setStatus(status, false)
		return
	}
	_, err := server.config.Manager.ApplyUpdate(ctx, status.Version)
	finished := server.config.Now().UTC()
	status.UpdatedAt = finished
	status.FinishedAt = &finished
	if err == nil {
		status.Phase = PhaseSucceeded
		status.Error = ""
	} else {
		status.Phase = server.config.ClassifyFailure(err)
		if status.Phase != PhaseRolledBack && status.Phase != PhaseFailed {
			status.Phase = PhaseFailed
		}
		status.Error = boundedError(err)
	}
	_ = server.setStatus(status, false)
}

func boundedError(err error) string {
	const maximum = 16 * 1024
	message := err.Error()
	if len(message) <= maximum {
		return message
	}
	return message[:maximum] + "…"
}

func classifyFailure(err error) Phase {
	var rolledBack RolledBackError
	if errors.As(err, &rolledBack) && rolledBack.RollbackComplete() {
		return PhaseRolledBack
	}
	return PhaseFailed
}

func (server *Server) setStatus(status OperationStatus, active bool) error {
	server.mu.Lock()
	defer server.mu.Unlock()
	err := writeStatus(server.paths.status, status)
	if err != nil {
		// Keep the in-memory state useful even if the host filesystem fails. The
		// operation itself must continue so it can still roll back the service.
		if status.Error == "" {
			status.Error = fmt.Sprintf("persist update status: %v", err)
		} else {
			status.Error = errors.Join(errors.New(status.Error), fmt.Errorf("persist update status: %w", err)).Error()
		}
	}
	server.status = &status
	server.active = active
	return err
}

func (server *Server) handleStatus(response http.ResponseWriter, _ *http.Request) {
	server.mu.Lock()
	defer server.mu.Unlock()
	if server.status == nil {
		http.Error(response, "no update operation", http.StatusNotFound)
		return
	}
	writeJSON(response, http.StatusOK, *server.status)
}

func writeJSON(response http.ResponseWriter, statusCode int, value any) {
	response.Header().Set("Content-Type", "application/json")
	response.WriteHeader(statusCode)
	_ = json.NewEncoder(response).Encode(value)
}

// Close stops accepting local API calls and waits, within ctx, for an update to
// finish. It never cancels the update transaction.
func (server *Server) Close(ctx context.Context) error {
	server.close.Do(func() {
		go func() {
			shutdownContext, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			shutdownErr := server.http.Shutdown(shutdownContext)
			cancel()
			<-server.closed
			server.updates.Wait()
			removeEndpointErr := os.Remove(server.paths.endpoint)
			if errors.Is(removeEndpointErr, os.ErrNotExist) {
				removeEndpointErr = nil
			}
			removeTokenErr := os.Remove(server.paths.token)
			if errors.Is(removeTokenErr, os.ErrNotExist) {
				removeTokenErr = nil
			}
			server.closeMu.Lock()
			server.closeErr = errors.Join(shutdownErr, removeEndpointErr, removeTokenErr)
			server.closeMu.Unlock()
			close(server.closeDone)
		}()
	})
	select {
	case <-server.closeDone:
		server.closeMu.Lock()
		defer server.closeMu.Unlock()
		return server.closeErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

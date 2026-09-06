// Package channel implements Mira Server's outbound Node WebSocket broker,
// App Server proxy, dynamic capabilities, account tunnel, and SSH byte relay.
package channel

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5"
	"github.com/ssine/mira/node/internal/miraserver/foundation"
	"github.com/ssine/mira/node/internal/miraserver/nodes"
)

const (
	MaxChannelPayload               = 16 * 1024 * 1024
	MaxDeveloperInstructionsFile    = 256 * 1024
	DefaultCapabilityTimeout        = 30 * time.Second
	MaximumCapabilityTimeout        = 120 * time.Second
	DefaultDetachedThreadStartGrace = 30 * time.Second
)

type Database interface {
	foundation.DBTX
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

type NodeRegistry interface {
	Get(context.Context, string, bool) (*nodes.Node, error)
	List(context.Context, bool) ([]nodes.Node, error)
	Resolve(context.Context, string, bool) (nodes.Result, error)
	SetChannelStatus(context.Context, string, any) error
}

type Authenticator interface {
	Authenticate(context.Context, *http.Request, string) (*foundation.Principal, error)
	AuthenticateNodeToken(context.Context, string, string) (*foundation.Principal, error)
	Permits(*foundation.Principal, string) bool
}

type AuditFunc func(context.Context, foundation.AuditEvent) error

type Options struct {
	Database                 Database
	Nodes                    NodeRegistry
	Auth                     Authenticator
	Audit                    AuditFunc
	TrustProxyHeaders        bool
	Logger                   *slog.Logger
	CapabilityTimeout        time.Duration
	DetachedThreadStartGrace time.Duration
	AccountTimeout           time.Duration
	SSHMaxSessions           int
	SSHMaxSessionsPerNode    int
}

type Error struct {
	Status  int
	Code    string
	Message string
}

func (err *Error) Error() string { return err.Message }

func channelError(message string, status int, code string) *Error {
	return &Error{Status: status, Code: code, Message: message}
}

type Result struct {
	Status int
	Body   any
}

type socket struct {
	connection *websocket.Conn
	writeMu    sync.Mutex
	closeOnce  sync.Once
}

func newSocket(connection *websocket.Conn, limit int64) *socket {
	connection.SetReadLimit(limit)
	return &socket{connection: connection}
}

func (socket *socket) writeJSON(value any) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return socket.write(websocket.TextMessage, payload)
}

func (socket *socket) writeText(payload []byte) error {
	return socket.write(websocket.TextMessage, payload)
}

func (socket *socket) write(messageType int, payload []byte) error {
	socket.writeMu.Lock()
	defer socket.writeMu.Unlock()
	if err := socket.connection.SetWriteDeadline(time.Now().Add(30 * time.Second)); err != nil {
		return err
	}
	return socket.connection.WriteMessage(messageType, payload)
}

func (socket *socket) close(code int, reason string) {
	socket.closeOnce.Do(func() {
		socket.writeMu.Lock()
		defer socket.writeMu.Unlock()
		_ = socket.connection.SetWriteDeadline(time.Now().Add(5 * time.Second))
		_ = socket.connection.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, reason), time.Now().Add(5*time.Second))
		_ = socket.connection.Close()
	})
}

func decodeObject(payload []byte) (map[string]any, error) {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	var value map[string]any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, fmt.Errorf("multiple JSON values")
		}
		return nil, err
	}
	if value == nil {
		return map[string]any{}, nil
	}
	return value, nil
}

func randomUUID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(raw[:])
	return fmt.Sprintf("%s-%s-%s-%s-%s", encoded[:8], encoded[8:12], encoded[12:16], encoded[16:20], encoded[20:]), nil
}

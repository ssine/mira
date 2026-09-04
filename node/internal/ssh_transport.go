package node

import (
	"context"
	"crypto/ed25519"
	"crypto/hkdf"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"golang.org/x/crypto/ssh"
)

const sshFrameLimit = 64 * 1024

// Purpose-separated keys belong to the existing protected Node credential.
// Derivation avoids another identity file, races between CLI/Node startup, and
// accidentally losing host keys during APK privilege-mode switches or updates.
func nodeSSHSigner(token, role string) (ssh.Signer, error) {
	key, err := nodeSSHPrivateKey(token, role)
	if err != nil {
		return nil, err
	}
	return ssh.NewSignerFromKey(key)
}

func nodeSSHPrivateKey(token, role string) (ed25519.PrivateKey, error) {
	match := nodeTokenPattern.FindStringSubmatch(token)
	if match == nil || (role != "host" && role != "client") {
		return nil, fmt.Errorf("invalid SSH identity")
	}
	secret, err := base64.RawURLEncoding.DecodeString(match[2])
	if err != nil {
		return nil, err
	}
	seed, err := hkdf.Key(sha256.New, secret, []byte(match[1]), "mira/ssh/v1/"+role, ed25519.SeedSize)
	if err != nil {
		return nil, err
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

func sshPublicKeys(token string) (map[string]string, error) {
	host, err := nodeSSHSigner(token, "host")
	if err != nil {
		return nil, err
	}
	client, err := nodeSSHSigner(token, "client")
	if err != nil {
		return nil, err
	}
	return map[string]string{"hostKey": strings.TrimSpace(string(ssh.MarshalAuthorizedKey(host.PublicKey()))),
		"clientKey": strings.TrimSpace(string(ssh.MarshalAuthorizedKey(client.PublicKey())))}, nil
}

// A WebSocket is only a framed byte transport. SSH channels retain their own
// stdout/stderr, EOF, exit status and flow control end-to-end.
type sshWebSocketConn struct {
	*websocket.Conn
	reader  io.Reader
	writeMu sync.Mutex
}

func dialSSHTransport(ctx context.Context, server, token, sessionID, side string) (*sshWebSocketConn, error) {
	u, err := url.Parse(server)
	if err != nil {
		return nil, err
	}
	if u.Scheme == "https" {
		u.Scheme = "wss"
	} else if u.Scheme == "http" {
		u.Scheme = "ws"
	} else {
		return nil, fmt.Errorf("invalid Server URL")
	}
	u.Path, u.RawQuery, u.Fragment = "/v1/ssh/sessions/"+sessionID+"/"+side, "", ""
	dialer := websocket.Dialer{HandshakeTimeout: 15 * time.Second, ReadBufferSize: sshFrameLimit, WriteBufferSize: sshFrameLimit,
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: platformCertificatePool(), NextProtos: []string{"http/1.1"}},
		Subprotocols:    []string{"mira-ssh-v1", "auth." + base64.RawURLEncoding.EncodeToString([]byte(token))}}
	ws, response, err := dialer.DialContext(ctx, u.String(), nil)
	if err != nil {
		if response != nil {
			return nil, fmt.Errorf("SSH transport rejected (HTTP %d)", response.StatusCode)
		}
		return nil, err
	}
	if ws.Subprotocol() != "mira-ssh-v1" {
		ws.Close()
		return nil, fmt.Errorf("SSH transport protocol mismatch")
	}
	ws.SetReadLimit(sshFrameLimit)
	return &sshWebSocketConn{Conn: ws}, nil
}

func (conn *sshWebSocketConn) Read(p []byte) (int, error) {
	for {
		if conn.reader != nil {
			n, err := conn.reader.Read(p)
			if err != io.EOF {
				return n, err
			}
			conn.reader = nil
			if n > 0 {
				return n, nil
			}
		}
		kind, reader, err := conn.NextReader()
		if err != nil {
			return 0, err
		}
		if kind != websocket.BinaryMessage {
			return 0, fmt.Errorf("SSH transport requires binary frames")
		}
		conn.reader = reader
	}
}
func (conn *sshWebSocketConn) Write(p []byte) (int, error) {
	conn.writeMu.Lock()
	defer conn.writeMu.Unlock()
	written := 0
	for len(p) > 0 {
		n := min(len(p), sshFrameLimit)
		if err := conn.WriteMessage(websocket.BinaryMessage, p[:n]); err != nil {
			return written, err
		}
		written += n
		p = p[n:]
	}
	return written, nil
}
func (conn *sshWebSocketConn) LocalAddr() net.Addr  { return conn.UnderlyingConn().LocalAddr() }
func (conn *sshWebSocketConn) RemoteAddr() net.Addr { return conn.UnderlyingConn().RemoteAddr() }
func (conn *sshWebSocketConn) SetDeadline(t time.Time) error {
	if err := conn.SetReadDeadline(t); err != nil {
		return err
	}
	return conn.SetWriteDeadline(t)
}

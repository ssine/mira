package node

import (
	"context"
	"crypto/tls"
	"fmt"
	"golang.org/x/crypto/ssh"
	"io"
	"net/http"
	"strings"
)

func (client *cliClient) sshTransport(ctx context.Context, selector string) (*sshWebSocketConn, ssh.PublicKey, string, error) {
	node, err := client.resolveNode(ctx, selector)
	if err != nil {
		return nil, nil, "", err
	}
	keys, err := sshPublicKeys(client.identity.Token)
	if err != nil {
		return nil, nil, "", err
	}
	if err := client.request(ctx, http.MethodPost, "/v1/nodes/"+client.identity.NodeID+"/ssh/keys", keys, nil); err != nil {
		return nil, nil, "", err
	}
	var session struct {
		SessionID       string `json:"sessionId"`
		HostKey         string `json:"hostKey"`
		Username        string `json:"username"`
		ProtocolVersion int    `json:"protocolVersion"`
	}
	if err := client.request(ctx, http.MethodPost, "/v1/nodes/"+selectorValue(node, "nodeId")+"/ssh/sessions", nil, &session); err != nil {
		return nil, nil, "", err
	}
	host, _, _, rest, err := ssh.ParseAuthorizedKey([]byte(session.HostKey))
	if err != nil || len(rest) != 0 || session.ProtocolVersion != 1 || host.Type() != ssh.KeyAlgoED25519 {
		return nil, nil, "", fmt.Errorf("Server returned invalid SSH session identity")
	}
	if session.Username == "" || len(session.Username) > 256 || strings.ContainsAny(session.Username, "\x00\r\n") {
		return nil, nil, "", fmt.Errorf("Server returned invalid SSH username")
	}
	conn, err := dialSSHTransport(ctx, client.identity.ServerURL, client.identity.Token, session.SessionID, "source")
	return conn, host, session.Username, err
}

func (client *cliClient) configureSSHHTTP() {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: platformCertificatePool()}
	client.http.Transport = transport
}

func (client *cliClient) runSSHCommands(ctx context.Context, command string, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	client.configureSSHHTTP()
	return client.runOpenSSHClient(ctx, command, args, stdin, stdout, stderr)
}

func splitRemoteOperand(value string) (string, string, bool) {
	// Double-colon supports full node keys; Windows drive paths remain local.
	if index := strings.Index(value, "::"); index > 0 {
		return value[:index], value[index+2:], true
	}
	index := strings.IndexByte(value, ':')
	if index <= 0 || (index == 1 && len(value) > 2 && (value[2] == '/' || value[2] == '\\')) {
		return "", value, false
	}
	return value[:index], value[index+1:], true
}

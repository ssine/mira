package channel

import (
	"net/http"
	"strings"

	"github.com/ssine/mira/node/internal/miraserver/foundation"
	"github.com/ssine/mira/node/internal/transport"
)

func (channel *Channel) authorizeTransport(r *http.Request) (string, int) {
	var principal *foundation.Principal
	var err error
	if token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		principal, err = channel.auth.AuthenticateNodeToken(r.Context(), token, "node")
	} else {
		principal, err = channel.auth.Authenticate(r.Context(), r, "https-transport")
	}
	if err != nil || principal == nil {
		return "", 401
	}
	if principal.Revoked || !channel.auth.Permits(principal, "trusted") {
		return "", 403
	}
	if principal.Kind == "admin" {
		if r.Method != "GET" {
			valid, ok := channel.auth.(interface {
				ValidCSRF(*http.Request, *foundation.Principal) bool
			})
			if !ok || !valid.ValidCSRF(r, principal) || !sameBrowserOrigin(r, channel.trustProxy) {
				return "", 403
			}
		}
		return "admin:" + principal.SessionID, 0
	}
	return "node:" + principal.CredentialID, 0
}
func (channel *Channel) upgradeTransport(w http.ResponseWriter, r *http.Request, protocol string) (transport.Conn, error) {
	if r.URL.Query().Get("transport") == "https" {
		return channel.transports.Upgrade(w, r, protocol)
	}
	return channel.upgrader.Upgrade(w, r, http.Header{"Sec-WebSocket-Protocol": []string{protocol}})
}
func (relay *SSHRelay) upgradeTransport(w http.ResponseWriter, r *http.Request) (transport.Conn, error) {
	if r.URL.Query().Get("transport") == "https" {
		return relay.transports.Upgrade(w, r, "mira-ssh-v1")
	}
	return relay.upgrader.Upgrade(w, r, http.Header{"Sec-WebSocket-Protocol": []string{"mira-ssh-v1"}})
}

package miraserver

import (
	"context"
	"crypto/ecdh"
	"encoding/base64"
	"net"
	"net/http"
	"net/url"
	"strings"

	webpush "github.com/SherClockHolmes/webpush-go"
	"github.com/jackc/pgx/v5"
	"github.com/ssine/mira/node/internal/miraserver/foundation"
)

func (server *Server) pushKeys(ctx context.Context) (private, public string, err error) {
	err = server.pool.QueryRow(ctx, `SELECT private_key,public_key FROM mira_push_keys WHERE singleton`).Scan(&private, &public)
	if err == nil {
		return
	}
	if err != pgx.ErrNoRows {
		return
	}
	// INSERT is race safe across concurrent config requests. Never return the
	// generated loser: existing subscriptions depend on one stable public key.
	private, public, err = webpush.GenerateVAPIDKeys()
	if err != nil {
		return
	}
	_, err = server.pool.Exec(ctx, `INSERT INTO mira_push_keys(private_key,public_key) VALUES($1,$2) ON CONFLICT DO NOTHING`, private, public)
	if err != nil {
		return
	}
	err = server.pool.QueryRow(ctx, `SELECT private_key,public_key FROM mira_push_keys WHERE singleton`).Scan(&private, &public)
	return
}

func validPushEndpoint(endpoint string) bool {
	u, err := url.Parse(endpoint)
	if err != nil || len(endpoint) > 4096 || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" || (u.Port() != "" && u.Port() != "443") {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") {
		return false
	}
	return net.ParseIP(host) == nil || publicPushIP(net.ParseIP(host))
}

func validPushSubscription(s webpush.Subscription) bool {
	if !validPushEndpoint(s.Endpoint) {
		return false
	}
	key, err := base64.RawURLEncoding.DecodeString(s.Keys.P256dh)
	if err != nil {
		return false
	}
	if _, err = ecdh.P256().NewPublicKey(key); err != nil {
		return false
	}
	auth, err := base64.RawURLEncoding.DecodeString(s.Keys.Auth)
	return err == nil && len(auth) == 16
}

func (server *Server) routePush(ctx context.Context, response http.ResponseWriter, request *http.Request) (bool, error) {
	if !strings.HasPrefix(request.URL.Path, "/v1/push/") {
		return false, nil
	}
	principal, err := server.authorize(ctx, response, request, "admin", authOptions{CSRF: true})
	if err != nil || principal == nil {
		return true, err
	}
	if request.Method == http.MethodGet && request.URL.Path == "/v1/push/config" {
		_, public, err := server.pushKeys(ctx)
		if err != nil {
			return true, err
		}
		return true, writeJSON(response, 200, map[string]any{"publicKey": public})
	}
	if request.URL.Path != "/v1/push/subscription" || (request.Method != http.MethodPost && request.Method != http.MethodDelete) {
		return true, foundation.WriteErrorJSON(response, 404, "not found", "not_found")
	}
	var subscription webpush.Subscription
	if err := foundation.ReadJSON(request, &subscription, 8192); err != nil {
		return true, err
	}
	if request.Method == http.MethodDelete {
		_, err = server.pool.Exec(ctx, `DELETE FROM mira_push_subscriptions WHERE endpoint=$1 AND session_id=$2`, subscription.Endpoint, principal.SessionID)
		if err != nil {
			return true, err
		}
		return true, writeJSON(response, 200, map[string]any{"enabled": false})
	}
	if !validPushSubscription(subscription) {
		return true, foundation.WriteErrorJSON(response, 400, "无效的通知订阅", "invalid_push_subscription")
	}
	// Bound device registrations, serialize only this very infrequent operation.
	tx, err := server.pool.Begin(ctx)
	if err != nil {
		return true, err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('mira-push-subscriptions',0))`); err != nil {
		return true, err
	}
	if _, err = tx.Exec(ctx, `DELETE FROM mira_push_subscriptions s USING mira_admin_sessions a WHERE s.session_id=a.session_id AND (a.revoked_at IS NOT NULL OR a.expires_at<=now())`); err != nil {
		return true, err
	}
	var count int
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM mira_push_subscriptions WHERE endpoint<>$1`, subscription.Endpoint).Scan(&count); err != nil {
		return true, err
	}
	if count >= 32 {
		return true, foundation.WriteErrorJSON(response, 409, "通知设备数量已达上限", "push_device_limit")
	}
	_, err = tx.Exec(ctx, `INSERT INTO mira_push_subscriptions(endpoint,p256dh,auth,session_id) VALUES($1,$2,$3,$4)
	 ON CONFLICT(endpoint) DO UPDATE SET p256dh=EXCLUDED.p256dh,auth=EXCLUDED.auth,session_id=EXCLUDED.session_id`, subscription.Endpoint, subscription.Keys.P256dh, subscription.Keys.Auth, principal.SessionID)
	if err != nil {
		return true, err
	}
	if err = tx.Commit(ctx); err != nil {
		return true, err
	}
	return true, writeJSON(response, 200, map[string]any{"enabled": true})
}

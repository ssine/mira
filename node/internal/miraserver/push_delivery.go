package miraserver

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"
	"github.com/jackc/pgx/v5"
)

func publicPushIP(ip net.IP) bool {
	return ip.IsGlobalUnicast() && !ip.IsPrivate() && !ip.IsLoopback() && !ip.IsLinkLocalUnicast() &&
		!(&net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}).Contains(ip)
}

// Subscription URLs are supplied by browsers. Resolve at dial time and connect
// only to public IPs, with no redirects or environment proxy that could bypass
// this boundary. Never log endpoint URLs, payloads or VAPID credentials.
func pushHTTPClient() *http.Client {
	transport := &http.Transport{TLSHandshakeTimeout: 5 * time.Second, ResponseHeaderTimeout: 10 * time.Second, MaxIdleConns: 4, IdleConnTimeout: time.Minute}
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil || port != "443" {
			return nil, errors.New("invalid push address")
		}
		ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, errors.New("push DNS lookup failed")
		}
		for _, ip := range ips {
			if !publicPushIP(ip.IP) {
				return nil, errors.New("non-public push address")
			}
		}
		for _, ip := range ips {
			conn, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, network, net.JoinHostPort(ip.IP.String(), port))
			if err == nil {
				return conn, nil
			}
		}
		return nil, errors.New("push connection failed")
	}
	return &http.Client{Transport: transport, Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func (server *Server) startPushWorker(ctx context.Context) func() {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	client := pushHTTPClient()
	go func() {
		defer close(done)
		defer client.CloseIdleConnections()
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			// One bounded worker, one request at a time; backlog survives restarts.
			if err := server.processPushDelivery(ctx, client); err != nil && ctx.Err() == nil {
				server.config.Logger.Print("push delivery processing failed")
			}
		}
	}()
	return func() { cancel(); <-done }
}

func (server *Server) processPushDelivery(ctx context.Context, client webpush.HTTPClient) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	tx, err := server.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `DELETE FROM mira_push_deliveries WHERE delivery_id IN (SELECT delivery_id FROM mira_push_deliveries WHERE expires_at<=now() LIMIT 128)`); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `DELETE FROM mira_push_subscriptions s USING mira_admin_sessions a WHERE s.session_id=a.session_id AND (a.revoked_at IS NOT NULL OR a.expires_at<=now())`); err != nil {
		return err
	}
	var id, generation int64
	var subscriptionID, runtime, storeID, threadID, turnID, title string
	var attempts int
	var subscription webpush.Subscription
	err = tx.QueryRow(ctx, `SELECT d.delivery_id,d.subscription_id,d.runtime,d.store_id,d.thread_id,d.generation,d.turn_id,d.title,d.attempts,s.endpoint,s.p256dh,s.auth
	 FROM mira_push_deliveries d JOIN mira_push_subscriptions s USING(subscription_id)
	 JOIN mira_admin_sessions a USING(session_id)
	 WHERE NOT d.finished AND d.next_attempt_at<=now() AND d.expires_at>now() AND a.revoked_at IS NULL AND a.expires_at>now()
	 ORDER BY d.next_attempt_at,d.delivery_id LIMIT 1 FOR UPDATE OF d SKIP LOCKED`).Scan(&id, &subscriptionID, &runtime, &storeID, &threadID, &generation, &turnID, &title, &attempts, &subscription.Endpoint, &subscription.Keys.P256dh, &subscription.Keys.Auth)
	if err == pgx.ErrNoRows {
		return tx.Commit(ctx)
	}
	if err != nil {
		return err
	}
	var exists bool
	err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM codex_thread_projections p WHERE store_id=$1 AND thread_id=$2 AND active_generation=$3 AND parent_thread_id IS NULL AND lower(COALESCE(p.source_kind,'')) NOT LIKE '%subagent%'
	 AND NOT EXISTS(SELECT 1 FROM mira_agent_graph_edges g WHERE g.store_id=p.store_id AND g.child_thread_id=p.thread_id AND g.child_generation=p.active_generation)
	 AND NOT EXISTS(SELECT 1 FROM mira_thread_actions a WHERE a.store_id=p.store_id AND a.thread_id=p.thread_id AND a.action='delete'))`, storeID, threadID, generation).Scan(&exists)
	if err != nil {
		return err
	}
	if !exists || attempts >= 6 {
		if _, err = tx.Exec(ctx, `UPDATE mira_push_deliveries SET finished=true WHERE delivery_id=$1`, id); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	var private, public string
	if err = tx.QueryRow(ctx, `SELECT private_key,public_key FROM mira_push_keys WHERE singleton`).Scan(&private, &public); err != nil {
		return err
	}
	attempts++
	// Release every database lock before contacting the provider. A short lease
	// makes restart recovery safe without blocking subscription changes or the
	// canonical completion transaction behind a slow push service.
	if _, err = tx.Exec(ctx, `UPDATE mira_push_deliveries SET attempts=$2,next_attempt_at=now()+interval '2 minutes' WHERE delivery_id=$1`, id, attempts); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return err
	}
	finished := attempts >= 6
	link := "/?thread=" + url.QueryEscape(threadID)
	hash := sha256.Sum256([]byte(runtime + "/" + storeID + "/" + threadID + "/" + turnID))
	tag := base64.RawURLEncoding.EncodeToString(hash[:])
	payload, _ := json.Marshal(map[string]string{"title": "对话已完成", "body": title, "url": link, "tag": tag})
	subscriber := server.config.Foundation.CodexStoreEndpoint
	if !strings.HasPrefix(subscriber, "https://") {
		subscriber = "https://github.com/ssine/mira"
	}
	response, sendErr := webpush.SendNotificationWithContext(ctx, payload, &subscription, &webpush.Options{HTTPClient: client, Subscriber: subscriber, VAPIDPublicKey: public, VAPIDPrivateKey: private, TTL: 86400, Urgency: webpush.UrgencyNormal, Topic: tag[:32]})
	if sendErr == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		response.Body.Close()
		if response.StatusCode == 404 || response.StatusCode == 410 {
			_, err = server.pool.Exec(ctx, `DELETE FROM mira_push_subscriptions WHERE subscription_id=$1 AND p256dh=$2 AND auth=$3`, subscriptionID, subscription.Keys.P256dh, subscription.Keys.Auth)
			return err
		}
		finished = finished || (response.StatusCode >= 200 && response.StatusCode < 300) || (response.StatusCode >= 400 && response.StatusCode < 500 && response.StatusCode != 429)
	}
	_, err = server.pool.Exec(ctx, `UPDATE mira_push_deliveries SET finished=$2,next_attempt_at=now()+$4*interval '1 second' WHERE delivery_id=$1 AND attempts=$3`, id, finished, attempts, 30*(1<<min(attempts, 6)))
	return err
}

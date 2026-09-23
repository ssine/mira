package miraserver

import (
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"net"
	"testing"

	webpush "github.com/SherClockHolmes/webpush-go"
)

func testPushSubscription(t *testing.T) webpush.Subscription {
	t.Helper()
	key, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return webpush.Subscription{Endpoint: "https://push.example.test/device", Keys: webpush.Keys{
		P256dh: base64.RawURLEncoding.EncodeToString(key.PublicKey().Bytes()), Auth: base64.RawURLEncoding.EncodeToString(make([]byte, 16)),
	}}
}

func TestPushInputBoundaries(t *testing.T) {
	s := testPushSubscription(t)
	if !validPushSubscription(s) {
		t.Fatal("valid browser subscription rejected")
	}
	for _, endpoint := range []string{"http://push.example.test/x", "https://user:pass@push.example.test/x", "https://localhost/x", "https://127.0.0.1/x", "https://[::1]/x", "https://10.1.2.3/x", "https://100.64.0.1/x", "https://push.example.test:8443/x", "https://push.example.test/x#secret"} {
		s.Endpoint = endpoint
		if validPushSubscription(s) {
			t.Errorf("unsafe endpoint accepted: %s", endpoint)
		}
	}
	s = testPushSubscription(t)
	s.Keys.Auth = "invalid"
	if validPushSubscription(s) {
		t.Fatal("invalid auth accepted")
	}
	s = testPushSubscription(t)
	s.Keys.P256dh = base64.RawURLEncoding.EncodeToString(make([]byte, 65))
	if validPushSubscription(s) {
		t.Fatal("invalid curve key accepted")
	}
	for _, address := range []string{"169.254.169.254", "192.168.1.1", "::ffff:127.0.0.1", "fc00::1", "fe80::1", "0.0.0.0"} {
		if publicPushIP(net.ParseIP(address)) {
			t.Errorf("private dial allowed: %s", address)
		}
	}
}

func TestPushCompletionOnlyAcceptsSuccessfulLifecycle(t *testing.T) {
	for _, raw := range []string{
		`{"type":"event_msg","payload":{"type":"task_complete","turn_id":"one"}}`,
		`{"type":"event_msg","payload":{"type":"turn_complete","turn_id":"one","error":null}}`,
	} {
		if completedPushTurn([]byte(raw)) != "one" {
			t.Errorf("completion rejected: %s", raw)
		}
	}
	for _, raw := range []string{
		`{"type":"event_msg","payload":{"type":"task_complete","turn_id":"one","error":{"message":"failed"}}}`,
		`{"type":"event_msg","payload":{"type":"turn_aborted","turn_id":"one"}}`,
		`{"type":"event_msg","payload":{"type":"agent_message","turn_id":"one"}}`,
		`{"type":"response_item","payload":{"type":"task_complete","turn_id":"one"}}`,
		`{"type":"event_msg","payload":{"type":"task_complete"}}`,
		`{broken`,
	} {
		if completedPushTurn([]byte(raw)) != "" {
			t.Errorf("non-completion accepted: %s", raw)
		}
	}
}

package channel

import (
	"context"
	"strings"
	"testing"
)

func TestInvalidEncryptedContent(t *testing.T) {
	const poolMessage = "encrypted history has no known compatibility pool"
	cases := []struct {
		name  string
		value any
		want  bool
	}{
		{"upstream code", map[string]any{"code": "invalid_encrypted_content"}, true},
		{"gateway code", map[string]any{"error": map[string]any{"code": "unknown_reasoning_pool"}}, true},
		{"gateway JSON", `{"error":{"type":"gateway_error","code":"unknown_reasoning_pool","message":"unknown pool"}}`, true},
		{"upstream wrapped JSON", `unexpected status 400 Bad Request: {"error":{"code":"invalid_encrypted_content"}}`, true},
		{"gateway wrapped JSON", `unexpected status 409 Conflict: {"error":{"code":"unknown_reasoning_pool"}}`, true},
		{"gateway message", poolMessage, true},
		{"Codex gateway error", map[string]any{"message": "unexpected status 409 Conflict: " + poolMessage + ", url: http://127.0.0.1:8787/v1/responses"}, true},
		{"additional details", map[string]any{"additionalDetails": "unexpected status 409 Conflict: " + poolMessage}, true},
		{"conflicting pools", "unexpected status 409 Conflict: history belongs to different compatibility pools", true},
		{"unrelated conflict", "unexpected status 409 Conflict: another request is running", false},
		{"authentication", map[string]any{"code": "invalid_api_key", "message": "authentication failed"}, false},
		{"wrong status", "unexpected status 401 Unauthorized: " + poolMessage, false},
		{"quoted prose", "The tool reported: " + poolMessage, false},
		{"unrelated code", map[string]any{"code": "unknown_response_account"}, false},
		{"tool body", map[string]any{"output": map[string]any{"code": "unknown_reasoning_pool"}}, false},
		{"oversized message", strings.Repeat(" ", 32769) + poolMessage, false},
		{"missing error", nil, false},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if got := invalidEncryptedContent(test.value, 0); got != test.want {
				t.Fatalf("invalidEncryptedContent() = %v, want %v", got, test.want)
			}
		})
	}
	var nested any = map[string]any{"code": "unknown_reasoning_pool"}
	for range 9 {
		nested = map[string]any{"error": nested}
	}
	if invalidEncryptedContent(nested, 0) {
		t.Fatal("unbounded error traversal")
	}
}

func TestRecordInputFailureIgnoresNonModelErrors(t *testing.T) {
	db := &fakeDB{}
	channel := &Channel{db: db}
	client := &proxy{nodeAccountID: testCredA}
	failure := map[string]any{"code": "unknown_reasoning_pool"}
	for _, message := range []map[string]any{
		{"method": "item/completed", "params": map[string]any{"threadId": "thread", "error": failure}},
		{"method": "turn/completed", "params": map[string]any{"threadId": "thread", "turn": map[string]any{"id": "turn", "items": []any{failure}}}},
		{"id": 1, "error": failure},
		{"method": "error", "params": map[string]any{"error": failure}},
	} {
		if err := channel.recordInputFailure(context.Background(), client, message); err != nil {
			t.Fatal(err)
		}
	}
	if len(db.queries) != 0 {
		t.Fatal("non-model error triggered input recovery")
	}
}

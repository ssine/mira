package miraserver

import (
	"encoding/json"
	"testing"
)

func TestRawJSONEqualPreservesUnpairedSurrogates(t *testing.T) {
	tests := []struct {
		left, right string
		want        bool
	}{
		{`{"value":"\ud800"}`, `{"value":"\ud800"}`, true},
		{`{"value":"\ud800"}`, `{"value":"\ud801"}`, false},
		{`{"value":"\ud83d\ude00"}`, `{"value":"😀"}`, true},
		{`{"first":1,"second":"\u0061"}`, `{ "second": "a", "first": 1 }`, true},
		{`[1,2]`, `[2,1]`, false},
		{`1`, `1.0`, false},
	}
	for _, test := range tests {
		if got := rawJSONEqual(json.RawMessage(test.left), json.RawMessage(test.right)); got != test.want {
			t.Fatalf("rawJSONEqual(%s, %s) = %v, want %v", test.left, test.right, got, test.want)
		}
	}
}

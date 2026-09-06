package views

import (
	"testing"

	"github.com/ssine/mira/node/internal/miraserver/nodes"
)

func TestRuntimeKeyMatchesJavaScriptIdentity(t *testing.T) {
	tests := []struct {
		node *nodes.Node
		want string
	}{{&nodes.Node{ReportedAppServer: map[string]any{}}, "95cb9b4f84ceff132cc7a875d8c192bf4997016a939ee64141c1fd628c0e8738"}, {&nodes.Node{ReportedAppServer: map[string]any{"codexHome": "/home/sine/.codex", "codexPath": "/usr/bin/codex"}}, "fbc86f4e8eb42af5d067175b110c6dc09f7304692a070823cbac630f5282a856"}, {&nodes.Node{ReportedAppServer: map[string]any{"codexHome": "/<x>", "codexPath": "a&b"}}, "4b29aad58662b726da67fe87f892512dc268b493975458162ddcaddc160240f6"}}
	for _, test := range tests {
		if got := RuntimeKey(test.node); got != test.want {
			t.Fatalf("got %s want %s", got, test.want)
		}
	}
}

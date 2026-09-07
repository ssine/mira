package node

import "testing"

func TestSystemRolesRequireAnExplicitFirstArgument(t *testing.T) {
	for _, arguments := range [][]string{
		{"node-worker"}, {"server-worker"}, {"server", "admin"},
		{"supervisor"}, {"supervisor-check"}, {"ssh-worker"}, {"--internal-ssh-worker"}, {"ssh-command-worker"},
	} {
		if !isSystemRole(arguments) {
			t.Fatalf("explicit system role was not recognized: %v", arguments)
		}
	}
	for _, arguments := range [][]string{{}, {"nodes", "list"}, {"cli", "node-worker"}, {"mira-node"}} {
		if isSystemRole(arguments) {
			t.Fatalf("non-role arguments were recognized as a system role: %v", arguments)
		}
	}
}

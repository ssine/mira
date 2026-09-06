//go:build windows

package node

import "testing"

func TestNormalizeWindowsOpenSSHUsername(t *testing.T) {
	tests := []struct {
		name        string
		computer    string
		localSystem bool
		want        string
	}{
		{name: `WORKGROUP\SINE-DESKTOP-2$`, computer: "SINE-DESKTOP-2", localSystem: true, want: "system"},
		{name: `SINE-DESKTOP-2\Sine`, computer: "SINE-DESKTOP-2", want: "sine"},
		{name: `CONTOSO\Alice`, computer: "SINE-DESKTOP-2", want: `contoso\alice`},
		{name: "LocalUser", computer: "SINE-DESKTOP-2", want: "localuser"},
	}
	for _, test := range tests {
		if got := normalizeWindowsOpenSSHUsername(test.name, test.computer, test.localSystem); got != test.want {
			t.Errorf("normalizeWindowsOpenSSHUsername(%q, %q, %v)=%q want %q", test.name, test.computer, test.localSystem, got, test.want)
		}
	}
}

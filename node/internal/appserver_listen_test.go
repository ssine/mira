//go:build !android

package node

import (
	"fmt"
	"net"
	"net/url"
	"testing"
)

func TestAvailableAppServerListenURLFallsBackWhenRequestedPortIsBusy(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	requested := "ws://" + occupied.Addr().String()
	selected, err := availableAppServerListenURL(requested)
	if err != nil {
		t.Fatal(err)
	}
	if selected == requested {
		t.Fatal("busy App Server port was not replaced")
	}
	parsed, err := url.Parse(selected)
	if err != nil {
		t.Fatal(err)
	}
	probe, err := net.Listen("tcp", parsed.Host)
	if err != nil {
		t.Fatalf("selected fallback is unavailable: %v (%s)", err, selected)
	}
	probe.Close()
	t.Log(fmt.Sprintf("requested %s, selected %s", requested, selected))
}

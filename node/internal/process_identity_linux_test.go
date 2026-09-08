//go:build linux

package node

import (
	"reflect"
	"testing"
)

func TestParseAndCompareProcessGroupIDs(t *testing.T) {
	groups, ok := parseGroupIDs("1001 4 1001 27\n")
	if !ok || !reflect.DeepEqual(groups, []int{4, 27, 1001}) {
		t.Fatalf("groups=%v ok=%v", groups, ok)
	}
	if !equalGroupIDs(groups, []int{4, 27, 1001}) {
		t.Fatal("equal group lists reported as different")
	}
	if equalGroupIDs(groups, []int{4, 1001}) {
		t.Fatal("changed group lists reported as equal")
	}
	for _, invalid := range []string{"", "1001 group", "-1"} {
		if _, ok := parseGroupIDs(invalid); ok {
			t.Fatalf("accepted invalid group list %q", invalid)
		}
	}
}

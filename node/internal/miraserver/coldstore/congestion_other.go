//go:build !linux && !android

package coldstore

import "fmt"

func setCongestion(uintptr, string) error {
	return fmt.Errorf("cold storage TCP congestion control is only supported on Linux")
}

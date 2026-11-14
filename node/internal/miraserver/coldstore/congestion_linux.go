//go:build linux || android

package coldstore

import "syscall"

// tcpCongestion is TCP_CONGESTION from <netinet/tcp.h>.
const tcpCongestion = 13

func setCongestion(fd uintptr, name string) error {
	return syscall.SetsockoptString(int(fd), syscall.IPPROTO_TCP, tcpCongestion, name)
}

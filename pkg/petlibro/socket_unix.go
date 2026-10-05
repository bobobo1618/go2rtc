//go:build !windows

package petlibro

import "syscall"

func socketReceiveBuffer(fd uintptr) (int, error) {
	return syscall.GetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF)
}

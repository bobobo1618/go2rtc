package petlibro

import "syscall"

func socketReceiveBuffer(fd uintptr) (int, error) {
	return syscall.GetsockoptInt(syscall.Handle(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF)
}

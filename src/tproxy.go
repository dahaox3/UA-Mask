//go:build linux
// +build linux

package main

import (
	"fmt"
	"net"
	"unsafe"

	"github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"
)

// getOriginalDst 获取被 REDIRECT 规则重定向前的原始目标地址
// 使用 SO_ORIGINAL_DST socket 选项，这是 iptables REDIRECT 目标填充的
func getOriginalDst(conn *net.TCPConn) (*net.TCPAddr, error) {
	rawConn, err := conn.SyscallConn()
	if err != nil {
		return nil, fmt.Errorf("failed to get file descriptor: %w", err)
	}

	// SO_ORIGINAL_DST = 80
	const SO_ORIGINAL_DST = 80

	// 使用 sockaddr 结构获取原始目标地址
	var addr unix.RawSockaddrInet4
	addrLen := uint32(unsafe.Sizeof(addr))

	var socketErr error
	// File() duplicates the descriptor and changes its blocking semantics.
	// Control keeps Go's network poller, cancellation and deadlines intact.
	err = rawConn.Control(func(fd uintptr) {
		_, _, errno := unix.Syscall6(
			unix.SYS_GETSOCKOPT, fd, uintptr(unix.SOL_IP), uintptr(SO_ORIGINAL_DST),
			uintptr(unsafe.Pointer(&addr)), uintptr(unsafe.Pointer(&addrLen)), 0,
		)
		if errno != 0 {
			socketErr = errno
		}
	})
	if err != nil {
		return nil, fmt.Errorf("access original destination socket: %w", err)
	}
	if socketErr != nil {
		return nil, fmt.Errorf("getsockopt SO_ORIGINAL_DST failed: %w", socketErr)
	}
	if addrLen < uint32(unsafe.Sizeof(addr)) || addr.Family != unix.AF_INET {
		return nil, fmt.Errorf("unexpected original destination address")
	}

	ip := net.IPv4(addr.Addr[0], addr.Addr[1], addr.Addr[2], addr.Addr[3])
	port := int(addr.Port>>8 | addr.Port<<8)
	logrus.Debugf("[Tproxy] getOriginalDst raw value: %d, parsed port: %d", addr.Port, port)
	return &net.TCPAddr{
		IP:   ip,
		Port: port,
	}, nil
}

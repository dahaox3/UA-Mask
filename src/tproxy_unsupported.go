//go:build !linux

package main

import (
	"fmt"
	"net"
)

// The redirect ingress is Linux-only; this keeps portable lifecycle tests usable.
func getOriginalDst(*net.TCPConn) (*net.TCPAddr, error) {
	return nil, fmt.Errorf("SO_ORIGINAL_DST requires Linux")
}

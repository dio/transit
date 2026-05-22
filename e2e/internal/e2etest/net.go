package e2etest

import "net"

// MustTCPPort returns the TCP port for addr.
func MustTCPPort(addr net.Addr) int {
	tcpAddr, ok := addr.(*net.TCPAddr)
	if !ok {
		panic("e2etest: address is not TCP")
	}
	return tcpAddr.Port
}

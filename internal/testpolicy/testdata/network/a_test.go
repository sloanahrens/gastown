package network

import (
	"context"
	"net"
	"testing"
	"time"
)

func TestNetwork(t *testing.T) {
	t.Parallel()
	_, _ = net.Dial("unix", "/tmp/x")
	_, _ = net.DialTimeout("unix", "/tmp/x", time.Second)
	_, _ = net.Listen("tcp", "127.0.0.1:0")
	_, _ = net.ListenUnix("unix", &net.UnixAddr{Name: "/tmp/x", Net: "unix"})
	d := net.Dialer{}
	_, _ = d.DialContext(context.Background(), "tcp", "127.0.0.1:1")
	_ = net.ParseIP("127.0.0.1") // not network I/O
}

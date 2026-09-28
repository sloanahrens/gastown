package networkmore

import (
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

func TestNetworkMore(t *testing.T) {
	t.Parallel()
	_ = httptest.NewServer(http.NotFoundHandler())
	_ = httptest.NewUnstartedServer(http.NotFoundHandler())
	_ = httptest.NewTLSServer(http.NotFoundHandler())
	_, _ = net.DialIP("ip4:icmp", nil, &net.IPAddr{})
	_, _ = net.ListenIP("ip4:icmp", &net.IPAddr{})
	_, _ = net.ListenMulticastUDP("udp", nil, &net.UDPAddr{})
	_, _ = net.FileConn(os.Stdin)
	var d net.Dialer
	var lc net.ListenConfig
	p := new(net.Dialer)
	_, _, _ = d, lc, p
	_ = httptest.NewRecorder() // no socket: fine
	var addr net.UDPAddr       // not a dialer: fine
	_ = addr
}

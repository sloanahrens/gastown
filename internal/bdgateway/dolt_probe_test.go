package bdgateway

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

// recordingConn is a net.Conn a probe can dial. The probe only dials and closes,
// so Close's record is the one behaviour the probe's test reads.
type recordingConn struct {
	closed bool
}

func (c *recordingConn) Read([]byte) (int, error)         { return 0, io.EOF }
func (c *recordingConn) Write(p []byte) (int, error)      { return len(p), nil }
func (c *recordingConn) Close() error                     { c.closed = true; return nil }
func (c *recordingConn) LocalAddr() net.Addr              { return nil }
func (c *recordingConn) RemoteAddr() net.Addr             { return nil }
func (c *recordingConn) SetDeadline(time.Time) error      { return nil }
func (c *recordingConn) SetReadDeadline(time.Time) error  { return nil }
func (c *recordingConn) SetWriteDeadline(time.Time) error { return nil }

// TestGatewayReadyDoltProbe: the probe makes one TCP dial to host:port, closes
// the connection it got, and reports the dial's error unchanged so the caller's
// refusal can name it.
func TestGatewayReadyDoltProbe(t *testing.T) {
	t.Parallel()
	t.Run("dials once and closes", func(t *testing.T) {
		t.Parallel()
		conn := &recordingConn{}
		var calls int
		var network, addr string
		probe := NewDoltProbe("127.0.0.1", "3307", func(_ context.Context, n, a string) (net.Conn, error) {
			calls++
			network, addr = n, a
			return conn, nil
		})
		if err := probe(context.Background()); err != nil {
			t.Fatalf("probe = %v, want nil", err)
		}
		if calls != 1 {
			t.Errorf("dial calls = %d, want 1", calls)
		}
		if network != "tcp" || addr != "127.0.0.1:3307" {
			t.Errorf("dialled %s %s, want tcp 127.0.0.1:3307", network, addr)
		}
		if !conn.closed {
			t.Error("the probe left the connection open")
		}
	})

	t.Run("returns the dial error", func(t *testing.T) {
		t.Parallel()
		dialErr := errors.New("connection refused")
		probe := NewDoltProbe("127.0.0.1", "3307", func(context.Context, string, string) (net.Conn, error) {
			return nil, dialErr
		})
		if err := probe(context.Background()); !errors.Is(err, dialErr) {
			t.Fatalf("probe = %v, want %v", err, dialErr)
		}
	})

	t.Run("no dialer refuses", func(t *testing.T) {
		t.Parallel()
		if err := NewDoltProbe("127.0.0.1", "3307", nil)(context.Background()); err == nil {
			t.Fatal("a probe with no dialer passed, want a refusal")
		}
	})
}

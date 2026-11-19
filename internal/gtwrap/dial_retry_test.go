package gtwrap

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/url"
	"os"
	"syscall"
	"testing"
	"time"
)

// dialFailure builds the error net returns when a dial fails with errno.
func dialFailure(errno syscall.Errno) *net.OpError {
	return &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", errno)}
}

func TestDialRetryClassify(t *testing.T) {
	t.Parallel()
	gatewayURL := "http://127.0.0.1:1/exec"
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"connection refused", dialFailure(syscall.ECONNREFUSED), true},
		{"host unreachable", dialFailure(syscall.EHOSTUNREACH), true},
		{"network unreachable", dialFailure(syscall.ENETUNREACH), true},
		{"connection refused in a url.Error", &url.Error{Op: "Post", URL: gatewayURL, Err: dialFailure(syscall.ECONNREFUSED)}, true},
		{"host unreachable in a url.Error", &url.Error{Op: "Post", URL: gatewayURL, Err: dialFailure(syscall.EHOSTUNREACH)}, true},
		{"network unreachable in a url.Error", &url.Error{Op: "Post", URL: gatewayURL, Err: dialFailure(syscall.ENETUNREACH)}, true},
		{"dial timeout", &net.OpError{Op: "dial", Net: "tcp", Err: os.ErrDeadlineExceeded}, false},
		{"context deadline in a url.Error", &url.Error{Op: "Post", URL: gatewayURL, Err: context.DeadlineExceeded}, false},
		{"connection reset after connect", &net.OpError{Op: "read", Net: "tcp", Err: os.NewSyscallError("read", syscall.ECONNRESET)}, false},
		{"EOF after connect", &url.Error{Op: "Post", URL: gatewayURL, Err: io.EOF}, false},
		{"TLS handshake error", &url.Error{Op: "Post", URL: "https://127.0.0.1:1/exec", Err: tls.RecordHeaderError{Msg: "first record does not look like a TLS handshake"}}, false},
		{"refused on a read", &net.OpError{Op: "read", Net: "tcp", Err: os.NewSyscallError("read", syscall.ECONNREFUSED)}, false},
		{"nil", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := RetryableDial(tc.err); got != tc.want {
				t.Errorf("RetryableDial(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestDialRetrySchedule(t *testing.T) {
	t.Parallel()
	t.Run("45 seconds", func(t *testing.T) {
		t.Parallel()
		const total = 45 * time.Second
		got := RetrySchedule(total)
		wantStart := []time.Duration{
			500 * time.Millisecond,
			time.Second,
			2 * time.Second,
			3 * time.Second,
			5 * time.Second,
		}
		if len(got) < len(wantStart) {
			t.Fatalf("RetrySchedule(%v) = %v, want at least %d delays", total, got, len(wantStart))
		}
		for i, want := range wantStart {
			if got[i] != want {
				t.Fatalf("delay %d = %v, want %v", i, got[i], want)
			}
		}
		var sum time.Duration
		for _, d := range got[4:] {
			if d != 5*time.Second {
				t.Fatalf("delay %v after the ladder = %v, want 5s", d, d)
			}
			sum += d
		}
		sum += 500*time.Millisecond + time.Second + 2*time.Second + 3*time.Second
		if sum > total {
			t.Fatalf("the delays sum to %v, past the %v total", sum, total)
		}
		if sum+5*time.Second <= total {
			t.Fatalf("the delays sum to %v but another 5s wait still fits in %v", sum, total)
		}
	})
	t.Run("total equal to the ladder sum", func(t *testing.T) {
		t.Parallel()
		const total = 6500 * time.Millisecond
		got := RetrySchedule(total)
		want := []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second, 3 * time.Second}
		if len(got) != len(want) {
			t.Fatalf("RetrySchedule(%v) = %v, want %d delays that fit exactly", total, got, len(want))
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("delay %d = %v, want %v", i, got[i], want[i])
			}
		}
	})
	t.Run("zero total", func(t *testing.T) {
		t.Parallel()
		if got := RetrySchedule(0); len(got) != 0 {
			t.Fatalf("RetrySchedule(0) = %v, want no delays", got)
		}
	})
}

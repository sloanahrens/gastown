package gtwrap

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"math"
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
		}
		if len(got) <= len(wantStart) {
			t.Fatalf("RetrySchedule(%v) = %v, want more than the %d ladder delays", total, got, len(wantStart))
		}
		for i, want := range wantStart {
			if got[i] != want {
				t.Fatalf("delay %d = %v, want %v", i, got[i], want)
			}
		}
		var sum time.Duration
		for _, d := range got {
			sum += d
		}
		for i, d := range got[len(wantStart):] {
			if d != 5*time.Second {
				t.Fatalf("delay %d after the ladder = %v, want 5s", len(wantStart)+i, d)
			}
		}
		steady := len(got) - len(wantStart)
		const wantSteady = 7
		if steady != wantSteady {
			t.Fatalf("the schedule has %d steady 5s steps after the ladder, want %d", steady, wantSteady)
		}
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

// TestDialRetryScheduleBounded pins the schedule's documented cap of 32 waits
// (RetrySchedule's doc): a total past the point the cap binds returns exactly
// that many waits, so the slice cannot grow with total. The 200s check runs
// first because it fails fast if the cap is dropped (uncapped, 200s yields 42
// waits); the math.MaxInt64 call, the input the bead names, would otherwise run
// the uncapped loop over about 1.8 billion waits before any assertion ran.
func TestDialRetryScheduleBounded(t *testing.T) {
	t.Parallel()
	if got := len(RetrySchedule(200 * time.Second)); got != 32 {
		t.Fatalf("RetrySchedule(200s) returned %d waits, want the documented cap of 32", got)
	}
	got := RetrySchedule(math.MaxInt64)
	if len(got) != 32 {
		t.Fatalf("RetrySchedule(math.MaxInt64) returned %d waits, want the documented cap of 32", len(got))
	}
}

func TestRetryableReadTable(t *testing.T) {
	t.Parallel()
	gatewayURL := "http://127.0.0.1:1/exec"
	transport := []struct {
		name string
		err  error
	}{
		{"timeout", &net.OpError{Op: "read", Net: "tcp", Err: os.ErrDeadlineExceeded}},
		{"deadline in a url.Error", &url.Error{Op: "Post", URL: gatewayURL, Err: context.DeadlineExceeded}},
		{"connection reset", &net.OpError{Op: "read", Net: "tcp", Err: os.NewSyscallError("read", syscall.ECONNRESET)}},
		{"unexpected EOF", &url.Error{Op: "Post", URL: gatewayURL, Err: io.ErrUnexpectedEOF}},
	}
	shapes := []struct {
		name       string
		argv       []string
		sendsStdin bool
		wroteOut   bool
		want       bool
	}{
		{"read verb, nothing sent or written", []string{"list"}, false, false, true},
		{"write verb", []string{"close", "gt-abc"}, false, false, false},
		{"unknown verb", []string{"frobnicate"}, false, false, false},
		{"read verb with stdin", []string{"list"}, true, false, false},
		{"read verb after output was written", []string{"list"}, false, true, false},
	}
	for _, tr := range transport {
		for _, sh := range shapes {
			t.Run(tr.name+", "+sh.name, func(t *testing.T) {
				t.Parallel()
				if got := RetryableRead(tr.err, sh.argv, sh.sendsStdin, sh.wroteOut); got != sh.want {
					t.Errorf("RetryableRead(%v, %q, stdin=%v, output=%v) = %v, want %v",
						tr.err, sh.argv, sh.sendsStdin, sh.wroteOut, got, sh.want)
				}
			})
		}
	}
}

func TestRetryableReadOther(t *testing.T) {
	t.Parallel()
	gatewayURL := "http://127.0.0.1:1/exec"
	cases := []struct {
		name string
		err  error
	}{
		{"nil", nil},
		{"refused dial", dialFailure(syscall.ECONNREFUSED)},
		{"refused dial in a url.Error", &url.Error{Op: "Post", URL: gatewayURL, Err: dialFailure(syscall.ECONNREFUSED)}},
		{"TLS handshake error", &url.Error{Op: "Post", URL: "https://127.0.0.1:1/exec", Err: tls.RecordHeaderError{Msg: "first record does not look like a TLS handshake"}}},
		{"plain EOF", &url.Error{Op: "Post", URL: gatewayURL, Err: io.EOF}},
		{"non-transport error", errors.New("bdgateway: verb not allowlisted")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if RetryableRead(tc.err, []string{"list"}, false, false) {
				t.Errorf("RetryableRead(%v, list) = true, want false", tc.err)
			}
		})
	}
}

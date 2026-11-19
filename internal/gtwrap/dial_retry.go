package gtwrap

import (
	"errors"
	"net"
	"syscall"
	"time"
)

// RetryableDial reports whether err is a dial that never reached the gateway,
// so retrying the call cannot run it twice. A refused, host unreachable or
// network unreachable dial qualifies, bare or wrapped in a *url.Error. A
// timeout, a deadline, a reset or EOF after the connection was made, a TLS or
// HTTP error, and nil do not.
func RetryableDial(err error) bool {
	var opErr *net.OpError
	if !errors.As(err, &opErr) || opErr.Op != "dial" {
		return false
	}
	return errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.EHOSTUNREACH) ||
		errors.Is(err, syscall.ENETUNREACH)
}

// RetrySchedule returns the waits before each retry of a call that failed with
// a retryable dial error, given the total time it may spend waiting: half a
// second, then one, two and three seconds, then five seconds each. A wait that
// would take the sum past total is left out, so a zero total gives none and the
// waits always fit. The result is pure: no clock and no sleep.
func RetrySchedule(total time.Duration) []time.Duration {
	ladder := [...]time.Duration{
		500 * time.Millisecond,
		time.Second,
		2 * time.Second,
		3 * time.Second,
	}
	const steady = 5 * time.Second
	var (
		delays []time.Duration
		sum    time.Duration
	)
	for {
		next := steady
		if len(delays) < len(ladder) {
			next = ladder[len(delays)]
		}
		// sum never passes total, so total-sum cannot overflow, and each wait is
		// longer than zero, so the sum rises and a wait that does not fit ends
		// the loop.
		if next > total-sum {
			return delays
		}
		delays = append(delays, next)
		sum += next
	}
}

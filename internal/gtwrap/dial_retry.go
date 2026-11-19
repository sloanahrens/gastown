package gtwrap

import (
	"context"
	"errors"
	"io"
	"net"
	"syscall"
	"time"

	"github.com/steveyegge/gastown/internal/bdgateway"
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

// RetryableRead reports whether a bd call that failed with err may be sent
// again after the transport failed mid-call. It answers true only when all of
// these hold: err is a timeout, a deadline, a reset or an unexpected EOF; argv
// runs a read verb (bdgateway.IsReadCall); and the call neither sent stdin
// nor wrote output, so a second run repeats nothing the first run delivered.
// A refused dial is RetryableDial's case, and nil, a TLS or HTTP error and a
// write or unknown verb all answer false.
func RetryableRead(err error, argv []string, sendsStdin, wroteOutput bool) bool {
	if sendsStdin || wroteOutput {
		return false
	}
	if !transportFailure(err) {
		return false
	}
	return bdgateway.IsReadCall(argv)
}

// transportFailure reports whether err is one of the mid-call failures a read
// may be repeated after: a timeout, a deadline, a connection reset or an
// unexpected EOF. A plain io.EOF is left out: the retry set names only the
// unexpected form.
func transportFailure(err error) bool {
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	return errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, io.ErrUnexpectedEOF)
}

// RetrySchedule returns the waits before each retry of a call that failed with
// a retryable error, a refused dial or a read call's mid-call transport
// failure, given the total time it may spend waiting: half a second, then one,
// two and three seconds, then five seconds each. A wait that would take the sum
// past total is left out, so a zero total gives none and the waits always fit.
// The waits are what a call spends waiting, not the whole budget: time an
// attempt spends failing counts against the same total, so the caller stops
// when what is left is too little to try with. The result is pure: no clock and
// no sleep.
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

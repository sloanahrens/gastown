// Package lintlock holds the one policy for surviving golangci-lint's
// module-wide lock: how long to wait before re-running a lint, and how to tell
// a contended lint from a lint that found something.
//
// Both gate callers share it — `gt done`'s pre-verification and the refinery's
// gate runner — because two copies of this policy drift in exactly the way
// that matters. The refinery once keyed its verdict on the number of retries
// rather than on what the final attempt did, so a lint that contended once and
// then reported real errors was forwarded as "nothing was linted; re-run the
// gate", and the agent re-ran it into the same errors (gt-ijqw).
package lintlock

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Marker is what golangci-lint prints when another instance holds the lock. It
// exits without analyzing anything, so the text is evidence of a lint that
// never happened rather than of a finding.
const Marker = "parallel golangci-lint is running"

// TimeoutSentinel is what golangci-lint prints when it stops at its own
// --timeout without reporting findings. run.timeout's clock starts only once
// the module lock is held, so this covers a lint that took the lock and ran too
// long — never one that was still waiting for it (gt-ijqw, gt-kqwu).
const TimeoutSentinel = "Timeout exceeded: try increasing it by passing --timeout option"

// RetryDelay is how long to wait before each re-run of a contended lint: one
// entry per retry, so at most len+1 attempts. The schedule widens and then
// holds, because the size of a wait is not what decides a retry —
// golangci-lint's own TryLockContext already spins 5s on each attempt, so an
// attempt is a ticket in the race for the lock, and what is chosen here is how
// many tickets to buy and how much CPU to spend between them.
//
// More attempts is the cure, not longer waits: with five polecats plus the
// refinery on one host the lock was measured passing straight from one holder
// to the next with no gap, so a would-be holder that gives up early gives up
// while the lock is merely busy (gt-xsty).
//
// A var so tests need not sleep.
var RetryDelay = []time.Duration{
	15 * time.Second, 30 * time.Second, 30 * time.Second, 45 * time.Second,
	45 * time.Second, 45 * time.Second, 45 * time.Second, 45 * time.Second,
}

// Reserve is the slice of a lint budget a caller keeps for the lint that
// eventually gets the lock. A contended attempt costs only its own failure, but
// the winner then needs a whole lint, and one cut short by the budget reports a
// failure indistinguishable from findings — the misattribution this package
// exists to prevent.
const Reserve = 3 * time.Minute

// Schedule is the total time RetryDelay spends waiting.
func Schedule() time.Duration {
	var total time.Duration
	for _, d := range RetryDelay {
		total += d
	}
	return total
}

// ContendedAttempt is what one attempt costs when the lock is held: the ~5s
// golangci-lint spends in TryLockContext before giving up on it.
const ContendedAttempt = 5 * time.Second

// MinBudget is a lint budget the whole schedule can be spent inside: every
// wait, Reserve for the lint that finally gets the lock, and one contended
// attempt. Size a lint gate from this rather than from a guess — below it,
// RoomForRetry refuses early waits and the retry quietly stops happening
// (gt-xsty).
//
// It is a floor for the shape where a contended attempt fails fast. A lint
// command that hides the lock — a serial runner, or a wrapper that waits
// internally — reports no contention at all, so its budget buys no retries and
// the waiting has to happen inside that command, where no caller can attribute
// or bound it (gt-kqwu).
func MinBudget() time.Duration {
	return Schedule() + Reserve + ContendedAttempt
}

// RoomForRetry reports whether ctx can still afford to wait for wait and then
// run a full lint, so a retry is never started that the budget cannot finish.
// A context with no deadline gets no retries: every caller is bounded, and
// answering "yes" here would turn a contention loop into a hung gate.
func RoomForRetry(ctx context.Context, wait time.Duration) bool {
	deadline, ok := ctx.Deadline()
	if !ok {
		return false
	}
	return time.Until(deadline) >= wait+Reserve
}

// Attempt is one run of a lint command: the runner's error, and that attempt's
// own output — the part that says whether the failure was the lock or a finding.
type Attempt struct {
	Err    error
	Output string
}

// Contended reports whether a failed attempt's output has golangci-lint saying
// another instance held the lock.
func Contended(output string) bool {
	return strings.Contains(output, Marker)
}

// Unfinished reports whether a failed attempt's output says the lint never
// analyzed anything: the lock marker, or golangci-lint's own timeout, which is
// how a lint that held the lock and outran run.timeout ends (gt-ijqw). Retrying
// on this is safe from the opposite error, because a lint that ran and found
// something prints findings rather than either sentinel.
func Unfinished(output string) bool {
	return Contended(output) || strings.Contains(output, TimeoutSentinel)
}

// Outcome is Retry's result. Waits is the number of waits it spent on the lock.
// Output and the two verdicts describe the FINAL attempt, so a caller reports
// what actually failed instead of inferring it from the retry count (gt-ijqw).
type Outcome struct {
	Err        error
	Waits      int
	Output     string
	Contended  bool
	Unfinished bool
}

// Retry calls attempt until it succeeds or its failure stops looking like a
// lint that never ran, waiting RetryDelay between tries (len+1 attempts at
// most). attempt runs the lint once and reports that attempt's own output;
// onRetry, when non-nil, is called before each wait so the caller can attribute
// the delay in its own log.
//
// The retry count and the waits are not the only bounds: ctx must also carry a
// deadline (RoomForRetry refuses one that does not), and if it expires while
// waiting, the last attempt's error is returned unchanged rather than a
// truncated attempt's.
func Retry(ctx context.Context, attempt func() Attempt, onRetry func(attempt, attempts int, wait time.Duration)) Outcome {
	for n := 0; ; n++ {
		got := attempt()
		outcome := Outcome{
			Err:        got.Err,
			Waits:      n,
			Output:     got.Output,
			Contended:  Contended(got.Output),
			Unfinished: Unfinished(got.Output),
		}
		if got.Err == nil {
			outcome.Err = nil
			return outcome
		}
		if n >= len(RetryDelay) || !outcome.Unfinished || !RoomForRetry(ctx, RetryDelay[n]) {
			return outcome
		}
		wait := RetryDelay[n]
		if onRetry != nil {
			onRetry(n+1, len(RetryDelay)+1, wait)
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return outcome
		case <-timer.C:
		}
	}
}

// LockWait names what the gate spent on the lock, for a caller's failure
// message to drop into a sentence: the retries it bought, or — for a gate with
// no budget to wait in, or one whose own output was the last word — nothing.
//
// Callers word their own failure detail (the command to re-run differs), but
// they take the verdict from here so a lint that found something is never
// described as one that never ran (gt-ijqw).
func (o Outcome) LockWait() string {
	if o.Waits == 0 {
		return "while this lint ran"
	}
	return fmt.Sprintf("across %d retries", o.Waits)
}

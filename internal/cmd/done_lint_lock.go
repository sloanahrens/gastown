package cmd

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// gt-xsty: golangci-lint takes a module-wide file lock for the duration of a
// run, so two lints started at the same time make the loser give up after its
// own 5s wait with "parallel golangci-lint is running" — nothing analyzed, no
// findings, and a non-zero exit that gt done's gates could not tell apart from
// real lint output. With a five-polecat cap plus the refinery's own batch
// lint, two lints overlapping is routine rather than exotic: marble's gate died
// this way on 09-18 while granite's gt done held the lock.
//
// The lock means two things at once — real contention (the other lint is doing
// useful work and will release) and a lie (nothing was linted) — so the gate
// waits the holder out and retries a bounded number of times before it reports
// anything, and writes every wait to the verify log and the polecat's pane so
// the delay is attributable rather than looking like a hung agent (gt-hkhu).
//
// gt-taoz: this rig's own lint_command no longer reaches the retry below.
// .golangci.yml sets run.allow-serial-runners, which makes a contended
// golangci-lint block on the module lock — TryLockContext against a context
// carrying no deadline — rather than exit with the marker, so there is no
// marker to match and one attempt absorbs the whole wait. The retry stays for
// the callers that cannot see this repo's config, a lint_command that is a
// bare `golangci-lint run` among them; for this rig's, the wait is attributed
// by lintFailureDetail's budget case instead.
const lintLockContentionMarker = "parallel golangci-lint is running"

// lintLockRetryDelay is how long the gate waits before each retry of a
// lock-contended lint: one entry per retry, so at most len+1 attempts. The
// schedule widens and then holds, because the size of the wait is not what
// decides a retry — golangci-lint's own TryLockContext already spins for 5s on
// each attempt, so an attempt is a ticket in the race for the lock, and what
// is being chosen here is how many tickets to buy and how much CPU to spend
// between them (a losing attempt costs its process setup plus that 5s spin,
// which is not free on a host already at load 47).
//
// The length is measured, not guessed. On 09-18, with five polecats plus the
// refinery on one host, this command failed through 165s and again through 235s
// of retries while no single process held the lock for anything like that long
// — a quiet lint takes ~90s, so the lock was being handed straight from one
// holder to the next with no gap. That is a race, not a wedge: each attempt
// wins with roughly 1/(number of waiters) probability, so the fix is more
// attempts, and the failure to avoid is giving up while the lock is merely
// busy.
//
// It stays inside the budget the gate already has (defaultLintVerifyTimeout,
// 10m): these waits total 285s, and lintLockRetryReserve keeps a lint's worth
// of that budget unspent. A var so tests need not sleep.
var lintLockRetryDelay = []time.Duration{
	15 * time.Second, 30 * time.Second, 30 * time.Second, 45 * time.Second,
	45 * time.Second, 45 * time.Second, 45 * time.Second, 45 * time.Second,
}

// lintLockRetryReserve is the slice of the lint budget a gate keeps for the
// lint that eventually gets the lock. A contended attempt costs only the ~5s
// golangci-lint spends failing to take the lock, but the winner then needs a
// whole lint — ~90s on a quiet host, longer on a loaded one — and a lint cut
// short by the budget would be reported as a failure, which is the
// misattribution this whole file exists to prevent.
const lintLockRetryReserve = 3 * time.Minute

// roomForRetry reports whether ctx can still afford to wait for wait and then
// run a full lint, so a retry is never started that the budget cannot finish.
// A context with no deadline gets no retries: both callers are bounded, and
// answering "yes" here would turn an unbounded loop into a hung gate.
func roomForRetry(ctx context.Context, wait time.Duration) bool {
	deadline, ok := ctx.Deadline()
	if !ok {
		return false
	}
	return time.Until(deadline) >= wait+lintLockRetryReserve
}

// lintAttempt is one run of a lint command: the runner's error, and the output
// that attempt itself produced — the part that decides whether a failure was
// the lock or a finding.
type lintAttempt struct {
	err    error
	output string
}

// isLintLockContention reports whether a failed lint attempt failed because
// another golangci-lint held the lock, rather than because it found anything.
// golangci-lint prints the marker to stderr, which both of gt done's gates
// capture into their logs alongside stdout, so the text is available wherever
// the exit code is.
func isLintLockContention(output string) bool {
	return strings.Contains(output, lintLockContentionMarker)
}

// lockRetryOutcome is retryLintLockContention's result. retries is the number
// of waits it spent on the lock, which is what lets a caller explain a failure
// that was contention rather than a finding.
type lockRetryOutcome struct {
	err     error
	retries int
}

// retryLintLockContention calls attempt until it succeeds or its failure stops
// looking like golangci-lint's lock, waiting lintLockRetryDelay between tries
// (len+1 attempts at most). attempt runs the lint command once and reports
// that attempt's own output; onRetry, when non-nil, is called before each wait
// so both callers can attribute the delay in their own log.
//
// The retry count and the waits are not the only bounds: ctx must also carry a
// deadline (roomForRetry refuses one that does not), and if it expires while
// waiting, the last attempt's error is returned unchanged rather than a
// truncated attempt's.
func retryLintLockContention(ctx context.Context, attempt func() lintAttempt, onRetry func(attempt, attempts int, wait time.Duration)) lockRetryOutcome {
	for n := 0; ; n++ {
		got := attempt()
		if got.err == nil {
			return lockRetryOutcome{retries: n}
		}
		if n >= len(lintLockRetryDelay) || !isLintLockContention(got.output) ||
			!roomForRetry(ctx, lintLockRetryDelay[n]) {
			return lockRetryOutcome{err: got.err, retries: n}
		}
		wait := lintLockRetryDelay[n]
		if onRetry != nil {
			onRetry(n+1, len(lintLockRetryDelay)+1, wait)
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return lockRetryOutcome{err: got.err, retries: n}
		case <-timer.C:
		}
	}
}

// lintFailureDetail is what a failed lint attempt tells the polecat. Only a
// lint that ran to completion and reported findings may ask for findings to be
// fixed; the other two failures linted nothing, and sending a polecat after
// output that does not exist is the misattribution this file exists to prevent
// (gt-xsty).
//
// The budget case is the one gt-taoz opened. With run.allow-serial-runners a
// contended lint blocks rather than exiting with the marker, so the retry
// never fires and the wait is spent inside a single attempt — one that the
// outer budget can and does kill, at which point cmd.Run returns a signal
// error carrying no marker, retries is 0, and the default detail would claim
// findings. budgetExpired tells the two apart.
func lintFailureDetail(retries int, budgetExpired bool, budget time.Duration) string {
	switch {
	case retries > 0:
		return fmt.Sprintf("another golangci-lint held the lock across %d retries (a concurrent gt done, or the refinery's batch lint) — nothing was linted and no finding is reported; re-run gt done once the other lint finishes", retries)
	case budgetExpired:
		return fmt.Sprintf("the lint was killed at its %s budget without finishing — nothing was linted and no finding is reported; a concurrent golangci-lint holding the module lock is the likeliest reason it never finished, so re-run gt done once other lints have", humanDuration(budget))
	default:
		return "fix the lint findings before resubmitting"
	}
}

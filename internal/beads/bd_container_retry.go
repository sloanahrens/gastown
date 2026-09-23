package beads

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"strings"
	"time"
)

const (
	// bdContainerRetryAttempts bounds how many times one bd command is retried
	// against a test Dolt container. Five attempts sleep 500ms+1s+2s+4s ≈ 7.5s
	// in total, which is small beside the per-command subprocess budget (60s,
	// bdSubprocessTimeout) and the per-package gate budget (20m, Makefile), but
	// wide enough to ride out a contention burst on the Docker VM.
	bdContainerRetryAttempts = 5

	// bdContainerRetryBaseBackoff is the delay before the second attempt. Each
	// further delay doubles, so a short hiccup costs a short pause.
	bdContainerRetryBaseBackoff = 500 * time.Millisecond

	// bdContainerRetryMaxBackoff caps a single delay so the attempts above stay
	// inside the budget above even if the constants drift.
	bdContainerRetryMaxBackoff = 8 * time.Second
)

// bdContainerRetryWindow caps the wall clock the whole retry sequence may
// spend. The attempt count alone does not bound it: each attempt runs its own
// subprocess, and one that stalls against a dead container can take most of
// bdSubprocessTimeout to fail, so five attempts could cost five minutes.
// Matching the single-command budget means a container that is gone rather than
// busy reports in roughly twice the wait it would have cost without any retry.
// A var so tests can collapse the window and pin that bound.
var bdContainerRetryWindow = 60 * time.Second

// bdConnectionFailureMarkers are the stderr fragments that mean bd lost its
// Dolt connection rather than answering the command. The observed shape
// (gt-6uhq) carried two of them at once:
//
//	show MR gt-rqn: bd show gt-rqn --json: [mysql] read tcp 127.0.0.1:65473->127.0.0.1:55107: i/o timeout
//	Error: failed to open database: schema skew check: probing schema_migrations existence: invalid connection
//
// bd's own client already retries this class, but its pool read timeout
// (buildServerDSN: 10s ReadTimeout) and its retry budget are both smaller than
// a saturated Docker VM can stall a query for, so the retry lands here too.
//
// All but the timeout name a connection that could not be established, so bd
// aborted before the command ran and retrying is safe for writes as well as
// reads. The timeout is the ambiguous one: it can land on a response that never
// arrived after the command took effect, and there a retry of a non-idempotent
// command can leave a duplicate bead in the container's throwaway database —
// still the better outcome than the red gate on an unbroken package that it
// replaces.
var bdConnectionFailureMarkers = []string{
	"failed to open database",  // bd's open path: connect + schema-skew probe
	"invalid connection",       // go-sql-driver: connection died before use
	"i/o timeout",              // read/write deadline on the Dolt socket
	"connection reset by peer", // server dropped the connection mid-handshake
	"broken pipe",              // write to a connection the server already closed
}

// bdCatalogRaceMarkers are the stderr fragments that mean bd's store open died
// on a database of the test Dolt server's that the session has no starting root
// for (gt-unq4l).
//
// The name in the message is never the caller's own database, and that mismatch
// is the signature:
//
//	bd init --prefix pt13dd3b6a --database testdb_21eb6271a1b36e34 ...
//	Error: failed to open Dolt store: ... could not resolve initial root for
//	database testdb_e7165de82d57b7dd/
//
// The trailing slash is Dolt's revision qualifier carrying an empty branch, the
// name it builds when a session is asked about a database it took no start
// point for (dolthub/dolt: Database.WithBranchRevision, dsess.TransactionRoot).
// The test server's catalog is shared by every test in the package, so a
// concurrent test's CREATE or DROP DATABASE is the leading explanation for a
// session meeting such a database; what is certain is that the name leaves: the
// next attempt opens against a catalog the offending name is gone from.
//
// Every one of these fires in bd's open path, before the command has any
// effect, so retrying is safe for writes as well as reads.
var bdCatalogRaceMarkers = []string{
	"could not resolve initial root for database",
}

// targetsTestDoltContainer reports whether this wrapper points at testutil's
// ephemeral Dolt container rather than a production server. Only
// NewIsolatedWithPort sets both fields, and its only callers are tests.
//
// The retry below is deliberately scoped to this case rather than applied to
// every bd call. A contended container is infrastructure: nothing is wrong with
// the command, and the same command succeeds moments later. A contended
// production server is a different claim — it may mean a wedged Dolt, a
// half-applied write, or a genuine outage — and that decision belongs to the
// operator, not to a hidden retry loop inside a client library.
func (b *Beads) targetsTestDoltContainer() bool {
	return b.isolated && b.serverPort > 0
}

// ContainerUnavailable reports whether err is a lost connection to the
// ephemeral test Dolt container rather than an answer from bd, so that a
// container-backed suite can skip a container that is gone without excusing a
// real failure (gt-cbtl). Only a wrapper built by NewIsolatedWithPort can
// answer true, and the container retry loop has already spent its attempts by
// the time a caller sees the error.
//
// A catalog-race failure (bdCatalogRaceMarkers) is deliberately not part of
// this verdict: nothing about it says the container is gone, so a run that
// exhausts its retries on one still fails rather than skipping.
func (b *Beads) ContainerUnavailable(err error) bool {
	return b.retryableBdConnectionFailure(err)
}

// retryableBdConnectionFailure reports whether err is a connection-stage
// failure worth retrying against a test Dolt container.
func (b *Beads) retryableBdConnectionFailure(err error) bool {
	return b.matchesTransientMarkers(err, bdConnectionFailureMarkers)
}

// retryableBdCatalogRace reports whether err is the test Dolt server's catalog
// changing under bd's store open rather than an answer from bd.
func (b *Beads) retryableBdCatalogRace(err error) bool {
	return b.matchesTransientMarkers(err, bdCatalogRaceMarkers)
}

// retryableBdTransientFailure is the class the retry loop rides on: a failure
// that is worth another attempt against the test container rather than an
// answer from bd.
func (b *Beads) retryableBdTransientFailure(err error) bool {
	return b.retryableBdConnectionFailure(err) || b.retryableBdCatalogRace(err)
}

// matchesTransientMarkers applies the scoping every retry class shares: only a
// wrapper pointed at the test container, and never a subprocess killed at its
// own deadline — that one is wedged, not contended: bd had its whole budget and
// still said nothing, so retrying multiplies the wait for a failure that will
// repeat (gt-824d). A marker in the message is bd saying it never reached the
// command, which is what makes the retry safe and what an answer — ErrNotFound
// and behavioral refusals alike — is not.
func (b *Beads) matchesTransientMarkers(err error, markers []string) bool {
	if err == nil || !b.targetsTestDoltContainer() {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	msg := err.Error()
	for _, marker := range markers {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

// bdContainerRetryBackoff returns the pause before retry attempt attempt+1 —
// the caller passes the number of attempts already made, so 1 yields the base
// delay. Exponential with ±25% jitter and a hard cap, matching slingBackoff:
// parallel tests share one container, and unjittered retries would land in
// lockstep and re-create the contention that caused the failure.
func bdContainerRetryBackoff(attempt int) time.Duration {
	backoff := bdContainerRetryBaseBackoff
	for i := 1; i < attempt; i++ {
		backoff *= 2
		if backoff >= bdContainerRetryMaxBackoff {
			backoff = bdContainerRetryMaxBackoff
			break
		}
	}
	jitter := 1.0 + (rand.Float64()-0.5)*0.5 // range [0.75, 1.25]
	result := time.Duration(float64(backoff) * jitter)
	if result > bdContainerRetryMaxBackoff {
		result = bdContainerRetryMaxBackoff
	}
	return result
}

// bdContainerRetryBackoffFn is the pause before an attempt, as a var so the
// retry loop's tests can exercise the loop without waiting out real backoff —
// the same seam hookBeadWithRetryFn provides in internal/cmd.
var bdContainerRetryBackoffFn = bdContainerRetryBackoff

// runBdWithRetry runs one bd command (stdinData, args, runEnv as built by the
// caller's run path) and retries it while the failure looks like infrastructure
// rather than an answer: a lost connection to the test Dolt container, or its
// catalog changing under bd's store open. Retries stop at the first of: a
// failure that is neither, the attempt cap, or the retry window.
//
// Outside the container case the loop runs exactly once, so this is the same
// single invocation callers had before it existed. The last attempt's error is
// returned unchanged, so an exhausted retry reads as the failure it is, and
// every attempt is recorded by telemetry in runBdOnce.
func (b *Beads) runBdWithRetry(stdinData []byte, runEnv []string, args []string) ([]byte, error) {
	attempts := 1
	if b.targetsTestDoltContainer() {
		attempts = bdContainerRetryAttempts
	}
	deadline := time.Now().Add(bdContainerRetryWindow)

	var lastErr error
	for attempt := 1; ; attempt++ {
		out, err := b.runBdOnce(stdinData, runEnv, args)
		if err == nil {
			return out, nil
		}
		lastErr = err
		if !b.retryableBdTransientFailure(err) || attempt >= attempts {
			break
		}
		if time.Now().After(deadline) {
			// The container is not merely busy: the attempts so far have already
			// spent the retry window, so another one multiplies the wait without
			// changing the answer.
			break
		}
		// Say so on stderr: a retried gate must stay distinguishable from a
		// clean one, or the container's contention reads as fixed rather than
		// absorbed, and the next reader starts from a false premise.
		fmt.Fprintln(os.Stderr, retryNotice(attempt, attempts, err))
		time.Sleep(bdContainerRetryBackoffFn(attempt))
	}
	return nil, lastErr
}

// retryNotice renders the one-line warning for a failed attempt that another
// attempt follows. Only the failure's first line is quoted: the wrapper error
// repeats bd's full multi-line stderr, and five copies of it would bury the
// reason the log was written.
func retryNotice(attempt, attempts int, err error) string {
	first := err.Error()
	if idx := strings.IndexByte(first, '\n'); idx >= 0 {
		first = first[:idx]
	}
	return fmt.Sprintf("beads: bd call against the test Dolt container failed on attempt %d of %d, retrying: %s",
		attempt, attempts, first)
}

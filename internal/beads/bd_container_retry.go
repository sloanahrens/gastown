package beads

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/workspace"
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

// bdInitSchemaRaceMarkers are the stderr fragments that mean bd refused the
// schema era of the workspace it opened instead of answering the command
// (gt-w4sxk):
//
//	bd init --prefix pt42ed0697 --database testdb_1a13842eb92b6edf --server ...
//	Error: legacy Dolt workspace detected; explicit migration is required ...
//
//	bd init --prefix pkcf3e21b8 --database testdb_f5c50a8ddf8f5ea8 --server ...
//	refusing to auto-apply 11 pending schema migrations to a server-mode
//	database (v55 -> v66)
//
// On a name Init minted microseconds earlier (testDatabaseName) a legacy
// refusal has two sources. One is the catalog race one check past
// bdCatalogRaceMarkers: the open resolved a root and the era read off it
// belonged to another database, so an attempt that resolves again can land on
// the right one. The other is the loop's own leavings — a failed attempt writes
// <dir>/.beads with a Dolt root but no config, and that shape is exactly what
// the legacy guard refuses — so the retry re-reads the workspace the failure
// just wrote (gt-o8i9f). bdInitRetryReset clears the second before each
// attempt; without it this class is bd answering a workspace the loop created,
// which is what turned one attempt-1 catalog race into four more failures.
//
// Both refusals fire in bd's open path, before the command has any effect,
// which is what makes the retry safe for writes as well as reads.
//
// Unlike the classes above, this one is scoped to the command and the database
// name it may fire on, not just to the container: bd's era refusal is a real
// answer anywhere else.
var bdInitSchemaRaceMarkers = []string{
	"legacy Dolt workspace detected", // bd refused the era of the workspace it opened
	"refusing to auto-apply",         // bd refused to migrate a server-mode database
}

// bdInitOnTestDatabase reports whether args are the one command shape the
// schema-era class may retry: bd init against a database name Init minted for
// the shared test Dolt container (testDatabaseName).
//
// The gate is the name, because the name is what proves the era foreign. An
// init against a rig database on a production server can meet the same two
// messages for a reason an operator has to see, and there the era belongs to
// the database named; retrying would spend five attempts reporting an answer
// that had already arrived.
func bdInitOnTestDatabase(args []string) bool {
	if len(args) == 0 || args[0] != "init" {
		return false
	}
	for i, arg := range args {
		switch {
		case arg == "--database" && i+1 < len(args):
			return strings.HasPrefix(args[i+1], testDatabasePrefix)
		case strings.HasPrefix(arg, "--database="):
			return strings.HasPrefix(arg[len("--database="):], testDatabasePrefix)
		}
	}
	return false
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

// retryableBdInitSchemaRace reports whether err is bd refusing an init of a test
// Dolt container database on a schema era it read from another database. args
// are the command's own argv, which is what keeps the class inside the one shape
// where a foreign era is provable (bdInitOnTestDatabase).
func (b *Beads) retryableBdInitSchemaRace(args []string, err error) bool {
	if !bdInitOnTestDatabase(args) {
		return false
	}
	return b.matchesTransientMarkers(err, bdInitSchemaRaceMarkers)
}

// retryableBdTransientFailure is the class the retry loop rides on: a failure
// that is worth another attempt against the test container rather than an
// answer from bd.
func (b *Beads) retryableBdTransientFailure(args []string, err error) bool {
	return b.retryableBdConnectionFailure(err) ||
		b.retryableBdCatalogRace(err) ||
		b.retryableBdInitSchemaRace(args, err)
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
// rather than an answer: a lost connection to the test Dolt container, its
// catalog changing under bd's store open, or — on an init of a database it just
// minted — a schema era read off a workspace the attempt did not write. Retries
// stop at the first of: a failure that is none of those, the attempt cap, or the
// retry window.
//
// An init is the one command whose failure changes the directory it ran in, so
// its retry rebuilds the argv as well as re-running it: bdInitRetryReset clears
// the workspace the failed attempt left and re-mints the database name, and nil
// (every other command) leaves both alone.
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
	reset := newBdInitRetryReset(b, attempts, args)

	var lastErr error
	for attempt := 1; ; attempt++ {
		out, err := b.runBdOnce(stdinData, runEnv, args)
		if err == nil {
			return out, nil
		}
		lastErr = err
		if !b.retryableBdTransientFailure(args, err) || attempt >= attempts {
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
		args = reset.next(args)
	}
	return nil, lastErr
}

// bdInitRetryReset repairs what a failed bd init attempt left in the caller's
// directory, so the attempt that follows meets the directory attempt 1 did
// (gt-o8i9f).
//
// bd init writes <dir>/.beads and a Dolt root inside it before it writes the
// server-mode config that says what that root is. An attempt that dies in
// between — on the catalog race, on a lost connection — leaves a root with no
// config, and bd's own legacy_upgrade_guard reads that shape as a pre-migration
// workspace:
//
//	Error: legacy Dolt workspace detected; explicit migration is required ...
//
// which is what the attempt after it then re-reads: one transient race on
// attempt 1, a deterministic five-attempt failure on the gate.
//
// Between attempts this clears that workspace and re-mints the database name in
// argv, so the attempt cannot land on the half-made database the failed one left
// on the server either. Nothing else about the command changes.
//
// The snapshot is the safety property: only a .beads this reset watched appear
// is ever removed. A workspace that was already there belongs to whoever put it
// there, and deleting it is not this loop's business — that case keeps the
// behavior of retrying a directory no retry can fix.
type bdInitRetryReset struct {
	dir string // the <workDir>/.beads this loop may clear between attempts
}

// newBdInitRetryReset returns the reset for one retry sequence, or nil when the
// command is not the one shape it applies to: not an init of a minted testdb_
// name, not against the test container, no retry to make, or a workspace that
// was already there. A nil *bdInitRetryReset is the no-op state and next() is
// safe to call on it, which is how every other command reaches this loop
// unchanged.
func newBdInitRetryReset(b *Beads, attempts int, args []string) *bdInitRetryReset {
	if attempts < 2 || !bdInitOnTestDatabase(args) {
		return nil
	}
	// The path bd init writes to, derived exactly as the run path derives
	// BEADS_DIR. Anything else — a redirected workspace, a beadsDir pointed at
	// another directory — is not this loop's to delete, and neither is one under
	// the harness-forbidden live town.
	dir := b.getResolvedBeadsDir()
	if dir != filepath.Join(b.workDir, ".beads") {
		return nil
	}
	if err := workspace.GuardForbiddenRoot("bd init retry workspace reset", dir); err != nil {
		return nil
	}
	if _, err := os.Stat(dir); err == nil {
		return nil // pre-existing workspace: its owner's, not the failed attempt's
	} else if !os.IsNotExist(err) {
		return nil
	}
	return &bdInitRetryReset{dir: dir}
}

// next returns the argv for the attempt after a failure: the workspace the
// failed attempt created is cleared and the minted database name is replaced, so
// the next attempt starts from the state attempt 1 did.
func (r *bdInitRetryReset) next(args []string) []string {
	if r == nil {
		return args
	}
	if err := os.RemoveAll(r.dir); err != nil {
		// Without the removal, re-minting would only blur which workspace the
		// failure was read from, so this attempt runs as the failed one did.
		fmt.Fprintf(os.Stderr, "beads: could not clear the workspace a failed bd init left at %s, retrying as-is: %v\n", r.dir, err)
		return args
	}
	return remintTestDatabase(args)
}

// remintTestDatabase returns args with Init's minted testdb_ name replaced by a
// fresh one. Both flag spellings bdInitOnTestDatabase accepts are rewritten, so
// the predicate and the rewrite cannot disagree about which argv they mean.
func remintTestDatabase(args []string) []string {
	out := make([]string, len(args))
	copy(out, args)
	for i, arg := range args {
		switch {
		case arg == "--database" && i+1 < len(args):
			out[i+1] = testDatabaseName()
			return out
		case strings.HasPrefix(arg, "--database="):
			out[i] = "--database=" + testDatabaseName()
			return out
		}
	}
	return out
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

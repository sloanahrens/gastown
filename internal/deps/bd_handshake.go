package deps

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beadsql"
)

// KnownBDContractVersions are the bd JSON contract versions (bd version
// --json "contract_version", beads cmd/bd/envelope.go JSONContractVersion)
// this gt was written against. A bd reporting any other contract may have
// changed a payload gt parses, so the town does not run on it.
var KnownBDContractVersions = []int{1}

// ErrBDHandshake marks a refusal to run the town against the store it was
// pointed at.
var ErrBDHandshake = errors.New("bd handshake failed")

// ErrDoltUnreachable marks a handshake that passed every check of the bd behind
// the store and then could not read the database because the Dolt server did
// not answer. The bd is not the problem, so the operator's remedy is to start
// Dolt, not to reinstall bd.
var ErrDoltUnreachable = errors.New("Dolt is not reachable")

// ErrTrackerHandshake marks a refusal to run the town against a store whose
// route names the tracker backend. It is not an ErrBDHandshake: no bd took
// part, so the remedy is the tracker's, never a bd install.
var ErrTrackerHandshake = errors.New("tracker handshake failed")

// bdStoreRetryWait is the pause between attempts to read the database
// migration level while the store is unavailable (gt-2gctr).
const bdStoreRetryWait = 5 * time.Second

// handshakeClock is the handshake's source of the budget that ends it and of
// the pause between its retries. Production runs on the caller's context
// (ctxClock); a test injects a clock it ends itself, so a case that runs out of
// budget never depends on how fast the machine answered (gt-3hcp9).
type handshakeClock interface {
	// Done closes when the budget ends; nil is a budget with no end.
	Done() <-chan struct{}
	// Err is why Done closed, and nil while it is open.
	Err() error
	// After returns a channel that receives once d has passed. A channel that
	// never receives is a wait the budget ended first.
	After(d time.Duration) <-chan time.Time
}

// ctxClock is the caller's context read as the handshake's clock: the budget is
// its deadline or cancellation, and a retry pause is a timer.
type ctxClock struct{ ctx context.Context }

func (c ctxClock) Done() <-chan struct{}                  { return c.ctx.Done() }
func (c ctxClock) Err() error                             { return c.ctx.Err() }
func (c ctxClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// BDInstallHint is the only supported way to install bd. gastown never
// installs bd itself: the fork's safe-install checks it is building main and
// never moves the schema backwards.
const BDInstallHint = "install the beads fork's bd with `make safe-install` from the beads repository"

// Store is the part of a store the startup handshake reads: what the bd behind
// it says about itself, and the migration level of the database it serves.
//
// Build the store with the handshake's context (beads.WithCallContext), so the
// handshake's budget bounds the bd calls and not only the retry wait.
// SchemaLevel reports a read the budget ended while the store was silent as an
// error wrapping beads.ErrStoreUnavailable; any other read the budget ends is
// an error carrying context.DeadlineExceeded.
type Store interface {
	Version() (beads.VersionInfo, error)
	SchemaLevel() (int, error)
}

// NewHandshakeStore returns the store CheckBDHandshake reads for the town at
// townRoot: the town's own database, with the BEADS target variables stripped
// from the inherited environment, so a stale BEADS_DIR or BEADS_DOLT_* in the
// caller's shell cannot steer a startup gate to another database, and with
// ctx parenting every bd call. opts are appended for a test that swaps the bd
// those calls run through (beads.WithRunner); production passes none.
func NewHandshakeStore(ctx context.Context, townRoot string, opts ...beads.Option) Store {
	all := append([]beads.Option{
		beads.WithEnv(beads.StripBDTargetEnv(os.Environ())),
		beads.WithCallContext(ctx),
	}, opts...)
	return beads.New(townRoot, all...)
}

// BDHandshake is a bd that passed: its version report and the migration
// level of the database it serves, which are equal.
type BDHandshake struct {
	Path     string
	Found    beads.VersionInfo
	DBSchema int
}

// String summarizes the handshake for status output.
func (h *BDHandshake) String() string {
	return fmt.Sprintf("bd %s (build %s, schema<=%d, contract %d); database at %d",
		h.Found.Version, h.Found.BuildID, h.Found.DBSchemaVersion, h.Found.ContractVersion, h.DBSchema)
}

// CheckBDHandshake decides whether the town may run against the store. It
// requires a fork build (build_id present) whose JSON contract version this gt
// knows and whose schema level equals the migration level of the database it
// serves, and that level must be beadsql.SchemaVersion, the one gastown's SQL
// reads were written for. The database is read only after the version passes,
// waiting while the store is unavailable. Every refusal wraps ErrBDHandshake: a
// bd or schema problem names what was found, what is required and how to
// install bd, while a Dolt that did not answer wraps ErrDoltUnreachable and
// names Dolt instead; bdPath is for the message.
func CheckBDHandshake(ctx context.Context, bdPath string, store Store) (*BDHandshake, error) {
	return checkBDHandshake(ctx, bdPath, store, bdStoreRetryWait)
}

// checkBDHandshake is CheckBDHandshake with the wait between store-unavailable
// retries passed in, so a test drives the retry without spending it.
func checkBDHandshake(ctx context.Context, bdPath string, store Store, storeRetryWait time.Duration) (*BDHandshake, error) {
	return checkBDHandshakeClock(bdPath, store, storeRetryWait, ctxClock{ctx})
}

// checkBDHandshakeClock is the handshake with the clock it reads its budget
// from passed in: the budget end and the retry pause both come from clk.
func checkBDHandshakeClock(bdPath string, store Store, storeRetryWait time.Duration, clk handshakeClock) (*BDHandshake, error) {
	hs := &BDHandshake{Path: bdPath}
	info, err := store.Version()
	if err != nil {
		if errors.Is(err, beads.ErrNotInstalled) {
			return nil, refuseBD(hs, false, "bd not found on PATH")
		}
		// A version read the budget ended did not fail: bd never answered, so
		// the operator gets the budget, not the install hint.
		if clkErr := clk.Err(); clkErr != nil {
			return nil, refuseHandshakeBudget(hs, "the handshake budget ended before bd reported its version", clkErr)
		}
		return nil, refuseBD(hs, false, err.Error())
	}
	hs.Found = info

	var problems []string
	if info.BuildID == "" {
		problems = append(problems, "no build_id (not a beads fork build with the machine surface)")
	}
	if info.ContractVersion == 0 {
		problems = append(problems, "no contract_version")
	} else if !knownContract(info.ContractVersion) {
		problems = append(problems, fmt.Sprintf("contract %d is not one this gt knows (known: %s)", info.ContractVersion, knownContractList()))
	}
	if info.DBSchemaVersion == 0 {
		problems = append(problems, "no db_schema_version")
	}
	if len(problems) > 0 {
		return nil, refuseBD(hs, false, strings.Join(problems, "; "))
	}

	level, err := readSchemaLevelWaiting(store, storeRetryWait, clk)
	if err != nil {
		if errors.Is(err, beads.ErrStoreUnavailable) {
			return nil, refuseDoltUnreachable(hs, err)
		}
		return nil, refuseBD(hs, false, err.Error())
	}
	hs.DBSchema = level
	if level != info.DBSchemaVersion {
		return nil, refuseBD(hs, true, fmt.Sprintf("bd migrates to schema<=%d but the database is at %d", info.DBSchemaVersion, level))
	}
	// gastown's own SQL reads (internal/beadsql) and the ones it sends
	// through bd sql are written for one schema level.
	if level != beadsql.SchemaVersion {
		return nil, refuseBD(hs, true, fmt.Sprintf("the database is at schema %d but this gt's SQL reads are written for %d (beadsql.SchemaVersion)", level, beadsql.SchemaVersion))
	}
	return hs, nil
}

// Tracker is the tracker's part of the startup handshake for a store whose
// route names the tracker backend: it names itself for a refusal and answers
// one readiness read, which carries the schema level trackerd reports.
// internal/cmd asks the town's endpoint over /readyz, which takes no token; a
// test passes its own.
type Tracker interface {
	// URL names the tracker, without credentials.
	URL() string
	// Readyz reports the tracker ready at the schema level its store is at. A
	// tracker that did not answer, or answered that it is not ready, is an
	// error.
	Readyz(ctx context.Context) (schemaLevel int, err error)
}

// CheckTrackerHandshake decides whether the town may run against trackerd, the
// store a tracker route names. No bd build and no Dolt database are behind such
// a route, so the bd checks do not apply: the town may run when the tracker
// answers ready at the schema level this gt speaks, which the caller passes as
// wantLevel (internal/cmd passes tracker/api.SchemaLevel; this package holds no
// tracker client). Every refusal wraps ErrTrackerHandshake and names the
// tracker and its URL.
func CheckTrackerHandshake(ctx context.Context, tr Tracker, wantLevel int) error {
	level, err := tr.Readyz(ctx)
	if err != nil {
		return refuseTracker(tr.URL(), err.Error())
	}
	if level != wantLevel {
		return refuseTracker(tr.URL(), fmt.Sprintf("the tracker is at schema level %d but this gt speaks level %d", level, wantLevel))
	}
	return nil
}

// TrackerStatusCommand is the gt command the remedy names, so a test can pin
// that it is a command in the tree rather than a name from memory.
const TrackerStatusCommand = "gt tracker status"

// trackerFixHint is the remedy a tracker refusal names: trackerd serves from
// Postgres, so the bd install hint would send the operator down the wrong path.
// TestTrackerStatusRemedyNamesACommandInTheTree (internal/cmd) pins the command.
const trackerFixHint = "check the tracker with `" + TrackerStatusCommand + "`, and that trackerd and its Postgres are running"

// refuseTracker is the refusal for a store on the tracker, where the bd
// messages' "install bd" remedy would be wrong.
func refuseTracker(url, reason string) error {
	return fmt.Errorf("%w: %s\n  found:    tracker at %s\n  fix:      %s", ErrTrackerHandshake, reason, url, trackerFixHint)
}

// readSchemaLevelWaiting reads the database migration level, waiting out a
// Dolt server that has not started answering yet.
//
// A cold boot can start the daemon before Dolt accepts connections, and this
// read is the one that fails, with bd's store_unavailable exit (gt-2gctr), so
// that exit is what the wait keys on. It is bounded by the handshake's budget,
// and every other failure — a schema skew, a bd that will not run — returns at
// once.
func readSchemaLevelWaiting(store Store, retryWait time.Duration, clk handshakeClock) (int, error) {
	for {
		level, err := store.SchemaLevel()
		if err == nil || !errors.Is(err, beads.ErrStoreUnavailable) {
			return level, err
		}
		if !waitForStoreRetry(clk, retryWait) {
			return 0, err
		}
	}
}

// waitForStoreRetry waits one retry interval and reports whether the caller may
// try again: false means the clock's budget ended first.
func waitForStoreRetry(clk handshakeClock, wait time.Duration) bool {
	select {
	case <-clk.Done():
		return false
	case <-clk.After(wait):
		return true
	}
}

// refuseDoltUnreachable is the refusal when Dolt, not bd, is what failed: the
// version report passed, so naming the bd install would send the operator down
// the wrong path (gt-2gctr). The cause keeps the store's own failure for the
// log.
func refuseDoltUnreachable(hs *BDHandshake, cause error) error {
	bdPath := hs.Path
	if bdPath == "" {
		bdPath = "bd"
	}
	return fmt.Errorf("%w: %w: %w\n  found:    bd at %s passed its version check, so the bd on PATH is not the problem\n  fix:      check Dolt with `gt dolt status`, then start it with `gt dolt start`",
		ErrBDHandshake, ErrDoltUnreachable, cause, bdPath)
}

// refuseHandshakeBudget is the refusal when the caller's budget ended before
// bd answered. bd is not known to be wrong, so the message names the budget and
// the thing to check instead of sending the operator to reinstall it.
func refuseHandshakeBudget(hs *BDHandshake, what string, cause error) error {
	bdPath := hs.Path
	if bdPath == "" {
		bdPath = "bd"
	}
	return fmt.Errorf("%w: %s (%v)\n  found:    bd at %s did not answer\n  fix:      check that bd runs, or raise the check's timeout",
		ErrBDHandshake, what, cause, bdPath)
}

func refuseBD(hs *BDHandshake, dbRead bool, reason string) error {
	found := "bd at " + hs.Path
	if hs.Path == "" {
		found = "bd (not on PATH)"
	}
	if hs.Found.Version != "" {
		f := hs.Found
		found += fmt.Sprintf(": version %s, build %s, schema<=%s, contract %s",
			f.Version, orUnknown(firstNonEmpty(f.BuildID, f.Build)), intOrUnknown(f.DBSchemaVersion), intOrUnknown(f.ContractVersion))
	}
	if dbRead {
		found += fmt.Sprintf("; database at %d", hs.DBSchema)
	}
	return fmt.Errorf("%w: %s\n  found:    %s\n  required: a beads fork build with contract %s whose schema level equals the database migration level, at schema %d\n  fix:      %s",
		ErrBDHandshake, reason, found, knownContractList(), beadsql.SchemaVersion, BDInstallHint)
}

func knownContract(v int) bool {
	for _, k := range KnownBDContractVersions {
		if k == v {
			return true
		}
	}
	return false
}

func knownContractList() string {
	vs := append([]int(nil), KnownBDContractVersions...)
	sort.Ints(vs)
	parts := make([]string, len(vs))
	for i, v := range vs {
		parts[i] = strconv.Itoa(v)
	}
	return strings.Join(parts, " or ")
}

func intOrUnknown(n int) string {
	if n == 0 {
		return "?"
	}
	return strconv.Itoa(n)
}

func orUnknown(s string) string {
	if s == "" {
		return "?"
	}
	return s
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

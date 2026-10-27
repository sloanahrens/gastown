package deps

import (
	"context"
	"errors"
	"fmt"
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

// bdStoreRetryWait is the pause between attempts to read the database
// migration level while the store is unavailable (gt-2gctr).
const bdStoreRetryWait = 5 * time.Second

// BDInstallHint is the only supported way to install bd. gastown never
// installs bd itself: the fork's safe-install checks it is building main and
// never moves the schema backwards.
const BDInstallHint = "install the beads fork's bd with `make safe-install` from the beads repository"

// Store is the part of a store the startup handshake reads: what the bd behind
// it says about itself, and the migration level of the database it serves.
//
// Build the store with the handshake's context (beads.WithCallContext), so the
// handshake's budget bounds the bd calls and not only the retry wait; a read
// the budget ended while the store was silent is an error wrapping
// beads.ErrStoreUnavailable.
type Store interface {
	Version() (beads.VersionInfo, error)
	SchemaLevel() (int, error)
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
	hs := &BDHandshake{Path: bdPath}
	info, err := store.Version()
	if err != nil {
		if errors.Is(err, beads.ErrNotInstalled) {
			return nil, refuseBD(hs, false, "bd not found on PATH")
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

	level, err := readSchemaLevelWaiting(ctx, store, storeRetryWait)
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

// readSchemaLevelWaiting reads the database migration level, waiting out a
// Dolt server that has not started answering yet.
//
// A cold boot can start the daemon before Dolt accepts connections, and this
// read is the one that fails, with bd's store_unavailable exit (gt-2gctr), so
// that exit is what the wait keys on. It is bounded by the caller's budget, and
// every other failure — a schema skew, a bd that will not run — returns at
// once.
func readSchemaLevelWaiting(ctx context.Context, store Store, retryWait time.Duration) (int, error) {
	for {
		level, err := store.SchemaLevel()
		if err == nil || !errors.Is(err, beads.ErrStoreUnavailable) {
			return level, err
		}
		if !waitForStoreRetry(ctx, retryWait) {
			return 0, err
		}
	}
}

// waitForStoreRetry waits one retry interval and reports whether the caller
// may try again: false means ctx ended first, which is the whole budget the
// caller gave the handshake.
func waitForStoreRetry(ctx context.Context, wait time.Duration) bool {
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
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

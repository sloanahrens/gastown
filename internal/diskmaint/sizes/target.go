// Package sizes reports how big the backups the operator decides about are: the
// Dolt .bak directories, the cache-trash directories and gt-backups. It is
// report-only. It lists each path with its size and age and never deletes or
// moves anything, until the operator sets a retention.
package sizes

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/diskmaint"
)

// planTarget is the Target name a plan and a ledger entry carry.
const planTarget = "sizes"

// ruleOwner names who decides about every path this target lists.
const ruleOwner = "owner: operator"

// deferredReason is the Result.Deferred text Apply returns: nothing was applied.
const deferredReason = "report-only target: nothing is deleted until the operator sets a retention"

// errNoLedger reports a target built without a ledger. Every apply must leave a
// ledger line, so an apply with nowhere to record it refuses to run.
var errNoLedger = errors.New("sizes apply: no ledger configured")

// errEmptyRoot reports a Config whose Town or Home is blank. Listing from a blank
// root reads the working directory, so a plan or measure refuses to run.
var errEmptyRoot = errors.New("sizes: town and home must both be set")

// Compile-time check that the sizes target satisfies the diskmaint contract.
var _ diskmaint.Target = (*Target)(nil)

// Config is what the sizes target needs to measure and plan.
type Config struct {
	// Town is the town root whose Dolt .bak and cache-trash directories are listed.
	Town string
	// Home is the operator's home directory, which holds gt-backups.
	Home string
	// Run runs du -sk on each path.
	Run diskmaint.Runner
	// Now is the clock ages are read against; nil uses the system clock.
	Now func() time.Time
	// Ledger records every apply; Apply refuses to run without one.
	Ledger *diskmaint.Ledger
}

// Target reports the size and age of the town's backup paths.
type Target struct {
	cfg Config
}

// NewTarget returns a Target that reads the paths named by cfg.
func NewTarget(cfg Config) *Target {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Target{cfg: cfg}
}

// Name is the target's name in plans and ledger entries.
func (t *Target) Name() string { return planTarget }

// row is one matching path as the target read it. A failed read leaves its
// field unset and its flag false, so the path is reported as unreadable rather
// than given a guessed value.
type row struct {
	path   string
	bytes  int64
	sizeOK bool
	age    time.Duration
	ageOK  bool
}

// rule is the Kept rule for a row: its size, its age in whole days, and who
// owns it. An unreadable size or age says so in its place.
func (r row) rule() string {
	size := "unreadable"
	if r.sizeOK {
		size = humanBytes(r.bytes)
	}
	age := "age unreadable"
	if r.ageOK {
		days := max(int(r.age/(24*time.Hour)), 0)
		age = fmt.Sprintf("%dd old", days)
	}
	return fmt.Sprintf("report-only: %s, %s, %s", size, age, ruleOwner)
}

// Measure reports the bytes the listed paths hold. A path whose size cannot be
// read makes the measurement an error naming it, because a total that leaves a
// path out would be a guess.
func (t *Target) Measure(ctx context.Context) (diskmaint.Measurement, error) {
	rows, err := t.scan(ctx)
	if err != nil {
		return diskmaint.Measurement{}, fmt.Errorf("measure: %w", err)
	}
	var used int64
	var unreadable []string
	for _, r := range rows {
		if !r.sizeOK {
			unreadable = append(unreadable, r.path)
			continue
		}
		used += r.bytes
	}
	if len(unreadable) > 0 {
		return diskmaint.Measurement{}, fmt.Errorf("measure: size unreadable for %s", strings.Join(unreadable, ", "))
	}
	return diskmaint.Measurement{
		Used:   used,
		Detail: fmt.Sprintf("%d paths holding %s", len(rows), humanBytes(used)),
	}, nil
}

// Plan lists every matching path as a kept ref whose rule carries its size and
// age. Plan.Actions is always empty: nothing in this target is removed. A
// path that cannot be read is listed as unreadable and does not fail the plan.
func (t *Target) Plan(ctx context.Context, _ diskmaint.Policy) (diskmaint.Plan, error) {
	rows, err := t.scan(ctx)
	if err != nil {
		return diskmaint.Plan{}, fmt.Errorf("plan: %w", err)
	}
	plan := diskmaint.Plan{Target: planTarget}
	for _, r := range rows {
		plan.Kept = append(plan.Kept, diskmaint.Kept{Ref: r.path, Rule: r.rule()})
	}
	return plan, nil
}

// Apply records the run and deletes nothing. It appends one ledger line that
// counts the rows the plan listed, with no bytes freed, and returns a Result
// deferred with the report-only reason.
func (t *Target) Apply(_ context.Context, plan diskmaint.Plan) (diskmaint.Result, error) {
	result := diskmaint.Result{Target: planTarget, Deferred: deferredReason}
	if t.cfg.Ledger == nil {
		return result, errNoLedger
	}
	err := t.cfg.Ledger.Append(diskmaint.LedgerEntry{
		TS:       t.cfg.Now().UTC(),
		Target:   planTarget,
		Mode:     "apply",
		Planned:  len(plan.Kept),
		Deferred: deferredReason,
	})
	if err != nil {
		return result, fmt.Errorf("record the apply: %w", err)
	}
	return result, nil
}

// scan reads the size and age of every listed path, in path order.
func (t *Target) scan(ctx context.Context) ([]row, error) {
	paths, err := t.paths()
	if err != nil {
		return nil, err
	}
	rows := make([]row, 0, len(paths))
	for _, path := range paths {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		r := row{path: path}
		if info, err := os.Stat(path); err == nil {
			r.age = t.cfg.Now().Sub(info.ModTime())
			r.ageOK = true
		}
		// The error is dropped on purpose: a runner's error carries its stderr,
		// and the row needs only the fact that the size could not be read.
		if out, err := t.cfg.Run(ctx, nil, "du", "-sk", path); err == nil {
			r.bytes, r.sizeOK = duBytes(out)
		}
		rows = append(rows, r)
	}
	return rows, nil
}

// paths lists the paths the target reads, sorted: the Dolt .bak and cache-trash
// directories directly under the town, and gt-backups under home when it exists.
// A blank Town or Home is refused, because it would join to a relative path and
// list the working directory. A town that exists but cannot be read fails the
// plan, where it used to list nothing; a town that does not exist lists nothing.
// A gt-backups that cannot be stat'd for a reason other than absence is still
// listed, so its row says unreadable.
func (t *Target) paths() ([]string, error) {
	if t.cfg.Town == "" || t.cfg.Home == "" {
		return nil, errEmptyRoot
	}
	var out []string
	for _, prefix := range []string{".dolt-data.bak-", ".dolt-remote-cache-trash-"} {
		matches, err := listPrefixed(t.cfg.Town, prefix)
		if err != nil {
			return nil, fmt.Errorf("list town: %w", err)
		}
		out = append(out, matches...)
	}
	backups := filepath.Join(t.cfg.Home, "gt-backups")
	if _, err := os.Stat(backups); !errors.Is(err, fs.ErrNotExist) {
		out = append(out, backups)
	}
	slices.Sort(out)
	return out, nil
}

// listPrefixed returns the entries of dir whose names start with prefix, joined
// to dir. The directory is read by name, not matched as a glob, so a dir whose
// own path holds glob characters is read as text. A dir that does not exist
// lists nothing; any other read error is returned.
func listPrefixed(dir, prefix string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), prefix) {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	return out, nil
}

// duBytes reads the size du -sk prints: a count of 1024-byte blocks, then the
// path. A count that does not parse, or is negative, is not a size.
func duBytes(out []byte) (int64, bool) {
	fields := strings.Fields(string(out))
	if len(fields) == 0 {
		return 0, false
	}
	kb, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil || kb < 0 {
		return 0, false
	}
	return kb * 1024, true
}

// humanBytes formats n in binary units, one decimal place above a kibibyte.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

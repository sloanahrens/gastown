package sweeprunner

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/atomicfile"
)

// ResultVersion numbers the result.json format this package writes. Read
// refuses any other number: an unknown version's fields mean nothing it can
// check (gt-u00wo).
const ResultVersion = 1

// ResultFileName is the result's name in the run's output directory: the host
// directory Spec.OutDir names, which the runner mounts at OutputDir, and which
// the daemon polls (gt-u00wo).
const ResultFileName = "result.json"

// maxResultBytes bounds what Read loads from a result file. A result is a few
// KB; a file larger than this at the name is not one this package wrote, and
// reading it whole would let a stray or hostile file cost the daemon its memory
// (gt-u00wo).
const maxResultBytes = 1 << 20

// Result is one sweep run's outcome: what ran, at which commit, in what, and
// the summary line each tier printed.
type Result struct {
	// Version is the file's format, stamped by Write so a caller cannot write
	// one Read refuses.
	Version int `json:"version"`
	// Rig is the rig whose sweep ran.
	Rig string `json:"rig"`
	// SHA is the commit the run tested.
	SHA string `json:"sha"`
	// Started and Finished bound the run, written in RFC 3339.
	Started  time.Time `json:"started"`
	Finished time.Time `json:"finished"`
	// Runner is the engine and limits the run ran under.
	Runner Runner `json:"runner"`
	// Tiers are the tiers the run tested, in the order it ran them.
	Tiers []Tier `json:"tiers"`
}

// Runner names the backend, image and CPU count a run ran under.
type Runner struct {
	Backend string `json:"backend"`
	Image   string `json:"image"`
	CPUs    int    `json:"cpus"`
}

// Tier is one tier the run tested: the exact summary line the sweep printed for
// it, which is the only place its counts live.
type Tier struct {
	Tier string `json:"tier"`
	// Summary is the tier's line as printed, e.g.
	// "tier-sweep: shell GREEN passed=5 failed=0 skipped=0 (logs /out/logs) in 45s".
	Summary string `json:"summary"`
	// Command is the tier's command in the runner's tree, omitted when the
	// runner recorded none.
	Command string `json:"command,omitempty"`
}

// TierResult is one tier's verdict in the shape internal/daemon's state file
// holds it (tierSweepTierResult, internal/daemon/tier_sweep.go), so a read
// result converts without the dashboard, the attention items or promotion
// noticing where it came from.
type TierResult struct {
	Verdict     string   `json:"verdict"`
	Passed      int      `json:"passed"`
	Failed      int      `json:"failed"`
	FailedNames []string `json:"failed_names"`
	// Log holds the line's "(logs <dir>)" group, which is the directory the
	// sweep wrote each unit's own log into, not a file. The daemon's own parser
	// puts that directory in both Log and LogDir, and its caller later replaces
	// Log with the path of the stage log it writes; a runner record reaches the
	// state file without that replacement, so its Log stays the directory
	// (internal/daemon/tier_sweep.go, parseTierSweepSummaries).
	Log string `json:"log,omitempty"`
	// LogDir is the same directory. The dashboard and a slowcheck finding
	// reach the unit logs through this key, so it is kept beside Log rather
	// than derived from it.
	LogDir string `json:"log_dir,omitempty"`
	// Command is the command the tier ran, when the result recorded one.
	Command string `json:"command,omitempty"`
	// SlotWaitSeconds is how long a tier's stage waited for a container-gate
	// slot. A runner holds its own reservation and waits on no gate, so it
	// records none and the key stays out of its JSON (gt-u00wo).
	SlotWaitSeconds int `json:"slot_wait_seconds,omitempty"`
	// SHA is the commit the verdict was taken at.
	SHA string `json:"sha,omitempty"`
}

// tierSummaryRE is a copy of the daemon's tierSweepSummaryRE
// (internal/daemon/tier_sweep.go), which parses the line a sweep prints for a
// tier. The daemon does not export it, so the copy lives here; a test reads the
// daemon's own expression out of its source and compares the two, so a change
// there that this copy does not follow fails the test.
//
//	tier-sweep: shell RED passed=5 failed=1 skipped=0 failed: scripts/x.sh (logs /out/logs) in 2m14s
var tierSummaryRE = regexp.MustCompile(`^tier-sweep: (\S+) (GREEN|RED) passed=(\d+) failed=(\d+) skipped=(\d+)(?: failed:(.*?))?(?: \(logs ([^)]*)\))?(?: in (\S+))?\s*$`)

// parseTierSummary reads one tier's summary line into the daemon's shape and
// reports whether the line matched.
func parseTierSummary(line string) (tier string, result TierResult, ok bool) {
	m := tierSummaryRE.FindStringSubmatch(strings.TrimRight(line, "\r"))
	if m == nil {
		return "", TierResult{}, false
	}
	passed, _ := strconv.Atoi(m[3])
	failed, _ := strconv.Atoi(m[4])
	return m[1], TierResult{
		Verdict:     m[2],
		Passed:      passed,
		Failed:      failed,
		FailedNames: strings.Fields(m[6]),
		Log:         m[7],
		LogDir:      m[7],
	}, true
}

// ToTierResults converts a result to the per-tier verdicts the daemon's state
// file holds, keyed as the daemon keys them, so the existing dashboard,
// attention items and promotion keep working.
//
// It refuses a tier it cannot convert instead of leaving it out: a tier
// dropped from the map would read to the daemon as a tier that never ran, so a
// partial conversion is a wrong answer, not a smaller one (gt-u00wo). The rules
// are the ones Write and Read apply, so a file they accept always converts.
func ToTierResults(r Result) (map[string]TierResult, error) {
	out, err := tierResults(r)
	if err != nil {
		return nil, fmt.Errorf("sweeprunner: convert result: %w", err)
	}
	return out, nil
}

// tierResults converts r's tiers, refusing any it cannot convert: a tier left
// out of the map would read to the daemon as a tier that never ran. Write and
// Read validate through here too, so a file they accept converts.
func tierResults(r Result) (map[string]TierResult, error) {
	// A run that tested no tier says nothing about the tree, and converting it
	// would hand the daemon an empty verdict map that reads as a clean sweep of
	// nothing (gt-u00wo).
	if len(r.Tiers) == 0 {
		return nil, errors.New("names no tiers")
	}
	out := make(map[string]TierResult, len(r.Tiers))
	for i, t := range r.Tiers {
		if t.Tier == "" {
			return nil, fmt.Errorf("tier %d has no name", i)
		}
		// The tier names a key in the daemon's state file, and the summary and
		// command are free text that reach its dashboard: a tier name that is
		// not one path segment, or a field holding a control character, is a
		// value this package did not write and will not pass on (gt-u00wo).
		if !nameSegment(t.Tier) {
			return nil, fmt.Errorf("tier %d (%q) is not a name: one path segment, with no whitespace or control character", i, t.Tier)
		}
		if hasControl(t.Summary) {
			return nil, fmt.Errorf("tier %q: summary line holds a control character", t.Tier)
		}
		if hasControl(t.Command) {
			return nil, fmt.Errorf("tier %q: command holds a control character", t.Tier)
		}
		name, result, ok := parseTierSummary(t.Summary)
		if !ok {
			return nil, fmt.Errorf("tier %q: summary line does not parse", t.Tier)
		}
		if name != t.Tier {
			return nil, fmt.Errorf("tier %q: summary line names %q", t.Tier, name)
		}
		if err := validateTierResult(t.Tier, result); err != nil {
			return nil, err
		}
		// A tier key names one verdict in the daemon's state file, so a second
		// tier with the same name would replace the first without either being
		// refused (gt-u00wo).
		if _, dup := out[name]; dup {
			return nil, fmt.Errorf("tier %q appears twice", name)
		}
		result.Command = t.Command
		result.SHA = r.SHA
		out[name] = result
	}
	return out, nil
}

// Write records r as result.json under dir, replacing any earlier file in one
// rename so a reader sees a whole result or none.
func Write(dir string, r Result) error {
	r.Version = ResultVersion
	if err := validateResult(r); err != nil {
		return err
	}
	path := filepath.Join(dir, ResultFileName)
	if err := atomicfile.WriteJSON(path, r); err != nil {
		return fmt.Errorf("sweeprunner: write result %s: %w", path, err)
	}
	return nil
}

// Read reads the result in dir and refuses one that is missing, torn, of an
// unknown version, or not the rig and commit the caller expected.
func Read(dir, wantRig, wantSHA string) (Result, error) {
	path := filepath.Join(dir, ResultFileName)
	data, err := readResultFile(path)
	if err != nil {
		return Result{}, err
	}
	// The version is read before the rest. A file from a later format may hold
	// fields of other types, and decoding those first would report a schema
	// change as a torn file, which is a different thing to investigate
	// (gt-u00wo).
	var head struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(data, &head); err != nil {
		return Result{}, fmt.Errorf("sweeprunner: read result %s: not a complete JSON document: %w", path, err)
	}
	if head.Version != ResultVersion {
		return Result{}, fmt.Errorf("sweeprunner: read result %s: version %d, want %d", path, head.Version, ResultVersion)
	}
	var r Result
	if err := json.Unmarshal(data, &r); err != nil {
		return Result{}, fmt.Errorf("sweeprunner: read result %s: version %d does not decode: %w", path, head.Version, err)
	}
	if r.Rig != wantRig {
		return Result{}, fmt.Errorf("sweeprunner: read result %s: rig %q, want %q", path, r.Rig, wantRig)
	}
	if r.SHA != wantSHA {
		return Result{}, fmt.Errorf("sweeprunner: read result %s: sha %q, want %q", path, r.SHA, wantSHA)
	}
	if err := validateResult(r); err != nil {
		return Result{}, fmt.Errorf("sweeprunner: read result %s: %w", path, err)
	}
	return r, nil
}

// readResultFile loads the result at path, at most maxResultBytes of it. A
// missing file stays tellable apart from an unreadable one through errors.Is:
// the daemon polls for the file, so "not there yet" must not read as a fault.
//
// The output directory is written by the container the sweep runs, which is the
// untrusted side of the boundary, so a result is only ever a regular file: a
// symlink there would be followed wherever it points, and a FIFO would block
// the open until a writer appeared (gt-u00wo).
func readResultFile(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, fmt.Errorf("sweeprunner: read result: no result file at %s: %w", path, err)
	case err != nil:
		return nil, fmt.Errorf("sweeprunner: read result %s: %w", path, err)
	case !info.Mode().IsRegular():
		return nil, notRegularFile(path, info.Mode())
	}
	// Lstat refuses what is there now, and the open refuses what is swapped in
	// over the same name between that check and this one: O_NOFOLLOW refuses a
	// symlink, and O_NONBLOCK keeps a FIFO from holding the open until a writer
	// appears (the read then fails at once rather than never returning). Read
	// only ever reads a file this package published by rename (gt-u00wo).
	f, err := os.OpenFile(path, os.O_RDONLY|oNoFollow|oNonBlock, 0) //nolint:gosec // G304: the caller's own run directory plus ResultFileName
	if err != nil {
		return nil, fmt.Errorf("sweeprunner: read result %s: %w", path, err)
	}
	defer f.Close()
	data, readErr := io.ReadAll(io.LimitReader(f, maxResultBytes+1))
	if readErr != nil || len(data) > maxResultBytes {
		if readErr == nil {
			readErr = fmt.Errorf("larger than %d bytes", maxResultBytes)
		}
		return nil, fmt.Errorf("sweeprunner: read result %s: %w", path, readErr)
	}
	return data, nil
}

// notRegularFile names what sits at the result path. A symlink and a FIFO are
// the two that matter: the first is a read outside the run's output, the second
// is an open that never returns.
func notRegularFile(path string, mode fs.FileMode) error {
	kind := "not a regular file"
	switch {
	case mode&fs.ModeSymlink != 0:
		kind = "a symlink"
	case mode&fs.ModeNamedPipe != 0:
		kind = "a FIFO"
	case mode&fs.ModeDir != 0:
		kind = "a directory"
	case mode&fs.ModeDevice != 0:
		kind = "a device"
	}
	return fmt.Errorf("sweeprunner: read result %s: %s; refusing to read it", path, kind)
}

// validateResult refuses a result Read or ToTierResults could not use, naming
// the field that is wrong. Write and Read share it, so a result Write accepts is
// one Read returns.
func validateResult(r Result) error {
	if !nameSegment(r.Rig) {
		return fmt.Errorf("rig %q is not one path segment", r.Rig)
	}
	if !shaPattern.MatchString(r.SHA) {
		return fmt.Errorf("sha %q is not 40 lower-case hex digits", r.SHA)
	}
	switch {
	case r.Started.IsZero():
		return errors.New("started is missing")
	case r.Finished.IsZero():
		return errors.New("finished is missing")
	case r.Finished.Before(r.Started):
		return fmt.Errorf("finished %s is before started %s", r.Finished.Format(time.RFC3339), r.Started.Format(time.RFC3339))
	}
	if r.Runner.Backend == "" {
		return errors.New("runner.backend is missing")
	}
	if r.Runner.Image == "" {
		return errors.New("runner.image is missing")
	}
	if r.Runner.CPUs < 1 {
		return fmt.Errorf("runner.cpus %d is below 1", r.Runner.CPUs)
	}
	_, err := tierResults(r)
	return err
}

// validateTierResult refuses a parsed tier whose verdict contradicts its own
// counts, or whose log path the daemon could not safely open.
func validateTierResult(tier string, result TierResult) error {
	// A line reporting failures under a GREEN verdict is not a verdict anyone
	// can act on. The reverse is left alone: the sweep reports a tier that
	// could not run as RED with no failing tests (internal/tiersweep).
	if result.Verdict == "GREEN" && result.Failed > 0 {
		return fmt.Errorf("tier %q is GREEN with %d failed", tier, result.Failed)
	}
	// Log and LogDir carry the one "(logs <dir>)" group, and the daemon joins
	// that directory with unit log names, so a path that climbs or a relative
	// one would leave the run's own output (gt-u00wo). Only shape is checked:
	// the line prints the path inside the container, and the result records no
	// other reference to the run's log root to compare it against.
	if err := validateLogDir(result.LogDir); err != nil {
		return fmt.Errorf("tier %q: log directory %q %w", tier, result.LogDir, err)
	}
	return nil
}

// validateLogDir refuses a log directory taken from a summary line that is not
// an absolute, already-clean path. An empty one is allowed: the group is
// optional in a line an older sweep printed.
func validateLogDir(dir string) error {
	if dir == "" {
		return nil
	}
	if !filepath.IsAbs(dir) {
		return errors.New("is not absolute")
	}
	if filepath.Clean(dir) != dir {
		return errors.New("is not a clean path")
	}
	return nil
}

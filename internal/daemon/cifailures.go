package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/land"
)

// LabelFlakyTest marks the P1 bead the daemon files for a test that keeps
// failing for beads that did not touch it.
const LabelFlakyTest = "flaky-test"

// ciFailureWindow is how far back a failure counts toward the repeat rule: a
// test failing on two different beads inside it is the daemon's to escalate.
const ciFailureWindow = 24 * time.Hour

// ciFailureRetention is how long the ledger keeps a record. It covers the
// cause battery's fourteen-day will_recur window; failcause.CIFailureRetention
// must match it.
const ciFailureRetention = 14 * 24 * time.Hour

// ciFailure is one line of the ledger: one failing (package, test) of one
// bead's candidate, or one unparsed failure that named no test.
type ciFailure struct {
	Time     time.Time `json:"time"`
	Rig      string    `json:"rig"`
	Bead     string    `json:"bead"`
	Package  string    `json:"package,omitempty"`
	Test     string    `json:"test,omitempty"`
	Unparsed bool      `json:"unparsed,omitempty"`
}

// ciFailureLedgerPath is the town's CI-failure ledger: one JSON line per
// failure, pruned to the last week.
func ciFailureLedgerPath(townRoot string) string {
	return filepath.Join(townRoot, ".runtime", "ci-failures.jsonl")
}

// failingTestBeads is the bead store the watch files repair beads in.
type failingTestBeads interface {
	List(opts beads.ListOptions) ([]*beads.Issue, error)
	Create(opts beads.CreateOptions) (*beads.Issue, error)
	AddComment(id, text string) error
}

// mainVerdict is what a red-main status says about one package on main. Only
// mainGreen suppresses a repair: an unknown main files, because a suppressed
// real failure leaves main red (gt-42ulv).
type mainVerdict int

const (
	// mainUnknown is a status that is missing, unreadable, or names no verdict
	// this watch can read.
	mainUnknown mainVerdict = iota
	// mainGreen is a status that does not list the package under RED.
	mainGreen
	// mainRed is a status that lists the package under RED.
	mainRed
)

// ciTestWatch records every CI-gate failure a rig's landing worker sees and,
// when one package's test has failed on two different beads inside a day and
// main is not green for it, raises one escalation keyed on that test and files
// one repair bead for it (gt-xvw20, gt-42ulv). It reruns nothing: a red gate is
// a verdict, and a test that fails on beads that did not touch it is a test to
// fix or remove.
type ciTestWatch struct {
	// Rig is the rig whose landing worker reports, and the scope a repeat is
	// counted in: another rig's package is a different test.
	Rig string
	// Ledger is the jsonl file every rig records its failures in.
	Ledger string
	// Beads files and finds the repair beads.
	Beads failingTestBeads
	// Escalate raises the alert under a fingerprint keyed on the test, so a
	// repeat upserts onto the one alert rather than minting a new one.
	Escalate func(key, message string)
	Logf     func(format string, args ...any)
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// Async runs the escalation and the bead filing off the caller's goroutine,
	// which is the landing's: a rejection must not wait on a retrying alert.
	// nil runs them inline, which is what a test wants.
	Async func(call func())
	// MainStatus reports what the rig's latest main status says about the
	// package, so a test failing for two beads that passes on main is not filed
	// as one to repair (gt-42ulv). nil is an unread main status, which files.
	MainStatus func(rig, pkg string) mainVerdict
}

// ciWindowText is ciFailureWindow as the escalation, the bead and the log name
// it: one wording for one span.
func ciWindowText() string {
	return fmt.Sprintf("%.0f hours", ciFailureWindow.Hours())
}

func (w *ciTestWatch) logf(format string, args ...any) {
	if w.Logf != nil {
		w.Logf(format, args...)
	}
}

func (w *ciTestWatch) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}

func (w *ciTestWatch) run(call func()) {
	if w.Async == nil {
		call()
		return
	}
	w.Async(call)
}

// verdict reports what main says about pkg. An unwired seam reads as an unknown
// main; only a green main suppresses a repair, and everything else files.
func (w *ciTestWatch) verdict(pkg string) mainVerdict {
	if w.MainStatus == nil {
		return mainUnknown
	}
	return w.MainStatus(w.Rig, pkg)
}

// mainStatusFromStatus is the MainStatus seam over the red-main status the
// landing worker writes after every post-land verdict. The file is read per
// repeat rather than captured, so a main that went red or green since the watch
// was built is seen. A status that is missing or unreadable is an unknown main,
// not a green one, and an unknown main files (gt-42ulv).
func mainStatusFromStatus(townRoot string) func(rig, pkg string) mainVerdict {
	return func(rig, pkg string) mainVerdict {
		data, err := os.ReadFile(RedMainStatusPath(townRoot, rig)) //nolint:gosec // G304: town runtime path
		if err != nil {
			// A missing status is a rig with no verdict; an unreadable one is a
			// verdict nobody saw. Reading either as green would suppress the
			// repair for a live failure.
			return mainUnknown
		}
		return mainVerdictOf(string(data), pkg)
	}
}

// mainVerdictOf is what a red-main status says about pkg. The status holds the
// latest verdict as one stamped line, "<stamp> main GREEN at <sha> (<by>)" or
// "<stamp> main RED at <sha> (<by>): <pkg> [<bead>], ..."; a file holding
// several verdicts is read at its last, so an older one cannot shadow the
// newest. A RED line that does not name pkg leaves pkg passing on main, and a
// GREEN line says so outright; any other text is a status this watch cannot
// read, which is an unknown main (gt-42ulv).
func mainVerdictOf(status, pkg string) mainVerdict {
	lines := strings.Split(strings.TrimSpace(status), "\n")
	line := strings.TrimSpace(lines[len(lines)-1])
	if strings.Contains(line, " main GREEN at ") {
		return mainGreen
	}
	i := strings.Index(line, " main RED at ")
	if i < 0 {
		return mainUnknown
	}
	rest := line[i+len(" main RED at "):]
	colon := strings.Index(rest, ": ")
	if colon < 0 {
		return mainUnknown
	}
	list := rest[colon+2:]
	if !redListReadable(list) {
		return mainUnknown
	}
	if redListNamesPackage(list, pkg) {
		return mainRed
	}
	return mainGreen
}

// redListReadable reports whether a RED list has at least one "<pkg> [<bead>]"
// entry. A RED line whose list cannot be read is not a verdict that pkg passes:
// it is an unknown main, the same as a line the watch cannot read at all.
func redListReadable(list string) bool {
	if semi := strings.Index(list, ";"); semi >= 0 {
		list = list[:semi]
	}
	for _, entry := range strings.Split(list, ", ") {
		if _, _, ok := strings.Cut(strings.TrimSpace(entry), " ["); ok {
			return true
		}
	}
	return false
}

// redListNamesPackage reports whether a RED list, "<pkg> [<bead>], ...", names
// pkg. The "; flaky (passed on rerun):" and "; reverting ..." tails follow the
// list, so the list ends at the first semicolon. A whole field matches: a
// package whose name is a prefix of another is not that other package.
func redListNamesPackage(list, pkg string) bool {
	if semi := strings.Index(list, ";"); semi >= 0 {
		list = list[:semi]
	}
	for _, entry := range strings.Split(list, ", ") {
		if field, _, ok := strings.Cut(strings.TrimSpace(entry), " ["); ok && field == pkg {
			return true
		}
	}
	return false
}

// Record logs f and reports every test in it that has now failed on a second
// bead while main is not green for it. A tail that named no test is recorded as
// unparsed and reported to nobody.
func (w *ciTestWatch) Record(f land.CIFailure) {
	now := w.now()
	recs, err := recordCIFailure(w.Ledger, w.Rig, f, now)
	if err != nil {
		w.logf("landing_worker: %s: ci-failure ledger %s: %v", w.Rig, w.Ledger, err)
	}
	for _, t := range distinctTests(f) {
		beads := w.beadsFor(recs, t, now)
		if len(beads) < 2 {
			continue
		}
		// A test that passes on main fails for these beads because their diffs
		// break what it guards, so the repair is in the diff: a P1 for the test
		// sends a worker to the wrong file (gt-42ulv).
		if w.verdict(t.Package) == mainGreen {
			w.logf("landing_worker: %s: %s %s failed on %d beads within %s: not filed: passes on main",
				w.Rig, t.Package, t.Test, len(beads), ciWindowText())
			continue
		}
		w.run(func() { w.report(t, beads) })
	}
}

// distinctTests is the tests f names, in order and once each.
func distinctTests(f land.CIFailure) []land.TestFailure {
	var out []land.TestFailure
	seen := map[land.TestFailure]bool{}
	for _, t := range f.Tests {
		if !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	return out
}

// beadsFor is the distinct beads that failed t on w.Rig inside the window,
// from the ledger plus this run. A bead that fails the same test twice is one
// bead, so it never reaches the rule's two.
func (w *ciTestWatch) beadsFor(recs []ciFailure, t land.TestFailure, now time.Time) []string {
	cutoff := now.Add(-ciFailureWindow)
	var out []string
	seen := map[string]bool{}
	for _, r := range recs {
		if r.Rig != w.Rig || r.Package != t.Package || r.Test != t.Test || r.Unparsed {
			continue
		}
		if r.Time.Before(cutoff) || r.Bead == "" || seen[r.Bead] {
			continue
		}
		seen[r.Bead] = true
		out = append(out, r.Bead)
	}
	return out
}

// failingTestKey is the alert fingerprint for one test on one rig, stable
// across the failures the rule keeps upserting onto it.
func failingTestKey(rig, pkg, test string) string {
	return "failing-test:" + rig + ":" + pkg + ":" + test
}

// failingTestTitle is the title of the repair bead for one test on one rig. It
// is the key an open bead is found by, so it must not change for a test.
func failingTestTitle(rig, pkg, test string) string {
	return fmt.Sprintf("failing test (%s): %s %s", rig, pkg, test)
}

// report raises the one escalation for t and files the one repair bead, or
// comments on the bead already filed for the test, whatever status it is in.
func (w *ciTestWatch) report(t land.TestFailure, beadIDs []string) {
	window := ciWindowText()
	msg := fmt.Sprintf("%s %s has failed on %d beads on rig %s within %s: %s. Nothing is retried, so this test is to be fixed or removed (gt-xvw20).",
		t.Package, t.Test, len(beadIDs), w.Rig, window, strings.Join(beadIDs, ", "))
	if w.Escalate != nil {
		w.Escalate(failingTestKey(w.Rig, t.Package, t.Test), msg)
	}
	if w.Beads == nil {
		return
	}
	detail := fmt.Sprintf("%s %s failed on rig %s within %s for beads: %s.", t.Package, t.Test, w.Rig, window, strings.Join(beadIDs, ", "))
	title := failingTestTitle(w.Rig, t.Package, t.Test)
	open, err := w.openBead(title)
	if err != nil {
		// A failed read files a duplicate rather than leaving the repeat
		// unreported, the way the red-main owner treats its own list.
		w.logf("landing_worker: %s: listing %s beads: %v", w.Rig, LabelFlakyTest, err)
	}
	if open != "" {
		if err := w.Beads.AddComment(open, detail); err != nil {
			w.logf("landing_worker: %s: commenting on %s: %v", w.Rig, open, err)
		}
		return
	}
	if _, err := w.Beads.Create(beads.CreateOptions{
		Title:    title,
		Labels:   []string{LabelFlakyTest},
		Priority: 1,
		Description: "The daemon's CI-failure watch found this test failing on more than one bead " +
			"(gt-xvw20); nothing is rerun or retried, so a failure that looks unreliable is a test to repair. " +
			detail + " Fix the test, or delete it if it no longer covers anything.",
	}); err != nil {
		w.logf("landing_worker: %s: filing the %s bead for %s: %v", w.Rig, LabelFlakyTest, title, err)
	}
}

// openRepairStatuses is the status filter openBead lists with: every status a
// repair bead can hold and still be the one already filed for the test.
// Closed and tombstone are deliberately absent — a bead that is gone does not
// stand in for the repair, so the test earns a fresh one (gt-0o7so).
const openRepairStatuses = "open,in_progress,blocked,hooked,deferred,pinned"

// openBead is the id of the repair bead already filed for title in any
// non-closed status, or "". A polecat that claimed the bead (in_progress,
// hooked) or is blocked on it must be found, or the repeat files a duplicate
// P1 beside it (gt-0o7so).
func (w *ciTestWatch) openBead(title string) (string, error) {
	issues, err := w.Beads.List(beads.ListOptions{Status: openRepairStatuses, Label: LabelFlakyTest, Priority: -1, Limit: 0})
	if err != nil {
		return "", err
	}
	for _, is := range issues {
		if is.Title == title {
			return is.ID, nil
		}
	}
	return "", nil
}

// ciLedgerMu serializes appends to the one town ledger: every rig's landing
// worker writes it from its own goroutine.
var ciLedgerMu sync.Mutex

// recordCIFailure appends f's failures to the ledger at path, prunes records
// older than the retention, and returns the records left.
func recordCIFailure(path, rig string, f land.CIFailure, now time.Time) ([]ciFailure, error) {
	var fresh []ciFailure
	if tests := distinctTests(f); len(tests) > 0 {
		for _, t := range tests {
			fresh = append(fresh, ciFailure{Time: now, Rig: rig, Bead: f.Bead, Package: t.Package, Test: t.Test})
		}
	} else {
		fresh = []ciFailure{{Time: now, Rig: rig, Bead: f.Bead, Unparsed: true}}
	}

	ciLedgerMu.Lock()
	defer ciLedgerMu.Unlock()
	recs, err := readCIFailureLedger(path)
	if err != nil {
		return fresh, err
	}
	cutoff := now.Add(-ciFailureRetention)
	kept := recs[:0]
	for _, r := range recs {
		if !r.Time.Before(cutoff) {
			kept = append(kept, r)
		}
	}
	kept = append(kept, fresh...)
	return kept, writeCIFailureLedger(path, kept)
}

// readCIFailureLedger reads path, skipping lines that are not records. A
// missing file is an empty ledger.
func readCIFailureLedger(path string) ([]ciFailure, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: town runtime path
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []ciFailure
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var r ciFailure
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

// writeCIFailureLedger rewrites path from recs through a temporary file, so a
// crash mid-write leaves the previous ledger rather than a torn one.
func writeCIFailureLedger(path string, recs []ciFailure) error {
	var b strings.Builder
	for _, r := range recs {
		line, err := json.Marshal(r)
		if err != nil {
			return err
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

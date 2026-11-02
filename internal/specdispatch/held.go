package specdispatch

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// HeldBead is one bead a working seat already holds, reduced to what the
// overlap rule reads: its id, the seat holding it, and the paths and tests its
// own text names. The caller reads the set and fills the fields; nothing here
// queries.
//
// Assignee lets the hold line say whose seat to patrol or which stray bead to
// close; a bead no assignee names was never slung and is not held at all, so
// HeldOverlap skips it whatever the caller passed (gt-z80p9, gt-xrog0).
type HeldBead struct {
	ID       string
	Assignee string
	Paths    []string
	// Tests are the test identifiers its text cites. A bead already in flight
	// on a test a candidate names is that candidate's defect however the two
	// spell the files they touch, and the sling-time guard refuses on the same
	// signal (gt-mqa3r).
	Tests []string
}

// WorksSeat reports whether a bead with this assignee and these labels is work
// on a seat the overlap hold protects: a polecat seat's bead that is not a
// handoff note (gt-asvow). Before changing what holds, read step 5 of
// docs/guides/spec-dispatcher.md: it states the rule and why a crew session's
// bead and a handoff log are not work on a candidate's files.
func WorksSeat(assignee string, labels []string) bool {
	if !namesPolecatSeat(assignee) {
		return false
	}
	for _, label := range labels {
		if isHandoffLabel(label) {
			return false
		}
	}
	return true
}

// namesPolecatSeat reports whether an assignee addresses a polecat seat: a
// "polecats" path component followed by the seat's name, wherever the two sit
// in the path (role.Actor writes "<rig>/polecats/<name>"). Components are read
// rather than counted, so an assignee with an extra or missing segment is a
// seat when it names one — a shape this rule misreads would let a second
// filing of a seat's work dispatch. An assignee naming no such component — a
// crew or operator session, a person, an empty assignee — addresses no seat.
func namesPolecatSeat(assignee string) bool {
	parts := strings.Split(strings.TrimSpace(assignee), "/")
	for i := 0; i+1 < len(parts); i++ {
		if parts[i] == "polecats" && parts[i+1] != "" {
			return true
		}
	}
	return false
}

// isHandoffLabel reports whether one label marks a bead as a handoff note. The
// town spells it both ways: a crew session's bead wears "handoff", and the
// runtime families carry "gt:handoff" (capacity.LabelHandoff in
// internal/scheduler/capacity), so a rule that read one spelling would miss
// half the logs (gt-asvow).
func isHandoffLabel(label string) bool {
	switch strings.ToLower(strings.TrimSpace(label)) {
	case "handoff", "gt:handoff":
		return true
	}
	return false
}

// maxNamedHeldSignals bounds the shared paths, and separately the shared
// tests, that the hold line names, so a pair of beads describing an area
// rather than a defect does not turn a skip line into a wall
// (maxNamedChildren's rule, gt-z80p9).
const maxNamedHeldSignals = 3

// heldBypassPriority is the priority below which a candidate skips the
// content-overlap hold: P0 and P1 wait behind no seat's rework, because a merge
// conflict at rebase costs less than that delay (gt-iydx1). P2 and lower keep
// the hold.
const heldBypassPriority = 2

// HeldMatch is the overlap rule's answer for one candidate: the hold it takes,
// or the hold its priority skips.
type HeldMatch struct {
	// Reason is why the candidate is held: the held bead, the seat holding it,
	// and what the two share. Empty when nothing overlaps, and when the
	// candidate's priority bypasses the hold.
	Reason string
	// Bypassed is set when the candidate is urgent enough to skip a hold it
	// would otherwise take; the peer answer reads the candidate as unheld.
	Bypassed bool
	// Holder names the held bead and the seat holding it, as "<bead> held by
	// <seat>". Empty when nothing overlaps.
	Holder string
}

// HeldOverlap reports why a candidate of the given priority is the same defect
// a working seat is already on: a bead in held names a path or a test the
// candidate's own text names too, and the reason names that bead, the seat
// holding it, and what the two share (gt-z80p9, gt-xrog0, gt-mqa3r).
//
// Paths and tests decide it, and only against work a seat already has in hand.
// Two beads describing one defect from different vantage points share no
// vocabulary but always share files — the signal the sling-time guard reads too
// — and against a held bead that signal stands alone, where against the whole
// open pool it does not: a shared file there is as often two genuinely distinct
// beads in one package (gt-mcq). A shared test name is the stronger signal the
// guard refuses live work on, and matching here on it too is what lets a
// candidate the guard would refuse be skipped before it is charged a seat
// (gt-mqa3r). Naming the lowest overlapping id keeps the line a candidate held
// twice reports identical. An entry with no assignee never holds: the hold
// fails open rather than blocking a candidate on a bead no seat is working.
//
// A candidate below heldBypassPriority takes no hold: the match comes back
// with Bypassed set and the holder named, so the peer answer does not count it
// as held (gt-iydx1).
func HeldOverlap(paths, tests []string, priority int, held []HeldBead) HeldMatch {
	if len(paths) == 0 && len(tests) == 0 || len(held) == 0 {
		return HeldMatch{}
	}
	mine := make(map[string]bool, len(paths))
	for _, p := range paths {
		if p != "" {
			mine[p] = true
		}
	}
	var match, assignee string
	var sharedPaths, sharedTests []string
	for _, h := range held {
		if h.ID == "" || h.Assignee == "" || (match != "" && h.ID >= match) {
			continue
		}
		bothPaths := make([]string, 0, len(h.Paths))
		for _, p := range h.Paths {
			if mine[p] {
				bothPaths = append(bothPaths, p)
			}
		}
		bothTests := sharedTestNames(tests, h.Tests)
		if len(bothPaths) == 0 && len(bothTests) == 0 {
			continue
		}
		sort.Strings(bothPaths)
		match, assignee, sharedPaths, sharedTests = h.ID, h.Assignee, bothPaths, bothTests
	}
	if match == "" {
		return HeldMatch{}
	}
	holder := fmt.Sprintf("%s held by %s", match, assignee)
	if priority < heldBypassPriority {
		return HeldMatch{Bypassed: true, Holder: holder}
	}
	work, more := heldWork(sharedPaths, sharedTests)
	reason := fmt.Sprintf("%s (held by %s) is already hooked to a working seat and %s", match, assignee, work)
	if more > 0 {
		reason += fmt.Sprintf(" (+%d more)", more)
	}
	return HeldMatch{Reason: reason + "; the hold lifts when that bead closes", Holder: holder}
}

// sharedTestNames returns the candidate's test identifiers that name the same
// test or test family as one of the held bead's, sorted and deduplicated.
func sharedTestNames(mine, held []string) []string {
	found := map[string]bool{}
	for _, a := range mine {
		for _, b := range held {
			if TestNamesOverlap(a, b) {
				found[a] = true
				break
			}
		}
	}
	if len(found) == 0 {
		return nil
	}
	out := make([]string, 0, len(found))
	for name := range found {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// heldWork renders what a candidate shares with the held bead that made the
// match — "names a, b and runs TestX" — capping each list and reporting how
// many signals the cap left unnamed.
func heldWork(paths, tests []string) (string, int) {
	more := 0
	if len(paths) > maxNamedHeldSignals {
		more += len(paths) - maxNamedHeldSignals
		paths = paths[:maxNamedHeldSignals]
	}
	if len(tests) > maxNamedHeldSignals {
		more += len(tests) - maxNamedHeldSignals
		tests = tests[:maxNamedHeldSignals]
	}
	var parts []string
	if len(paths) > 0 {
		parts = append(parts, "names "+strings.Join(paths, ", "))
	}
	if len(tests) > 0 {
		parts = append(parts, "runs "+strings.Join(tests, ", "))
	}
	return strings.Join(parts, " and "), more
}

// TestNamesOverlap reports whether two extracted identifiers name the same test
// or the same test family. Beads often cite a prefix rather than each member
// ("TestRunPrimeExternalTools_*" against
// "TestRunPrimeExternalTools_BoundsSlowMailCheck"), so a prefix that ends on an
// identifier boundary counts as the same family; a prefix that ends mid-word
// does not. It is the rule both the sling-time guard and the pre-dispatch hold
// read, so the two cannot disagree about what a shared test is.
func TestNamesOverlap(a, b string) bool {
	a = strings.TrimRight(a, "_")
	b = strings.TrimRight(b, "_")
	if a == b {
		return true
	}
	shorter, longer := a, b
	if len(shorter) > len(longer) {
		shorter, longer = longer, shorter
	}
	if shorter == "" || !strings.HasPrefix(longer, shorter) {
		return false
	}
	rest := longer[len(shorter):]
	return strings.HasPrefix(rest, "_")
}

// HoldFiles is the set of paths the overlap hold reads from a bead's text: the
// paths it names, minus the paths it only reads. A bead cites the standards and
// research it must follow (every test bead cites the test-engineering standard)
// without changing them, so two such beads do not hold each other. A reference
// document under docs/ that the bead's Constraints section lists is a file the
// bead changes, so it stays; a path in the town's claude/docs standards tree
// never stays, listed or not, because that tree lives in ~/.claude and no
// polecat edits it (gt-6t44f, gt-kr17g).
//
// A bead can also mark a path read-only: a path its text follows with
// "(read-only)" is not a named path, so two beads that cite one document that
// way do not hold each other (gt-5v13p). The marker is exact, and it must
// follow every mention in text to count, so a bead that also names the path as
// work is still held on it.
//
// named is the bead's paths; listed is the paths its Constraints section names;
// text is the bead's own words, every field named was extracted from: a mention
// in a field left out is unmarked and keeps the path held.
func HoldFiles(named, listed []string, text ...string) []string {
	constrained := make(map[string]bool, len(listed))
	for _, p := range listed {
		constrained[p] = true
	}
	whole := strings.Join(text, "\n")
	out := make([]string, 0, len(named))
	for _, p := range named {
		if markedReadOnly(whole, p) || isStandardDoc(p) || (IsReferenceDoc(p) && !constrained[p]) {
			continue
		}
		out = append(out, p)
	}
	return out
}

// readOnlyMarker matches the (read-only) marker right after a mention; see
// HoldFiles. A closing backtick or quote may sit between the path and the marker.
var readOnlyMarker = regexp.MustCompile("^[`\"']?\\s*\\(read-only\\)")

// markedReadOnly reports whether every mention of path in text is followed by
// the (read-only) marker. A mention counts only at a path-token start
// (startsPathToken), and text with no mention marks nothing.
func markedReadOnly(text, path string) bool {
	if path == "" {
		return false
	}
	mentions, marked := 0, 0
	for _, line := range strings.Split(text, "\n") {
		for off := 0; off < len(line); {
			i := strings.Index(line[off:], path)
			if i < 0 {
				break
			}
			at := off + i
			end := at + len(path)
			off = end
			if !startsPathToken(line[:at]) {
				continue
			}
			mentions++
			if readOnlyMarker.MatchString(line[end:]) {
				marked++
			}
		}
	}
	return mentions > 0 && mentions == marked
}

// startsPathToken reports whether prefix ends where a path token starts: at the
// line's start, after a character no path carries, or after the "./" or "../"
// that spells the same repo-relative path.
func startsPathToken(prefix string) bool {
	if prefix == "" {
		return true
	}
	// "../" is checked first: it ends in "./", so checking "./" first would strip
	// just that suffix, leave a "." and reject the mention.
	for _, up := range []string{"../", "./"} {
		if trimmed, ok := strings.CutSuffix(prefix, up); ok {
			return startsPathToken(trimmed)
		}
	}
	last := prefix[len(prefix)-1]
	if last >= 'a' && last <= 'z' || last >= 'A' && last <= 'Z' || last >= '0' && last <= '9' {
		return false
	}
	return last != '_' && last != '.' && last != '/' && last != '-'
}

// IsReferenceDoc reports whether a normalized path names a read-only reference
// document: the repo's docs/ tree, or the town's claude/docs standards tree
// (isStandardDoc) (gt-6t44f).
func IsReferenceDoc(path string) bool {
	return strings.HasPrefix(path, "docs/") || isStandardDoc(path)
}

// isStandardDoc reports whether a normalized path names a document in the
// town's claude/docs standards tree in any spelling it arrives in
// ("claude/docs/...", "~/.claude/docs/...", an absolute path into it). Only
// the bare "claude/docs/" prefix and a ".claude/docs/" directory match, so a
// repo path that merely embeds a claude/docs directory, such as
// "internal/x/claude/docs/render.go", stays a hold path (gt-014o6, gt-rrr4x).
func isStandardDoc(path string) bool {
	return strings.HasPrefix(path, "claude/docs/") ||
		strings.Contains(path, "/.claude/docs/")
}

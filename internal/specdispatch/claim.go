package specdispatch

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Target claims: two beads that name one test file are two seats on one line.
//
// The content guard a sling already runs compares Go test identifiers and file
// paths, and refuses a manual sling on a shared identifier. It cannot see two
// beads that rewrite one assertion of a test file without citing a Test*
// symbol, and the dispatcher's sling admits content overlap by design
// (gt-e1ufo). What such beads share is the test file, the unit a landing
// conflicts in, so that is what this rule reads (gt-n6jbw).
//
// A test file is matched by its full path, and only by it: two beads that spell
// one path name one file, while `internal/specdispatch/claim_test.go` and
// `internal/cmd/claim_test.go` are two files. A bare name states no directory,
// so it cannot say which of two same-named files a bead means; matched across
// directories it held beads that shared no line, and it no longer matches
// (gt-dnp9c). A peer that already landed holds nothing either: a polecat
// starts from main, which carries that landing, and holding the bead behind it
// left seats idle while the conflict would have merged normally (gt-dnp9c).
//
// The hold is the spec dispatcher's, applied in its tick beside the other
// holds, and it is a skip rather than a failure: the bead stays ready and is
// slung once the claim lifts.

// claimLiveStatuses are the statuses of a bead a working seat already has in
// hand. Such a bead holds its target until it closes, and the hold lifts by
// itself.
var claimLiveStatuses = map[string]bool{"hooked": true, "in_progress": true}

// testFileSuffixes are the file-name endings that make a file a test file. A
// test file is the assertion's container, so naming it is naming a target. The
// set covers the spellings this town's rigs use: Go, TypeScript, JavaScript,
// Python and Ruby.
var testFileSuffixes = []string{
	"_test.go",
	".test.ts", ".test.tsx", ".test.js", ".test.jsx",
	".spec.ts", ".spec.tsx", ".spec.js", ".spec.jsx",
	"_test.py", "_spec.rb",
}

// IsTestFile reports whether path names a test file, by the endings in
// testFileSuffixes or Python's leading test_. The path may be a bare name or
// carry directories; only the last segment is read, because the ending names
// the file's kind and the directory is not part of it.
func IsTestFile(path string) bool {
	name := strings.TrimSpace(path)
	if i := strings.LastIndexByte(name, '/'); i >= 0 {
		name = name[i+1:]
	}
	if strings.HasPrefix(name, "test_") && strings.HasSuffix(name, ".py") {
		return true
	}
	for _, suffix := range testFileSuffixes {
		if len(name) > len(suffix) && strings.HasSuffix(name, suffix) {
			return true
		}
	}
	return false
}

// testFile is one test file a bead names: the path as the bead spelled it, and
// its last segment, which a hold reason prints so the line stays short.
type testFile struct {
	path string
	name string
}

// testFiles reduces a bead's paths to the test files it names, deduplicated by
// path and sorted so a pair's reason does not depend on the order the pool
// listed them.
func testFiles(paths []string) []testFile {
	seen := make(map[string]testFile, len(paths))
	for _, path := range paths {
		if !IsTestFile(path) {
			continue
		}
		path = strings.TrimPrefix(strings.TrimSpace(path), "./")
		seen[path] = testFile{path: path, name: baseName(path)}
	}
	if len(seen) == 0 {
		return nil
	}
	out := make([]testFile, 0, len(seen))
	for _, file := range seen {
		out = append(out, file)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].path < out[j].path })
	return out
}

// baseName is a path's last segment.
func baseName(path string) string {
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		return path[i+1:]
	}
	return path
}

// sharedTestFile returns the base name of the first test file both sides name,
// in sorted order so a pair's reason does not depend on the order the pool
// listed them.
func sharedTestFile(mine []testFile, peerPaths []string) string {
	peer := testFiles(peerPaths)
	for _, a := range mine {
		for _, b := range peer {
			if sameTestFile(a, b) {
				return a.name
			}
		}
	}
	return ""
}

// sameTestFile reports whether two beads' paths name one file. They do only
// when they spell the same full path: packages repeat test-file names, and
// holding `internal/a/claim_test.go` against `internal/b/claim_test.go` would
// stop both. A bare name leaves the directory unstated, so it names no one
// file and matches nothing (gt-dnp9c).
func sameTestFile(a, b testFile) bool {
	return spellsDirectory(a.path) && a.path == b.path
}

// spellsDirectory reports whether a path carries a directory as well as a file.
func spellsDirectory(path string) bool { return strings.Contains(path, "/") }

// Claimant is one bead the target-claim check weighs: what its text says it
// will change — the paths and test identifiers its own words name — and the
// state that decides whether it holds another bead's target. The caller reads
// the set and fills the fields; nothing here queries.
type Claimant struct {
	ID       string
	Status   string
	Assignee string
	Priority int
	// CreatedAt and ClosedAt are bd's timestamps as written. CreatedAt places
	// the bead in dispatch order; a value that does not parse sorts after every
	// parseable one. ClosedAt is carried beside the landed flag and, like it,
	// is not read by the target-claim rule (gt-dnp9c).
	CreatedAt string
	ClosedAt  string
	// Landed marks a bead closed by a landing (CloseReasonIsLanding). The
	// target-claim rule does not read it: a landed peer holds nothing, because
	// the candidate starts from main and already has that work (gt-dnp9c).
	Landed bool
	// ClaimUnread marks a peer whose design and notes were not read. The
	// target-claim rule does not read it either: a peer that names no readable
	// full path holds nothing (gt-dnp9c).
	ClaimUnread bool
	Paths       []string
	Tests       []string
}

// CloseReasonIsLanding reports whether reason is the close reason a landing
// writes: the first line "Landed on <target>". Before changing that format,
// read land.LandingRecord.CloseReason, which writes it. Only the first line is
// read, so a person's own "Landed on main ..." close says the same thing.
func CloseReasonIsLanding(reason string) bool {
	line, _, _ := strings.Cut(strings.TrimSpace(reason), "\n")
	return strings.HasPrefix(strings.TrimSpace(line), "Landed on ")
}

// NamesTestFile reports whether the candidate names a test file at all. A
// candidate that names none has nothing to claim, so the pool of other beads'
// targets need not be read for it, and a failed read must not hold it.
func NamesTestFile(candidate Claimant) bool { return len(testFiles(candidate.Paths)) > 0 }

// TargetClaimHold reports why candidate's target is already claimed by one of
// peers, and false when no peer claims it (gt-n6jbw).
//
// A claim is a test file the candidate names by its full path. A peer claims it
// when it spells that same path and is one of:
//   - in flight, a seat already on the file; the hold lifts when it closes;
//   - open and slated, and ahead of the candidate in dispatch order, so at most
//     one bead of a ready cluster spends a seat.
//
// No other peer holds: a closed or landed peer's work is already on main, which
// the candidate starts from, and a peer that names the file only by a bare name
// or whose claim could not be read names no path this rule can compare
// (gt-dnp9c).
//
// slated is the set of beads the tick has decided to sling this tick, not the
// set its ready board carries. A board-carried peer the tick holds for a reason
// of its own — the shape gate refuses it, it needs planning, it rests out its
// startup backoff — is never slung, so ranking it ahead would hold the file for
// as long as that hold lasts (gt-mknmb, gt-00gee). The tick walks its board in
// dispatch order, so a peer it slates is always one that precedes the candidate.
//
// Two beads naming the same test file but different test identifiers
// (TestFoo against TestBar) target different assertions, so they do not hold
// each other. A bead naming no test file claims nothing and is never held.
//
// The lowest claiming id answers, so a bead several peers claim reports one
// stable reason. An empty id and the candidate itself claim nothing; an epic
// enumerates its children's files rather than naming one target, so the caller
// drops it before the set arrives here (sling.Duplicate.IsEpic).
//
// The clock parameter is not read: no hold ends with a window, and the
// signature keeps the dispatcher's call unchanged.
func TargetClaimHold(candidate Claimant, peers []Claimant, slated map[string]bool, _ time.Time) (string, bool) {
	files := testFiles(candidate.Paths)
	if len(files) == 0 {
		return "", false
	}
	var heldID string
	var reason string
	for _, peer := range peers {
		if peer.ID == "" || peer.ID == candidate.ID || (heldID != "" && peer.ID >= heldID) {
			continue
		}
		shared := sharedTestFile(files, peer.Paths)
		if shared == "" || distinctTests(candidate.Tests, peer.Tests) {
			continue
		}
		match := fmt.Sprintf("names the same test file, %s,", shared)
		var why string
		switch {
		case claimLiveStatuses[peer.Status]:
			why = fmt.Sprintf("%s %s and is in flight on it%s; the hold lifts when that bead closes", peer.ID, match, seatOf(peer))
		case peer.Status == "open" && slated[peer.ID] && precedes(peer, candidate):
			why = fmt.Sprintf("%s %s and comes first in dispatch order; the hold lifts when that bead closes", peer.ID, match)
		default:
			continue
		}
		heldID, reason = peer.ID, why
	}
	if heldID == "" {
		return "", false
	}
	return reason, true
}

// seatOf names the seat a claimant is in flight on, for a hold reason, and
// names nothing when the record lost its assignee.
func seatOf(peer Claimant) string {
	if strings.TrimSpace(peer.Assignee) == "" {
		return ""
	}
	return " (" + peer.Assignee + ")"
}

// distinctTests reports whether both sides name test identifiers and none of
// them overlap: the same test file holding two different tests is two targets.
// A side naming no identifier leaves its target unknown, which is not a
// different one.
func distinctTests(a, b []string) bool {
	if len(a) == 0 || len(b) == 0 {
		return false
	}
	for _, x := range a {
		for _, y := range b {
			if TestNamesOverlap(x, y) {
				return false
			}
		}
	}
	return true
}

// precedes reports whether a comes before b in dispatch order: lower priority
// number first, then earlier creation, then bead ID. It is Order's total order,
// so the bead a ready cluster dispatches first is the one that claims its test
// files.
func precedes(a, b Claimant) bool {
	if a.Priority != b.Priority {
		return a.Priority < b.Priority
	}
	ta, tb := parseTime(a.CreatedAt), parseTime(b.CreatedAt)
	if !ta.Equal(tb) {
		if ta.IsZero() {
			return false
		}
		if tb.IsZero() {
			return true
		}
		return ta.Before(tb)
	}
	return a.ID < b.ID
}

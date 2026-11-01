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
// A test file is matched by its path when both beads spell one, because
// `internal/specdispatch/claim_test.go` and `internal/cmd/claim_test.go` are two
// files. A bare name states no directory, so it matches any spelling of the
// file it names: one bead of the gt-n6jbw pair spelled the path and the other
// the bare name, and a check that compared only the spellings would have let
// that pair through.
//
// The hold is the spec dispatcher's, applied in its tick beside the other
// holds, and it is a skip rather than a failure: the bead stays ready and is
// slung once the claim lifts.

// claimLiveStatuses are the statuses of a bead a working seat already has in
// hand. Such a bead holds its target until it closes, and the hold lifts by
// itself.
var claimLiveStatuses = map[string]bool{"hooked": true, "in_progress": true}

// claimLandingWindow bounds how long a landed bead's claim outlives the
// landing. Long enough to catch a bead written against the base the landing
// replaced, short enough that follow-up work is not held past the next day.
const claimLandingWindow = 24 * time.Hour

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
// carry directories; only the last segment is read, because that is the name
// two beads spell one file by.
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
// its last segment. The two are kept apart because a path names one file while
// a bare name names whichever file carries it (see sameTestFile).
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

// sameTestFile reports whether two beads' paths name one file. Two full paths
// name one file only when they are equal: packages repeat test-file names, and
// holding `internal/a/claim_test.go` against `internal/b/claim_test.go` would
// stop both. A bare name leaves the directory unstated, so it answers the file
// every spelling with that name answers.
func sameTestFile(a, b testFile) bool {
	if a.path == b.path {
		return true
	}
	if a.name != b.name {
		return false
	}
	return !spellsDirectory(a.path) || !spellsDirectory(b.path)
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
	// CreatedAt and ClosedAt are bd's timestamps as written. A value that will
	// not parse is unknown, not an empty time: the landing branch below holds
	// rather than passing a bead the two times cannot place.
	CreatedAt string
	ClosedAt  string
	// Landed marks a bead closed by a landing (CloseReasonIsLanding): its work
	// is on the branch the candidate would rebase onto.
	Landed bool
	Paths  []string
	Tests  []string
}

// CloseReasonIsLanding reports whether reason is the close reason a landing
// writes: the first line "Landed on <target>". Before changing that format,
// read land.LandingRecord.CloseReason, which writes it. Only the first line is
// read, so a person's own "Landed on main ..." close says the same thing.
func CloseReasonIsLanding(reason string) bool {
	line, _, _ := strings.Cut(strings.TrimSpace(reason), "\n")
	return strings.HasPrefix(strings.TrimSpace(line), "Landed on ")
}

// TargetClaimHold reports why candidate's target is already claimed by one of
// peers, and false when no peer claims it (gt-n6jbw).
//
// A claim is a test file the candidate names. A peer claims it when it names
// the same test file and is one of:
//   - in flight, a seat already on the file; the hold lifts when it closes;
//   - open and slated, and ahead of the candidate in dispatch order, so at most
//     one bead of a ready cluster spends a seat;
//   - closed by a landing within claimLandingWindow, when the candidate was
//     filed before that landing, so its author wrote it against a base the
//     landing has since replaced.
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
func TargetClaimHold(candidate Claimant, peers []Claimant, slated map[string]bool, now time.Time) (string, bool) {
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
		var why string
		switch {
		case claimLiveStatuses[peer.Status]:
			why = fmt.Sprintf("%s names the same test file, %s, and is in flight on it%s; the hold lifts when that bead closes", peer.ID, shared, seatOf(peer))
		case peer.Status == "open" && slated[peer.ID] && precedes(peer, candidate):
			why = fmt.Sprintf("%s names the same test file, %s, and comes first in dispatch order; the hold lifts when that bead closes", peer.ID, shared)
		case peer.Status == "closed" && peer.Landed && landedRecently(peer, now) && filedBeforeLanding(candidate, peer):
			why = fmt.Sprintf("%s names the same test file, %s, and landed after this bead was filed; the hold lifts when the landing leaves the window", peer.ID, shared)
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

// landedRecently reports whether a landed peer's claim outlives its landing.
// A close time that cannot be read cannot bound the claim, so it counts as
// recent: an unknown window holds rather than passing the candidate.
func landedRecently(peer Claimant, now time.Time) bool {
	closed := parseTime(peer.ClosedAt)
	if closed.IsZero() {
		return true
	}
	return now.Sub(closed) < claimLandingWindow
}

// filedBeforeLanding reports whether the candidate was written before the peer
// landed. A time that cannot be read cannot place the two, so the landing is
// read as having happened after the filing: the hold takes, and the bead is
// left for the operator rather than dispatched onto a base that may have moved.
func filedBeforeLanding(candidate, peer Claimant) bool {
	filed := parseTime(candidate.CreatedAt)
	landed := parseTime(peer.ClosedAt)
	if filed.IsZero() || landed.IsZero() {
		return true
	}
	return filed.Before(landed)
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

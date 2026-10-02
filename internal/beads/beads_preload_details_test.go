package beads

import (
	"reflect"
	"strings"
	"testing"
)

// The fixtures below are what the one preload read answers with once it
// carries its dependency arms (gt-7dctf, gt-59p7e): the wisp and issue rows
// tagged with the table they came from, then a dep_row = 1 row per dependency,
// keyed to its dependent by dep_issue_id. Three merge requests between them pin
// the two ways a dependency decides readiness — an open `blocks` dependency
// holds its MR back, a `merge-blocks` dependency whose target is closed rides
// on the target's close_reason, and one closed for any other reason still
// blocks.

const preloadBeadsJSON = `[
{"id":"gt-wisp-mr","src":"wisp","title":"Merge: gt-src","description":"branch: polecat/test/gt-src@abc\nsource_issue: gt-src\nrig: gastown\n","status":"open","priority":1,"created_at":"2026-06-29T00:00:00Z","updated_at":"2026-06-29T00:00:00Z","labels_csv":"gt:merge-request","dep_row":0},
{"id":"gt-wisp-mr","src":"wisp","dep_row":1,"dep_issue_id":"gt-wisp-mr","dep_type":"merge-blocks","dep_id":"gt-merged","dep_status":"closed","dep_close_reason":"Merged in gt-wisp-x","dep_title":"already landed","dep_priority":1,"dep_issue_type":"task"},
{"id":"gt-wisp-mr-rejected","src":"wisp","title":"Merge: gt-other","description":"branch: polecat/test/gt-other@abc\nsource_issue: gt-other\nrig: gastown\n","status":"open","priority":1,"created_at":"2026-06-29T00:00:00Z","updated_at":"2026-06-29T00:00:00Z","labels_csv":"gt:merge-request","dep_row":0},
{"id":"gt-wisp-mr-rejected","src":"wisp","dep_row":1,"dep_issue_id":"gt-wisp-mr-rejected","dep_type":"merge-blocks","dep_id":"gt-rejected","dep_status":"closed","dep_close_reason":"rejected: superseded","dep_title":"went nowhere","dep_priority":1,"dep_issue_type":"task"},
{"id":"gt-om-witness","src":"wisp","title":"witness agent","description":"role: witness\n","status":"open","priority":1,"created_at":"2026-06-29T00:00:00Z","updated_at":"2026-06-29T00:00:00Z","labels_csv":"gt:agent","dep_row":0},
{"id":"gt-mr-issue","src":"issue","title":"Merge: gt-src","description":"branch: polecat/test/gt-src@abc\nsource_issue: gt-src\nrig: gastown\n","status":"open","priority":1,"issue_type":"task","assignee":"","created_at":"2026-06-29T00:00:00Z","updated_at":"2026-06-29T00:00:00Z","created_by":"t","ephemeral":0,"close_reason":"","labels_csv":"gt:merge-request","dep_row":0},
{"id":"gt-mr-issue","src":"issue","dep_row":1,"dep_issue_id":"gt-mr-issue","dep_type":"blocks","dep_id":"gt-blocker","dep_status":"open","dep_close_reason":"","dep_title":"the blocker","dep_priority":1,"dep_issue_type":"bug"}
]`

// preloadWispsOnlyJSON is one PreloadBeads read that returned the wisps and no
// issues — what a read scoped to labels the merge requests do not carry looks
// like here. The merge request's own issues-table row is then one no snapshot
// covered, which is the case the hydration fallback exists for.
const preloadWispsOnlyJSON = `[
{"id":"gt-wisp-mr","src":"wisp","title":"Merge: gt-src","description":"branch: polecat/test/gt-src@abc\nsource_issue: gt-src\nrig: gastown\n","status":"open","priority":1,"created_at":"2026-06-29T00:00:00Z","updated_at":"2026-06-29T00:00:00Z","labels_csv":"gt:merge-request","dep_row":0},
{"id":"gt-wisp-mr-rejected","src":"wisp","title":"Merge: gt-other","description":"branch: polecat/test/gt-other@abc\nsource_issue: gt-other\nrig: gastown\n","status":"open","priority":1,"created_at":"2026-06-29T00:00:00Z","updated_at":"2026-06-29T00:00:00Z","labels_csv":"gt:merge-request","dep_row":0},
{"id":"gt-om-witness","src":"wisp","title":"witness agent","description":"role: witness\n","status":"open","priority":1,"created_at":"2026-06-29T00:00:00Z","updated_at":"2026-06-29T00:00:00Z","labels_csv":"gt:agent","dep_row":0}
]`

// preloadMRWispsJSON is what the label-filtered wisps read answers — the same
// rows without the witness, which carries no gt:merge-request label.
// ListMergeRequests reaches for it only when the preload cache does not cover
// its label.
const preloadMRWispsJSON = `[
{"id":"gt-wisp-mr","src":"wisp","title":"Merge: gt-src","description":"branch: polecat/test/gt-src@abc\nsource_issue: gt-src\nrig: gastown\n","status":"open","priority":1,"created_at":"2026-06-29T00:00:00Z","updated_at":"2026-06-29T00:00:00Z","labels_csv":"gt:merge-request","dep_row":0},
{"id":"gt-wisp-mr-rejected","src":"wisp","title":"Merge: gt-other","description":"branch: polecat/test/gt-other@abc\nsource_issue: gt-other\nrig: gastown\n","status":"open","priority":1,"created_at":"2026-06-29T00:00:00Z","updated_at":"2026-06-29T00:00:00Z","labels_csv":"gt:merge-request","dep_row":0}
]`

// What `bd list` and `bd show` still answer on the paths the preloads do not
// cover, so a test can tell "answered from the preload" from "paid a round
// trip".
const listMergeIssueJSON = `[
{"id":"gt-mr-issue","title":"Merge: gt-src","description":"branch: polecat/test/gt-src@abc\nsource_issue: gt-src\nrig: gastown\n","status":"open","priority":1,"issue_type":"task","created_at":"2026-06-29T00:00:00Z","updated_at":"2026-06-29T00:00:00Z","created_by":"t","close_reason":"","labels":["gt:merge-request"]}
]`

const showMergeRequestsJSON = `[
{"id":"gt-mr-issue","title":"Merge: gt-src","description":"branch: polecat/test/gt-src@abc\nsource_issue: gt-src\nrig: gastown\n","status":"open","priority":1,"created_at":"2026-06-29T00:00:00Z","updated_at":"2026-06-29T00:00:00Z","close_reason":"","ephemeral":false,"labels":["gt:merge-request"],"dependencies":[{"id":"gt-blocker","status":"open","issue_type":"bug","title":"the blocker","dependency_type":"blocks"}]},
{"id":"gt-wisp-mr","title":"Merge: gt-src","description":"branch: polecat/test/gt-src@abc\nsource_issue: gt-src\nrig: gastown\n","status":"open","priority":1,"created_at":"2026-06-29T00:00:00Z","updated_at":"2026-06-29T00:00:00Z","close_reason":"","ephemeral":true,"labels":["gt:merge-request"],"dependencies":[]},
{"id":"gt-wisp-mr-rejected","title":"Merge: gt-other","description":"branch: polecat/test/gt-other@abc\nsource_issue: gt-other\nrig: gastown\n","status":"open","priority":1,"created_at":"2026-06-29T00:00:00Z","updated_at":"2026-06-29T00:00:00Z","close_reason":"","ephemeral":true,"labels":["gt:merge-request"],"dependencies":[{"id":"gt-rejected","status":"closed","close_reason":"rejected: superseded","issue_type":"task","title":"went nowhere","dependency_type":"merge-blocks"}]}
]`

// preloadAnswer is the bd the preload tests run against: the one preload read
// answers with the fixture above, `bd list` and `bd show` answer what the
// uncovered paths would see, and anything else fails the test rather than
// quietly returning nothing.
func preloadAnswer(args []string) reply {
	return preloadAnswerWith(preloadBeadsJSON)(args)
}

// preloadAnswerWith is preloadAnswer with a caller-chosen answer to the preload
// read, for the cases where which rows that read covered is the thing under
// test. The preload is the one union; a later label-filtered wisps read is what
// ListMergeRequests falls back to for a label the cache does not cover, and
// answers the rows that label selects.
func preloadAnswerWith(beads string) func([]string) reply {
	return func(args []string) reply {
		joined := strings.Join(args, " ")
		switch {
		case strings.HasPrefix(joined, "sql --json ") && strings.Contains(joined, "UNION ALL"):
			return reply{stdout: beads}
		case strings.HasPrefix(joined, "sql --json "):
			return reply{stdout: preloadMRWispsJSON}
		case strings.HasPrefix(joined, "list --json"):
			return reply{stdout: listMergeIssueJSON}
		case strings.HasPrefix(joined, "show --json"):
			return reply{stdout: showMergeRequestsJSON}
		}
		return reply{stdout: "unexpected bd read: " + joined, err: exitError{code: 7}}
	}
}

func newPreloadBeads(t *testing.T) (*Beads, *recorder) {
	t.Helper()
	r := newRecorder(preloadAnswer)
	return newRecordedBeads(t.TempDir(), r), r
}

func warmPreloads(t *testing.T, b *Beads) {
	t.Helper()
	if err := b.PreloadBeads([]string{"gt:agent", "gt:merge-request"}, []IssueStatus{StatusOpen}); err != nil {
		t.Fatalf("PreloadBeads() error = %v", err)
	}
}

func mergeRequestsByID(t *testing.T, b *Beads) map[string]*Issue {
	t.Helper()
	mrs, err := b.ListMergeRequests(ListOptions{Status: "all", Label: "gt:merge-request", Priority: -1})
	if err != nil {
		t.Fatalf("ListMergeRequests() error = %v", err)
	}
	byID := make(map[string]*Issue, len(mrs))
	for _, mr := range mrs {
		byID[mr.ID] = mr
	}
	return byID
}

// TestMergeRequestHydrationReadsPreloadedDependencies is the gt-7dctf fix: a
// warmed *Beads hydrates its merge requests out of the preloaded snapshots, so
// the `bd show --json <ids>` that used to be one process per rig is not paid
// at all. The stub fails every read the preloads were supposed to make
// unnecessary, so a fallback is a visible failure rather than a silent one.
func TestMergeRequestHydrationReadsPreloadedDependencies(t *testing.T) {
	b, r := newPreloadBeads(t)
	warmPreloads(t, b)

	byID := mergeRequestsByID(t, b)

	for _, argv := range r.argvs() {
		if strings.HasPrefix(argv, "show ") {
			t.Fatalf("hydration ran %q; the preloaded dependency rows should have answered it\ncalls:\n%s", argv, strings.Join(r.argvs(), "\n"))
		}
	}
	if got, want := len(byID), 3; got != want {
		t.Fatalf("ListMergeRequests() returned %d merge requests (%v), want %d", got, byID, want)
	}

	// An open blocking dependency holds its MR back, and the dep's own fields
	// come with it — the target join, not just the relation type.
	blocked := byID["gt-mr-issue"]
	if blocked == nil {
		t.Fatal("gt-mr-issue missing from the hydrated merge requests")
	}
	if got, want := blocked.BlockedBy, []string{"gt-blocker"}; !reflect.DeepEqual(got, want) {
		t.Errorf("gt-mr-issue BlockedBy = %v, want %v", got, want)
	}
	if blocked.BlockedByCount != 1 || !HasUnresolvedBlockers(blocked) {
		t.Errorf("gt-mr-issue blocked count = %d, HasUnresolvedBlockers = %v; want blocked", blocked.BlockedByCount, HasUnresolvedBlockers(blocked))
	}
	if len(blocked.Dependencies) != 1 || blocked.Dependencies[0].Title != "the blocker" || blocked.Dependencies[0].Type != "bug" {
		t.Errorf("gt-mr-issue dependencies = %+v, want the blocker's own title and type", blocked.Dependencies)
	}

	// A merge-blocks dependency whose target landed is resolved by its close
	// reason, which only the dependency half of the read carries.
	landed := byID["gt-wisp-mr"]
	if landed == nil {
		t.Fatal("gt-wisp-mr missing from the hydrated merge requests")
	}
	if HasUnresolvedBlockers(landed) {
		t.Errorf("gt-wisp-mr reads blocked, but its merge-blocks dependency landed: %+v", landed.Dependencies)
	}

	// The same shape, closed for any other reason, still blocks.
	rejected := byID["gt-wisp-mr-rejected"]
	if rejected == nil {
		t.Fatal("gt-wisp-mr-rejected missing from the hydrated merge requests")
	}
	if got, want := rejected.BlockedBy, []string{"gt-rejected"}; !reflect.DeepEqual(got, want) {
		t.Errorf("gt-wisp-mr-rejected BlockedBy = %v, want %v", got, want)
	}
}

// TestMergeRequestHydrationFallsBackWhenNotCovered: a *Beads whose snapshot
// does not cover every merge request pays the one `bd show` for the whole set,
// wisp merge requests included, rather than mixing a preloaded answer with a
// remembered one.
func TestMergeRequestHydrationFallsBackWhenNotCovered(t *testing.T) {
	// The read was scoped to gt:agent, so the listing's issues half comes from
	// `bd list` — gt-mr-issue among them — and that is a row no snapshot
	// covered. One uncovered id is enough to send the whole set to bd show.
	r := newRecorder(preloadAnswerWith(preloadWispsOnlyJSON))
	b := newRecordedBeads(t.TempDir(), r)
	if err := b.PreloadBeads([]string{"gt:agent"}, []IssueStatus{StatusOpen}); err != nil {
		t.Fatalf("PreloadBeads() error = %v", err)
	}

	byID := mergeRequestsByID(t, b)

	var shows []string
	for _, argv := range r.argvs() {
		if strings.HasPrefix(argv, "show ") {
			shows = append(shows, argv)
		}
	}
	if len(shows) != 1 {
		t.Fatalf("bd show calls = %v, want exactly one for the whole set\ncalls:\n%s", shows, strings.Join(r.argvs(), "\n"))
	}
	for _, id := range []string{"gt-mr-issue", "gt-wisp-mr", "gt-wisp-mr-rejected"} {
		if !strings.Contains(shows[0], id) {
			t.Errorf("the fallback show %q does not name %s — a partial snapshot must not answer for the set", shows[0], id)
		}
	}
	if blocked := byID["gt-mr-issue"]; blocked == nil || !HasUnresolvedBlockers(blocked) {
		t.Errorf("gt-mr-issue = %+v, want the blockers bd show reports", blocked)
	}
}

// TestPreloadDetailsCoverAnIDOnlyByItsRow: a row with no dependency rows is a
// covered id with no dependencies, not an unknown one, and an id the read did
// not return is unknown either way. That distinction is what the fallback
// rests on, so it is pinned directly.
func TestPreloadDetailsCoverAnIDOnlyByItsRow(t *testing.T) {
	details := newPreloadDetails(
		[]*Issue{{ID: "gt-a"}, {ID: "gt-b"}},
		[]bdSQLIssueRow{
			{DepRow: 1, DepIssueID: "gt-a", DepType: "blocks", DepID: "gt-blocker", DepStatus: "open"},
			{DepRow: 0, DepIssueID: "", DepType: "blocks", DepID: "ignored-row-half"},
		},
	)

	detail, ok := details.detail("gt-a")
	if !ok || detail.ID != "gt-a" {
		t.Fatalf("detail(gt-a) = %+v, %v; want the row", detail, ok)
	}
	if got, want := detail.BlockedBy, []string{"gt-blocker"}; !reflect.DeepEqual(got, want) {
		t.Errorf("detail(gt-a).BlockedBy = %v, want %v", got, want)
	}

	undepended, ok := details.detail("gt-b")
	if !ok || undepended.ID != "gt-b" {
		t.Fatalf("detail(gt-b) = %+v, %v; want the row", undepended, ok)
	}
	if len(undepended.Dependencies) != 0 || HasUnresolvedBlockers(undepended) {
		t.Errorf("detail(gt-b) = %+v, want no dependencies", undepended)
	}

	// The row stored by the snapshot is untouched: detail copies before it
	// adds anything.
	if len(details.byID["gt-a"].Dependencies) != 0 {
		t.Errorf("detail() wrote its dependencies back into the stored row: %+v", details.byID["gt-a"])
	}

	if _, ok := details.detail("gt-nosuch"); ok {
		t.Error("detail(gt-nosuch) answered for an id the read never returned")
	}
	var cold *preloadDetails
	if _, ok := cold.detail("gt-a"); ok {
		t.Error("a nil *preloadDetails answered detail")
	}
	var coldSnap *issueSnapshot
	if _, ok := coldSnap.detail("gt-a"); ok {
		t.Error("a nil *issueSnapshot answered detail")
	}
}

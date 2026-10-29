package metrics

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/land"
)

// notesStore answers ShowMultiple from a table of notes and refuses every other
// read, so a test sees the rows a store hands back without a board behind it.
type notesStore map[string]string

func (s notesStore) List(beads.ListOptions) ([]*beads.Issue, error) {
	return nil, errors.New("the attribution read the board")
}

func (s notesStore) ReadyFiltered(beads.ReadyOptions) ([]*beads.Issue, error) {
	return nil, errors.New("the attribution read the ready board")
}

func (s notesStore) Blocked() ([]*beads.Issue, error) {
	return nil, errors.New("the attribution read the blocked board")
}

func (s notesStore) ShowMultiple(ids []string) (map[string]*beads.Issue, error) {
	found := make(map[string]*beads.Issue, len(ids))
	for _, id := range ids {
		notes, ok := s[id]
		if !ok {
			continue
		}
		found[id] = &beads.Issue{ID: id, Notes: notes}
	}
	return found, nil
}

// notesStores is the seam a test reads through: every directory answers with
// the same notes table, so a test names only what the store holds.
func notesStores(notes notesStore) BeadStores {
	return func(context.Context, string) BeadStore { return notes }
}

// AttributeRecords refuses a missing store seam, a seam that hands back no
// store, and a store whose read failed: a kind derived from a read that did not
// answer would be printed as the unknown seat's rather than the read being
// reported.
func TestAttributeRecordsRefusesAFailedStoreRead(t *testing.T) {
	t.Parallel()
	small := []land.ReviewRecord{{BeadID: "gt-a", Rig: "gastown", Verdict: land.VerdictApprove}}
	large, _ := authorFixture(2*storeShowChunk + 2)
	noStore := func(context.Context, string) BeadStore { return nil }
	failing := func(context.Context, string) BeadStore {
		return failingStore{err: errors.New("dolt is down")}
	}
	tests := []struct {
		name    string
		records []land.ReviewRecord
		env     Env
	}{
		{"no seam", small, Env{Town: "/town"}},
		{"no seam, a whole-board listing", large, Env{Town: "/town"}},
		{"a seam with no store", small, Env{Town: "/town", Beads: noStore}},
		{"a seam with no store, a whole-board listing", large, Env{Town: "/town", Beads: noStore}},
		{"a read that failed", small, Env{Town: "/town", Beads: failing}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got, err := AttributeRecords(context.Background(), tc.env, tc.records); err == nil {
				t.Errorf("AttributeRecords = %v, nil; want a refused read", got)
			}
		})
	}
}

// AttributeRecords reads each store's branches through the seam, so a bead the
// store holds is attributed from its notes and a bead it leaves out is the
// unknown author's rather than an error.
func TestAttributeRecordsReadsItsBranchesThroughTheStore(t *testing.T) {
	t.Parallel()
	records := []land.ReviewRecord{
		{BeadID: "gt-a", Rig: "gastown", Verdict: land.VerdictApprove},
		{BeadID: "gt-b", Rig: "gastown", Verdict: land.VerdictApprove},
		{BeadID: "gt-gone", Rig: "gastown", Verdict: land.VerdictApprove},
	}
	env := Env{Town: "/town", Beads: notesStores(notesStore{
		"gt-a": "READY TO LAND\nBranch: polecat/opal/gt-a\n",
		"gt-b": "READY TO LAND\nBranch: crew/merlin/gt-b\n",
	})}
	got, err := AttributeRecords(context.Background(), env, records)
	if err != nil {
		t.Fatalf("AttributeRecords: %v, want a bead the store leaves out to be an answer", err)
	}
	want := []string{AuthorKindPolecat, AuthorKindCrew, AuthorKindUnknown}
	if len(got) != len(want) {
		t.Fatalf("attributed records = %d, want %d", len(got), len(want))
	}
	for i, kind := range want {
		if got[i].AuthorKind != kind {
			t.Errorf("record %d (%s) kind = %q, want %q", i, got[i].Record.BeadID, got[i].AuthorKind, kind)
		}
	}
}

// TestStoreBeadsKeepsTheIDsSorted: the store answers with a map, so a reader's
// rows would otherwise follow the map's iteration order. The rows come back in
// id order whatever the store's map does. Ten beads are enough that a map
// ranging in id order by chance is not what the assertion rests on.
func TestStoreBeadsKeepsTheIDsSorted(t *testing.T) {
	t.Parallel()
	shuffled := []string{"gt-d", "gt-a", "gt-j", "gt-f", "gt-b", "gt-h", "gt-e", "gt-c", "gt-i", "gt-g"}
	records := make([]land.ReviewRecord, 0, len(shuffled))
	notes := notesStore{}
	for _, id := range shuffled {
		records = append(records, land.ReviewRecord{BeadID: id})
		notes[id] = "Branch: polecat/opal/" + id + "\n"
	}
	env := Env{Town: "/town", Beads: notesStores(notes)}
	rows, err := storeBeads(context.Background(), env, "/town/gastown", records)
	if err != nil {
		t.Fatalf("storeBeads: %v", err)
	}
	var ids []string
	for _, row := range rows {
		ids = append(ids, row.ID)
	}
	want := []string{"gt-a", "gt-b", "gt-c", "gt-d", "gt-e", "gt-f", "gt-g", "gt-h", "gt-i", "gt-j"}
	if !reflect.DeepEqual(ids, want) {
		t.Errorf("rows = %v, want %v in id order", ids, want)
	}
}

// listingStore serves a bead table both ways bd does: a whole-board List and a
// ShowMultiple by id. It records every read, so a test sees which of the two the
// attribution took and with which options, and it serves the same notes either
// way, as bd does.
type listingStore struct {
	branches map[string]string // bead id -> the branch its notes name, "" for none
	lists    []beads.ListOptions
	shows    [][]string
	listErr  error
}

func (s *listingStore) List(opts beads.ListOptions) ([]*beads.Issue, error) {
	s.lists = append(s.lists, opts)
	if s.listErr != nil {
		return nil, s.listErr
	}
	issues := make([]*beads.Issue, 0, len(s.branches))
	for id, branch := range s.branches {
		issues = append(issues, &beads.Issue{ID: id, Notes: landingNotes(branch)})
	}
	return issues, nil
}

func (s *listingStore) ReadyFiltered(beads.ReadyOptions) ([]*beads.Issue, error) {
	return nil, errors.New("the attribution read the ready board")
}

func (s *listingStore) Blocked() ([]*beads.Issue, error) {
	return nil, errors.New("the attribution read the blocked board")
}

func (s *listingStore) ShowMultiple(ids []string) (map[string]*beads.Issue, error) {
	s.shows = append(s.shows, append([]string(nil), ids...))
	found := make(map[string]*beads.Issue, len(ids))
	for _, id := range ids {
		branch, ok := s.branches[id]
		if !ok {
			continue
		}
		found[id] = &beads.Issue{ID: id, Notes: landingNotes(branch)}
	}
	return found, nil
}

// landingNotes is the note the landing worker writes for a branch, or a note
// naming no branch when branch is empty.
func landingNotes(branch string) string {
	if branch == "" {
		return "READY TO LAND\nHead: abc\n"
	}
	return "READY TO LAND\nBranch: " + branch + "\nHead: abc\n"
}

// listingEnv is the seam a test reads a store through without a town behind it.
func listingEnv(store *listingStore) Env {
	return Env{Town: "/town", Beads: func(context.Context, string) BeadStore { return store }}
}

// authorFixture returns n review records of one rig and the branch each bead's
// notes name, cycling a polecat branch, a crew branch, the overseer's crew
// branch and a bead naming no branch. A case larger than storeShowChunk takes
// the listing read, so its rows come from every shape a kind is read from.
func authorFixture(n int) ([]land.ReviewRecord, map[string]string) {
	records := make([]land.ReviewRecord, 0, n)
	branches := make(map[string]string, n)
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("gt-%03d", i)
		records = append(records, land.ReviewRecord{BeadID: id, Rig: "gastown", Verdict: land.VerdictApprove})
		switch i % 4 {
		case 0:
			branches[id] = "polecat/alfa/" + id
		case 1:
			branches[id] = "crew/merlin/" + id
		case 2:
			branches[id] = "crew/overseer/" + id
		default:
			branches[id] = ""
		}
	}
	return records, branches
}

// assertKinds checks that each record is attributed the kind its bead's branch
// names, in the order the records were passed.
func assertKinds(t *testing.T, got []AttributedRecord, records []land.ReviewRecord, branches map[string]string) {
	t.Helper()
	if len(got) != len(records) {
		t.Fatalf("attributed records = %d, want %d", len(got), len(records))
	}
	for i, rec := range records {
		if got[i].Record.BeadID != rec.BeadID {
			t.Fatalf("record %d = %s, want %s in the log's order", i, got[i].Record.BeadID, rec.BeadID)
		}
		if want := AuthorKind(branches[rec.BeadID]); got[i].AuthorKind != want {
			t.Errorf("record %d (%s) kind = %q, want %q for the branch its notes name",
				i, rec.BeadID, got[i].AuthorKind, want)
		}
	}
}

// A store naming more than storeShowChunk beads is read once with the
// whole-board listing, not shown a chunk at a time, and the rows it yields are
// the ones the show path gives the same beads.
func TestAttributeRecordsReadsALargeStoreWithOneList(t *testing.T) {
	t.Parallel()
	records, branches := authorFixture(2*storeShowChunk + 2)
	store := &listingStore{branches: branches}
	got, err := AttributeRecords(context.Background(), listingEnv(store), records)
	if err != nil {
		t.Fatalf("AttributeRecords: %v", err)
	}
	if len(store.lists) != 1 {
		t.Fatalf("the store was listed %d time(s), want one whole-board read", len(store.lists))
	}
	if opts := store.lists[0]; opts.Status != "all" || opts.Limit != 0 {
		t.Errorf("List(%+v), want Status %q and Limit 0: bd's default of 50 rows would drop every bead past it", opts, "all")
	}
	if len(store.shows) != 0 {
		t.Errorf("the store was shown %d time(s), want none: a store this size is one listing", len(store.shows))
	}
	assertKinds(t, got, records, branches)

	// The same beads read through the show path agree row for row with the
	// listing's, so the listing changes how the notes are read, not which rows
	// the table gets.
	smallStore := &listingStore{branches: branches}
	small, err := AttributeRecords(context.Background(), listingEnv(smallStore), records[:storeShowChunk])
	if err != nil {
		t.Fatalf("AttributeRecords of the show path: %v", err)
	}
	if len(smallStore.shows) != 1 || len(smallStore.lists) != 0 {
		t.Fatalf("the %d-bead store was shown %d time(s) and listed %d, want one show and no listing",
			storeShowChunk, len(smallStore.shows), len(smallStore.lists))
	}
	if !reflect.DeepEqual(small, got[:storeShowChunk]) {
		t.Errorf("the show path's rows differ from the listing's for the same %d beads", storeShowChunk)
	}
}

// A store naming storeShowChunk beads, the size of the show's own read, keeps
// the show: the listing costs a whole board for ids it may not name.
func TestAttributeRecordsKeepsShowForASmallStore(t *testing.T) {
	t.Parallel()
	records, branches := authorFixture(storeShowChunk)
	store := &listingStore{branches: branches}
	got, err := AttributeRecords(context.Background(), listingEnv(store), records)
	if err != nil {
		t.Fatalf("AttributeRecords: %v", err)
	}
	if len(store.lists) != 0 {
		t.Errorf("the store was listed %d time(s), want none: %d beads are one show", len(store.lists), storeShowChunk)
	}
	if len(store.shows) != 1 {
		t.Fatalf("the store was shown %d time(s), want one", len(store.shows))
	}
	if len(store.shows[0]) != storeShowChunk {
		t.Errorf("the show asked for %d ids, want %d", len(store.shows[0]), storeShowChunk)
	}
	assertKinds(t, got, records, branches)
}

// A bead the whole-board listing no longer holds is the unknown author's, as a
// bead the show leaves out is: a gone bead is an answer, not a broken read.
func TestAttributeRecordsLeavesOutABeadTheListingDoesNotHold(t *testing.T) {
	t.Parallel()
	records, branches := authorFixture(2*storeShowChunk + 2)
	gone := records[0].BeadID
	delete(branches, gone)
	got, err := AttributeRecords(context.Background(), listingEnv(&listingStore{branches: branches}), records)
	if err != nil {
		t.Fatalf("AttributeRecords: %v", err)
	}
	if got[0].AuthorKind != AuthorKindUnknown {
		t.Errorf("%s kind = %q, want %q for a bead the listing leaves out", gone, got[0].AuthorKind, AuthorKindUnknown)
	}
	assertKinds(t, got[1:], records[1:], branches)
}

// A failing listing is an error naming the read, never the unknown author's: a
// kind derived from a read that broke would be printed as a seat's.
func TestAttributeRecordsRefusesAFailedListing(t *testing.T) {
	t.Parallel()
	records, branches := authorFixture(2*storeShowChunk + 2)
	store := &listingStore{branches: branches, listErr: errors.New("dolt is down")}
	got, err := AttributeRecords(context.Background(), listingEnv(store), records)
	if err == nil {
		t.Fatalf("AttributeRecords = %v, nil; want the failed listing reported", got)
	}
	if !strings.Contains(err.Error(), "bd list in") {
		t.Errorf("err = %v, want it to name the listing read", err)
	}
}

// storeBeads returns the rows the records' ids name and no other, so the
// whole-board listing answers what a show of those ids would: a bead the
// records do not name is left out of the rows, and an id bd would read as a
// flag is never matched.
func TestStoreBeadsTakesOnlyTheListedRowsTheIDsName(t *testing.T) {
	t.Parallel()
	records, branches := authorFixture(2*storeShowChunk + 2)
	branches["gt-extra"] = "polecat/alfa/gt-extra"
	branches["-gt-flag"] = "polecat/alfa/-gt-flag"
	records = append(records, land.ReviewRecord{BeadID: "-gt-flag", Rig: "gastown"})
	rows, err := storeBeads(context.Background(), listingEnv(&listingStore{branches: branches}), "/town/gastown", records)
	if err != nil {
		t.Fatalf("storeBeads: %v", err)
	}
	want := make([]string, 0, len(records))
	for _, rec := range records {
		if isBeadID(rec.BeadID) {
			want = append(want, rec.BeadID)
		}
	}
	sort.Strings(want)
	var got []string
	for _, row := range rows {
		got = append(got, row.ID)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("rows = %v, want the %d named ids %v", got, len(want), want)
	}
}

func TestAuthorKindTable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		branch string
		want   string
	}{
		{"a polecat branch", "polecat/corpus/gt-1+mv1", AuthorKindPolecat},
		{"a polecat seat named overseer is still a polecat's", "polecat/overseer/gt-1+mv1", AuthorKindPolecat},
		{"a crew branch", "crew/sloan/gt-1", AuthorKindCrew},
		{"the overseer's own crew branch", "crew/overseer/gt-1", AuthorKindOverseer},
		{"a branch prefix the town does not use", "sloan/post-land-shell-scope", AuthorKindUnknown},
		{"no branch", "", AuthorKindUnknown},
		{"a branch that names no seat", "polecat/corpus", AuthorKindUnknown},
		{"a crew branch that names no seat", "crew/sloan", AuthorKindUnknown},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := AuthorKind(tc.branch); got != tc.want {
				t.Errorf("AuthorKind(%q) = %q, want %q", tc.branch, got, tc.want)
			}
		})
	}
}

func TestAuthorSeatTable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		branch string
		want   string
	}{
		{"a polecat branch", "polecat/corpus/gt-1+mv1", "corpus"},
		{"a crew branch", "crew/sloan/gt-1", "sloan"},
		{"a crew branch the overseer wrote", "crew/overseer/gt-1", "overseer"},
		{"a branch with no third segment", "polecat/corpus", ""},
		{"a branch prefix the town does not use", "sloan/post-land-shell-scope", ""},
		{"no branch", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := AuthorSeat(tc.branch); got != tc.want {
				t.Errorf("AuthorSeat(%q) = %q, want %q", tc.branch, got, tc.want)
			}
		})
	}
}

// The two functions read one branch, so a branch a seat can be read from is
// never an unknown author, and a branch with no seat is never a seat kind: a
// crew/overseer branch is the overseer's work from either function.
func TestAuthorKindNamesASeatExactlyWhenAuthorSeatDoes(t *testing.T) {
	t.Parallel()
	branches := []string{
		"polecat/corpus/gt-1+mv1",
		"crew/sloan/gt-1",
		"crew/overseer/gt-1",
		"polecat/corpus",
		"crew/sloan",
		"sloan/post-land-shell-scope",
		"",
	}
	for _, branch := range branches {
		seat := AuthorSeat(branch)
		kind := AuthorKind(branch)
		if (seat == "") != (kind == AuthorKindUnknown) {
			t.Errorf("branch %q: seat %q, kind %q; want no seat exactly when the kind is unknown", branch, seat, kind)
		}
		if seat == "overseer" && kind != AuthorKindOverseer {
			t.Errorf("branch %q: seat %q, kind %q; want the overseer's kind", branch, seat, kind)
		}
	}
}

func TestBranchLineTable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		notes string
		want  string
	}{
		{"one Branch line", "READY TO LAND\nBranch: polecat/opal/gt-1+mv1\nHead: abc\n", "polecat/opal/gt-1+mv1"},
		{
			name:  "the last Branch line is the live branch",
			notes: "READY TO LAND\nBranch: polecat/opal/gt-1+mv1\nHead: abc\n\nMERGE REJECTION (attempt 1): review - r\nBranch: polecat/opal/gt-1+mv2\nTarget: main\n",
			want:  "polecat/opal/gt-1+mv2",
		},
		{
			name:  "a quoted gate tail cannot name a branch",
			notes: "MERGE REJECTION (attempt 1): gate - red\nBranch: crew/sloan/gt-2\nGate tail:\n  | Branch: evil/branch\n",
			want:  "crew/sloan/gt-2",
		},
		{"a Branch line inside prose is not a line", "see the Branch: polecat/opal/gt-3 note\n", ""},
		{"a note with no Branch line", "READY TO LAND\nHead: abc\n", ""},
		{"an empty Branch line names nothing", "Branch: \n", ""},
		{"a trailing space and a CRLF", "Branch: crew/sloan/gt-4 \r\n", "crew/sloan/gt-4"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := BranchLine(tc.notes); got != tc.want {
				t.Errorf("BranchLine(%q) = %q, want %q", tc.notes, got, tc.want)
			}
		})
	}
}

// authorKindBdFixture holds one bead per attribution case: a polecat branch, a
// crew branch, a crew branch the overseer took over after the review, a bead
// with no Branch line, a bead whose Branch line is only in its description, and
// the overseer's own branch. Every bead carries the assignee bd sends, which
// attribution reads past: gt-5's branch is the crew's even though the bead is
// the overseer's now.
const authorKindBdFixture = `[
 {"id": "gt-1", "notes": "READY TO LAND\nBranch: polecat/opal/gt-1+mv1\nHead: abc\n", "assignee": "gastown/polecats/opal"},
 {"id": "gt-2", "notes": "READY TO LAND\nBranch: crew/merlin/gt-2\nHead: abc\n", "assignee": "gastown/crew/merlin"},
 {"id": "gt-3", "notes": "READY TO LAND\nHead: abc\n", "assignee": "gastown/polecats/robin"},
 {"id": "gt-4", "description": "Branch: polecat/emerald/gt-4+mv1", "assignee": "gastown/polecats/emerald"},
 {"id": "gt-5", "notes": "READY TO LAND\nBranch: crew/merlin/gt-5\nHead: abc\n", "assignee": "town/crew/overseer"},
 {"id": "gt-6", "notes": "READY TO LAND\nBranch: crew/overseer/gt-6\nHead: abc\n", "assignee": "town/crew/overseer"}
]`

var authorKindReviewLines = []string{
	`{"bead": "gt-1", "rig": "gastown", "attempt": 1, "score": 0.9, "verdict": "approve", "at": "2026-10-26T22:00:00Z"}`,
	`{"bead": "gt-2", "rig": "gastown", "attempt": 1, "score": 0.9, "verdict": "approve", "at": "2026-10-26T22:05:00Z"}`,
	`{"bead": "gt-3", "rig": "gastown", "attempt": 1, "score": 0.4, "verdict": "request_changes", "at": "2026-10-26T22:10:00Z"}`,
	`{"bead": "gt-4", "rig": "gastown", "attempt": 1, "score": 0.9, "verdict": "approve", "at": "2026-10-26T22:12:00Z"}`,
	`{"bead": "gt-5", "rig": "gastown", "attempt": 1, "score": 0.9, "verdict": "approve", "at": "2026-10-26T22:13:00Z"}`,
	`{"bead": "gt-6", "rig": "gastown", "attempt": 1, "score": 0.9, "verdict": "approve", "at": "2026-10-26T22:14:00Z"}`,
	`{"bead": "gt-9", "rig": "gastown", "attempt": 2, "score": 0.9, "verdict": "approve", "at": "2026-10-26T22:15:00Z"}`,
}

// The attribution runs through the production loaders: bd's listing supplies
// the Branch line, the review log supplies the attempt, and AttributeReviews
// joins them.
func TestAttributeReviewsJoinsEachReviewToItsBeadsBranch(t *testing.T) {
	t.Parallel()
	var calls []bdCall
	beads := LoadBeads(fakeBd(authorKindBdFixture, true, &calls), []string{"/g"}, &Dropped{})
	if len(beads) != 6 {
		t.Fatalf("beads = %d, want 6", len(beads))
	}
	if beads[0].Branch != "polecat/opal/gt-1+mv1" {
		t.Errorf("gt-1 branch = %q, want the listing's", beads[0].Branch)
	}
	if beads[3].Branch != "" {
		t.Errorf("gt-4 branch = %q, want none: a Branch line in the description is not the bead's", beads[3].Branch)
	}
	reviews := LoadReviews(authorKindReviewLines, &Dropped{})
	if len(reviews) != 7 {
		t.Fatalf("reviews = %d, want 7", len(reviews))
	}
	got := AttributeReviews(reviews, beads)
	want := []struct {
		author, kind string
		round        int
	}{
		{"opal", AuthorKindPolecat, 1},
		{"merlin", AuthorKindCrew, 1},
		{"", AuthorKindUnknown, 1},          // no Branch line on the bead
		{"", AuthorKindUnknown, 1},          // a Branch line only in the description names nothing
		{"merlin", AuthorKindCrew, 1},       // a crew branch the overseer took over after the review
		{"overseer", AuthorKindOverseer, 1}, // the overseer's own branch
		{"", AuthorKindUnknown, 2},          // a bead the listing does not hold
	}
	if len(got) != len(want) {
		t.Fatalf("attributed reviews = %d, want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i].Author != w.author || got[i].AuthorKind != w.kind || got[i].Attempt != w.round {
			t.Errorf("review %d (%s) = author %q kind %q attempt %d; want %q, %q, %d",
				i, got[i].Bead, got[i].Author, got[i].AuthorKind, got[i].Attempt, w.author, w.kind, w.round)
		}
	}
	if reviews[0].AuthorKind != "" {
		t.Errorf("input review kind = %q, want the input left unattributed", reviews[0].AuthorKind)
	}
}

// The round a review is counted under is the attempt its log line carries, not
// a recount of how many reviews the bead has.
func TestReviewAttemptIsTheAttemptTheReviewLogCarries(t *testing.T) {
	t.Parallel()
	reviews := LoadReviews([]string{
		`{"bead": "gt-1", "rig": "gastown", "attempt": 3, "score": 0.9, "verdict": "approve", "at": "2026-10-26T22:00:00Z"}`,
	}, &Dropped{})
	if len(reviews) != 1 {
		t.Fatalf("reviews = %d, want 1", len(reviews))
	}
	if reviews[0].Attempt != 3 {
		t.Errorf("attempt = %d, want the log's 3", reviews[0].Attempt)
	}
}

func TestAttributeReviewsWithNoBeadsIsUnknownNotPolecat(t *testing.T) {
	t.Parallel()
	got := AttributeReviews([]Review{{Bead: "gt-1", Attempt: 1}}, nil)
	if got[0].AuthorKind != AuthorKindUnknown {
		t.Errorf("kind = %q, want %q", got[0].AuthorKind, AuthorKindUnknown)
	}
}

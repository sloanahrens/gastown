package metrics

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/land"
)

// firstPassAt is the evening the fixtures land and review at, and
// firstPassEarlier is a landing a week before it, for the window's cases.
var (
	firstPassAt      = time.Date(2026, 10, 26, 20, 0, 0, 0, time.UTC)
	firstPassEarlier = firstPassAt.AddDate(0, 0, -7)
)

// firstPassTown writes a town holding one landing log per rig, keyed by the rig
// the file names, and the review log the verdicts go to. The record's own rig
// and commit are filled here, so a case names only the bead and the time.
func firstPassTown(t *testing.T, landings map[string][]land.LandingRecord, reviews ...land.ReviewRecord) string {
	t.Helper()
	town := t.TempDir()
	for rig, recs := range landings {
		file, err := land.RigLandingsFile(town, rig)
		if err != nil {
			t.Fatal(err)
		}
		var log strings.Builder
		for _, rec := range recs {
			rec.Rig = rig
			if rec.LandedCommit == "" {
				rec.LandedCommit = "c0ffee"
			}
			data, err := json.Marshal(rec)
			if err != nil {
				t.Fatal(err)
			}
			log.Write(data)
			log.WriteString("\n")
		}
		writeTownFile(t, file.Path, log.String())
	}
	var log strings.Builder
	for _, rec := range reviews {
		data, err := json.Marshal(rec)
		if err != nil {
			t.Fatal(err)
		}
		log.Write(data)
		log.WriteString("\n")
	}
	writeTownFile(t, land.TownReviewsFile(town).Path, log.String())
	return town
}

// firstPassLanded is a landing of bead in rig gastown at at.
func firstPassLanded(bead string, at time.Time) land.LandingRecord {
	return land.LandingRecord{BeadID: bead, LandedAt: at}
}

// firstPassVerdict is one review of bead in rig gastown at firstPassAt.
func firstPassVerdict(bead string, attempt int, verdict string) land.ReviewRecord {
	return land.ReviewRecord{
		BeadID: bead, Rig: "gastown", Attempt: attempt, Verdict: verdict, At: firstPassAt,
	}
}

// firstPassStores is the seam the first-pass section reads its beads through:
// the store's ShowMultiple answers the Branch line each bead's notes carry, as
// the attribution reads it, and records the ids each read asked for. A bead the
// case names no branch for is a fixture bug, so the case fails rather than
// answering a listing that would hide it.
func firstPassStores(t *testing.T, branches map[string]string, calls *[][]string) BeadStores {
	t.Helper()
	return func(context.Context, string) BeadStore {
		return firstPassStore{t: t, branches: branches, calls: calls}
	}
}

// firstPassStore is one rig's store over the fixture's branches. A case that
// passes lists records the options each whole-board read asked with, so a test
// sees a big store listed once rather than shown a chunk at a time.
type firstPassStore struct {
	t        *testing.T
	branches map[string]string
	calls    *[][]string
	lists    *[]beads.ListOptions
}

func (s firstPassStore) List(opts beads.ListOptions) ([]*beads.Issue, error) {
	if s.lists != nil {
		*s.lists = append(*s.lists, opts)
	}
	issues := make([]*beads.Issue, 0, len(s.branches))
	for id, branch := range s.branches {
		notes := ""
		if branch != "" {
			notes = "Branch: " + branch
		}
		issues = append(issues, &beads.Issue{ID: id, Notes: notes})
	}
	return issues, nil
}

func (s firstPassStore) ReadyFiltered(beads.ReadyOptions) ([]*beads.Issue, error) {
	return nil, errors.New("the first-pass section read the ready board")
}

func (s firstPassStore) Blocked() ([]*beads.Issue, error) {
	return nil, errors.New("the first-pass section read the blocked board")
}

func (s firstPassStore) ShowMultiple(ids []string) (map[string]*beads.Issue, error) {
	if s.calls != nil {
		*s.calls = append(*s.calls, append([]string(nil), ids...))
	}
	found := make(map[string]*beads.Issue, len(ids))
	if s.branches == nil {
		// A store holding no bead at all: every id asked for is left out, the
		// way a store that no longer has the bead answers.
		return found, nil
	}
	for _, id := range ids {
		branch, known := s.branches[id]
		if !known {
			s.t.Fatalf("the store was asked for %q, which the case names no branch for", id)
		}
		notes := ""
		if branch != "" {
			notes = "Branch: " + branch
		}
		found[id] = &beads.Issue{ID: id, Notes: notes}
	}
	return found, nil
}

// firstPassSection runs the section over town with a store answering branches.
func firstPassSection(t *testing.T, town string, since time.Time, branches map[string]string) string {
	t.Helper()
	text, err := MangoFirstPassSection(context.Background(), Env{
		Town: town, ReadFile: os.ReadFile, ReadDir: listDir,
		Beads: firstPassStores(t, branches, nil),
	}, since)
	if err != nil {
		t.Fatalf("MangoFirstPassSection: %v", err)
	}
	return text
}

// TestMangoFirstPassSectionCountsKinds is the view the bead asks for: first-pass
// rate and reviews per merged bead for polecat, crew and unknown, with bead
// counts beside each rate.
func TestMangoFirstPassSectionCountsKinds(t *testing.T) {
	t.Parallel()
	town := firstPassTown(t, map[string][]land.LandingRecord{
		"gastown": {
			firstPassLanded("gt-a", firstPassAt),
			firstPassLanded("gt-b", firstPassAt),
			firstPassLanded("gt-c", firstPassAt),
			firstPassLanded("gt-d", firstPassAt),
			firstPassLanded("gt-e", firstPassAt),
			firstPassLanded("gt-f", firstPassAt),
		},
	},
		firstPassVerdict("gt-a", 1, land.VerdictApprove),
		firstPassVerdict("gt-b", 1, land.VerdictRequestChanges),
		firstPassVerdict("gt-b", 2, land.VerdictApprove),
		firstPassVerdict("gt-c", 1, land.VerdictApprove),
		firstPassVerdict("gt-d", 1, land.VerdictRequestChanges),
		firstPassVerdict("gt-e", 1, land.VerdictRequestChanges),
	)
	text := firstPassSection(t, town, time.Time{}, map[string]string{
		"gt-a": "polecat/alfa/gt-a",
		"gt-b": "polecat/beta/gt-b",
		"gt-c": "crew/merlin/gt-c",
		"gt-d": "crew/merlin/gt-d",
		"gt-e": "feature/gt-e",
	})
	for _, want := range []string{
		"### First-pass rate by author kind",
		"Window: all merged beads.",
		"| polecat | 2 | 1 | 50.0% | 3 | 1.5 |",
		"| crew | 2 | 1 | 50.0% | 2 | 1.0 |",
		"| unknown | 1 | 0 | 0.0% | 1 | 1.0 |",
		"1 merged bead(s) carry no review line and count in no row.",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("text = %q, want it to hold %q", text, want)
		}
	}
}

// TestMangoFirstPassSectionCountsABeadByItsFirstReview holds the rule to the
// log's order: a bead whose first verdict asked for changes is not a first
// pass, even though a later verdict on the same bead approved it.
func TestMangoFirstPassSectionCountsABeadByItsFirstReview(t *testing.T) {
	t.Parallel()
	town := firstPassTown(t, map[string][]land.LandingRecord{
		"gastown": {firstPassLanded("gt-b", firstPassAt)},
	},
		firstPassVerdict("gt-b", 1, land.VerdictRequestChanges),
		firstPassVerdict("gt-b", 2, land.VerdictApprove),
	)
	text := firstPassSection(t, town, time.Time{}, map[string]string{"gt-b": "polecat/beta/gt-b"})
	if want := "| polecat | 1 | 0 | 0.0% | 2 | 2.0 |"; !strings.Contains(text, want) {
		t.Errorf("text = %q, want the bead's row to hold %q", text, want)
	}
}

// TestMangoFirstPassSectionCountsARefusedApprovalAsRework drives the branch a
// later verdict cannot show: the review said approve and the landing turned it
// away on its findings, which is a refusal and not a first pass.
func TestMangoFirstPassSectionCountsARefusedApprovalAsRework(t *testing.T) {
	t.Parallel()
	refused := firstPassVerdict("gt-a", 1, land.VerdictApprove)
	refused.Rejected = true
	town := firstPassTown(t, map[string][]land.LandingRecord{
		"gastown": {firstPassLanded("gt-a", firstPassAt)},
	}, refused)
	text := firstPassSection(t, town, time.Time{}, map[string]string{"gt-a": "polecat/alfa/gt-a"})
	if want := "| polecat | 1 | 0 | 0.0% | 1 | 1.0 |"; !strings.Contains(text, want) {
		t.Errorf("text = %q, want a refused approval to read as no first pass (%q)", text, want)
	}
}

// TestMangoFirstPassSectionReadsTheDerivedAuthorKind holds the constraint: the
// kind is the one the review attribution derives from the bead's branch, so a
// landing record naming another branch cannot reclassify the bead.
func TestMangoFirstPassSectionReadsTheDerivedAuthorKind(t *testing.T) {
	t.Parallel()
	landing := firstPassLanded("gt-a", firstPassAt)
	landing.Branch = "crew/merlin/gt-a"
	town := firstPassTown(t, map[string][]land.LandingRecord{"gastown": {landing}},
		firstPassVerdict("gt-a", 1, land.VerdictApprove))
	text := firstPassSection(t, town, time.Time{}, map[string]string{"gt-a": "polecat/alfa/gt-a"})
	if want := "| polecat | 1 | 1 | 100.0% | 1 | 1.0 |"; !strings.Contains(text, want) {
		t.Errorf("text = %q, want the bead under the derived kind %q", text, want)
	}
	if strings.Contains(text, "| crew | 1 |") {
		t.Errorf("text = %q, want no crew row counting the bead", text)
	}
}

// TestMangoFirstPassSectionWindowsMergedBeads drives the window: a landing
// before since counts in no row, and the text names the window it covers.
func TestMangoFirstPassSectionWindowsMergedBeads(t *testing.T) {
	t.Parallel()
	town := firstPassTown(t, map[string][]land.LandingRecord{
		"gastown": {
			firstPassLanded("gt-a", firstPassEarlier),
			firstPassLanded("gt-b", firstPassAt),
		},
	},
		firstPassVerdict("gt-a", 1, land.VerdictApprove),
		firstPassVerdict("gt-b", 1, land.VerdictApprove),
	)
	since := firstPassAt.AddDate(0, 0, -1)
	text := firstPassSection(t, town, since, map[string]string{
		"gt-a": "polecat/alfa/gt-a",
		"gt-b": "polecat/beta/gt-b",
	})
	for _, want := range []string{
		"Window: merged beads landed at or after " + since.Format(time.RFC3339) + ".",
		"| polecat | 1 | 1 | 100.0% | 1 | 1.0 |",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("text = %q, want it to hold %q", text, want)
		}
	}
}

// TestMangoFirstPassSectionNamesTheOverseerBesideTheTable holds the row set to
// the kinds the bead names: the overseer's merges are counted in a sentence,
// not in a row.
func TestMangoFirstPassSectionNamesTheOverseerBesideTheTable(t *testing.T) {
	t.Parallel()
	town := firstPassTown(t, map[string][]land.LandingRecord{
		"gastown": {firstPassLanded("gt-a", firstPassAt)},
	},
		firstPassVerdict("gt-a", 1, land.VerdictApprove),
	)
	text := firstPassSection(t, town, time.Time{}, map[string]string{"gt-a": "crew/overseer/gt-a"})
	if strings.Contains(text, "| overseer |") {
		t.Errorf("text = %q, want no overseer row", text)
	}
	if want := "1 merged bead(s) are the overseer's, a kind this table does not name."; !strings.Contains(text, want) {
		t.Errorf("text = %q, want it to hold %q", text, want)
	}
}

// TestMangoFirstPassSectionCountsADroppedBeadAsUnknown drives a store that no
// longer lists a reviewed bead: its branch is unknown, which is an answer, and
// the read is not an error.
func TestMangoFirstPassSectionCountsADroppedBeadAsUnknown(t *testing.T) {
	t.Parallel()
	town := firstPassTown(t, map[string][]land.LandingRecord{
		"gastown": {firstPassLanded("gt-a", firstPassAt)},
	},
		firstPassVerdict("gt-a", 1, land.VerdictApprove),
	)
	text, err := MangoFirstPassSection(context.Background(), Env{
		Town: town, ReadFile: os.ReadFile, ReadDir: listDir,
		Beads: firstPassStores(t, nil, nil),
	}, time.Time{})
	if err != nil {
		t.Fatalf("MangoFirstPassSection: %v", err)
	}
	if want := "| unknown | 1 | 1 | 100.0% | 1 | 1.0 |"; !strings.Contains(text, want) {
		t.Errorf("text = %q, want the dropped bead under unknown (%q)", text, want)
	}
}

// TestMangoFirstPassSectionNamesTheLinesItCouldNotRead holds the table to
// saying how much of each log it read: a torn landing line and review line are
// counted, not silently dropped.
func TestMangoFirstPassSectionNamesTheLinesItCouldNotRead(t *testing.T) {
	t.Parallel()
	town := firstPassTown(t, map[string][]land.LandingRecord{
		"gastown": {firstPassLanded("gt-a", firstPassAt)},
	}, firstPassVerdict("gt-a", 1, land.VerdictApprove))
	file, err := land.RigLandingsFile(town, "gastown")
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(file.Path)
	if err != nil {
		t.Fatal(err)
	}
	writeTownFile(t, file.Path, string(data)+"{\"bead\":\"gt-b\"\n")
	reviews := land.TownReviewsFile(town).Path
	log, err := os.ReadFile(reviews)
	if err != nil {
		t.Fatal(err)
	}
	writeTownFile(t, reviews, string(log)+"not a review line\n")
	text := firstPassSection(t, town, time.Time{}, map[string]string{"gt-a": "polecat/alfa/gt-a"})
	for _, want := range []string{
		"1 review-log line(s) were not readable as records and count in no bead's reviews.",
		"1 landing-record line(s) were not readable and count as no bead's merge.",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("text = %q, want it to hold %q", text, want)
		}
	}
}

func TestMangoFirstPassSectionSaysWhenNothingMerged(t *testing.T) {
	t.Parallel()
	town := firstPassTown(t, nil)
	text := firstPassSection(t, town, time.Time{}, nil)
	if want := "No merged bead landed in the window: no first-pass rate to read."; !strings.Contains(text, want) {
		t.Errorf("text = %q, want it to hold %q", text, want)
	}
}

func TestMangoFirstPassSectionNeedsItsSeams(t *testing.T) {
	t.Parallel()
	stores := firstPassStores(t, nil, nil)
	cases := []struct {
		name string
		env  Env
		want string
	}{
		{"no town", Env{ReadFile: os.ReadFile, ReadDir: listDir, Beads: stores}, "Env.Town is unset"},
		{"no reader", Env{Town: t.TempDir(), ReadDir: listDir, Beads: stores}, "Env.ReadFile is unset"},
		{"no directory listing", Env{Town: t.TempDir(), ReadFile: os.ReadFile, Beads: stores}, "Env.ReadDir is unset"},
		{"no store", Env{Town: t.TempDir(), ReadFile: os.ReadFile, ReadDir: listDir}, "Env.Beads is unset"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			_, err := MangoFirstPassSection(context.Background(), c.env, time.Time{})
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want %q", err, c.want)
			}
		})
	}
}

func TestMangoFirstPassSectionFailsOnUnreadableReads(t *testing.T) {
	t.Parallel()
	town := firstPassTown(t, map[string][]land.LandingRecord{
		"gastown": {firstPassLanded("gt-a", firstPassAt)},
	}, firstPassVerdict("gt-a", 1, land.VerdictApprove))
	file, err := land.RigLandingsFile(town, "gastown")
	if err != nil {
		t.Fatal(err)
	}
	stores := firstPassStores(t, map[string]string{"gt-a": "polecat/alfa/gt-a"}, nil)
	cases := []struct {
		name string
		env  Env
		want string
	}{
		{
			name: "the review log",
			env: Env{Town: town, ReadDir: listDir, Beads: stores, ReadFile: func(string) ([]byte, error) {
				return nil, errors.New("permission denied")
			}},
			want: "reviews.jsonl",
		},
		{
			name: "the landings directory",
			env: Env{Town: town, ReadFile: os.ReadFile, Beads: stores, ReadDir: func(string) ([]DirEntry, error) {
				return nil, errors.New("permission denied")
			}},
			want: "landings directory",
		},
		{
			name: "a landing log",
			env: Env{Town: town, ReadDir: listDir, Beads: stores, ReadFile: func(path string) ([]byte, error) {
				if path == file.Path {
					return nil, errors.New("permission denied")
				}
				return os.ReadFile(path)
			}},
			want: "landings file",
		},
		{
			name: "a bead read that failed",
			env: Env{Town: town, ReadFile: os.ReadFile, ReadDir: listDir, Beads: func(context.Context, string) BeadStore {
				return failingStore{err: errors.New("dolt is down")}
			}},
			want: "bd show",
		},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			_, err := MangoFirstPassSection(context.Background(), c.env, time.Time{})
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want a refusal naming %q", err, c.want)
			}
		})
	}
}

// TestMangoFirstPassSectionRefusesAFileThatNamesNoRig drives a stray file in
// the landings directory: the section refuses rather than dropping a landing
// log it cannot attribute to a rig.
func TestMangoFirstPassSectionRefusesAFileThatNamesNoRig(t *testing.T) {
	t.Parallel()
	town := firstPassTown(t, nil)
	writeTownFile(t, filepath.Join(constants.TownRuntimePath(town), "landings", ".jsonl"), "")
	_, err := MangoFirstPassSection(context.Background(), Env{
		Town: town, ReadFile: os.ReadFile, ReadDir: listDir, Beads: firstPassStores(t, nil, nil),
	}, time.Time{})
	if err == nil || !strings.Contains(err.Error(), "landings file") {
		t.Fatalf("err = %v, want a refusal naming the landings file", err)
	}
}

// TestMangoFirstPassSectionReadsABigStoreWithOneList holds a store naming more
// than storeShowChunk beads to one whole-board listing with the unclipped row
// count, not a show chunked by 64.
func TestMangoFirstPassSectionReadsABigStoreWithOneList(t *testing.T) {
	t.Parallel()
	const merged = 2*storeShowChunk + 2
	landings := make([]land.LandingRecord, 0, merged)
	reviews := make([]land.ReviewRecord, 0, merged)
	branches := make(map[string]string, merged)
	for i := 0; i < merged; i++ {
		id := fmt.Sprintf("gt-%03d", i)
		landings = append(landings, firstPassLanded(id, firstPassAt))
		reviews = append(reviews, firstPassVerdict(id, 1, land.VerdictApprove))
		branches[id] = "polecat/alfa/" + id
	}
	town := firstPassTown(t, map[string][]land.LandingRecord{"gastown": landings}, reviews...)
	var calls [][]string
	var lists []beads.ListOptions
	text, err := MangoFirstPassSection(context.Background(), Env{
		Town: town, ReadFile: os.ReadFile, ReadDir: listDir,
		Beads: func(context.Context, string) BeadStore {
			return firstPassStore{t: t, branches: branches, calls: &calls, lists: &lists}
		},
	}, time.Time{})
	if err != nil {
		t.Fatalf("MangoFirstPassSection: %v", err)
	}
	if len(calls) != 0 {
		t.Errorf("the store was shown %d time(s), want none: %d beads are one listing", len(calls), merged)
	}
	if len(lists) != 1 {
		t.Fatalf("the store was listed %d time(s), want one whole-board read", len(lists))
	}
	if opts := lists[0]; opts.Status != "all" || opts.Limit != 0 {
		t.Errorf("List(%+v), want Status %q and Limit 0: bd's default of 50 rows would drop every bead past it", opts, "all")
	}
	want := fmt.Sprintf("| polecat | %d | %d | 100.0%% | %d | 1.0 |", merged, merged, merged)
	if !strings.Contains(text, want) {
		t.Errorf("text = %q, want it to hold %q", text, want)
	}
}

// TestMangoFirstPassSectionRefusesAMissingLandingsDirectory drives a read that
// cannot find the directory: it is an error naming the path, not a town that
// merged nothing.
func TestMangoFirstPassSectionRefusesAMissingLandingsDirectory(t *testing.T) {
	t.Parallel()
	_, err := mangoFirstPassSection(context.Background(), Env{
		Town: t.TempDir(), ReadFile: os.ReadFile, Beads: firstPassStores(t, nil, nil),
		ReadDir: func(string) ([]DirEntry, error) { return nil, fs.ErrNotExist },
	}, time.Time{}, nil, 0)
	if err == nil || !strings.Contains(err.Error(), "landings directory") {
		t.Fatalf("err = %v, want a refusal naming the landings directory", err)
	}
}

// TestFirstPassReviewsRefusesARigThatIsNotOnePathElement holds the attribution
// to a rig name it may join to a directory: the review log is a file, and a rig
// read from it is never a path.
func TestFirstPassReviewsRefusesARigThatIsNotOnePathElement(t *testing.T) {
	t.Parallel()
	review := firstPassVerdict("gt-a", 1, land.VerdictApprove)
	review.Rig = "../evil"
	_, err := firstPassReviews(context.Background(), Env{
		Town: t.TempDir(), ReadFile: os.ReadFile, ReadDir: listDir,
		Beads: firstPassStores(t, map[string]string{"gt-a": "polecat/alfa/gt-a"}, nil),
	}, []land.ReviewRecord{review}, []MergedBead{{Rig: "../evil", Bead: "gt-a"}})
	if err == nil || !strings.Contains(err.Error(), "not one path element") {
		t.Fatalf("err = %v, want a refusal naming the rig", err)
	}
}

// TestCountFirstPassCountsABeadOnce drives the dedupe a log that landed one
// bead twice leaves to the count, so a re-landed bead is one merged bead.
func TestCountFirstPassCountsABeadOnce(t *testing.T) {
	t.Parallel()
	table := CountFirstPass(
		[]MergedBead{{Rig: "gastown", Bead: "gt-a"}, {Rig: "gastown", Bead: "gt-a"}},
		[]AttributedRecord{{
			Record:     land.ReviewRecord{Rig: "gastown", BeadID: "gt-a", Verdict: land.VerdictApprove},
			AuthorKind: AuthorKindPolecat,
		}},
	)
	if got := table.Rows[0]; got.Beads != 1 || got.FirstPass != 1 || got.Reviews != 1 {
		t.Errorf("polecat row = %+v, want one bead, one first pass, one review", got)
	}
}

// TestCountFirstPassKeepsEveryNamedKind drives a kind with no merged bead: its
// row stays, so the table's rows do not move with the data.
func TestCountFirstPassKeepsEveryNamedKind(t *testing.T) {
	t.Parallel()
	table := CountFirstPass(nil, nil)
	if len(table.Rows) != len(firstPassKinds) {
		t.Fatalf("rows = %+v, want one row per named kind", table.Rows)
	}
	for i, kind := range firstPassKinds {
		if table.Rows[i].Kind != kind {
			t.Errorf("row %d = %q, want %q", i, table.Rows[i].Kind, kind)
		}
	}
}

// TestMangoSectionCarriesTheFirstPassTable is the digest's side of the
// section: MangoSection appends the first-pass table and passes its window.
func TestMangoSectionCarriesTheFirstPassTable(t *testing.T) {
	t.Parallel()
	dir := mangoSectionTown(t, mangoSectionRows())
	mango, _ := mangoSectionRun(t, mangoSectionResult)
	town := firstPassTown(t, map[string][]land.LandingRecord{
		"gastown": {firstPassLanded("gt-a", firstPassAt)},
	}, firstPassVerdict("gt-a", 1, land.VerdictApprove))
	env := Env{Town: town, ReadFile: os.ReadFile, ReadDir: listDir, Run: mango,
		Beads: firstPassStores(t, map[string]string{"gt-a": "polecat/alfa/gt-a"}, nil)}
	text, err := MangoSection(context.Background(), env,
		dir, "/opt/mango", MangoSectionOptions{MinPositives: 1})
	if err != nil {
		t.Fatalf("MangoSection: %v", err)
	}
	for _, want := range []string{
		"### First-pass rate by author kind",
		"| polecat | 1 | 1 | 100.0% | 1 | 1.0 |",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("text = %q, want it to hold %q", text, want)
		}
	}
	windowed, err := MangoSection(context.Background(), env,
		dir, "/opt/mango", MangoSectionOptions{MinPositives: 1, Since: firstPassAt.Add(time.Hour)})
	if err != nil {
		t.Fatalf("MangoSection: %v", err)
	}
	if want := "No merged bead landed in the window: no first-pass rate to read."; !strings.Contains(windowed, want) {
		t.Errorf("text = %q, want its window passed through (%q)", windowed, want)
	}
}

// TestMangoSectionNeedsItsSeams drives each seam the digest's tables read
// through, so a host missing one is told which rather than given a table
// built from nothing.
func TestMangoSectionNeedsItsSeams(t *testing.T) {
	t.Parallel()
	town := firstPassTown(t, nil)
	run := func(context.Context, []string) ([]byte, error) { return []byte(`[]`), nil }
	cases := []struct {
		name string
		env  Env
		want string
	}{
		{"no town", Env{ReadFile: os.ReadFile, ReadDir: listDir, Run: run}, "Env.Town is unset"},
		{"no reader", Env{Town: town, ReadDir: listDir, Run: run}, "Env.ReadFile is unset"},
		{"no directory listing", Env{Town: town, ReadFile: os.ReadFile, Run: run}, "Env.ReadDir is unset"},
		{"no runner", Env{Town: town, ReadFile: os.ReadFile, ReadDir: listDir}, "Env.Run is unset"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			_, err := MangoSection(context.Background(), c.env, t.TempDir(), "/opt/mango", MangoSectionOptions{})
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want %q", err, c.want)
			}
		})
	}
}

// TestOSEnvClearsTheDigestSeamCheck holds the town's own Env wiring to every
// seam MangoSection requires: the section reads through OSEnv in production, so
// the digest reaches its files rather than refusing a host for a seam the
// production constructor leaves unset.
func TestOSEnvClearsTheDigestSeamCheck(t *testing.T) {
	t.Parallel()
	_, err := MangoSection(context.Background(), OSEnv(t.TempDir()), t.TempDir(), "/opt/mango", MangoSectionOptions{})
	if err == nil {
		t.Fatal("MangoSection over OSEnv on an empty town succeeded, want the missing table's refusal")
	}
	if !strings.Contains(err.Error(), landingsName) {
		t.Fatalf("err = %v, want the section past its seam check to %s", err, landingsName)
	}
}

// TestMangoSectionReportsARejectionTableFailure holds the digest to failing on
// a refusal table whose bd read broke, rather than appending the tables it
// could build.
func TestMangoSectionReportsARejectionTableFailure(t *testing.T) {
	t.Parallel()
	dir := mangoSectionTown(t, mangoSectionRows())
	mango, _ := mangoSectionRun(t, mangoSectionResult)
	town := rejectionTown(t, rejectionRecord("gt-a", 1, "opus"))
	env := Env{Town: town, ReadFile: os.ReadFile, ReadDir: listDir, Run: mango,
		Beads: func(context.Context, string) BeadStore {
			return failingStore{err: errors.New("dolt is down")}
		}}
	_, err := MangoSection(context.Background(), env,
		dir, "/opt/mango", MangoSectionOptions{MinPositives: 1})
	if err == nil || !strings.Contains(err.Error(), "bd show") {
		t.Fatalf("err = %v, want a refusal naming the bd read", err)
	}
}

// TestMangoSectionReportsAFirstPassReadFailure holds the digest to failing on
// a landings directory it cannot read.
func TestMangoSectionReportsAFirstPassReadFailure(t *testing.T) {
	t.Parallel()
	dir := mangoSectionTown(t, mangoSectionRows())
	mango, _ := mangoSectionRun(t, mangoSectionResult)
	town := firstPassTown(t, nil)
	_, err := MangoSection(context.Background(), Env{
		Town: town, ReadFile: os.ReadFile, Run: mango,
		ReadDir: func(string) ([]DirEntry, error) { return nil, errors.New("permission denied") },
	}, dir, "/opt/mango", MangoSectionOptions{MinPositives: 1})
	if err == nil || !strings.Contains(err.Error(), "landings directory") {
		t.Fatalf("err = %v, want a refusal naming the landings directory", err)
	}
}

// TestMangoSectionReadsTheReviewLogOnce holds the digest to one read of the
// review log: the first-pass table reads the same lines the refusal tables do.
func TestMangoSectionReadsTheReviewLogOnce(t *testing.T) {
	t.Parallel()
	dir := mangoSectionTown(t, mangoSectionRows())
	mango, _ := mangoSectionRun(t, mangoSectionResult)
	town := firstPassTown(t, map[string][]land.LandingRecord{
		"gastown": {firstPassLanded("gt-a", firstPassAt)},
	}, firstPassVerdict("gt-a", 1, land.VerdictApprove))
	reviewLog := land.TownReviewsFile(town).Path
	reads := 0
	readFile := func(path string) ([]byte, error) {
		if path == reviewLog {
			reads++
		}
		return os.ReadFile(path)
	}
	env := Env{Town: town, ReadFile: readFile, ReadDir: listDir, Run: mango,
		Beads: firstPassStores(t, map[string]string{"gt-a": "polecat/alfa/gt-a"}, nil)}
	if _, err := MangoSection(context.Background(), env,
		dir, "/opt/mango", MangoSectionOptions{MinPositives: 1}); err != nil {
		t.Fatalf("MangoSection: %v", err)
	}
	if reads != 1 {
		t.Errorf("review log read %d times, want once per digest", reads)
	}
}

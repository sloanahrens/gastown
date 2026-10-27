package metrics

import (
	"context"
	"fmt"
	"io/fs"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/land"
)

// reviewTestTown is the town root every review test reads under.
const reviewTestTown = "/town"

// reviewKindColumnNames are the four author-kind columns a collection
// publishes beside the counts.
var reviewKindColumnNames = []string{
	"om_polecat_reviews_1h",
	"om_crew_reviews_1h",
	"om_overseer_reviews_1h",
	"om_unknown_reviews_1h",
}

// reviewTestEnv serves the review log the test names: one file's content, no
// file at all, or the error a read gets instead.
func reviewTestEnv(t *testing.T, content string, missing bool, tailErr error) Env {
	t.Helper()
	path := land.TownReviewsFile(reviewTestTown).Path
	env := envAt(fixedNow())
	env.Town = reviewTestTown
	env.Tail = func(got string, maxBytes int64) ([]byte, error) {
		if got != path {
			t.Errorf("Tail(%s), want %s", got, path)
		}
		if maxBytes != sourceLogTailBytes {
			t.Errorf("Tail(%s) read %d bytes, want the %d-byte bound", got, maxBytes, sourceLogTailBytes)
		}
		if tailErr != nil {
			return nil, tailErr
		}
		if missing {
			return nil, fs.ErrNotExist
		}
		return []byte(content), nil
	}
	return env
}

// reviewLine is one verdict line in the shape land.ReviewRecord decodes.
func reviewLine(bead, verdict string, attempt int, score float64, rejected bool, at time.Time) string {
	return fmt.Sprintf(`{"bead":%q,"rig":"gastown","attempt":%d,"score":%g,"verdict":%q,"rejected":%t,"findings":2,"bar":7,"at":%q}`,
		bead, attempt, score, verdict, rejected, at.UTC().Format(time.RFC3339))
}

// reviewBdFake answers `bd show --json` from a table of beads and records the
// argv it was asked, so a test can see which rig each bead was read in.
type reviewBdFake struct {
	beads map[string]string // bead id -> the JSON object bd show prints for it
	fail  map[string]bool

	mu              sync.Mutex
	calls           [][]string
	deadlineMissing bool
}

func (f *reviewBdFake) run(ctx context.Context, argv []string) ([]byte, error) {
	if _, ok := ctx.Deadline(); !ok {
		f.mu.Lock()
		f.deadlineMissing = true
		f.mu.Unlock()
	}
	f.mu.Lock()
	f.calls = append(f.calls, argv)
	f.mu.Unlock()
	if len(argv) != 7 || argv[0] != "bd" || argv[1] != "-C" || argv[3] != "show" || argv[4] != "--json" || argv[5] != "--" {
		return nil, fmt.Errorf("unexpected argv %v", argv)
	}
	bead := argv[6]
	if f.fail[bead] {
		return nil, fmt.Errorf("bd show %s: no Dolt server", bead)
	}
	entry, ok := f.beads[bead]
	if !ok {
		return []byte("[]"), nil // a bead bd no longer holds
	}
	return []byte("[" + entry + "]"), nil
}

// reviewBeadJSON is one bead in the shape `bd show --json` prints it.
func reviewBeadJSON(id, branch string) string {
	notes := `"READY TO LAND\nHead: abc\n"`
	if branch != "" {
		notes = fmt.Sprintf("%q", "READY TO LAND\nBranch: "+branch+"\nHead: abc\n")
	}
	return fmt.Sprintf(`{"id":%q,"notes":%s}`, id, notes)
}

func TestSourceReviewCountsTheLastHour(t *testing.T) {
	t.Parallel()
	now := fixedNow()
	content := reviewLine("gt-a1", land.VerdictApprove, 1, 8.5, false, now.Add(-50*time.Minute)) + "\n" +
		reviewLine("gt-a2", land.VerdictRequestChanges, 1, 4, false, now.Add(-40*time.Minute)) + "\n" +
		// An approving verdict the landing refused is a rejection too.
		reviewLine("gt-a3", land.VerdictApprove, 2, 6, true, now.Add(-30*time.Minute)) + "\n" +
		reviewLine("gt-a4", land.VerdictApprove, 3, 9, false, now.Add(-20*time.Minute)) + "\n" +
		reviewLine("gt-old", land.VerdictRequestChanges, 1, 1, false, now.Add(-90*time.Minute)) + "\n" + // outside the hour
		reviewLine("gt-future", land.VerdictApprove, 1, 10, false, now.Add(5*time.Minute)) + "\n" + // not yet
		`{"bead":"gt-torn","verdict":` + "\n" + // torn append
		`{"verdict":"approve","at":"2026-10-24T12:30:00Z"}` + "\n" + // no bead
		`{"bead":"gt-nv","at":"2026-10-24T12:30:00Z"}` + "\n" // no verdict

	bdfake := &reviewBdFake{beads: map[string]string{
		"gt-a1": reviewBeadJSON("gt-a1", "polecat/opal/gt-a1+mv1"),
		"gt-a2": reviewBeadJSON("gt-a2", "crew/merlin/gt-a2"),
		"gt-a3": reviewBeadJSON("gt-a3", "crew/overseer/gt-a3"),
		"gt-a4": reviewBeadJSON("gt-a4", ""),
	}}
	env := reviewTestEnv(t, content, false, nil)
	env.Run = bdfake.run
	cols, err := reviewSource{}.Collect(context.Background(), env)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	want := map[string]any{
		"om_reviews_1h":          4,
		"om_rejections_1h":       2,
		"om_mean_score_1h":       6.875,
		"om_second_attempts_1h":  2,
		"om_polecat_reviews_1h":  1,
		"om_crew_reviews_1h":     1,
		"om_overseer_reviews_1h": 1,
		"om_unknown_reviews_1h":  1,
	}
	if !reflect.DeepEqual(cols, want) {
		t.Fatalf("review = %#v, want %#v", cols, want)
	}
	if bdfake.deadlineMissing {
		t.Error("a bd read ran without a deadline")
	}
	// Each review bead is read once, in its review's rig, with the id after --.
	for _, call := range bdfake.calls {
		if call[2] != filepath.Join(reviewTestTown, "gastown") {
			t.Errorf("bd read in %q, want the review's rig", call[2])
		}
	}
	if len(bdfake.calls) != 4 {
		t.Errorf("bd reads = %d, want one per reviewed bead", len(bdfake.calls))
	}
}

// A bead read that fails leaves the kind columns null, because the hour's
// reviews were not attributed; the counts the source can still make stand.
func TestSourceReviewAFailedBeadReadLeavesTheKindsNull(t *testing.T) {
	t.Parallel()
	now := fixedNow()
	content := reviewLine("gt-a1", land.VerdictApprove, 1, 8.5, false, now.Add(-30*time.Minute)) + "\n" +
		reviewLine("gt-a2", land.VerdictApprove, 1, 8.5, false, now.Add(-20*time.Minute)) + "\n"
	env := reviewTestEnv(t, content, false, nil)
	env.Run = (&reviewBdFake{
		beads: map[string]string{"gt-a1": reviewBeadJSON("gt-a1", "polecat/opal/gt-a1")},
		fail:  map[string]bool{"gt-a2": true},
	}).run
	cols, err := reviewSource{}.Collect(context.Background(), env)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if cols["om_reviews_1h"] != 2 {
		t.Errorf("om_reviews_1h = %v, want the 2 reviews it could count", cols["om_reviews_1h"])
	}
	for _, column := range reviewKindColumnNames {
		if value, ok := cols[column]; !ok || value != nil {
			t.Errorf("%s = %v, want null: a review it could not attribute is not an unknown author's", column, value)
		}
	}
}

// Two reviews of one bead are one bead read, and a review that names no rig is
// read from the town root, where bd routes the bead's prefix to its rig.
func TestSourceReviewReadsEachBeadOnce(t *testing.T) {
	t.Parallel()
	now := fixedNow()
	content := reviewLine("gt-a1", land.VerdictRequestChanges, 1, 4, false, now.Add(-40*time.Minute)) + "\n" +
		fmt.Sprintf(`{"bead":"gt-a1","rig":"gastown","attempt":2,"score":8,"verdict":"approve","at":%q}`,
			now.Add(-20*time.Minute).UTC().Format(time.RFC3339)) + "\n" +
		fmt.Sprintf(`{"bead":"gt-a2","attempt":1,"score":8,"verdict":"approve","at":%q}`,
			now.Add(-10*time.Minute).UTC().Format(time.RFC3339)) + "\n"
	bdfake := &reviewBdFake{beads: map[string]string{
		"gt-a1": reviewBeadJSON("gt-a1", "polecat/opal/gt-a1+mv1"),
		"gt-a2": reviewBeadJSON("gt-a2", "polecat/opal/gt-a2+mv1"),
	}}
	env := reviewTestEnv(t, content, false, nil)
	env.Run = bdfake.run
	cols, err := reviewSource{}.Collect(context.Background(), env)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if cols["om_polecat_reviews_1h"] != 3 {
		t.Errorf("om_polecat_reviews_1h = %v, want all three reviews", cols["om_polecat_reviews_1h"])
	}
	if len(bdfake.calls) != 2 {
		t.Fatalf("bd reads = %v, want one per bead, not one per review", bdfake.calls)
	}
	dirs := map[string]string{bdfake.calls[0][6]: bdfake.calls[0][2], bdfake.calls[1][6]: bdfake.calls[1][2]}
	if dirs["gt-a1"] != filepath.Join(reviewTestTown, "gastown") {
		t.Errorf("gt-a1 read in %q, want its review's rig", dirs["gt-a1"])
	}
	if dirs["gt-a2"] != reviewTestTown {
		t.Errorf("gt-a2 read in %q, want the town root for a review that names no rig", dirs["gt-a2"])
	}
}

// A crew branch whose bead the overseer holds is still the crew branch's: the
// kind is read from the branch, written once, and not from the assignee, which
// dispatch and takeover change after the review.
func TestSourceReviewReadsTheKindFromTheBranchNotTheAssignee(t *testing.T) {
	t.Parallel()
	now := fixedNow()
	content := reviewLine("gt-a1", land.VerdictApprove, 1, 8, false, now.Add(-30*time.Minute)) + "\n"
	bdfake := &reviewBdFake{beads: map[string]string{
		"gt-a1": `{"id":"gt-a1","notes":"READY TO LAND\nBranch: crew/merlin/gt-a1\nHead: abc\n","assignee":"town/crew/overseer"}`,
	}}
	env := reviewTestEnv(t, content, false, nil)
	env.Run = bdfake.run
	cols, err := reviewSource{}.Collect(context.Background(), env)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if cols["om_crew_reviews_1h"] != 1 || cols["om_overseer_reviews_1h"] != 0 {
		t.Errorf("crew, overseer = %v, %v; want the crew branch's kind, not the bead's assignee",
			cols["om_crew_reviews_1h"], cols["om_overseer_reviews_1h"])
	}
}

// A rig that is not one path element under the town is not a directory this
// source runs bd in: the hour's kinds are null and no bd starts.
func TestSourceReviewARigOutsideTheTownLeavesTheKindsNull(t *testing.T) {
	t.Parallel()
	now := fixedNow()
	for _, rig := range []string{".", "..", "../gastown", "gastown/../..", "/etc", `..\..\etc`} {
		t.Run(rig, func(t *testing.T) {
			t.Parallel()
			content := fmt.Sprintf(`{"bead":"gt-a1","rig":%q,"attempt":1,"score":8,"verdict":"approve","at":%q}`,
				rig, now.Add(-30*time.Minute).UTC().Format(time.RFC3339)) + "\n"
			bdfake := &reviewBdFake{beads: map[string]string{
				"gt-a1": reviewBeadJSON("gt-a1", "polecat/opal/gt-a1+mv1"),
			}}
			env := reviewTestEnv(t, content, false, nil)
			env.Run = bdfake.run
			cols, err := reviewSource{}.Collect(context.Background(), env)
			if err != nil {
				t.Fatalf("Collect: %v", err)
			}
			if cols["om_reviews_1h"] != 1 {
				t.Errorf("om_reviews_1h = %v, want the review it could count", cols["om_reviews_1h"])
			}
			for _, column := range reviewKindColumnNames {
				if value, ok := cols[column]; !ok || value != nil {
					t.Errorf("%s = %v, want null: a rig that escapes the town names no directory to read", column, value)
				}
			}
			if len(bdfake.calls) != 0 {
				t.Errorf("bd reads = %v, want none", bdfake.calls)
			}
		})
	}
}

// The reads are sequential, so they are bounded: an hour that names more
// distinct beads than reviewBeadReadCap is not attributed at all, and none of
// it is read.
func TestSourceReviewStopsAtTheBeadReadCap(t *testing.T) {
	t.Parallel()
	now := fixedNow()
	beads := map[string]string{}
	for i := 0; i <= reviewBeadReadCap; i++ {
		id := fmt.Sprintf("gt-c%d", i)
		beads[id] = reviewBeadJSON(id, "polecat/opal/"+id+"+mv1")
	}
	lines := func(n int) string {
		var b strings.Builder
		for i := 0; i < n; i++ {
			fmt.Fprintf(&b, `{"bead":"gt-c%d","rig":"gastown","attempt":1,"score":8,"verdict":"approve","at":%q}`+"\n",
				i, now.Add(-30*time.Minute).UTC().Format(time.RFC3339))
		}
		return b.String()
	}
	t.Run("at the cap", func(t *testing.T) {
		t.Parallel()
		bdfake := &reviewBdFake{beads: beads}
		env := reviewTestEnv(t, lines(reviewBeadReadCap), false, nil)
		env.Run = bdfake.run
		cols, err := reviewSource{}.Collect(context.Background(), env)
		if err != nil {
			t.Fatalf("Collect: %v", err)
		}
		if cols["om_polecat_reviews_1h"] != reviewBeadReadCap {
			t.Errorf("om_polecat_reviews_1h = %v, want all %d attributed", cols["om_polecat_reviews_1h"], reviewBeadReadCap)
		}
		if len(bdfake.calls) != reviewBeadReadCap {
			t.Errorf("bd reads = %d, want one per bead at the cap", len(bdfake.calls))
		}
	})
	t.Run("over the cap", func(t *testing.T) {
		t.Parallel()
		bdfake := &reviewBdFake{beads: beads}
		env := reviewTestEnv(t, lines(reviewBeadReadCap+1), false, nil)
		env.Run = bdfake.run
		cols, err := reviewSource{}.Collect(context.Background(), env)
		if err != nil {
			t.Fatalf("Collect: %v", err)
		}
		if cols["om_reviews_1h"] != reviewBeadReadCap+1 {
			t.Errorf("om_reviews_1h = %v, want the %d reviews it could count", cols["om_reviews_1h"], reviewBeadReadCap+1)
		}
		for _, column := range reviewKindColumnNames {
			if value, ok := cols[column]; !ok || value != nil {
				t.Errorf("%s = %v, want null: more beads than the source will read", column, value)
			}
		}
		if len(bdfake.calls) != 0 {
			t.Errorf("bd reads = %d, want none over the cap", len(bdfake.calls))
		}
	})
}

// The per-call deadline and the cap bound one factor each; the budget bounds all
// the reads together. Every read here answers well inside its own deadline, and
// each takes longer than a third of the budget, so only the budget can end the
// loop: the attribution fails instead of holding the snapshot for the product of
// the two bounds.
func TestReviewBeadsStopsWhenTheReadsOutlastTheBudget(t *testing.T) {
	t.Parallel()
	reviews := []Review{
		{Bead: "gt-b1", Rig: "gastown"},
		{Bead: "gt-b2", Rig: "gastown"},
		{Bead: "gt-b3", Rig: "gastown"},
	}
	const perRead = 30 * time.Millisecond
	env := Env{Town: t.TempDir(), Run: func(ctx context.Context, argv []string) ([]byte, error) {
		select {
		case <-time.After(perRead):
			return []byte("[" + reviewBeadJSON(argv[len(argv)-1], "polecat/opal/x+mv1") + "]"), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}}
	if rows, ok := reviewBeads(context.Background(), env, reviews, 2*perRead); ok || rows != nil {
		t.Errorf("reviewBeads under a budget of two reads = %v, %v, want a failed attribution", rows, ok)
	}
	if rows, ok := reviewBeads(context.Background(), env, reviews, time.Minute); !ok || len(rows) != 3 {
		t.Errorf("reviewBeads under a generous budget = %v, %v, want all three beads read", rows, ok)
	}
}

// A snapshot with no runner cannot attribute, so its kind columns are null: a
// review whose branch was never read is not a review by an unknown author.
func TestSourceReviewWithoutARunnerLeavesTheKindsNull(t *testing.T) {
	t.Parallel()
	now := fixedNow()
	content := reviewLine("gt-a1", land.VerdictApprove, 1, 8.5, false, now.Add(-30*time.Minute)) + "\n"
	cols, err := reviewSource{}.Collect(context.Background(), reviewTestEnv(t, content, false, nil))
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if cols["om_reviews_1h"] != 1 {
		t.Errorf("om_reviews_1h = %v, want the review it could count", cols["om_reviews_1h"])
	}
	for _, column := range reviewKindColumnNames {
		if value, ok := cols[column]; !ok || value != nil {
			t.Errorf("%s = %v, want null without a runner", column, value)
		}
	}
}

func TestSourceReviewMissingLogIsAnHourWithNoReviews(t *testing.T) {
	t.Parallel()
	bdfake := &reviewBdFake{}
	env := reviewTestEnv(t, "", true, nil)
	env.Run = bdfake.run
	cols, err := reviewSource{}.Collect(context.Background(), env)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	want := map[string]any{
		"om_reviews_1h":          0,
		"om_rejections_1h":       0,
		"om_mean_score_1h":       nil,
		"om_second_attempts_1h":  0,
		"om_polecat_reviews_1h":  0,
		"om_crew_reviews_1h":     0,
		"om_overseer_reviews_1h": 0,
		"om_unknown_reviews_1h":  0,
	}
	if !reflect.DeepEqual(cols, want) {
		t.Fatalf("review = %#v, want %#v (no review is not a score of zero)", cols, want)
	}
	if len(bdfake.calls) != 0 {
		t.Errorf("bd reads = %v, want none: an hour of no reviews has no bead to read", bdfake.calls)
	}
}

func TestSourceReviewSkipsEveryLineItCannotUse(t *testing.T) {
	t.Parallel()
	content := "{\n" + `{"bead":"","verdict":"approve","at":"2026-10-24T12:30:00Z"}` + "\n" + `{"bead":"gt-a1"}` + "\n"
	cols, err := reviewSource{}.Collect(context.Background(), reviewTestEnv(t, content, false, nil))
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if cols["om_reviews_1h"] != 0 || cols["om_mean_score_1h"] != nil {
		t.Fatalf("review = %v; want no reviews and no mean from lines it cannot attribute", cols)
	}
	// No review is every kind at zero, not an attribution nobody could make:
	// there is no bead to read and no runner is needed.
	for _, column := range reviewKindColumnNames {
		if value, ok := cols[column]; !ok || value != 0 {
			t.Errorf("%s = %v, want 0: an hour with no reviews", column, value)
		}
	}
}

func TestSourceReviewAnUnreadableLogLeavesTheGroupNull(t *testing.T) {
	t.Parallel()
	env := reviewTestEnv(t, "", false, fs.ErrPermission)
	cols, err := reviewSource{}.Collect(context.Background(), env)
	if err == nil || cols != nil {
		t.Fatalf("Collect = %v, %v; want an error and no columns", cols, err)
	}
	rec := Collect(context.Background(), env, []Source{reviewSource{}}, SourceTimeout)
	if rec.Groups["review"] != nil || len(rec.Errors) != 1 || rec.Errors[0].Group != "review" {
		t.Errorf("record review = %v, errors = %v; want null and one review error", rec.Groups["review"], rec.Errors)
	}
}

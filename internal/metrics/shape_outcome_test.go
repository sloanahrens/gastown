package metrics

import (
	"strings"
	"testing"
	"time"
)

// shapeDay is the day the shape fixtures are read on, so a test that changes
// it changes only the fact it is asserting.
var shapeDay = time.Date(2026, 9, 10, 9, 0, 0, 0, time.UTC)

func TestFirstFiledVersionsKeepsTheFirstRow(t *testing.T) {
	t.Parallel()
	commits := []ArchiveCommit{
		{At: shapeDay.AddDate(0, 0, -1), Rows: []ArchiveIssueRow{
			{ID: "gt-a", Description: "as filed", CreatedAt: shapeDay.AddDate(0, 0, -2)},
		}},
		{At: shapeDay, Rows: []ArchiveIssueRow{
			{ID: "gt-a", Description: "edited", CreatedAt: shapeDay.AddDate(0, 0, -2)},
			{ID: "gt-b", Description: "filed today", CreatedAt: shapeDay},
		}},
		{At: shapeDay.AddDate(0, 0, 1), Rows: []ArchiveIssueRow{
			{ID: "gt-a", Description: "edited twice", CreatedAt: shapeDay.AddDate(0, 0, -2)},
		}},
	}
	got := FirstFiledVersions(commits)
	if len(got) != 2 {
		t.Fatalf("rows = %d, want one per bead (2)", len(got))
	}
	if got[0].Row.ID != "gt-a" || got[0].Row.Description != "as filed" {
		t.Errorf("gt-a = %+v, want the text it was filed with", got[0])
	}
	if got[0].EditedSameDay {
		t.Error("gt-a created before its first sample's day is marked edited_same_day")
	}
	if got[1].Row.ID != "gt-b" {
		t.Errorf("gt-b = %+v, want its only row", got[1])
	}
	if !got[1].EditedSameDay {
		t.Error("gt-b created on its sample's day is not marked edited_same_day")
	}
}

// TestFirstFiledVersionsComparesDaysInTheCommitsZone pins the edited_same_day
// rule to one calendar: the bead's creation is read in the commit's zone, so a
// creation at 02:00 UTC, which is 19:00 the day before where the commit was
// made, is filed on the commit's day.
func TestFirstFiledVersionsComparesDaysInTheCommitsZone(t *testing.T) {
	t.Parallel()
	west := time.FixedZone("UTC-7", -7*60*60)
	commitAt := time.Date(2026, 9, 9, 23, 0, 0, 0, west) // 2026-09-10 06:00 UTC
	got := FirstFiledVersions([]ArchiveCommit{{
		At: commitAt,
		Rows: []ArchiveIssueRow{{
			ID:        "gt-a",
			CreatedAt: time.Date(2026, 9, 10, 2, 0, 0, 0, time.UTC),
		}},
	}})
	if len(got) != 1 {
		t.Fatalf("rows = %d, want 1", len(got))
	}
	if !got[0].EditedSameDay {
		t.Error("a creation at 19:00 in the commit's zone is not marked edited_same_day against a commit at 23:00 that day")
	}
}

// TestFirstFiledVersionsMarksAsFiledFromUpdatedAt pins the import's proof: a
// row is the text as filed only when its updated_at equals its created_at, and
// a row that does not date its last write is never taken as filed.
func TestFirstFiledVersionsMarksAsFiledFromUpdatedAt(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 9, 10, 9, 0, 0, 0, time.UTC)
	got := FirstFiledVersions([]ArchiveCommit{{
		At: at,
		Rows: []ArchiveIssueRow{
			{ID: "gt-clean", CreatedAt: at, UpdatedAt: at},
			{ID: "gt-edited", CreatedAt: at, UpdatedAt: at.Add(time.Hour)},
			{ID: "gt-undated", CreatedAt: at},
		},
	}})
	if len(got) != 3 {
		t.Fatalf("rows = %d, want 3", len(got))
	}
	if !got[0].AsFiled {
		t.Error("gt-clean, written no more than at creation, is not marked as filed")
	}
	if got[1].AsFiled {
		t.Error("gt-edited, written an hour after creation, is marked as filed")
	}
	if got[2].AsFiled {
		t.Error("gt-undated, with no updated_at, is marked as filed")
	}
}

func TestShapeFeaturesCountsBoxesAndConstraintWords(t *testing.T) {
	t.Parallel()
	row := ArchiveIssueRow{
		ID: "gt-a",
		Description: "## Goal\nship it.\n\n" +
			"## Constraints\n- only `internal/metrics/shape_outcome.go`\n- do not touch docs/reference.md\n\n" +
			"## Size\nOne worker, one landing.\n",
		AcceptanceCriteria: "- [ ] one\n- [x] two\n\n* [ ] three\n- not a box\n",
	}
	got := ShapeFeatures(row)
	if got.AcceptanceBoxes != 3 {
		t.Errorf("AcceptanceBoxes = %d, want 3", got.AcceptanceBoxes)
	}
	// The Constraints section's two "-" list markers are not words:
	// only, internal/metrics/shape_outcome.go, do, not, touch, docs/reference.md.
	if got.ConstraintWords != 6 {
		t.Errorf("ConstraintWords = %d, want 6 words without the bullet markers", got.ConstraintWords)
	}
	if !got.SizeOneWorker {
		t.Error("SizeOneWorker = false, want true for a Size line saying one worker")
	}
	if got.FilesNamed != 2 {
		t.Errorf("FilesNamed = %d, want 2 (shape_outcome.go and reference.md)", got.FilesNamed)
	}
	if want := len([]rune(row.Description)); got.DescriptionLength != want {
		t.Errorf("DescriptionLength = %d, want %d runes", got.DescriptionLength, want)
	}
}

func TestShapeFeaturesWithoutTheSections(t *testing.T) {
	t.Parallel()
	got := ShapeFeatures(ArchiveIssueRow{ID: "gt-a", Description: "just prose.", AcceptanceCriteria: "no boxes here"})
	if got.AcceptanceBoxes != 0 || got.ConstraintWords != 0 || got.SizeOneWorker || got.FilesNamed != 0 {
		t.Errorf("shape of a bead filed without sections = %+v, want every count 0", got)
	}
}

// TestShapeSectionMatchesHeadingAndStopsAtTheNext drives the section lookup:
// any heading level, case-insensitively, the exact name only, and text after
// the next heading is not part of the section.
func TestShapeSectionMatchesHeadingAndStopsAtTheNext(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		body    string
		section string
		want    string
	}{
		{"lowercase heading", "## constraints\nkeep it small\n", "Constraints", "keep it small"},
		{"any heading level", "### Size\nOne worker.\n", "Size", "One worker."},
		{"stops at the next heading", "## Constraints\na b\n## Goal\nnot counted\n", "Constraints", "a b"},
		{"a heading whose name merely starts with it", "## Consequences\nx\n", "Constraints", ""},
		{"no such heading", "## Goal\nx\n", "Constraints", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := shapeSection(c.body, c.section); got != c.want {
				t.Errorf("shapeSection(%q, %q) = %q, want %q", c.body, c.section, got, c.want)
			}
		})
	}
}

// TestShapeFeaturesReadsTheSizeAndConstraintsSections drives both section uses
// through ShapeFeatures: Size matches only its own heading, in either case,
// and Constraints ends at the heading after it with its bullets uncounted.
func TestShapeFeaturesReadsTheSizeAndConstraintsSections(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		description string
		size        bool
		words       int
	}{
		{"Size says one worker in either case", "## Size\nONE WORKER, one landing.", true, 0},
		{"a heading that starts with Size is not the Size section", "## Sizes\none worker", false, 0},
		{"Constraints ends at the next heading", "## Constraints\n- alpha\n- beta gamma\n## Size\none worker", true, 3},
		{"Constraints words exclude every bullet marker", "## Constraints\n- alpha\n* beta\n+ gamma\n", false, 3},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := ShapeFeatures(ArchiveIssueRow{ID: "gt-a", Description: c.description})
			if got.SizeOneWorker != c.size {
				t.Errorf("SizeOneWorker = %v, want %v", got.SizeOneWorker, c.size)
			}
			if got.ConstraintWords != c.words {
				t.Errorf("ConstraintWords = %d, want %d", got.ConstraintWords, c.words)
			}
		})
	}
}

func TestBucketOutcomesMarksThinBuckets(t *testing.T) {
	t.Parallel()
	newFeatures := func(n int) ([]Shape, map[string]ShapeOutcome) {
		features := make([]Shape, 0, n)
		outcomes := make(map[string]ShapeOutcome, n)
		for i := 0; i < n; i++ {
			id := "gt-" + string(rune('a'+i%26)) + string(rune('a'+i/26))
			features = append(features, Shape{Bead: id, AcceptanceBoxes: 0, DescriptionLength: 100})
			o := ShapeOutcome{Rounds: 1}
			if i%2 == 0 {
				o.Rounds = 2
				o.ApprovedFirst = true
			}
			outcomes[id] = o
		}
		return features, outcomes
	}
	find := func(buckets []BucketOutcome) BucketOutcome {
		for _, b := range buckets {
			if b.Feature == FeatureAcceptanceBoxes && b.Label == "0" {
				return b
			}
		}
		t.Fatalf("no acceptance_boxes bucket 0 in %d buckets", len(buckets))
		return BucketOutcome{}
	}

	t.Run("19 is thin", func(t *testing.T) {
		t.Parallel()
		features, outcomes := newFeatures(19)
		if b := find(BucketOutcomes(features, outcomes)); b.N != 19 || !b.Thin {
			t.Errorf("bucket = %+v, want n 19 marked thin", b)
		}
	})
	t.Run("20 is not thin", func(t *testing.T) {
		t.Parallel()
		features, outcomes := newFeatures(20)
		b := find(BucketOutcomes(features, outcomes))
		if b.N != 20 || b.Thin {
			t.Fatalf("bucket = %+v, want n 20 not thin", b)
		}
		if b.MedianRounds != 1.5 {
			t.Errorf("MedianRounds = %v, want 1.5 over ten 1s and ten 2s", b.MedianRounds)
		}
		if b.ApproveShare != 0.5 {
			t.Errorf("ApproveShare = %v, want 0.5", b.ApproveShare)
		}
	})
	t.Run("a bead with no outcome is not in a bucket", func(t *testing.T) {
		t.Parallel()
		features, outcomes := newFeatures(20)
		delete(outcomes, features[0].Bead)
		if b := find(BucketOutcomes(features, outcomes)); b.N != 19 {
			t.Errorf("bucket n = %d, want 19: a bead that did not land is left out", b.N)
		}
	})
}

func TestDecodeArchiveIssuesRefusesRowsItCannotDate(t *testing.T) {
	t.Parallel()
	data := []byte(strings.Join([]string{
		`{"id":"gt-a","description":"ok","created_at":"2026-09-10T09:00:00Z"}`,
		`not json`,
		`{"description":"no id","created_at":"2026-09-10T09:00:00Z"}`,
		`{"id":"gt-b","description":"no created_at"}`,
	}, "\n"))
	var dropped Dropped
	rows := DecodeArchiveIssues(data, "gt-1234", &dropped)
	if len(rows) != 1 || rows[0].ID != "gt-a" {
		t.Fatalf("rows = %+v, want only gt-a", rows)
	}
	if dropped.Count() != 3 {
		t.Fatalf("dropped = %d (%+v), want the three unreadable rows", dropped.Count(), dropped.Items)
	}
}

func TestShapeOutcomesKeepsOnlyLandedBeads(t *testing.T) {
	t.Parallel()
	at := shapeDay
	reviews := []ShapeReview{
		{Bead: "gt-a", Attempt: 1, Score: 0.4, Verdict: "request_changes", Rejected: true, At: at},
		{Bead: "gt-a", Attempt: 2, Score: 0.9, Verdict: "approve", At: at.Add(time.Hour)},
		{Bead: "gt-b", Attempt: 1, Score: 0.8, Verdict: "approve", At: at},
		{Bead: "gt-c", Attempt: 1, Score: 0.7, Verdict: "approve", Rejected: true, At: at},
		{Bead: "gt-d", Attempt: 1, Score: 0.3, Verdict: "request_changes", At: at},
	}
	hours := map[string]time.Duration{"gt-a": 12 * time.Hour}
	got := ShapeOutcomes(reviews, hours)
	if len(got) != 2 {
		t.Fatalf("outcomes = %+v, want gt-a and gt-b only: gt-c and gt-d did not land", got)
	}
	a := got["gt-a"]
	if a.Rounds != 2 || a.Score != 0.9 || a.ApprovedFirst {
		t.Errorf("gt-a = %+v, want 2 rounds, the last score 0.9 and no first-round approval", a)
	}
	if !a.HasHours || a.HoursToClose != 12 {
		t.Errorf("gt-a hours = %+v, want 12 from the archive's events", a)
	}
	b := got["gt-b"]
	if b.Rounds != 1 || !b.ApprovedFirst || b.HasHours {
		t.Errorf("gt-b = %+v, want one approved round and no hours", b)
	}
}

func TestDecodeShapeReviewsDropsWhatIsNotAVerdict(t *testing.T) {
	t.Parallel()
	lines := []string{
		`{"bead":"gt-a","attempt":1,"score":0.8,"verdict":"approve","at":"2026-09-10T12:00:00Z"}`,
		`{"followup_queue":{"bead":"gt-z"}}`,
		`{"bead":"gt-b","score":0.8,"verdict":"approve","at":"2026-09-10T12:00:00Z"}`,
		`nope`,
	}
	var dropped Dropped
	rows := DecodeShapeReviews(lines, "reviews.jsonl", &dropped)
	if len(rows) != 1 || rows[0].Bead != "gt-a" {
		t.Fatalf("rows = %+v, want only gt-a: a queue line is skipped, a broken line is dropped", rows)
	}
	if dropped.Count() != 2 {
		t.Fatalf("dropped = %d, want the missing-attempt line and the non-JSON line", dropped.Count())
	}
}

func TestArchiveCloseHoursTakesEarliestCreatedAndLatestClosed(t *testing.T) {
	t.Parallel()
	events := []byte(strings.Join([]string{
		`{"issue_id":"gt-a","event_type":"created","created_at":"2026-09-10 09:00:00"}`,
		`{"issue_id":"gt-a","event_type":"created","created_at":"2026-09-10 10:00:00"}`,
		`{"issue_id":"gt-a","event_type":"closed","created_at":"2026-09-10 21:00:00"}`,
		`{"issue_id":"gt-a","event_type":"closed","created_at":"2026-09-10 20:00:00"}`,
		`{"issue_id":"gt-b","event_type":"created","created_at":"2026-09-10 09:00:00"}`,
		`{"issue_id":"gt-c","event_type":"created","created_at":"not a time"}`,
	}, "\n"))
	var dropped Dropped
	got := ArchiveCloseHours(events, "gt/events.jsonl", &dropped)
	if got["gt-a"] != 12*time.Hour {
		t.Errorf("gt-a = %v, want 12h from the earliest creation to the latest close", got["gt-a"])
	}
	if _, ok := got["gt-b"]; ok {
		t.Error("gt-b has no closed event and must have no time to close")
	}
	if _, ok := got["gt-c"]; ok {
		t.Error("gt-c's undated creation must not produce a time to close")
	}
	if dropped.Count() != 1 {
		t.Errorf("dropped = %d, want the undated line", dropped.Count())
	}
}

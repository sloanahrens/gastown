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
		{Day: "2026-09-09", Rows: []ArchiveIssueRow{
			{ID: "gt-a", Description: "as filed", CreatedAt: shapeDay.AddDate(0, 0, -2)},
		}},
		{Day: "2026-09-10", Rows: []ArchiveIssueRow{
			{ID: "gt-a", Description: "edited", CreatedAt: shapeDay.AddDate(0, 0, -2)},
			{ID: "gt-b", Description: "filed today", CreatedAt: shapeDay},
		}},
		{Day: "2026-09-11", Rows: []ArchiveIssueRow{
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
	constraints := "- only `internal/metrics/shape_outcome.go`\n- do not touch docs/reference.md"
	if want := len(strings.Fields(constraints)); got.ConstraintWords != want {
		t.Errorf("ConstraintWords = %d, want %d", got.ConstraintWords, want)
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

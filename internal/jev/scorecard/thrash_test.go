package scorecard

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/jev/thrash"
)

// thrashTown is a town whose runtime directory exists, with the respawn counts
// and overrun records the test writes beneath it.
func thrashTown(t *testing.T) string {
	t.Helper()
	town := t.TempDir()
	if err := os.MkdirAll(filepath.Join(constants.TownRuntimePath(town), "overrun-guard"), 0o700); err != nil {
		t.Fatal(err)
	}
	return town
}

// writeRespawnCounts writes a fixture bead-respawn-counts.json holding counts.
// Each last_respawn is 08:30, after the 08:00 placement thrashLine logs.
func writeRespawnCounts(t *testing.T, town string, counts map[string]int) {
	t.Helper()
	beads := map[string]map[string]any{}
	for id, n := range counts {
		beads[id] = respawnEntry(id, n, time.Date(2026, 10, 21, 8, 30, 0, 0, time.UTC))
	}
	writeRespawnFile(t, town, beads)
}

// respawnEntry is one bead's record as the file holds it. A zero last_respawn is
// left out, as the record of a bead respawned before the stamp was written.
func respawnEntry(id string, count int, lastRespawn time.Time) map[string]any {
	rec := map[string]any{"bead_id": id, "count": count}
	if !lastRespawn.IsZero() {
		rec["last_respawn"] = lastRespawn
	}
	return rec
}

// writeRespawnFile writes a fixture bead-respawn-counts.json holding records as
// given, for shapes respawnEntry alone cannot build.
func writeRespawnFile(t *testing.T, town string, beads map[string]map[string]any) {
	t.Helper()
	data, err := json.Marshal(map[string]any{"beads": beads})
	if err != nil {
		t.Fatal(err)
	}
	writeRawFixture(t, filepath.Join(constants.TownRuntimePath(town), "bead-respawn-counts.json"), data)
}

// writeOverrun writes the overrun guard's record for bead, stopped or not.
func writeOverrun(t *testing.T, town, bead string, stopped bool) {
	t.Helper()
	rec := map[string]any{"bead": bead}
	if stopped {
		rec["stopped_at"] = time.Date(2026, 10, 21, 10, 0, 0, 0, time.UTC)
	}
	data, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	writeRawFixture(t, filepath.Join(constants.TownRuntimePath(town), "overrun-guard", bead+".json"), data)
}

// writeOverrunStamps writes the overrun guard's record for bead carrying the
// given stop stamps; a zero stamp is left out, as the guard's omitzero does.
func writeOverrunStamps(t *testing.T, town, bead string, began, stopped time.Time) {
	t.Helper()
	rec := map[string]any{"bead": bead}
	if !began.IsZero() {
		rec["stop_began_at"] = began
	}
	if !stopped.IsZero() {
		rec["stopped_at"] = stopped
	}
	data, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	writeRawFixture(t, filepath.Join(constants.TownRuntimePath(town), "overrun-guard", bead+".json"), data)
}

func writeRawFixture(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// thrashLine is one logged thrash line for bead, placed at 08:00, as the pass
// writes it.
func thrashLine(t *testing.T, bead string, willRespawn, needsSplit float64) Item {
	t.Helper()
	return thrashPlacement(t, bead, time.Date(2026, 10, 21, 8, 0, 0, 0, time.UTC), willRespawn, needsSplit)
}

// thrashPlacement is one logged thrash line for bead placed at attached.
func thrashPlacement(t *testing.T, bead string, attached time.Time, willRespawn, needsSplit float64) Item {
	t.Helper()
	raw, err := json.Marshal(thrash.Line{
		TS:         time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC),
		Bead:       bead,
		AttachedAt: attached,
		Agent:      "polecat",
		Answers:    thrash.Answers{WillRespawn: willRespawn, NeedsSplit: needsSplit},
		Model:      "jev-test",
	})
	if err != nil {
		t.Fatal(err)
	}
	item, err := (&thrashAdapter{}).Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return item
}

// The adapter labels will_respawn from the count file and needs_split from the
// overrun record, a record without a stop being a negative as a count below the
// threshold is. A bead with neither file is unlabeled.
func TestThrashLabelsFromTheRespawnCountsAndOverrunRecords(t *testing.T) {
	t.Parallel()
	town := thrashTown(t)
	writeRespawnCounts(t, town, map[string]int{"gt-a": 2, "gt-b": 1})
	writeOverrun(t, town, "gt-a", false)
	writeOverrun(t, town, "gt-c", true)
	a := &thrashAdapter{town: town}

	cases := []struct {
		bead    string
		labels  map[QuestionID]bool
		labeled bool
	}{
		{"gt-a", map[QuestionID]bool{qWillRespawn: true, qNeedsSplit: false}, true},
		{"gt-b", map[QuestionID]bool{qWillRespawn: false}, true},
		{"gt-c", map[QuestionID]bool{qNeedsSplit: true}, true},
		{"gt-d", nil, false},
	}
	for _, tc := range cases {
		got, err := a.Label(context.Background(), thrashLine(t, tc.bead, 0.5, 0.5))
		if err != nil {
			t.Fatalf("%s: %v", tc.bead, err)
		}
		if got.Labeled != tc.labeled {
			t.Errorf("%s: labeled %v, want %v", tc.bead, got.Labeled, tc.labeled)
		}
		if !reflect.DeepEqual(got.Labels, tc.labels) {
			t.Errorf("%s: labels %v, want %v", tc.bead, got.Labels, tc.labels)
		}
	}
}

// A stop the guard has begun but not finished is the guard's normal in-progress
// form, and its outcome is not known yet, so needs_split is left unlabeled, as
// the dispatch battery leaves fits_twenty_minutes. A finished stop is a
// positive and a record with neither stamp a negative.
func TestThrashPendingStopLeavesNeedsSplitUnlabeled(t *testing.T) {
	t.Parallel()
	town := thrashTown(t)
	began := time.Date(2026, 10, 21, 9, 50, 0, 0, time.UTC)
	stopped := time.Date(2026, 10, 21, 10, 0, 0, 0, time.UTC)
	writeOverrunStamps(t, town, "gt-pending", began, time.Time{})
	writeOverrunStamps(t, town, "gt-done", began, stopped)
	writeOverrunStamps(t, town, "gt-bare", time.Time{}, time.Time{})
	a := &thrashAdapter{town: town}

	cases := []struct {
		bead    string
		labels  map[QuestionID]bool
		labeled bool
	}{
		{"gt-pending", nil, false},
		{"gt-done", map[QuestionID]bool{qNeedsSplit: true}, true},
		{"gt-bare", map[QuestionID]bool{qNeedsSplit: false}, true},
	}
	for _, tc := range cases {
		got, err := a.Label(context.Background(), thrashLine(t, tc.bead, 0.5, 0.5))
		if err != nil {
			t.Fatalf("%s: %v", tc.bead, err)
		}
		if got.Labeled != tc.labeled {
			t.Errorf("%s: labeled %v, want %v", tc.bead, got.Labeled, tc.labeled)
		}
		if !reflect.DeepEqual(got.Labels, tc.labels) {
			t.Errorf("%s: labels %v, want %v", tc.bead, got.Labels, tc.labels)
		}
		if !tc.labeled && got.Rule != "" {
			t.Errorf("%s: unlabeled line carries rule %q, want none", tc.bead, got.Rule)
		}
	}

	// With a respawn count the pending bead still labels will_respawn alone.
	writeRespawnCounts(t, town, map[string]int{"gt-pending": 2})
	got, err := a.Label(context.Background(), thrashLine(t, "gt-pending", 0.5, 0.5))
	if err != nil {
		t.Fatal(err)
	}
	if want := map[QuestionID]bool{qWillRespawn: true}; !reflect.DeepEqual(got.Labels, want) {
		t.Errorf("pending with a count: labels %v, want %v", got.Labels, want)
	}
}

// A broken respawn file or overrun record labels nothing and is not an error,
// and Context names each broken file so the report does not read it as a bead
// with no data.
func TestThrashLabelsIgnoreBrokenFiles(t *testing.T) {
	t.Parallel()
	town := thrashTown(t)
	writeRawFixture(t, filepath.Join(constants.TownRuntimePath(town), "bead-respawn-counts.json"), []byte("{not json"))
	writeRawFixture(t, filepath.Join(constants.TownRuntimePath(town), "overrun-guard", "gt-a.json"), []byte("stopped"))
	a := &thrashAdapter{town: town}

	got, err := a.Label(context.Background(), thrashLine(t, "gt-a", 0.5, 0.5))
	if err != nil {
		t.Fatalf("a broken file failed the report: %v", err)
	}
	if got.Labeled || len(got.Labels) != 0 {
		t.Errorf("labels %+v from broken files, want none", got)
	}
	if got, want := a.Context(), []string{
		"bead-respawn-counts.json is unreadable: will_respawn is left unlabeled",
		"overrun-guard records are unreadable: needs_split is left unlabeled on 1 line(s)",
	}; !reflect.DeepEqual(got, want) {
		t.Errorf("context = %q, want %q", got, want)
	}
}

// A town with neither file reports no broken files: a missing file is normal.
func TestThrashMissingFilesAreNotReported(t *testing.T) {
	t.Parallel()
	a := &thrashAdapter{town: thrashTown(t)}

	got, err := a.Label(context.Background(), thrashLine(t, "gt-a", 0.5, 0.5))
	if err != nil {
		t.Fatal(err)
	}
	if got.Labeled {
		t.Errorf("labeled %+v with no files on record", got)
	}
	if lines := a.Context(); len(lines) != 0 {
		t.Errorf("context = %q, want none for missing files", lines)
	}
}

// The respawn counts are reread when the file changes, so a report that
// outlives a rewrite labels from the new counts.
func TestThrashRespawnCountsRereadWhenTheFileChanges(t *testing.T) {
	t.Parallel()
	town := thrashTown(t)
	writeRespawnCounts(t, town, map[string]int{"gt-a": 1})
	a := &thrashAdapter{town: town}

	first, err := a.Label(context.Background(), thrashLine(t, "gt-a", 0.5, 0.5))
	if err != nil {
		t.Fatal(err)
	}
	if got := first.Labels[qWillRespawn]; got {
		t.Fatalf("count 1 labeled will_respawn %v, want false", got)
	}

	// A second bead changes the file's size, so the rewrite is seen even when
	// the mtime resolution is coarse.
	writeRespawnCounts(t, town, map[string]int{"gt-a": 2, "gt-z": 1})
	second, err := a.Label(context.Background(), thrashLine(t, "gt-a", 0.5, 0.5))
	if err != nil {
		t.Fatal(err)
	}
	if got := second.Labels[qWillRespawn]; !got {
		t.Errorf("count 2 after the rewrite labeled will_respawn %v, want true", got)
	}
}

// A respawn labels only the placements it followed. Its 08:30 stamp follows the
// 08:00 placement, so that placement is a positive. The newest placement has no
// respawn after it yet and stays unlabeled. A placement that a later one
// followed with no respawn between is a negative. A first run is a negative
// wherever it sits, and a bead with no respawn record, or a record with no
// last_respawn stamp, stays unlabeled.
func TestThrashRespawnLabelIsPerPlacement(t *testing.T) {
	t.Parallel()
	town := thrashTown(t)
	stamp := time.Date(2026, 10, 21, 8, 30, 0, 0, time.UTC)
	writeRespawnFile(t, town, map[string]map[string]any{
		"gt-a": respawnEntry("gt-a", 2, stamp),
		"gt-b": respawnEntry("gt-b", 2, stamp),
		"gt-c": respawnEntry("gt-c", 2, stamp),
		"gt-d": respawnEntry("gt-d", 2, stamp),
		"gt-f": respawnEntry("gt-f", 1, stamp),
		"gt-g": respawnEntry("gt-g", 2, time.Time{}),
	})
	at := func(hour int) time.Time { return time.Date(2026, 10, 21, hour, 0, 0, 0, time.UTC) }

	cases := []struct {
		bead    string
		attach  time.Time
		labels  map[QuestionID]bool
		labeled bool
	}{
		{"gt-a", at(8), map[QuestionID]bool{qWillRespawn: true}, true},
		{"gt-a", at(10), nil, false},
		{"gt-b", at(8), map[QuestionID]bool{qWillRespawn: true}, true},
		{"gt-b", at(10), map[QuestionID]bool{qWillRespawn: false}, true},
		{"gt-b", at(12), nil, false},
		{"gt-c", at(8), map[QuestionID]bool{qWillRespawn: true}, true},
		{"gt-d", at(9), nil, false},
		{"gt-e", at(8), nil, false},
		{"gt-f", at(8), map[QuestionID]bool{qWillRespawn: false}, true},
		{"gt-f", at(10), map[QuestionID]bool{qWillRespawn: false}, true},
		{"gt-g", at(8), nil, false},
		{"gt-g", at(10), nil, false},
	}
	var items []Item
	for _, tc := range cases {
		items = append(items, thrashPlacement(t, tc.bead, tc.attach, 0.5, 0.5))
	}

	a := &thrashAdapter{town: town}
	a.Prefetch(context.Background(), items)
	for i, tc := range cases {
		got, err := a.Label(context.Background(), items[i])
		if err != nil {
			t.Fatalf("%s at %s: %v", tc.bead, tc.attach, err)
		}
		if got.Labeled != tc.labeled {
			t.Errorf("%s at %s: labeled %v, want %v", tc.bead, tc.attach, got.Labeled, tc.labeled)
		}
		if !reflect.DeepEqual(got.Labels, tc.labels) {
			t.Errorf("%s at %s: labels %v, want %v", tc.bead, tc.attach, got.Labels, tc.labels)
		}
	}
}

// Label without Prefetch cannot tell a placement a later one followed from the
// newest, so it leaves will_respawn unlabeled and Context says so rather than
// reporting a negative the batch never settled.
func TestThrashUnprefetchedLeavesWillRespawnUnlabeled(t *testing.T) {
	t.Parallel()
	town := thrashTown(t)
	writeRespawnCounts(t, town, map[string]int{"gt-a": 2})
	a := &thrashAdapter{town: town}
	placed := time.Date(2026, 10, 21, 10, 0, 0, 0, time.UTC)

	got, err := a.Label(context.Background(), thrashPlacement(t, "gt-a", placed, 0.5, 0.5))
	if err != nil {
		t.Fatal(err)
	}
	if got.Labeled {
		t.Errorf("labeled %+v without the batch, want will_respawn unlabeled", got)
	}
	if lines, want := a.Context(), []string{
		"the placements were not batched: will_respawn is left unlabeled on 1 line(s)",
	}; !reflect.DeepEqual(lines, want) {
		t.Errorf("context = %q, want %q", lines, want)
	}

	a.Prefetch(context.Background(), []Item{thrashPlacement(t, "gt-a", placed, 0.5, 0.5)})
	got, err = a.Label(context.Background(), thrashPlacement(t, "gt-a", placed, 0.5, 0.5))
	if err != nil {
		t.Fatal(err)
	}
	if got.Labeled {
		t.Errorf("labeled %+v, want the newest placement unlabeled", got)
	}
	if lines := a.Context(); len(lines) != 0 {
		t.Errorf("context = %q after Prefetch, want none", lines)
	}
}

// Parse reads both answers and keeps the agent as context beside will_respawn.
func TestThrashParseReadsBothAnswers(t *testing.T) {
	t.Parallel()
	item := thrashLine(t, "gt-a", 0.7, 0.2)
	if got := *item.Answers[qWillRespawn].Noul; got != 0.7 {
		t.Errorf("will_respawn = %v, want 0.7", got)
	}
	if got := *item.Answers[qNeedsSplit].Noul; got != 0.2 {
		t.Errorf("needs_split = %v, want 0.2", got)
	}
	if item.Context[qWillRespawn] != "polecat" {
		t.Errorf("context = %v, want the agent beside will_respawn", item.Context)
	}
}

// The question ids the adapter uses are the json keys the thrash line writes.
func TestScorecardThrashQuestionIDsMatchLogKeys(t *testing.T) {
	t.Parallel()
	raw, err := json.Marshal(thrash.Answers{})
	if err != nil {
		t.Fatal(err)
	}
	var keys map[string]float64
	if err := json.Unmarshal(raw, &keys); err != nil {
		t.Fatal(err)
	}
	var got []string
	for k := range keys {
		got = append(got, k)
	}
	sort.Strings(got)
	want := []string{string(qNeedsSplit), string(qWillRespawn)}
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("answer keys = %v, want %v", got, want)
	}
}

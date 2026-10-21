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
func writeRespawnCounts(t *testing.T, town string, counts map[string]int) {
	t.Helper()
	beads := map[string]map[string]any{}
	for id, n := range counts {
		beads[id] = map[string]any{"bead_id": id, "count": n}
	}
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

func writeRawFixture(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// thrashLine is one logged thrash line for bead, as the pass writes it.
func thrashLine(t *testing.T, bead string, willRespawn, needsSplit float64) Item {
	t.Helper()
	raw, err := json.Marshal(thrash.Line{
		TS:         time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC),
		Bead:       bead,
		AttachedAt: time.Date(2026, 10, 21, 8, 0, 0, 0, time.UTC),
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
// overrun record, and a bead with neither file is unlabeled.
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
		{"gt-a", map[QuestionID]bool{qWillRespawn: true}, true},
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

// A broken respawn file or overrun record labels nothing and is not an error.
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

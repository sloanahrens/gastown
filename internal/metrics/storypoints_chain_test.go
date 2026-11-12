package metrics

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
)

// chainFixture is n landed beads as the extract reads them: bd lists each with
// its story-point labels and bd shows each one's SP ESTIMATE v1 comment, the
// ledger lines the landings come from, and each bead's first dispatch. withMinutes
// adds each bead's sp-minutes label. Bead 0's estimate is written after its
// dispatch; the others are written before.
func chainFixture(n int, withMinutes bool) (list string, shows map[string]string, ledger []string, dispatched map[string]string) {
	points := []int{1, 2, 3, 5, 8, 13, 21, 34}
	beads := make([]string, 0, n)
	shows = map[string]string{}
	dispatched = map[string]string{}
	for i := range n {
		id := fmt.Sprintf("gt-c%d", i)
		est := points[i%len(points)]
		labels := fmt.Sprintf(`"sp:%d", "sp-rung:all"`, est)
		if withMinutes {
			labels += fmt.Sprintf(`, "sp-minutes:%d"`, 6+i%20)
		}
		beads = append(beads, fmt.Sprintf(`{"id": %q, "created_at": "2026-10-18T09:00:00Z", "issue_type": "task", "labels": [%s]}`, id, labels))

		estimatedAt := "2026-10-19T10:00:00Z"
		if i == 0 {
			estimatedAt = "2026-10-25T10:00:00Z"
		}
		shows[id] = fmt.Sprintf(`{"schema_version": 1, "data": [{"id": %q, "comments": [{"text": "SP ESTIMATE v1 sp:%d rung:all hash:x", "created_at": %q}]}]}`,
			id, est, estimatedAt)

		actualMinutes := 2 + (i*7)%40
		ledger = append(ledger, fmt.Sprintf(`{"bead": %q, "rig": "gt", "landed_commit": "c-%d", "landed_at": "2026-10-24T12:00:00Z", "route": "daemon", "om_verdict": "approve", "gate_result": "build", "polecat_secs": %d, "agent": "sbx-haiku"}`,
			id, i, actualMinutes*60))
		dispatched[id] = "2026-10-20T10:00:00Z"
	}
	return "[" + strings.Join(beads, ",") + "]", shows, ledger, dispatched
}

// extractChain runs the extract's join over a fixture and returns the landings
// table as the report reads it back: BuildLandings, written with WriteLandings
// and read with ReadLandings, so every cell has passed through the CSV.
func extractChain(t *testing.T, list string, shows map[string]string, ledger []string, dispatched map[string]string) []Row {
	t.Helper()
	var dropped Dropped
	var calls []bdCall
	beadRows := LoadBeads(routedBd(list, shows, &calls), []string{"/g"}, &dropped)
	beads := map[string]BeadRow{}
	for _, b := range beadRows {
		beads[b.ID] = b
	}
	rows, err := BuildLandings(context.Background(), LoadLedger(map[string][]string{"gt": ledger}, &dropped), nil,
		fakeDiffSource{}, beads, nil, nil, nil, nil, nil, StageStamps{Dispatched: dispatched}, nil, &dropped)
	if err != nil {
		t.Fatalf("BuildLandings: %v", err)
	}
	var table bytes.Buffer
	if err := WriteLandings(&table, landingColumns, rows); err != nil {
		t.Fatalf("WriteLandings: %v", err)
	}
	_, read, err := ReadLandings(&table)
	if err != nil {
		t.Fatalf("ReadLandings: %v", err)
	}
	return read
}

// TestTriggerTableIsPrintedFromTheExtractedMinutes: the trigger table reads the
// sp_minutes column the extract wrote from each bead's label, so a table the
// extract built prints the four trigger rows. Bead 0's estimate was written after
// its dispatch, so it is left out of the scored rows and counted.
func TestTriggerTableIsPrintedFromTheExtractedMinutes(t *testing.T) {
	t.Parallel()
	list, shows, ledger, dispatched := chainFixture(61, true)
	var out bytes.Buffer
	if err := WriteStoryPointsReport(&out, extractChain(t, list, shows, ledger, dispatched)); err != nil {
		t.Fatalf("WriteStoryPointsReport: %v", err)
	}
	got := out.String()
	for _, want := range []string{
		"story points shadow report: 60 estimated landings with a measured time",
		"estimates written after dispatch: 1",
		"  11 min  flagged",
		"  13 min  flagged",
		"  15 min  flagged",
		"  17 min  flagged",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("report lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "no estimated minutes recorded yet") || strings.Contains(got, "not computed") {
		t.Errorf("report says the trigger is missing although the extract wrote minutes:\n%s", got)
	}
}

// TestTriggerSectionSaysNoMinutesWhenTheExtractWroteNone: a landings table the
// extract built from beads that carry no sp-minutes label prints the no-minutes
// line in the trigger section, not a trigger table and not nothing.
func TestTriggerSectionSaysNoMinutesWhenTheExtractWroteNone(t *testing.T) {
	t.Parallel()
	// Bead 0's late estimate is left out, so 61 beads leave the 60 scored ones.
	list, shows, ledger, dispatched := chainFixture(61, false)
	var out bytes.Buffer
	if err := WriteStoryPointsReport(&out, extractChain(t, list, shows, ledger, dispatched)); err != nil {
		t.Fatalf("WriteStoryPointsReport: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "no estimated minutes recorded yet\n") {
		t.Errorf("report lacks the no-minutes line:\n%s", got)
	}
	if strings.Contains(got, "  11 min  flagged") {
		t.Errorf("report prints trigger rows with no minutes to flag on:\n%s", got)
	}
}

package cmd

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/jev/scorecard"
)

// lowTailFixture writes a fixture log of ten items answering two nouls. needs is
// one-sided: nine items are true and one, at the lowest answer, is false. fair
// is balanced: half the items are true.
func lowTailFixture(t *testing.T, town string) {
	t.Helper()
	var lines []string
	labels := map[string]map[scorecard.QuestionID]bool{}
	for i := 0; i < 10; i++ {
		id := fmt.Sprintf("lt-%d", i)
		needs, needsLabel := 0.9, true
		if i == 0 {
			needs, needsLabel = 0.1, false
		}
		fair, fairLabel := 0.9, true
		if i%2 == 1 {
			fair, fairLabel = 0.1, false
		}
		lines = append(lines, fmt.Sprintf(
			`{"ts":"2026-10-18T%02d:00:00Z","id":"%s","answers":{"needs":{"noul":%g},"fair":{"noul":%g}}}`,
			i, id, needs, fair))
		labels[id] = map[scorecard.QuestionID]bool{"needs": needsLabel, "fair": fairLabel}
	}
	if err := scorecard.WriteFixture(town, lines, labels); err != nil {
		t.Fatal(err)
	}
	jevSetModTime(t, scorecard.FixtureLogPath(town), scorecardFixtureWritten)
}

// TestJevScorecardLowTail prints the low-tail line under a one-sided noul, with
// its band's precision and recall for the minority label, and nothing under a
// balanced one.
func TestJevScorecardLowTail(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	lowTailFixture(t, town)

	var out bytes.Buffer
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	if err := jevScorecardWithFlags(context.Background(), &out, town, scorecard.FixtureBattery, "", false, false, now); err != nil {
		t.Fatal(err)
	}

	blocks := map[string]string{}
	for _, block := range strings.Split(out.String(), "\n\n") {
		for _, id := range []string{"needs", "fair"} {
			if strings.HasPrefix(strings.TrimPrefix(block, "\n"), id+" (noul)") {
				blocks[id] = block
			}
		}
	}
	if len(blocks) != 2 {
		t.Fatalf("the output has blocks for %v, want needs and fair:\n%s", blocks, out.String())
	}

	// The one false need is the lowest band's only labeled false line: the
	// band's precision and the false lines' recall are both 100%.
	const wantTail = "  low tail, band 0.0-0.2 (labeled 1), minority label false: precision 100.0%, recall 100.0%\n"
	if !strings.Contains(blocks["needs"], wantTail) {
		t.Errorf("needs block has no low-tail line %q:\n%s", wantTail, blocks["needs"])
	}
	if strings.Contains(blocks["fair"], "low tail") {
		t.Errorf("fair block prints a low tail, want none for a balanced question:\n%s", blocks["fair"])
	}
}

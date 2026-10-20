package cmd

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/jev/dedupe"
	"github.com/steveyegge/gastown/internal/jev/scorecard"
)

var updateJevScorecardFindingGolden = flag.Bool("update-jev-scorecard-finding-golden", false, "rewrite testdata/jev_scorecard_finding.golden")

const jevScorecardFindingGoldenPath = "jev_scorecard_finding.golden"

// findingFakeBeads serves the finding beads from a map.
type findingFakeBeads map[string]*beads.Issue

func (f findingFakeBeads) Show(id string) (*beads.Issue, error) {
	if b, ok := f[id]; ok {
		return b, nil
	}
	return nil, beads.ErrNotFound
}

// findingFakeDiffs sizes commits from a map; a commit not in it is not in the repo.
type findingFakeDiffs map[string]int

func (f findingFakeDiffs) ChangedLines(_ context.Context, commit string) (int, error) {
	if n, ok := f[commit]; ok {
		return n, nil
	}
	return 0, scorecard.ErrNoCommit
}

// findingFixtureLog is the finding battery's log for five findings, offering
// gt-c1 to each. The Choice picks the candidate for the first and none for the
// others.
func findingFixtureLog() []string {
	line := func(ts, finding, choice string, fix, report, any, test float64, severity int) string {
		return fmt.Sprintf(`{"ts":%q,"finding":%q,"candidate":%q,"confidence":0.9,"candidates_offered":["gt-c1"],`+
			`"outcome":"answered","model":"jev-1","question_version":1,"answers":{`+
			`"duplicate":{"choice":%q,"confidence":0.9},"fix_worthy":{"noul":%v},"report_only":{"noul":%v},`+
			`"any_duplicate":{"noul":%v},"test_only":{"noul":%v},"severity":{"score":%d,"confidence":0.6}}}`,
			ts, finding, choice, choice, fix, report, any, test, severity)
	}
	return []string{
		line("2026-10-18T09:00:00Z", "gt-f1", "gt-c1", 0.2, 0.1, 0.9, 0.1, 0),
		line("2026-10-18T10:00:00Z", "gt-f2", "none", 0.9, 0.1, 0.1, 0.2, 3),
		line("2026-10-18T11:00:00Z", "gt-f3", "none", 0.8, 0.2, 0.2, 0.7, 1),
		line("2026-10-18T12:00:00Z", "gt-f4", "none", 0.3, 0.8, 0.1, 0.1, 0),
		line("2026-10-18T13:00:00Z", "gt-f5", "none", 0.6, 0.2, 0.3, 0.1, 2),
		`not json`,
	}
}

func findingFixtureBeads() findingFakeBeads {
	landed := func(sha string) string {
		return "Landed on main\ntarget_branch: main\ncommit_sha: " + sha + "\nlanded_commit: " + sha + "\npatch_id: p"
	}
	closed := func(id, reason string) *beads.Issue {
		return &beads.Issue{ID: id, Status: "closed", CloseReason: reason}
	}
	return findingFakeBeads{
		"gt-f1": closed("gt-f1", "Duplicate of gt-c1"),
		"gt-f2": closed("gt-f2", landed("0123abc")),
		"gt-f3": closed("gt-f3", landed("0456def")),
		"gt-f4": closed("gt-f4", "no-changes: already fixed on main"),
		"gt-f5": {ID: "gt-f5", Status: "open"},
	}
}

// The finding scorecard prints one block per question, labeled from the beads'
// close reasons. The fake diffs make the fix for gt-f2 large (400 lines) and the one for gt-f3 small (10).
// This test swaps the adapter opener, so it does not run in parallel.
func TestScorecardFindingGolden(t *testing.T) {
	town := t.TempDir()
	logPath := dedupe.LogPath(town)
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, []byte(strings.Join(findingFixtureLog(), "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	orig := openScorecardAdapter
	t.Cleanup(func() { openScorecardAdapter = orig })
	openScorecardAdapter = func(name, townRoot string) (scorecard.Adapter, error) {
		if name != scorecard.FindingBattery {
			return orig(name, townRoot)
		}
		return scorecard.NewFinding(townRoot, findingFixtureBeads(), findingFakeDiffs{"0123abc": 400, "0456def": 10}), nil
	}

	var out bytes.Buffer
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	if err := jevScorecardWithFlags(context.Background(), &out, town, scorecard.FindingBattery, "", false, now); err != nil {
		t.Fatal(err)
	}

	golden := filepath.Join("testdata", jevScorecardFindingGoldenPath)
	if *updateJevScorecardFindingGolden {
		if err := os.WriteFile(golden, out.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if out.String() != string(want) {
		t.Errorf("output differs from %s (rewrite with -update-jev-scorecard-finding-golden):\n%s\nwant:\n%s", golden, out.String(), want)
	}
}

//go:build integration

package scorecard

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/jev/drift"
)

var updateDriftGolden = flag.Bool("update-drift-golden", false, "rewrite testdata/drift.golden")

// repoGit runs git in a fixture repository, standing in for the bare clone.
type repoGit struct {
	dir string
}

func (g repoGit) Run(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", g.dir}, args...)...)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w", args[0], err)
	}
	return string(out), nil
}

// driftReference is the doc the golden fixture history edits: a drift section
// and a scorecard section.
const driftReferenceV1 = `# Reference

## Drift battery

The battery asks Jev about each section.

## Scorecard

The scorecard reads each log.
`

// driftReferenceV2 adds a flag to the drift section, which is the edit the
// fixture's drift answers are checked against.
const driftReferenceV2 = `# Reference

## Drift battery

The battery asks Jev about each section.
Pass ` + "`--recent`" + ` to widen the window.

## Scorecard

The scorecard reads each log.
`

// fixtureRepo makes a repository whose main branch holds one commit per entry,
// each dated at.
func fixtureRepo(t *testing.T, commits []fixtureCommit) string {
	t.Helper()
	dir := t.TempDir()
	git := func(env []string, args ...string) {
		t.Helper()
		full := append([]string{"-C", dir, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "-c", "commit.gpgsign=false"}, args...)
		cmd := exec.Command("git", full...)
		cmd.Env = append(os.Environ(), env...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	git(nil, "init", "-q")
	git(nil, "symbolic-ref", "HEAD", "refs/heads/main")
	for _, c := range commits {
		for path, text := range c.files {
			full := filepath.Join(dir, path)
			if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(full, []byte(text), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		git(nil, "add", "-A")
		date := c.at.Format(time.RFC3339)
		git([]string{"GIT_AUTHOR_DATE=" + date, "GIT_COMMITTER_DATE=" + date}, "commit", "-q", "-m", "fixture")
	}
	return dir
}

// fixtureCommit is one commit of a fixture repository: the files it writes,
// by path, and its date.
type fixtureCommit struct {
	at    time.Time
	files map[string]string
}

func TestIntegrationDocGitEditsReadTheDocsHistory(t *testing.T) {
	t.Parallel()
	dir := fixtureRepo(t, []fixtureCommit{
		{at: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), files: map[string]string{"docs/reference.md": driftReferenceV1}},
		{at: time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC), files: map[string]string{"docs/reference.md": driftReferenceV2}},
	})
	edits, err := docGit{git: repoGit{dir: dir}}.Edits(context.Background(), "docs/reference.md")
	if err != nil {
		t.Fatal(err)
	}
	if len(edits) != 2 {
		t.Fatalf("edits = %d, want 2", len(edits))
	}
	if edits[0].Before != "" {
		t.Errorf("the first edit adds the doc, but Before = %q", edits[0].Before)
	}
	if edits[1].Before != driftReferenceV1 {
		t.Errorf("the second edit's Before is not the doc at its parent")
	}
	if !strings.Contains(edits[1].Patch, "+Pass `--recent`") {
		t.Errorf("the second edit's patch lacks the added line:\n%s", edits[1].Patch)
	}
	if !edits[1].Time.Equal(time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("second edit time = %v", edits[1].Time)
	}
}

// driftFixtureLog is the drift log the golden report reads. Its ask times and
// the fixture history put each section in one of the labeled states.
var driftFixtureLog = []string{
	// The drift section's edit touches it, and its flag is the one the edit names.
	`{"ts":"2026-09-01T00:00:00Z","bead":"gt-a","rig":"gastown","question_version":1,"model":"m1","sections":[{"doc":"docs/reference.md","heading":"Drift battery","answers":{"doc_now_stale":{"noul":0.9},"drift_kind":{"choice":"flag","probabilities":{"flag":0.8,"command":0.1,"config_key":0.05,"behavior":0.05}}}}]}`,
	// Nothing edits the scorecard section, and its window has passed.
	`{"ts":"2026-09-01T00:00:00Z","bead":"gt-b","rig":"gastown","question_version":1,"model":"m1","sections":[{"doc":"docs/reference.md","heading":"Scorecard","answers":{"doc_now_stale":{"noul":0.2}}}]}`,
	// Asked inside its window; the bead landed in the window names the doc, so
	// the section counts as stale even though no commit touched it.
	`{"ts":"2026-10-15T00:00:00Z","bead":"gt-c","rig":"gastown","question_version":1,"model":"m1","sections":[{"doc":"docs/reference.md","heading":"Drift battery","answers":{"doc_now_stale":{"noul":0.6},"drift_kind":{"choice":"command","probabilities":{"command":0.7,"flag":0.2,"behavior":0.1}}}}]}`,
	// The same edit as gt-a, picked as a command, which the edit does not name.
	`{"ts":"2026-09-01T00:00:00Z","bead":"gt-d","rig":"gastown","question_version":1,"model":"m1","sections":[{"doc":"docs/reference.md","heading":"Drift battery","answers":{"doc_now_stale":{"noul":0.7},"drift_kind":{"choice":"command","probabilities":{"command":0.7,"flag":0.2,"behavior":0.1}}}}]}`,
	// A line from another question set is skipped whole.
	`{"ts":"2026-09-01T00:00:00Z","bead":"gt-e","rig":"gastown","question_version":0,"sections":[]}`,
	// A landing that names no docs-relevant section has no item.
	`{"ts":"2026-09-02T00:00:00Z","bead":"gt-f","rig":"gastown","question_version":1,"model":"m1","sections":[]}`,
}

func TestIntegrationDriftScorecardPrintsPerQuestionBlocks(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	logPath := drift.LogPath(town)
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, []byte(strings.Join(driftFixtureLog, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	repo := fixtureRepo(t, []fixtureCommit{
		{at: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), files: map[string]string{"docs/reference.md": driftReferenceV1}},
		{at: time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC), files: map[string]string{"docs/reference.md": driftReferenceV2}},
	})
	// gt-c's bead landed inside its window and names the doc, so the bead rule
	// labels it stale even though no commit touched its section.
	landed := &beads.Issue{
		ID:          "gt-z",
		Title:       "docs: refresh docs/reference.md",
		Status:      statusClosed,
		CloseReason: "Landed on main (abc123)",
		ClosedAt:    time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC).Format(time.RFC3339),
	}
	a := newDriftAdapter(logPath,
		fakeDriftBeads{byRig: map[string][]*beads.Issue{"gastown": {landed}}},
		docGit{git: repoGit{dir: repo}},
		func() time.Time { return time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC) },
	)

	rep, err := Build(context.Background(), a, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Items != 4 || rep.LinesSkipped != 1 {
		t.Fatalf("items %d skipped %d, want 4 and 1", rep.Items, rep.LinesSkipped)
	}

	var out strings.Builder
	if err := WriteText(&out, rep); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	goldenPath := filepath.Join("testdata", "drift.golden")
	if *updateDriftGolden {
		if err := os.MkdirAll("testdata", 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(goldenPath, []byte(got), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Errorf("output differs from %s (rewrite with -update-drift-golden):\n%s\nwant:\n%s", goldenPath, got, want)
	}
}

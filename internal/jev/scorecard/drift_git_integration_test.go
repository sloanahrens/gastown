//go:build integration

package scorecard

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fixtureGitEnv is the caller's environment without the GIT_ variables that
// point git at another repository or config. The fixture then reads only its
// own repository, and the user's global and system git config stays out of it.
func fixtureGitEnv(extra ...string) []string {
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "GIT_") {
			env = append(env, kv)
		}
	}
	env = append(env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_TERMINAL_PROMPT=0")
	return append(env, extra...)
}

// repoGit runs git in a fixture repository, standing in for the bare clone.
type repoGit struct {
	dir string
}

func (g repoGit) Run(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", g.dir}, args...)...)
	cmd.Env = fixtureGitEnv()
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w", args[0], err)
	}
	return string(out), nil
}

// fixtureRepo makes a repository whose main branch holds one commit per entry,
// each dated at.
func fixtureRepo(t *testing.T, commits []fixtureCommit) string {
	t.Helper()
	dir := t.TempDir()
	git := func(env []string, args ...string) {
		t.Helper()
		full := append([]string{"-C", dir, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "-c", "commit.gpgsign=false"}, args...)
		cmd := exec.Command("git", full...)
		cmd.Env = fixtureGitEnv(env...)
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

// RunInput lets repoGit take the batched path, as the bare clone does.
func (g repoGit) RunInput(ctx context.Context, input string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", g.dir}, args...)...)
	cmd.Env = fixtureGitEnv()
	cmd.Stdin = strings.NewReader(input)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w", args[0], err)
	}
	return string(out), nil
}

// The two-call read of a doc's history returns the edits the call-per-edit read
// does, over a history that adds the doc, edits it, deletes it, adds it back,
// drops its final newline and merges a side branch.
func TestIntegrationDocGitBatchedEditsMatchOneByOne(t *testing.T) {
	t.Parallel()
	day := func(n int) time.Time { return time.Date(2026, 8, n, 0, 0, 0, 0, time.UTC) }
	dir := fixtureRepo(t, []fixtureCommit{
		{at: day(1), files: map[string]string{"docs/reference.md": driftReferenceV1, "docs/other.md": "other\n"}},
		{at: day(2), files: map[string]string{"docs/other.md": "other, edited\n"}},
		{at: day(3), files: map[string]string{"docs/reference.md": driftReferenceV2}},
		{at: day(4), files: map[string]string{"docs/reference.md": driftReferenceV2 + "no trailing newline"}},
		{at: day(5), files: map[string]string{"docs/reference.md": driftReferenceV1}},
	})
	git := repoGit{dir: dir}
	run := func(args ...string) {
		t.Helper()
		full := append([]string{"-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "-c", "commit.gpgsign=false"}, args...)
		if _, err := git.Run(context.Background(), full...); err != nil {
			t.Fatal(err)
		}
	}
	run("rm", "-q", "docs/reference.md")
	run("commit", "-q", "-m", "drop the doc")
	if err := os.WriteFile(filepath.Join(dir, "docs/reference.md"), []byte(driftReferenceV2), 0o600); err != nil {
		t.Fatal(err)
	}
	run("add", "-A")
	run("commit", "-q", "-m", "bring the doc back")

	ctx := context.Background()
	d := docGit{git: git}
	want, err := d.editsOneByOne(ctx, "docs/reference.md")
	if err != nil {
		t.Fatal(err)
	}
	got, err := d.Edits(ctx, "docs/reference.md")
	if err != nil {
		t.Fatal(err)
	}
	if len(want) < 5 {
		t.Fatalf("the fixture gave %d edits, want at least 5", len(want))
	}
	if len(got) != len(want) {
		t.Fatalf("batched edits = %d, call-per-edit edits = %d", len(got), len(want))
	}
	for i := range want {
		if !got[i].Time.Equal(want[i].Time) || got[i].Before != want[i].Before || got[i].Patch != want[i].Patch {
			t.Errorf("edit %d differs\n batched: %+v\n per-edit: %+v", i, got[i], want[i])
		}
	}
}

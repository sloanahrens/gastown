//go:build integration

package notes

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitDir is a clone stand-in that runs git in one directory, so the clone's
// read paths run against a real repository.
type gitDir string

func (g gitDir) Sync(context.Context) error { return nil }

func (g gitDir) Run(ctx context.Context, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, "git", append([]string{"-C", string(g)}, args...)...).Output()
	return string(out), err
}

// TestIntegrationCloneRepoReadsMainFromRealGit: the tree lists the files at
// refs/heads/main, and a key is found only when a Go source names it quoted,
// with a JSON tag option or without. git grep exits 1 for no match, which is
// false, not an error.
func TestIntegrationCloneRepoReadsMainFromRealGit(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		full := append([]string{"-C", dir, "-c", "user.name=test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false"}, args...)
		if out, err := exec.Command("git", full...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q", "-b", "main")
	for path, body := range map[string]string{
		"docs/café.md":            "# Café\n",
		"internal/config/conf.go": "package config\n\ntype C struct {\n\tJevStale bool `json:\"jev_stale,omitempty\"`\n\tPlain    bool `json:\"max_usd_per_day\"`\n}\n",
		"docs/notes.md":           "mentions \"only_in_markdown\" quoted\n",
	} {
		full := filepath.Join(dir, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git("add", "-A")
	git("commit", "-q", "-m", "files")

	repo := cloneRepo{c: gitDir(dir)}
	tree, err := repo.Tree(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(tree, ","), "docs/café.md,docs/notes.md,internal/config/conf.go,"; got != want {
		t.Errorf("Tree = %q, want %q", got, want)
	}
	for key, want := range map[string]bool{
		"jev_stale":        true,
		"max_usd_per_day":  true,
		"jev_gone":         false,
		"only_in_markdown": false,
	} {
		got, err := repo.HasKey(context.Background(), key)
		if err != nil || got != want {
			t.Errorf("HasKey(%q) = %v, %v; want %v", key, got, err, want)
		}
	}
}

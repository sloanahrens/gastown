//go:build integration

package drift

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

// TestIntegrationJevDriftListDocsReadsQuotedPathsFromRealGit: git quotes a
// path with non-ASCII bytes unless ls-tree is run with -z. The accented doc
// must still be listed, and a non-Markdown file must not.
func TestIntegrationJevDriftListDocsReadsQuotedPathsFromRealGit(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		full := append([]string{"-C", dir, "-c", "user.name=test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false"}, args...)
		if out, err := exec.Command("git", full...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q")
	for path, body := range map[string]string{
		"docs/reference.md": "# Reference\n",
		"docs/café.md":      "# Café\n",
		"docs/notes.txt":    "not markdown\n",
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
	git("commit", "-q", "-m", "docs")

	got, err := cloneRepo{c: gitDir(dir)}.ListDocs(context.Background(), "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if want := "docs/café.md,docs/reference.md"; strings.Join(got, ",") != want {
		t.Errorf("ListDocs = %v, want %s", got, want)
	}
}

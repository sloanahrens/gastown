package targets

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/constants"
)

// The source reads the file from the ref it names, with git run in the
// repository the caller gave it.
func TestJevTargetsSourceReadsTheRankedRef(t *testing.T) {
	t.Parallel()
	var dirs, calls []string
	run := func(_ context.Context, dir string, args ...string) (string, error) {
		dirs = append(dirs, dir)
		calls = append(calls, strings.Join(args, " "))
		return "package a\n", nil
	}
	g := &gitSource{repo: "/repo", ref: "origin/main", timeout: time.Second, run: run}

	text, err := g.ReadFile(context.Background(), "internal/a/one.go")
	if err != nil {
		t.Fatal(err)
	}
	if text != "package a\n" {
		t.Errorf("text = %q", text)
	}
	if len(calls) != 1 || calls[0] != "show origin/main:internal/a/one.go" || dirs[0] != "/repo" {
		t.Errorf("calls = %v in %v, want one show at the ranked ref in the named repo", calls, dirs)
	}
}

// A path or ref git would read as an option is refused before git runs.
func TestJevTargetsSourceRefusesUnsafeInput(t *testing.T) {
	t.Parallel()
	called := 0
	run := func(_ context.Context, _ string, _ ...string) (string, error) {
		called++
		return "", nil
	}
	g := &gitSource{repo: "/repo", ref: "HEAD", timeout: time.Second, run: run}
	if _, err := g.ReadFile(context.Background(), "../../etc/passwd"); err == nil {
		t.Error("a traversal path was read")
	}
	g.ref = "-reader"
	if _, err := g.ReadFile(context.Background(), "internal/a.go"); err == nil {
		t.Error("an option-shaped ref was read")
	}
	if called != 0 {
		t.Errorf("git ran %d times, want never", called)
	}
}

// The log sits under the town's runtime directory, beside the other batteries.
func TestJevTargetsLogPath(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	want := filepath.Join(town, constants.DirRuntime, "jev", "targets.jsonl")
	if got := LogPath(town); got != want {
		t.Errorf("LogPath = %q, want %q", got, want)
	}
}

// A run with no price configured stops before it reaches Jev, and the error
// names the key to set.
func TestJevTargetsOpenNeedsAPrice(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	_, err := Open(town, Settings{}, nil)
	if err == nil || !strings.Contains(err.Error(), "price per billion tokens") {
		t.Fatalf("err = %v, want the missing price named", err)
	}
	if _, statErr := os.Stat(LogPath(town)); !os.IsNotExist(statErr) {
		t.Errorf("a refused run wrote a log: %v", statErr)
	}
}

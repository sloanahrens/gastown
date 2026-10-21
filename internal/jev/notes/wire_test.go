package notes

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestMemoryDirNamesTheProjectFromTheTownRoot(t *testing.T) {
	t.Parallel()
	got := MemoryDir("/Users/sloan", "/Users/sloan/gt")
	if want := "/Users/sloan/.claude/projects/-Users-sloan-gt/memory"; got != want {
		t.Errorf("MemoryDir = %q, want %q", got, want)
	}
	if got, want := HandbookPath("/Users/sloan"), "/Users/sloan/.claude/commands/gastown.md"; got != want {
		t.Errorf("HandbookPath = %q, want %q", got, want)
	}
}

// ListFiles reads the memory directory's markdown and the handbook, in name
// order, and nothing else.
func TestListFiles(t *testing.T) {
	t.Parallel()
	home, town := t.TempDir(), "/town/gt"
	mem := MemoryDir(home, town)
	for path, body := range map[string]string{
		filepath.Join(mem, "b-note.md"):       "b",
		filepath.Join(mem, "a-note.md"):       "a",
		filepath.Join(mem, "notes.txt"):       "not markdown",
		filepath.Join(mem, "sub", "inner.md"): "in a subdirectory",
		HandbookPath(home):                    "handbook",
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	files, err := ListFiles(home, town)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, f := range files {
		got = append(got, f.key())
	}
	if want := []string{"handbook/gastown.md", "memory/a-note.md", "memory/b-note.md"}; !reflect.DeepEqual(got, want) {
		t.Errorf("files = %v, want %v", got, want)
	}
}

// Neither directory existing is no files, not an error.
func TestListFilesWithNothingThere(t *testing.T) {
	t.Parallel()
	files, err := ListFiles(t.TempDir(), "/town/gt")
	if err != nil || len(files) != 0 {
		t.Errorf("ListFiles = %v, %v; want none", files, err)
	}
}

// fakeClone answers git calls from a script and records them.
type fakeClone struct {
	out   string
	err   error
	calls [][]string
}

func (f *fakeClone) Sync(context.Context) error { return nil }

func (f *fakeClone) Run(_ context.Context, args ...string) (string, error) {
	f.calls = append(f.calls, args)
	return f.out, f.err
}

// The tree is read at the main branch ref, NUL-separated.
func TestCloneRepoTreeReadsMain(t *testing.T) {
	t.Parallel()
	c := &fakeClone{out: "docs/a.md\x00internal/b.go\x00"}
	got, err := cloneRepo{c: c}.Tree(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"docs/a.md", "internal/b.go", ""}; !reflect.DeepEqual(got, want) {
		t.Errorf("Tree = %q, want %q", got, want)
	}
	if want := []string{"ls-tree", "-r", "-z", "--name-only", "refs/heads/main"}; !reflect.DeepEqual(c.calls[0], want) {
		t.Errorf("git args = %v, want %v", c.calls[0], want)
	}
}

// A key that is not a plain config-key segment never reaches git.
func TestCloneRepoHasKeyRefusesAnUnsafeKey(t *testing.T) {
	t.Parallel()
	c := &fakeClone{}
	for _, key := range []string{"", "-e", "a b", "a\"b", "Upper", "a.b"} {
		if _, err := (cloneRepo{c: c}).HasKey(context.Background(), key); err == nil || !strings.Contains(err.Error(), "refused") {
			t.Errorf("HasKey(%q) = %v, want a refusal", key, err)
		}
	}
	if len(c.calls) != 0 {
		t.Errorf("git ran for a refused key: %v", c.calls)
	}
}

// A failure that is not git grep's "no match" exit is an error.
func TestCloneRepoHasKeyReportsRealFailures(t *testing.T) {
	t.Parallel()
	c := &fakeClone{err: errors.New("git grep: bad object")}
	if ok, err := (cloneRepo{c: c}).HasKey(context.Background(), "jev_stale"); err == nil || ok {
		t.Errorf("HasKey = %v, %v; want an error", ok, err)
	}
}

func TestOpenNeedsAPrice(t *testing.T) {
	t.Parallel()
	if _, err := Open(t.TempDir(), Settings{}, nil); err == nil || !strings.Contains(err.Error(), "jev_notes") {
		t.Errorf("Open without a price = %v, want an error naming jev_notes", err)
	}
}

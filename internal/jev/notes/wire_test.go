package notes

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/agentlog"
)

// The memory directory is the project directory agentlog names for the town
// root, under the config dir the caller resolved.
func TestMemoryDirNamesTheProjectFromTheTownRoot(t *testing.T) {
	t.Parallel()
	got, err := memoryDir("/cfg", "/Users/sloan/gt")
	if want := "/cfg/projects/-Users-sloan-gt/memory"; err != nil || got != want {
		t.Errorf("memoryDir = %q, %v; want %q, nil", got, err, want)
	}
	if got, want := handbookPath("/Users/sloan"), "/Users/sloan/.claude/commands/gastown.md"; got != want {
		t.Errorf("handbookPath = %q, want %q", got, want)
	}
}

// A non-ASCII letter in the town root stays in the project directory name: the
// hash is agentlog's, so the notes pass and the transcript reader name the same
// directory.
func TestMemoryDirHashesNonASCIILikeAgentlog(t *testing.T) {
	t.Parallel()
	const town = "/town/gt-café"
	got, err := memoryDir("/cfg", town)
	if err != nil {
		t.Fatal(err)
	}
	project, err := agentlog.ClaudeProjectDirIn("/cfg", town)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(project, "memory"); got != want {
		t.Errorf("memoryDir = %q, want agentlog's %q", got, want)
	}
}

// getenvFor is the environment the pass reads, with CLAUDE_CONFIG_DIR set to
// configDir when it is not empty.
func getenvFor(configDir string) func(string) string {
	return func(key string) string {
		if key == "CLAUDE_CONFIG_DIR" {
			return configDir
		}
		return ""
	}
}

// writeNote puts a markdown note at path.
func writeNote(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("note"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// noteNames returns the names of the files listed, which names the directory
// they came from.
func noteNames(files []File) []string {
	var names []string
	for _, f := range files {
		names = append(names, f.Name)
	}
	return names
}

// With CLAUDE_CONFIG_DIR set, the pass reads the memory directory under that
// config dir — the one its sessions write to — and leaves ~/.claude's alone.
// Before the fix the two disagreed, so the pass silently read nothing (gt-zrwgh).
func TestListNotesHonorsClaudeConfigDir(t *testing.T) {
	t.Parallel()
	configDir, home, town := t.TempDir(), t.TempDir(), "/town/gt"

	mem, err := memoryDir(configDir, town)
	if err != nil {
		t.Fatal(err)
	}
	live := filepath.Join(mem, "live-note.md")
	writeNote(t, live)

	stale, err := memoryDir(filepath.Join(home, ".claude"), town)
	if err != nil {
		t.Fatal(err)
	}
	writeNote(t, filepath.Join(stale, "stale-note.md"))

	// The handbook is the operator's, not the town's config: it stays under
	// home and is still read.
	writeNote(t, handbookPath(home))

	files, err := listNotes(getenvFor(configDir), func() (string, error) { return home, nil }, town)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := noteNames(files), []string{"gastown.md", "live-note.md"}; !reflect.DeepEqual(got, want) {
		t.Errorf("files = %v, want %v", got, want)
	}
	if len(files) == 2 && files[1].Path != live {
		t.Errorf("note path = %q, want %q", files[1].Path, live)
	}
}

// With CLAUDE_CONFIG_DIR unset, the memory directory is the home directory's
// ~/.claude one.
func TestListNotesFallsBackToHome(t *testing.T) {
	t.Parallel()
	home, town := t.TempDir(), "/town/gt"
	mem, err := memoryDir(filepath.Join(home, ".claude"), town)
	if err != nil {
		t.Fatal(err)
	}
	writeNote(t, filepath.Join(mem, "home-note.md"))

	files, err := listNotes(getenvFor(""), func() (string, error) { return home, nil }, town)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := noteNames(files), []string{"home-note.md"}; !reflect.DeepEqual(got, want) {
		t.Errorf("files = %v, want %v", got, want)
	}
}

// ListFiles reads the memory directory's markdown and the handbook, in name
// order, and nothing else.
func TestListFiles(t *testing.T) {
	t.Parallel()
	configDir, home, town := t.TempDir(), t.TempDir(), "/town/gt"
	mem, err := memoryDir(configDir, town)
	if err != nil {
		t.Fatal(err)
	}
	for path, body := range map[string]string{
		filepath.Join(mem, "b-note.md"):       "b",
		filepath.Join(mem, "a-note.md"):       "a",
		filepath.Join(mem, "notes.txt"):       "not markdown",
		filepath.Join(mem, "sub", "inner.md"): "in a subdirectory",
		handbookPath(home):                    "handbook",
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	files, err := listFiles(configDir, home, town)
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

// A note the operator symlinked into the memory directory is read, the way the
// handbook path follows its links. Before the fix the directory read's type
// check dropped it with no log line.
func TestListFilesFollowsSymlinkedNotes(t *testing.T) {
	t.Parallel()
	configDir, home, town := t.TempDir(), t.TempDir(), "/town/gt"
	mem, err := memoryDir(configDir, town)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(mem, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "linked-note.md")
	writeNote(t, target)
	if err := os.Symlink(target, filepath.Join(mem, "linked.md")); err != nil {
		t.Fatal(err)
	}
	// A link with no target is not a note.
	if err := os.Symlink(filepath.Join(t.TempDir(), "gone.md"), filepath.Join(mem, "dangling.md")); err != nil {
		t.Fatal(err)
	}

	files, err := listFiles(configDir, home, town)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := noteNames(files), []string{"linked.md"}; !reflect.DeepEqual(got, want) {
		t.Errorf("files = %v, want %v", got, want)
	}
	if len(files) == 1 && files[0].Path != filepath.Join(mem, "linked.md") {
		t.Errorf("note path = %q, want the link in the memory directory", files[0].Path)
	}
}

// Neither directory existing is no files, not an error.
func TestListFilesWithNothingThere(t *testing.T) {
	t.Parallel()
	files, err := listFiles(t.TempDir(), t.TempDir(), "/town/gt")
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

// exitFailure is a failed command whose exit status survives a %w wrap, as the
// *exec.ExitError Clone.Run wraps does.
type exitFailure int

func (e exitFailure) Error() string { return fmt.Sprintf("exit status %d", int(e)) }
func (e exitFailure) ExitCode() int { return int(e) }

// git grep exits 1 for no match and 2 for a real failure. submission.Clone.Run
// wraps either with %w, so HasKey reads the code through the wrap: the 1 is
// false, not an error, and every other exit stays a failure.
func TestCloneRepoHasKeyReadsTheWrappedExitStatus(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		code    int
		wantErr bool
	}{
		{code: 1},
		{code: 2, wantErr: true},
	} {
		c := &fakeClone{err: fmt.Errorf("git grep in /clone: %w: ", exitFailure(tt.code))}
		ok, err := (cloneRepo{c: c}).HasKey(context.Background(), "jev_stale")
		if ok {
			t.Errorf("exit %d: HasKey = true, want false", tt.code)
		}
		if gotErr := err != nil; gotErr != tt.wantErr {
			t.Errorf("exit %d: error = %v, want error: %v", tt.code, err, tt.wantErr)
		}
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

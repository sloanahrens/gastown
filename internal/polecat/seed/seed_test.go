package seed

import (
	"archive/tar"
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPlanSeedsTheBuildRigAtTheBuildCommit(t *testing.T) {
	t.Parallel()
	pins, err := Pins([]byte("# a comment\ndevops 92b9e510ac2f9ff8c80b0291f312ac482bf96c38\n"))
	if err != nil {
		t.Fatalf("Pins: %v", err)
	}
	got, err := Plan(pins, "gastown", "914de8c65d8cbc6302cd55ef2c06735a47166627")
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	want := []Source{
		{Rig: "gastown", Commit: "914de8c65d8cbc6302cd55ef2c06735a47166627"},
		{Rig: "devops", Commit: "92b9e510ac2f9ff8c80b0291f312ac482bf96c38"},
	}
	if len(got) != len(want) {
		t.Fatalf("Plan: got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Plan[%d]: got %v, want %v", i, got[i], want[i])
		}
	}
}

func TestPlanRefusesAPinForTheBuildRig(t *testing.T) {
	t.Parallel()
	pins := []Source{
		{Rig: "devops", Commit: "92b9e510"},
		{Rig: "gastown", Commit: "914de8c6"},
	}
	_, err := Plan(pins, "gastown", "f05e821c")
	if err == nil {
		t.Fatal("Plan accepted a pin for the build rig; the pin would read as gastown's source while the build commit is used")
	}
	if !strings.Contains(err.Error(), "gastown, but that rig is seeded from the build commit") {
		t.Errorf("Plan: error does not name the build rig: %v", err)
	}
}

func TestPlanRefusesABuildCommitItCannotRead(t *testing.T) {
	t.Parallel()
	pins := []Source{{Rig: "devops", Commit: "92b9e510"}}
	for _, commit := range []string{"", "HEAD", "f05e82", "zzzzzzz"} {
		_, err := Plan(pins, "gastown", commit)
		if err == nil {
			t.Errorf("Plan accepted %q as a build commit", commit)
			continue
		}
		if !strings.Contains(err.Error(), "cannot read the build commit") {
			t.Errorf("Plan(%q): error does not name the build commit: %v", commit, err)
		}
	}
}

func TestPins(t *testing.T) {
	t.Parallel()
	pins, err := Pins([]byte("\n# <rig> <commit>\n  devops 92b9e510  \n\n"))
	if err != nil {
		t.Fatalf("Pins: %v", err)
	}
	if len(pins) != 1 || pins[0] != (Source{Rig: "devops", Commit: "92b9e510"}) {
		t.Errorf("Pins: got %v", pins)
	}
	if _, err := Pins([]byte("devops\n")); err == nil || !strings.Contains(err.Error(), "line 1") {
		t.Errorf("Pins of a one-field line: got %v, want an error naming the line", err)
	}
	if _, err := Pins([]byte("# only a comment\n")); err == nil || !strings.Contains(err.Error(), "names no rig") {
		t.Errorf("Pins of an empty file: got %v, want an error naming the empty seed", err)
	}
}

// TestStageReadsTheBuildCommit stages gastown at the build commit while the
// checkout it reads from holds another, and devops at its pin: a sandbox then
// has the modules of the gt it runs, and every other rig its pinned set.
func TestStageReadsTheBuildCommit(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	dest := t.TempDir()
	const buildCommit = "f05e821cda74197a137049fc69f9a46c0c23c78f"
	const pin = "92b9e510ac2f9ff8c80b0291f312ac482bf96c38"
	g := &fakeGit{t: t, trees: map[string]map[string]string{
		buildCommit: {"go.mod": "module example.com/gastown\n"},
		pin:         {"go.mod": "module example.com/devops\n"},
	}}
	sources, err := Plan([]Source{{Rig: "devops", Commit: pin}}, "gastown", buildCommit)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	if err := Stage(dest, town, sources, g.archives); err != nil {
		t.Fatalf("Stage: %v", err)
	}

	for _, want := range []struct{ rig, commit string }{{"gastown", buildCommit}, {"devops", pin}} {
		got, err := os.ReadFile(filepath.Join(dest, want.rig, "go.mod"))
		if err != nil {
			t.Fatalf("stage of %s: %v", want.rig, err)
		}
		if !strings.Contains(string(got), "/"+want.rig) {
			t.Errorf("%s staged %q, want the tree of %s", want.rig, got, want.commit)
		}
	}
	wantCalls := []string{
		filepath.Join(town, "gastown", "mayor", "rig") + " archive --format=tar " + buildCommit,
		filepath.Join(town, "devops", "mayor", "rig") + " archive --format=tar " + pin,
	}
	if got := strings.Join(g.calls, "\n"); got != strings.Join(wantCalls, "\n") {
		t.Errorf("git calls:\n%s\nwant:\n%s", got, strings.Join(wantCalls, "\n"))
	}
}

func TestStageClearsARigStagedBefore(t *testing.T) {
	t.Parallel()
	dest := t.TempDir()
	stale := filepath.Join(dest, "gastown", "gone.txt")
	if err := os.MkdirAll(filepath.Dir(stale), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(stale, []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	const buildCommit = "f05e821cda74197a137049fc69f9a46c0c23c78f"
	g := &fakeGit{t: t, trees: map[string]map[string]string{buildCommit: {"go.mod": "module example.com/gastown\n"}}}

	if err := Stage(dest, t.TempDir(), []Source{{Rig: "gastown", Commit: buildCommit}}, g.archives); err != nil {
		t.Fatalf("Stage: %v", err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("a file the source no longer has survived the stage: %v", err)
	}
}

func TestStageNamesTheRigAndCommitTheCheckoutLacks(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	g := &fakeGit{t: t, trees: map[string]map[string]string{}}
	err := Stage(t.TempDir(), town, []Source{{Rig: "gastown", Commit: "f05e821c"}}, g.archives)
	if err == nil {
		t.Fatal("Stage accepted a commit no checkout holds")
	}
	for _, want := range []string{"cannot stage rig gastown at f05e821c", filepath.Join(town, "gastown", "mayor", "rig"), "not a valid object name"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Stage: error lacks %q: %v", want, err)
		}
	}
}

// tarEntry is one member of the archive Unpack is given: its header and, for a
// regular file, its body.
type tarEntry struct {
	h    tar.Header
	body string
}

// tarStream returns the tar stream of entries, each regular file sized to its
// body.
func tarStream(t *testing.T, entries ...tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		if e.h.Typeflag == tar.TypeReg {
			e.h.Size = int64(len(e.body))
		}
		if err := tw.WriteHeader(&e.h); err != nil {
			t.Fatalf("WriteHeader(%s): %v", e.h.Name, err)
		}
		if _, err := tw.Write([]byte(e.body)); err != nil {
			t.Fatalf("Write(%s): %v", e.h.Name, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return buf.Bytes()
}

func TestUnpackRefusesAPathOutsideTheSeed(t *testing.T) {
	t.Parallel()
	tree := tarStream(t, tarEntry{h: tar.Header{Name: "../escape", Mode: 0o644, Typeflag: tar.TypeReg}, body: "x"})
	if err := Unpack(filepath.Join(t.TempDir(), "rig"), tree); err == nil {
		t.Fatal("Unpack wrote an entry outside the seed directory")
	}
}

func TestUnpackWritesFilesAndSymlinks(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "rig")
	tree := tarStream(t,
		tarEntry{h: tar.Header{Name: "cmd/gt/main.go", Mode: 0o755, Typeflag: tar.TypeReg}, body: "main"},
		tarEntry{h: tar.Header{Name: "link", Mode: 0o777, Linkname: "cmd/gt/main.go", Typeflag: tar.TypeSymlink}},
	)

	if err := Unpack(dir, tree); err != nil {
		t.Fatalf("Unpack: %v", err)
	}
	fi, err := os.Stat(filepath.Join(dir, "cmd", "gt", "main.go"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if fi.Mode().Perm() != 0o755 {
		t.Errorf("mode: got %v, want 0755", fi.Mode().Perm())
	}
	if target, err := os.Readlink(filepath.Join(dir, "link")); err != nil || target != "cmd/gt/main.go" {
		t.Errorf("symlink: got %q, %v", target, err)
	}
}

// A link that stays inside the seed through ".." is a link Unpack writes: the
// refusal is for targets that leave the seed, not for every ".." (gt-eu4u0).
func TestUnpackWritesARelativeSymlinkThatStaysInside(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "rig")
	tree := tarStream(t,
		tarEntry{h: tar.Header{Name: "cmd/main.go", Mode: 0o644, Typeflag: tar.TypeReg}, body: "main"},
		tarEntry{h: tar.Header{Name: "cmd/gt", Mode: 0o755, Typeflag: tar.TypeDir}},
		tarEntry{h: tar.Header{Name: "cmd/gt/alias", Mode: 0o777, Linkname: "../main.go", Typeflag: tar.TypeSymlink}},
	)
	if err := Unpack(dir, tree); err != nil {
		t.Fatalf("Unpack: %v", err)
	}
	if target, err := os.Readlink(filepath.Join(dir, "cmd", "gt", "alias")); err != nil || target != "../main.go" {
		t.Errorf("symlink: got %q, %v", target, err)
	}
}

// A symlink to an absolute path or out of the seed is refused: the archive may
// write only inside the seed, wherever the link's target lies (gt-eu4u0).
func TestUnpackRefusesASymlinkOutOfTheSeed(t *testing.T) {
	t.Parallel()
	for _, linkname := range []string{"/etc", "../outside", "a/../../outside"} {
		tree := tarStream(t, tarEntry{h: tar.Header{Name: "link", Mode: 0o777, Linkname: linkname, Typeflag: tar.TypeSymlink}})
		if err := Unpack(filepath.Join(t.TempDir(), "rig"), tree); err == nil {
			t.Errorf("Unpack accepted a symlink to %q", linkname)
		}
	}
}

// A file entry whose parent is a symlink is refused even when the link stays
// inside the seed: git never puts a path under a symlink, so such a tree is not
// one a seed holds (gt-eu4u0).
func TestUnpackRefusesAnEntryUnderASymlink(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "rig")
	tree := tarStream(t,
		tarEntry{h: tar.Header{Name: "real", Mode: 0o755, Typeflag: tar.TypeDir}},
		tarEntry{h: tar.Header{Name: "sub", Mode: 0o777, Linkname: "real", Typeflag: tar.TypeSymlink}},
		tarEntry{h: tar.Header{Name: "sub/x", Mode: 0o644, Typeflag: tar.TypeReg}, body: "x"},
	)
	if err := Unpack(dir, tree); err == nil {
		t.Fatal("Unpack wrote an entry through a symlink")
	}
	if _, err := os.Stat(filepath.Join(dir, "real", "x")); !os.IsNotExist(err) {
		t.Errorf("the entry was written through the link: %v", err)
	}
}

// The escape the mutation review found: a tar that links to a directory
// outside the seed and then writes a file through it must leave that file
// unwritten. The target directory exists here, so only the guard stops the
// write (gt-eu4u0).
func TestUnpackRefusesAFileWrittenThroughAnEscapingSymlink(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	outside := filepath.Join(parent, "outside")
	if err := os.Mkdir(outside, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	tree := tarStream(t,
		tarEntry{h: tar.Header{Name: "a", Mode: 0o777, Linkname: "../outside", Typeflag: tar.TypeSymlink}},
		tarEntry{h: tar.Header{Name: "a/x", Mode: 0o644, Typeflag: tar.TypeReg}, body: "escaped"},
	)
	if err := Unpack(filepath.Join(parent, "rig"), tree); err == nil {
		t.Fatal("Unpack accepted a tar that writes through an escaping symlink")
	}
	if _, err := os.Stat(filepath.Join(outside, "x")); !os.IsNotExist(err) {
		t.Errorf("a file was written outside the seed: %v", err)
	}
}

// fakeGit answers the one git command Stage issues with the tree recorded for
// the commit: a tar stream of the file paths and contents given.
type fakeGit struct {
	t     *testing.T
	trees map[string]map[string]string // commit -> path -> content
	calls []string
}

func (g *fakeGit) archives(dir string, args ...string) ([]byte, error) {
	g.t.Helper()
	g.calls = append(g.calls, dir+" "+strings.Join(args, " "))
	if len(args) != 3 || args[0] != "archive" || args[1] != "--format=tar" {
		g.t.Fatalf("unexpected git call: %s %v", dir, args)
	}
	files, ok := g.trees[args[2]]
	if !ok {
		return nil, errors.New("exit status 128: fatal: not a valid object name")
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for path, content := range files {
		if err := tw.WriteHeader(&tar.Header{Name: path, Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
			g.t.Fatalf("WriteHeader: %v", err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			g.t.Fatalf("Write: %v", err)
		}
	}
	if err := tw.Close(); err != nil {
		g.t.Fatalf("Close: %v", err)
	}
	return buf.Bytes(), nil
}

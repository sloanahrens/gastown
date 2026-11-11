package seed

import (
	"archive/tar"
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"slices"
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

// A pinned build rig is seeded at its pin, not at the build commit, so the seed
// is the same for every commit that the pin covers. The build commit is still
// what the image compiles.
func TestPlanSeedsTheBuildRigAtTheCommitItIsGiven(t *testing.T) {
	t.Parallel()
	pins := []Source{
		{Rig: "devops", Commit: "92b9e510"},
		{Rig: "gastown", Commit: "914de8c6"},
	}
	plan, err := Plan(pins, "gastown", "914de8c6")
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	want := []Source{{Rig: "gastown", Commit: "914de8c6"}, {Rig: "devops", Commit: "92b9e510"}}
	if len(plan) != len(want) || plan[0] != want[0] || plan[1] != want[1] {
		t.Errorf("Plan = %v, want %v", plan, want)
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

// bareFixture makes <town>/<rig>/.repo.git hold the on-disk shape of a bare
// repository (HEAD, objects, refs), the shape bareRepo checks. The git calls a
// test makes are answered by a fake, so the directory holds no history.
func bareFixture(t *testing.T, town, rig string) string {
	t.Helper()
	dir := filepath.Join(town, rig, ".repo.git")
	for _, p := range []string{"objects", "refs"} {
		if err := os.MkdirAll(filepath.Join(dir, p), 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return dir
}

// TestStageReadsTheBuildCommit stages gastown at the build commit while the
// checkout it reads from holds another, and devops at its pin: a sandbox then
// has the modules of the gt it runs, and every other rig its pinned set.
func TestStageReadsTheBuildCommit(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	gastownBare := bareFixture(t, town, "gastown")
	devopsBare := bareFixture(t, town, "devops")
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

	if err := Stage(dest, town, "gastown", "", sources, g.archives); err != nil {
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
	if len(g.calls) != 2 {
		t.Fatalf("git calls = %v, want one archive per rig", g.calls)
	}
	checkArchiveCall(t, g.calls[0], gastownBare, gastownBare, buildCommit)
	checkArchiveCall(t, g.calls[1], devopsBare, devopsBare, pin)
}

// With a build source, the build rig's sources come from that directory while
// every other pinned rig is still read from its own .repo.git bare repo under
// the town.
func TestStageReadsTheBuildRigFromTheBuildSource(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	devopsBare := bareFixture(t, town, "devops")
	buildSrc := t.TempDir()
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

	if err := Stage(dest, town, "gastown", buildSrc, sources, g.archives); err != nil {
		t.Fatalf("Stage: %v", err)
	}

	if len(g.calls) != 2 {
		t.Fatalf("git calls = %v, want one archive per rig", g.calls)
	}
	checkArchiveCall(t, g.calls[0], buildSrc, "", buildCommit)
	checkArchiveCall(t, g.calls[1], devopsBare, devopsBare, pin)
	for _, want := range []struct{ rig, commit string }{{"gastown", buildCommit}, {"devops", pin}} {
		got, err := os.ReadFile(filepath.Join(dest, want.rig, "go.mod"))
		if err != nil {
			t.Fatalf("stage of %s: %v", want.rig, err)
		}
		if !strings.Contains(string(got), "/"+want.rig) {
			t.Errorf("%s staged %q, want the tree of %s", want.rig, got, want.commit)
		}
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
	town := t.TempDir()
	bareFixture(t, town, "gastown")

	if err := Stage(dest, town, "gastown", "", []Source{{Rig: "gastown", Commit: buildCommit}}, g.archives); err != nil {
		t.Fatalf("Stage: %v", err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("a file the source no longer has survived the stage: %v", err)
	}
}

func TestStageNamesTheRigAndCommitTheBareRepoLacks(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	bare := bareFixture(t, town, "gastown")
	g := &fakeGit{t: t, trees: map[string]map[string]string{}}
	err := Stage(t.TempDir(), town, "gastown", "", []Source{{Rig: "gastown", Commit: "f05e821c"}}, g.archives)
	if err == nil {
		t.Fatal("Stage accepted a commit no bare repo holds")
	}
	for _, want := range []string{"cannot stage rig gastown at f05e821c", bare, "not a valid object name"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Stage: error lacks %q: %v", want, err)
		}
	}
}

// A rig with no .repo.git is refused before git runs, and the error names the
// bare path the stage would have read.
func TestStageRefusesARigWithNoBareRepo(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	g := &fakeGit{t: t, trees: map[string]map[string]string{}}
	err := Stage(t.TempDir(), town, "gastown", "", []Source{{Rig: "gastown", Commit: "f05e821c"}}, g.archives)
	if err == nil {
		t.Fatal("Stage read a rig with no bare repo")
	}
	if want := filepath.Join(town, "gastown", ".repo.git"); !strings.Contains(err.Error(), want) {
		t.Errorf("Stage: error does not name the bare path %q: %v", want, err)
	}
	if len(g.calls) != 0 {
		t.Errorf("Stage ran git against a rig with no bare repo: %v", g.calls)
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
	err := Unpack(filepath.Join(t.TempDir(), "rig"), tree)
	if err == nil {
		t.Fatal("Unpack wrote an entry outside the seed directory")
	}
	if !strings.Contains(err.Error(), `archive entry "../escape" escapes the seed directory`) {
		t.Errorf("Unpack: error does not name the escaping entry: %v", err)
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

// A link that names an absolute path, or one whose ".." climbs past the seed,
// points outside wherever the seed is unpacked, so Unpack refuses it: the seed
// sits under the repository and must stay there (gt-eu4u0).
func TestUnpackRefusesASymlinkOutOfTheSeed(t *testing.T) {
	t.Parallel()
	for _, linkname := range []string{"/etc", "../outside", "a/../../outside"} {
		tree := tarStream(t, tarEntry{h: tar.Header{Name: "link", Mode: 0o777, Linkname: linkname, Typeflag: tar.TypeSymlink}})
		err := Unpack(filepath.Join(t.TempDir(), "rig"), tree)
		if err == nil {
			t.Errorf("Unpack accepted a symlink to %q", linkname)
			continue
		}
		if !strings.Contains(err.Error(), `archive symlink "link" escapes the seed directory`) {
			t.Errorf("Unpack of a link to %q: error does not name the escaping link: %v", linkname, err)
		}
	}
}

// A link's name is not where it points: here "." resolves b to the seed itself,
// so b/c/../.. ends one level above it while the written name stays inside.
// Unpack follows the links in the seed and refuses the one that leaves.
func TestUnpackRefusesALinkRoutingThroughAnotherLinkOutOfTheSeed(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	if err := os.Mkdir(filepath.Join(parent, "outside"), 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	tree := tarStream(t,
		tarEntry{h: tar.Header{Name: "c", Mode: 0o755, Typeflag: tar.TypeDir}},
		tarEntry{h: tar.Header{Name: "b", Mode: 0o777, Linkname: ".", Typeflag: tar.TypeSymlink}},
		tarEntry{h: tar.Header{Name: "l", Mode: 0o777, Linkname: "b/c/../../outside", Typeflag: tar.TypeSymlink}},
	)
	err := Unpack(filepath.Join(parent, "rig"), tree)
	if err == nil {
		t.Fatal("Unpack accepted a link that resolves outside the seed through another link")
	}
	if !strings.Contains(err.Error(), `archive symlink "l" escapes the seed directory`) {
		t.Errorf("Unpack: error does not name the escaping link: %v", err)
	}
}

// A link that routes through another link and lands inside the seed is one git
// can hold, so Unpack writes it: the refusal is for the links that leave.
func TestUnpackWritesALinkRoutingThroughAnotherLinkInsideTheSeed(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "rig")
	tree := tarStream(t,
		tarEntry{h: tar.Header{Name: "c", Mode: 0o755, Typeflag: tar.TypeDir}},
		tarEntry{h: tar.Header{Name: "b", Mode: 0o777, Linkname: ".", Typeflag: tar.TypeSymlink}},
		tarEntry{h: tar.Header{Name: "l", Mode: 0o777, Linkname: "b/c", Typeflag: tar.TypeSymlink}},
	)
	if err := Unpack(dir, tree); err != nil {
		t.Fatalf("Unpack: %v", err)
	}
}

// An absolute target is resolved against the host, not the seed, so one written
// as the seed's own path followed by ".." points outside it while its text
// still starts with the root: Unpack resolves the path instead of comparing its
// prefix (gt-eu4u0).
func TestUnpackRefusesAnAbsoluteSymlinkThatClimbsBackOutOfTheSeed(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "rig")
	linkname := dir + "/../../outside"
	tree := tarStream(t, tarEntry{h: tar.Header{Name: "link", Mode: 0o777, Linkname: linkname, Typeflag: tar.TypeSymlink}})
	err := Unpack(dir, tree)
	if err == nil {
		t.Fatal("Unpack accepted an absolute symlink that resolves outside the seed")
	}
	if !strings.Contains(err.Error(), `archive symlink "link" escapes the seed directory`) {
		t.Errorf("Unpack: error does not name the escaping link: %v", err)
	}
}

// The seed must unpack the same tree wherever it is built, so an absolute
// target is refused even when it names a path under dir: the target is a path
// on the host, not one in the seed (gt-eu4u0).
func TestUnpackRefusesAnAbsoluteSymlinkIntoTheSeed(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "rig")
	tree := tarStream(t,
		tarEntry{h: tar.Header{Name: "real", Mode: 0o755, Typeflag: tar.TypeDir}},
		tarEntry{h: tar.Header{Name: "link", Mode: 0o777, Linkname: filepath.Join(dir, "real"), Typeflag: tar.TypeSymlink}},
	)
	err := Unpack(dir, tree)
	if err == nil {
		t.Fatal("Unpack accepted an absolute symlink target")
	}
	if !strings.Contains(err.Error(), `archive symlink "link" escapes the seed directory`) {
		t.Errorf("Unpack: error does not name the escaping link: %v", err)
	}
}

// A link that routes through a regular file reaches nothing that could be a
// link, so Unpack writes the dangling link git holds instead of failing the
// seed with a stat error (gt-eu4u0).
func TestUnpackWritesALinkRoutingThroughARegularFile(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "rig")
	tree := tarStream(t,
		tarEntry{h: tar.Header{Name: "f", Mode: 0o644, Typeflag: tar.TypeReg}, body: "x"},
		tarEntry{h: tar.Header{Name: "l", Mode: 0o777, Linkname: "f/x", Typeflag: tar.TypeSymlink}},
	)
	if err := Unpack(dir, tree); err != nil {
		t.Fatalf("Unpack: %v", err)
	}
	if target, err := os.Readlink(filepath.Join(dir, "l")); err != nil || target != "f/x" {
		t.Errorf("symlink: got %q, %v", target, err)
	}
}

func TestUnpackRefusesASymlinkCycle(t *testing.T) {
	t.Parallel()
	tree := tarStream(t,
		tarEntry{h: tar.Header{Name: "a", Mode: 0o777, Linkname: "b", Typeflag: tar.TypeSymlink}},
		tarEntry{h: tar.Header{Name: "b", Mode: 0o777, Linkname: "a", Typeflag: tar.TypeSymlink}},
	)
	err := Unpack(filepath.Join(t.TempDir(), "rig"), tree)
	if err == nil || !strings.Contains(err.Error(), "too many levels of symbolic links") {
		t.Errorf("Unpack of a link cycle: got %v, want an error naming the cycle", err)
	}
}

// A file entry whose parent is a symlink is refused even when the link stays
// inside the seed: git never puts a path under a symlink, so such a tree is not
// one a seed holds, and the write would land wherever the link points.
func TestUnpackRefusesAnEntryUnderASymlink(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "rig")
	tree := tarStream(t,
		tarEntry{h: tar.Header{Name: "real", Mode: 0o755, Typeflag: tar.TypeDir}},
		tarEntry{h: tar.Header{Name: "sub", Mode: 0o777, Linkname: "real", Typeflag: tar.TypeSymlink}},
		tarEntry{h: tar.Header{Name: "sub/x", Mode: 0o644, Typeflag: tar.TypeReg}, body: "x"},
	)
	err := Unpack(dir, tree)
	if err == nil {
		t.Fatal("Unpack wrote an entry through a symlink")
	}
	if !strings.Contains(err.Error(), `archive entry "sub/x" writes through symlink "sub"`) {
		t.Errorf("Unpack: error does not name the link the entry writes through: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "real", "x")); !os.IsNotExist(err) {
		t.Errorf("the entry was written through the link: %v", err)
	}
}

// A file written through a link that leaves the seed lands outside it, so the
// write is refused before it happens; the target directory exists here, which
// leaves only the guard as the thing that stops it.
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
	err := Unpack(filepath.Join(parent, "rig"), tree)
	if err == nil {
		t.Fatal("Unpack accepted a tar that writes through an escaping symlink")
	}
	if !strings.Contains(err.Error(), `archive entry "a/x" writes through symlink "a"`) {
		t.Errorf("Unpack: error does not name the link the entry writes through: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "x")); !os.IsNotExist(err) {
		t.Errorf("a file was written outside the seed: %v", err)
	}
}

// gitCall is one call the fake received: the directory git ran in and its argv.
type gitCall struct {
	dir  string
	args []string
}

// gitDirFlag returns the value of the --git-dir flag in args and whether args
// carries one.
func gitDirFlag(args []string) (string, bool) {
	for _, a := range args {
		if v, ok := strings.CutPrefix(a, "--git-dir="); ok {
			return v, true
		}
	}
	return "", false
}

// withoutFlags returns args with every --flag removed: the verb and its operands.
func withoutFlags(args []string) []string {
	var out []string
	for _, a := range args {
		if !strings.HasPrefix(a, "--") {
			out = append(out, a)
		}
	}
	return out
}

// checkArchiveCall asserts one call Stage made: it ran in dir, carries
// --git-dir=gitDir when gitDir is set and no --git-dir flag otherwise, and its
// verb archives commit. Flags are matched by name, not position.
func checkArchiveCall(t *testing.T, c gitCall, dir, gitDir, commit string) {
	t.Helper()
	if c.dir != dir {
		t.Errorf("git ran in %q, want %q", c.dir, dir)
	}
	got, has := gitDirFlag(c.args)
	switch {
	case gitDir == "" && has:
		t.Errorf("git ran with --git-dir=%s, want the plain checkout form", got)
	case gitDir != "" && (!has || got != gitDir):
		t.Errorf("git --git-dir = %q (present %v), want %q", got, has, gitDir)
	}
	if !slices.Contains(c.args, "--format=tar") {
		t.Errorf("git args = %v, want --format=tar", c.args)
	}
	if want := []string{"archive", commit}; !slices.Equal(withoutFlags(c.args), want) {
		t.Errorf("git verb and operands = %v, want %v", withoutFlags(c.args), want)
	}
}

// fakeGit answers the one git command Stage issues with the tree recorded for
// the commit: a tar stream of the file paths and contents given.
type fakeGit struct {
	t     *testing.T
	trees map[string]map[string]string // commit -> path -> content
	calls []gitCall
}

func (g *fakeGit) archives(dir string, args ...string) ([]byte, error) {
	g.t.Helper()
	g.calls = append(g.calls, gitCall{dir: dir, args: args})
	operands := withoutFlags(args)
	if len(operands) != 2 || operands[0] != "archive" || !slices.Contains(args, "--format=tar") {
		g.t.Fatalf("unexpected git call: %s %v", dir, args)
	}
	files, ok := g.trees[operands[1]]
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

package seed

import (
	"fmt"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const (
	pinSHA  = "1111111111111111111111111111111111111111"
	mainSHA = "2222222222222222222222222222222222222222"
)

// guardGit answers the git calls the guard makes from a table keyed by the
// repository directory and the arguments; an unknown call is an error. Its town
// is a temporary directory, and bare creates each rig's bare repository there so
// the guard finds the on-disk shape it checks before it runs git.
type guardGit struct {
	t      *testing.T
	town   string
	calls  []string
	answer map[string]string
}

func newGuardGit(t *testing.T) *guardGit {
	t.Helper()
	return &guardGit{t: t, town: t.TempDir(), answer: map[string]string{}}
}

// bare makes the rig's bare repository under the town and returns its path.
func (f *guardGit) bare(rig string) string {
	return bareFixture(f.t, f.town, rig)
}

func (f *guardGit) run(dir string, args ...string) ([]byte, error) {
	key := dir + " " + strings.Join(args, " ")
	f.calls = append(f.calls, key)
	if out, ok := f.answer[key]; ok {
		return []byte(out), nil
	}
	return nil, fmt.Errorf("no answer for %q", key)
}

// fixtureVersion is a version whose major matches the module path, as modfile
// requires: github.com/x/a v1.0.0, github.com/jackc/pgx/v5 v5.0.0.
func fixtureVersion(module string) string {
	if i := strings.LastIndex(module, "/v"); i >= 0 {
		if _, err := fmt.Sscanf(module[i+2:], "%d", new(int)); err == nil {
			return "v" + module[i+2:] + ".0.0"
		}
	}
	return "v1.0.0"
}

func goMod(module string, requires ...string) string {
	s := "module " + module + "\n\ngo 1.21\n\nrequire (\n"
	for _, r := range requires {
		s += "\t" + r + " " + fixtureVersion(r) + "\n"
	}
	return s + ")\n"
}

// mainRevParse is the argv Guard sends to read a bare repo's main: the bare
// repo named by --git-dir, then the rev-parse.
func mainRevParse(dir string) string {
	return "--git-dir=" + dir + " rev-parse --verify main^{commit}"
}

// seedRig fills the table for one rig: its main, and the go.mod files at the pin
// and at main, each listed by ls-tree and read by show.
func seedRig(f *guardGit, rig, main string, files map[string]map[string]string) {
	seedRigAt(f, f.bare(rig), main, files)
}

// seedRigAt is seedRig for a repository at an arbitrary directory, such as a
// build source that is not under the town.
func seedRigAt(f *guardGit, dir, main string, files map[string]map[string]string) {
	f.answer[dir+" "+mainRevParse(dir)] = main + "\n"
	for commit, mods := range files {
		var names []string
		for name, body := range mods {
			names = append(names, name)
			f.answer[dir+" show "+commit+":"+name] = body
		}
		f.answer[dir+" ls-tree -r --name-only "+commit] = strings.Join(names, "\n") + "\n"
	}
}

func TestGuardPassesWhenThePinIsMain(t *testing.T) {
	t.Parallel()
	f := newGuardGit(t)
	seedRig(f, "devops", mainSHA, map[string]map[string]string{
		mainSHA: {"go.mod": goMod("example/devops", "github.com/x/a")},
	})
	pins := []Source{{Rig: "devops", Commit: mainSHA}}
	if _, err := Guard(pins, "gastown", f.town, f.run); err != nil {
		t.Fatalf("Guard on a pin equal to main = %v, want nil", err)
	}
}

func TestGuardPassesABehindPinWithTheSameModulesWithANote(t *testing.T) {
	t.Parallel()
	f := newGuardGit(t)
	seedRig(f, "devops", mainSHA, map[string]map[string]string{
		pinSHA:  {"go.mod": goMod("example/devops", "github.com/x/a")},
		mainSHA: {"go.mod": goMod("example/devops", "github.com/x/a")},
	})
	f.answer[f.bare("devops")+" merge-base --is-ancestor "+pinSHA+" "+mainSHA] = ""
	f.answer[f.bare("devops")+" rev-list --count "+pinSHA+".."+mainSHA] = "14\n"
	notes, err := Guard([]Source{{Rig: "devops", Commit: pinSHA}}, "gastown", f.town, f.run)
	if err != nil {
		t.Fatalf("Guard on a behind pin with the same modules = %v, want nil", err)
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "no module coverage gap") || !strings.Contains(notes[0], "14 commits behind") {
		t.Errorf("notes = %v, want one note naming the rig, the pin, the commits behind and no coverage gap", notes)
	}
}

func TestGuardFailsWhenABehindPinLacksAModuleMainRequires(t *testing.T) {
	t.Parallel()
	f := newGuardGit(t)
	seedRig(f, "devops", mainSHA, map[string]map[string]string{
		pinSHA:  {"go.mod": goMod("example/devops", "github.com/x/a")},
		mainSHA: {"go.mod": goMod("example/devops", "github.com/x/a", "github.com/x/new")},
	})
	f.answer[f.bare("devops")+" merge-base --is-ancestor "+pinSHA+" "+mainSHA] = ""
	_, err := Guard([]Source{{Rig: "devops", Commit: pinSHA}}, "gastown", f.town, f.run)
	if err == nil {
		t.Fatal("Guard on a pin lacking a module main requires = nil, want an error")
	}
	// The whole sentence is pinned: the refusal names the bare repo main is
	// read from, so an operator sees which .repo.git the advice refers to.
	want := fmt.Sprintf("pin-drift guard: rig devops pin %s lacks modules its main %s requires: github.com/x/new (in .); main is read from the .repo.git bare repo %s; run make polecat-seed-pins, then commit the pin",
		short(pinSHA), short(mainSHA), f.bare("devops"))
	if got := err.Error(); got != want {
		t.Errorf("refusal = %q, want %q", got, want)
	}
}

// Guard reads a rig's main with --git-dir naming the rig's bare repo, so the
// read needs no working tree.
func TestGuardReadsMainThroughTheBareRepoGitDir(t *testing.T) {
	t.Parallel()
	f := newGuardGit(t)
	seedRig(f, "devops", mainSHA, map[string]map[string]string{
		mainSHA: {"go.mod": goMod("example/devops")},
	})
	bare := filepath.Join(f.town, "devops", ".repo.git")
	var mainReads []gitCall
	record := func(dir string, args ...string) ([]byte, error) {
		if verb := withoutFlags(args); len(verb) > 0 && verb[0] == "rev-parse" {
			mainReads = append(mainReads, gitCall{dir: dir, args: args})
		}
		return f.run(dir, args...)
	}
	if _, err := Guard([]Source{{Rig: "devops", Commit: mainSHA}}, "gastown", f.town, record); err != nil {
		t.Fatalf("Guard = %v, want nil", err)
	}
	if len(mainReads) != 1 {
		t.Fatalf("main reads = %v, want one rev-parse", mainReads)
	}
	if got, has := gitDirFlag(mainReads[0].args); !has || got != bare {
		t.Errorf("main read --git-dir = %q (present %v), want %q", got, has, bare)
	}
	if mainReads[0].dir != bare {
		t.Errorf("main read ran in %q, want %q", mainReads[0].dir, bare)
	}
	if !slices.Contains(mainReads[0].args, "--verify") {
		t.Errorf("main read args = %v, want --verify", mainReads[0].args)
	}
	if want := []string{"rev-parse", "main^{commit}"}; !slices.Equal(withoutFlags(mainReads[0].args), want) {
		t.Errorf("main read verb and operands = %v, want %v", withoutFlags(mainReads[0].args), want)
	}
}

func TestGuardNamesANestedModuleThePinLacks(t *testing.T) {
	t.Parallel()
	f := newGuardGit(t)
	seedRig(f, "gastown", mainSHA, map[string]map[string]string{
		pinSHA: {
			"go.mod":         goMod("example/root", "github.com/x/a"),
			"tracker/go.mod": goMod("example/tracker", "github.com/x/a"),
		},
		mainSHA: {
			"go.mod":         goMod("example/root", "github.com/x/a"),
			"tracker/go.mod": goMod("example/tracker", "github.com/x/a", "github.com/jackc/pgx/v5"),
		},
	})
	f.answer[f.bare("gastown")+" merge-base --is-ancestor "+pinSHA+" "+mainSHA] = ""
	pins := []Source{{Rig: "gastown", Commit: pinSHA}}
	_, err := Guard(pins, "devops", f.town, f.run)
	if err == nil {
		t.Fatal("Guard on a pin that lacks a nested module = nil, want an error")
	}
	want := "github.com/jackc/pgx/v5 (in tracker)"
	if !strings.Contains(err.Error(), want) {
		t.Errorf("error %q does not name the missing nested module %q", err, want)
	}
}

func TestGuardSkipsTheBuildRig(t *testing.T) {
	t.Parallel()
	f := newGuardGit(t)
	pins := []Source{{Rig: "gastown", Commit: pinSHA}}
	if _, err := Guard(pins, "gastown", f.town, f.run); err != nil {
		t.Fatalf("Guard on the build rig = %v, want nil", err)
	}
	if len(f.calls) != 0 {
		t.Errorf("Guard read git for the build rig: %v", f.calls)
	}
}

// A rig with no .repo.git is refused before git runs, and the error names the
// bare path the guard would have read.
func TestGuardRefusesARigWithNoBareRepo(t *testing.T) {
	t.Parallel()
	f := newGuardGit(t)
	pins := []Source{{Rig: "devops", Commit: pinSHA}}
	_, err := Guard(pins, "gastown", f.town, f.run)
	if err == nil {
		t.Fatal("Guard on a rig with no bare repo = nil, want an error")
	}
	if want := path.Join(f.town, "devops", ".repo.git"); !strings.Contains(err.Error(), want) {
		t.Errorf("error %q does not name the bare path %q", err, want)
	}
	if len(f.calls) != 0 {
		t.Errorf("Guard ran git against a rig with no bare repo: %v", f.calls)
	}
}

func TestGuardFailsWhenMainCannotBeRead(t *testing.T) {
	t.Parallel()
	f := newGuardGit(t)
	f.bare("devops")
	pins := []Source{{Rig: "devops", Commit: pinSHA}}
	_, err := Guard(pins, "gastown", f.town, f.run)
	if err == nil {
		t.Fatal("Guard with no main = nil, want an error naming the rig")
	}
	if !strings.Contains(err.Error(), "devops") || !strings.Contains(err.Error(), "main^{commit}") {
		t.Errorf("error %q does not name the rig and main", err)
	}
}

func TestGuardPassesAnAheadOrDivergedPinWithEqualModulesWithANote(t *testing.T) {
	t.Parallel()
	f := newGuardGit(t)
	seedRig(f, "devops", mainSHA, map[string]map[string]string{
		pinSHA:  {"go.mod": goMod("example/devops")},
		mainSHA: {"go.mod": goMod("example/devops")},
	})
	// no merge-base answer: the pin is not an ancestor of main
	notes, err := Guard([]Source{{Rig: "devops", Commit: pinSHA}}, "gastown", f.town, f.run)
	if err != nil {
		t.Fatalf("Guard on an ahead or diverged pin with equal modules = %v, want nil", err)
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "ahead or diverged") {
		t.Errorf("notes = %v, want one note saying ahead or diverged", notes)
	}
}

func TestGuardNamesAPinThatIsNotInItsBareRepo(t *testing.T) {
	t.Parallel()
	f := newGuardGit(t)
	dir := f.bare("devops")
	f.answer[dir+" "+mainRevParse(dir)] = mainSHA + "\n"
	// no ls-tree answer for the pin: the commit is not in this repository
	_, err := Guard([]Source{{Rig: "devops", Commit: pinSHA}}, "gastown", f.town, f.run)
	if err == nil {
		t.Fatal("Guard on a pin the bare repo lacks = nil, want an error")
	}
	// The whole sentence is pinned: it names the .repo.git path the pin was
	// read from (gt-gg9hv.33).
	want := fmt.Sprintf("pin-drift guard: rig devops pin %s is not readable in the .repo.git bare repo %s, so it cannot be compared with main %s (no answer for %q): fetch or fix that repository, then rerun",
		short(pinSHA), dir, short(mainSHA), dir+" ls-tree -r --name-only "+pinSHA)
	if got := err.Error(); got != want {
		t.Errorf("refusal = %q, want %q", got, want)
	}
}

func TestGuardReadsOnlyGitQueries(t *testing.T) {
	t.Parallel()
	f := newGuardGit(t)
	seedRig(f, "devops", mainSHA, map[string]map[string]string{
		mainSHA: {"go.mod": goMod("example/devops")},
	})
	_, _ = Guard([]Source{{Rig: "devops", Commit: mainSHA}}, "gastown", f.town, f.run)
	allowed := map[string]bool{}
	for _, v := range ReadOnlyGitVerbs {
		allowed[v] = true
	}
	for _, c := range f.calls {
		// c is "<dir> <args>..."; the verb is the first argument that is not a flag
		args := withoutFlags(strings.Fields(c)[1:])
		if len(args) == 0 || !allowed[args[0]] {
			t.Errorf("Guard ran %q; only read-only queries are allowed", c)
		}
	}
	if len(f.calls) == 0 {
		t.Error("Guard made no git calls; the check did not run")
	}
}

// When the commit count cannot be read, the note still says the pin is behind
// its main, without the count.
func TestGuardNotesABehindPinWithoutACountWhenTheCountCannotBeRead(t *testing.T) {
	t.Parallel()
	f := newGuardGit(t)
	seedRig(f, "devops", mainSHA, map[string]map[string]string{
		pinSHA:  {"go.mod": goMod("example/devops", "github.com/x/a")},
		mainSHA: {"go.mod": goMod("example/devops", "github.com/x/a")},
	})
	f.answer[f.bare("devops")+" merge-base --is-ancestor "+pinSHA+" "+mainSHA] = ""
	// no rev-list answer: the count cannot be read
	notes, err := Guard([]Source{{Rig: "devops", Commit: pinSHA}}, "gastown", f.town, f.run)
	if err != nil {
		t.Fatalf("Guard = %v, want nil", err)
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "behind its main") || strings.Contains(notes[0], "commits behind") {
		t.Errorf("notes = %v, want one note saying behind its main with no count", notes)
	}
}

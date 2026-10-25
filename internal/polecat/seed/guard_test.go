package seed

import (
	"fmt"
	"path"
	"strings"
	"testing"
)

const (
	pinSHA  = "1111111111111111111111111111111111111111"
	mainSHA = "2222222222222222222222222222222222222222"
	town    = "/town"
)

// guardGit answers the three git calls the guard makes from a table keyed by
// the rig directory and the arguments; an unknown call is an error.
type guardGit struct {
	calls  []string
	answer map[string]string
}

func (f *guardGit) run(dir string, args ...string) ([]byte, error) {
	key := dir + " " + strings.Join(args, " ")
	f.calls = append(f.calls, key)
	if out, ok := f.answer[key]; ok {
		return []byte(out), nil
	}
	return nil, fmt.Errorf("no answer for %q", key)
}

func rigDir(rig string) string { return path.Join(town, rig, "mayor", "rig") }

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

// seedRig fills the table for one rig: its origin/main, and the go.mod files at
// the pin and at main, each listed by ls-tree and read by show.
func seedRig(f *guardGit, rig, main string, files map[string]map[string]string) {
	dir := rigDir(rig)
	f.answer[dir+" rev-parse --verify origin/main^{commit}"] = main + "\n"
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
	f := &guardGit{answer: map[string]string{}}
	seedRig(f, "devops", mainSHA, map[string]map[string]string{
		mainSHA: {"go.mod": goMod("example/devops", "github.com/x/a")},
	})
	pins := []Source{{Rig: "devops", Commit: mainSHA}}
	if err := Guard(pins, "gastown", town, f.run); err != nil {
		t.Fatalf("Guard on a pin equal to main = %v, want nil", err)
	}
}

func TestGuardFailsNamingTheRigPinAndMainWhenThePinIsBehind(t *testing.T) {
	t.Parallel()
	f := &guardGit{answer: map[string]string{}}
	seedRig(f, "devops", mainSHA, map[string]map[string]string{
		pinSHA:  {"go.mod": goMod("example/devops", "github.com/x/a")},
		mainSHA: {"go.mod": goMod("example/devops", "github.com/x/a")},
	})
	pins := []Source{{Rig: "devops", Commit: pinSHA}}
	err := Guard(pins, "gastown", town, f.run)
	if err == nil {
		t.Fatal("Guard on a pin behind main = nil, want an error")
	}
	for _, want := range []string{"devops", short(pinSHA), short(mainSHA), "make polecat-seed-pins"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
}

func TestGuardNamesANestedModuleThePinLacks(t *testing.T) {
	t.Parallel()
	f := &guardGit{answer: map[string]string{}}
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
	pins := []Source{{Rig: "gastown", Commit: pinSHA}}
	err := Guard(pins, "devops", town, f.run)
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
	f := &guardGit{answer: map[string]string{}}
	pins := []Source{{Rig: "gastown", Commit: pinSHA}}
	if err := Guard(pins, "gastown", town, f.run); err != nil {
		t.Fatalf("Guard on the build rig = %v, want nil", err)
	}
	if len(f.calls) != 0 {
		t.Errorf("Guard read git for the build rig: %v", f.calls)
	}
}

func TestGuardFailsWhenMainCannotBeRead(t *testing.T) {
	t.Parallel()
	f := &guardGit{answer: map[string]string{}}
	pins := []Source{{Rig: "devops", Commit: pinSHA}}
	err := Guard(pins, "gastown", town, f.run)
	if err == nil {
		t.Fatal("Guard with no origin/main = nil, want an error naming the rig")
	}
	if !strings.Contains(err.Error(), "devops") || !strings.Contains(err.Error(), "origin/main") {
		t.Errorf("error %q does not name the rig and origin/main", err)
	}
}

func TestGuardNeverFetches(t *testing.T) {
	t.Parallel()
	f := &guardGit{answer: map[string]string{}}
	seedRig(f, "devops", mainSHA, map[string]map[string]string{
		mainSHA: {"go.mod": goMod("example/devops")},
	})
	_ = Guard([]Source{{Rig: "devops", Commit: mainSHA}}, "gastown", town, f.run)
	for _, c := range f.calls {
		if strings.Contains(c, " fetch") {
			t.Errorf("Guard ran %q; it must read only", c)
		}
	}
}

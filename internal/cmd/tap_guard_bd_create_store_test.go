package cmd

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// bdStoreTown is a town on disk laid out as a host or sandbox has it: the
// town's own .beads, a rig whose shared store is mayor/rig/.beads, and a
// polecat worktree whose .beads/redirect points at that store.
type bdStoreTown struct {
	town, rigDir, worktree string
	// home is the fake hook process's HOME, a directory outside the town whose
	// .beads is a symlink to the town's. A ~ row then tests expansion from a
	// named HOME rather than from the town root, so its verdict does not ride
	// on the fixture happening to set HOME to the town (gt-utked).
	home string
	// storeLink is a symlink, outside the town, to the town's .beads.
	storeLink string
}

func newBdStoreTown(t *testing.T) bdStoreTown {
	t.Helper()
	town := t.TempDir()
	rigDir := filepath.Join(town, "gastown")
	worktree := filepath.Join(rigDir, "polecats", "ruby", "gastown")
	for _, dir := range []string{
		filepath.Join(town, "mayor"),
		filepath.Join(town, ".beads"),
		filepath.Join(rigDir, "mayor", "rig", ".beads"),
		filepath.Join(worktree, ".beads"),
		filepath.Join(worktree, "internal"),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string]string{
		filepath.Join(town, "mayor", "town.json"):     "{}",
		filepath.Join(rigDir, ".beads", "redirect"):   "mayor/rig/.beads\n",
		filepath.Join(worktree, ".beads", "redirect"): "../../../mayor/rig/.beads\n",
	}
	for path, body := range files {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	storeLink := filepath.Join(t.TempDir(), "store")
	if err := os.Symlink(filepath.Join(town, ".beads"), storeLink); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(t.TempDir(), "home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(town, ".beads"), filepath.Join(home, ".beads")); err != nil {
		t.Fatal(err)
	}
	return bdStoreTown{town: town, rigDir: rigDir, worktree: worktree, home: home, storeLink: storeLink}
}

const polecatRole = "gastown/polecats/ruby"

func (b bdStoreTown) run(t *testing.T, role, cwd, command string) (string, error) {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"tool_name":  "Bash",
		"cwd":        cwd,
		"tool_input": map[string]string{"command": command},
	})
	if err != nil {
		t.Fatal(err)
	}
	proc := fakeGuardProcess(map[string]string{"GT_ROLE": role, "HOME": b.home}, "")
	var stderr strings.Builder
	err = tapGuardBdCreateStore(strings.NewReader(string(payload)), &stderr, proc)
	return stderr.String(), err
}

func TestTapGuardBdCreateStore(t *testing.T) {
	t.Parallel()
	b := newBdStoreTown(t)
	tests := []struct {
		name    string
		role    string
		cwd     string
		command string
		blocked bool
	}{
		{"worktree create", polecatRole, b.worktree, `bd create --title=x`, false},
		{"worktree subdir create", polecatRole, filepath.Join(b.worktree, "internal"), `bd create --title=x`, false},
		{"rig dir create (rig redirect)", polecatRole, b.rigDir, `bd create --title=x`, false},
		{"town root create", polecatRole, b.town, `bd create --title=x`, true},
		{"town root bd path", polecatRole, b.town, `/usr/local/bin/bd create --title=x`, true},
		{"town root bd new alias", polecatRole, b.town, `bd new --title=x`, true},
		{"town root bd q", polecatRole, b.town, `bd q "x"`, true},
		{"town root after a global flag", polecatRole, b.town, `bd --db x create --title=x`, true},
		{"cd town then create", polecatRole, b.worktree, `cd ` + b.town + ` && bd create --title=x`, true},
		{"relative cd up to town", polecatRole, b.worktree, `cd ../../../.. && bd create --title=x`, true},
		{"cd to worktree then create from town", polecatRole, b.town, `cd ` + b.worktree + ` && bd create --title=x`, false},
		{"cd to rig dir then create", polecatRole, b.town, `cd ` + b.rigDir + ` && bd create --title=x`, false},
		// bd walks up from its directory to the nearest .beads, so a town
		// subdirectory with none of its own files into the town store.
		{"cd to town subdir without .beads then create", polecatRole, b.worktree, `cd ` + filepath.Join(b.town, "mayor") + ` && bd create --title=x`, true},
		{"town subdir without .beads, as the cwd", polecatRole, filepath.Join(b.town, "mayor"), `bd create --title=x`, true},
		{"worktree subdir without .beads, as the cwd", polecatRole, filepath.Join(b.worktree, "internal"), `bd create --title=x`, false},
		// bd -C/--directory moves the directory the store is resolved from,
		// before or after the verb, alone or glued — and a relative one is
		// resolved against the shell's directory, as bd resolves it.
		{"town via -C before the verb", polecatRole, b.worktree, `bd -C ` + b.town + ` create --title=x`, true},
		{"town via -C after the verb", polecatRole, b.worktree, `bd create --title=x -C ` + b.town, true},
		{"town via glued -C", polecatRole, b.worktree, `bd -C` + b.town + ` create --title=x`, true},
		{"town via --directory", polecatRole, b.worktree, `bd --directory ` + b.town + ` create --title=x`, true},
		{"town via -C=", polecatRole, b.worktree, `bd -C=` + b.town + ` create --title=x`, true},
		{"town via --directory=", polecatRole, b.worktree, `bd --directory=` + b.town + ` create --title=x`, true},
		{"town via -C home", polecatRole, b.worktree, `bd -C ~ create --title=x`, true},
		{"relative -C up to town", polecatRole, b.worktree, `bd -C ../../../.. create --title=x`, true},
		{"rig dir via -C from the town root", polecatRole, b.town, `bd -C ` + b.rigDir + ` create --title=x`, false},
		{"worktree via -C from the town root", polecatRole, b.town, `bd -C ` + b.worktree + ` create --title=x`, false},
		{"unresolvable -C then create", polecatRole, b.worktree, `bd -C "$NOPE" create --title=x`, true},
		{"-C with no value", polecatRole, b.worktree, `bd create --title=x -C`, true},
		// A token that starts with -C but is another flag's value is not a
		// directory flag; the value flag before it consumes it.
		{"-C as a --title value", polecatRole, b.worktree, `bd create --title -C ` + b.town, false},
		{"glued -C as a --title value", polecatRole, b.worktree, `bd create --title "-C` + b.town + `"`, false},
		{"-C-led --description value", polecatRole, b.worktree, `bd create --title=x --description "-C ` + b.town + `"`, false},
		{"-C as a --labels value", polecatRole, b.worktree, `bd create --title=x -l -C` + b.town, false},
		{"-C as a --db value", polecatRole, b.worktree, `bd create --title=x --db -C` + b.town, false},
		{"-C after a --title value", polecatRole, b.worktree, `bd create --title x -C ` + b.town, true},
		{"-C after a -C-valued --title", polecatRole, b.worktree, `bd create --title -C -C ` + b.town, true},
		{"--repo after a -C-valued --title", polecatRole, b.worktree, `bd create --title -C --repo ` + b.town, true},
		{"-C after a boolean flag", polecatRole, b.worktree, `bd create --silent -C ` + b.town + ` --title=x`, true},
		{"value flag at the end of the line", polecatRole, b.worktree, `bd create --title=x --description`, false},
		// bd create --repo names the workspace the bead is filed into, a
		// relative one resolved against the directory bd runs in.
		{"town via --repo", polecatRole, b.worktree, `bd create --title=x --repo ` + b.town, true},
		{"town via --repo=", polecatRole, b.worktree, `bd create --title=x --repo=` + b.town, true},
		// ~ is expanded from the hook process's HOME, which the fixture names:
		// a directory outside the town whose .beads is the town's store. A guard
		// that read "~" as a literal path name would allow the create.
		{"town via --repo home", polecatRole, b.worktree, `bd create --title=x --repo ~`, true},
		{"relative --repo up to town", polecatRole, b.worktree, `bd create --title=x --repo ../../../..`, true},
		{"relative --repo up from the rig dir", polecatRole, b.rigDir, `bd create --title=x --repo ..`, true},
		{"town via --repo after -C", polecatRole, b.worktree, `bd -C ` + b.rigDir + ` create --title=x --repo ..`, true},
		{"rig dir via --repo", polecatRole, b.worktree, `bd create --title=x --repo ` + b.rigDir, false},
		{"worktree via --repo from the town root", polecatRole, b.town, `bd create --title=x --repo ` + b.worktree, false},
		{"rig dir via --repo after -C from the town root", polecatRole, b.town, `bd -C ` + b.rigDir + ` create --title=x --repo .`, false},
		{"unresolvable --repo", polecatRole, b.worktree, `bd create --title=x --repo "$NOPE"`, true},
		{"--repo with no value", polecatRole, b.worktree, `bd create --title=x --repo`, true},
		{"--repo rig dir outranks BEADS_DIR town", polecatRole, b.worktree, `BEADS_DIR=` + filepath.Join(b.town, ".beads") + ` bd create --title=x --repo ` + b.rigDir, false},
		{"--repo town outranks BEADS_DIR rig", polecatRole, b.worktree, `BEADS_DIR=` + filepath.Join(b.rigDir, "mayor", "rig", ".beads") + ` bd create --title=x --repo ` + b.town, true},
		// bd --db names the store bd opens, and it opens it even when --repo is
		// given, so --db is judged beside --repo rather than yielding to it.
		{"--repo rig dir with --db rig store", polecatRole, b.worktree, `bd --db ` + filepath.Join(b.rigDir, "mayor", "rig", ".beads", "x.db") + ` create --title=x --repo ` + b.rigDir, false},
		{"--repo rig dir with --db town", polecatRole, b.worktree, `bd --db ` + filepath.Join(b.town, ".beads", "x.db") + ` create --title=x --repo ` + b.rigDir, true},
		{"--repo rig dir with unplaceable --db", polecatRole, b.worktree, `bd create --title=x --repo ` + b.rigDir + ` --db "$NOPE"`, true},
		{"--repo town with --db rig store", polecatRole, b.worktree, `bd --db ` + filepath.Join(b.rigDir, "mayor", "rig", ".beads", "x.db") + ` create --title=x --repo ` + b.town, true},
		{"--repo town inside bash -c", polecatRole, b.worktree, `bash -c "bd create --title=x --repo ` + b.town + `"`, true},
		// bd --db maps to the nearest .beads directory at or above the
		// path's parent, whether or not the path exists.
		{"town via --db file in .beads", polecatRole, b.worktree, `bd --db ` + filepath.Join(b.town, ".beads", "x.db") + ` create --title=x`, true},
		{"town via --db after the verb", polecatRole, b.worktree, `bd create --title=x --db ` + filepath.Join(b.town, ".beads", "x.db"), true},
		{"town via --db=", polecatRole, b.worktree, `bd --db=` + filepath.Join(b.town, ".beads", "x.db") + ` create --title=x`, true},
		{"town via --db .beads directory", polecatRole, b.worktree, `bd --db ` + filepath.Join(b.town, ".beads") + ` create --title=x`, true},
		{"town via --db dolt directory", polecatRole, b.worktree, `bd --db ` + filepath.Join(b.town, ".beads", "dolt") + ` create --title=x`, true},
		{"town via --db name beside the town .beads", polecatRole, b.worktree, `bd --db ` + filepath.Join(b.town, "nothing.db") + ` create --title=x`, true},
		{"town via --db under a town subdirectory", polecatRole, b.worktree, `bd --db ` + filepath.Join(b.town, "mayor", "x.db") + ` create --title=x`, true},
		{"town via relative --db", polecatRole, b.worktree, `bd --db ../../../../.beads/x.db create --title=x`, true},
		{"town via --db through a symlink", polecatRole, b.worktree, `bd --db ` + filepath.Join(b.storeLink, "x.db") + ` create --title=x`, true},
		{"town via --db after -C", polecatRole, b.worktree, `bd -C ` + b.rigDir + ` --db ../.beads/x.db create --title=x`, true},
		{"unresolvable --db", polecatRole, b.worktree, `bd --db "$NOPE" create --title=x`, true},
		{"--db with no value", polecatRole, b.worktree, `bd create --title=x --db`, true},
		{"worktree via --db file in .beads", polecatRole, b.worktree, `bd --db ` + filepath.Join(b.worktree, ".beads", "x.db") + ` create --title=x`, false},
		{"rig store via --db", polecatRole, b.town, `bd --db ` + filepath.Join(b.rigDir, "mayor", "rig", ".beads", "x.db") + ` create --title=x`, false},
		{"--db outranks BEADS_DIR", polecatRole, b.town, `BEADS_DIR=` + filepath.Join(b.town, ".beads") + ` bd --db ` + filepath.Join(b.rigDir, "mayor", "rig", ".beads", "x.db") + ` create --title=x`, false},
		// BEADS_DB and BD_DB select a database as --db does, as an inline
		// assignment here; --db and --repo outrank them.
		{"town via inline BEADS_DB", polecatRole, b.worktree, `BEADS_DB=` + filepath.Join(b.town, ".beads", "x.db") + ` bd create --title=x`, true},
		{"town via inline BD_DB", polecatRole, b.worktree, `BD_DB=` + filepath.Join(b.town, ".beads", "x.db") + ` bd create --title=x`, true},
		{"rig store via inline BEADS_DB", polecatRole, b.worktree, `BEADS_DB=` + filepath.Join(b.rigDir, "mayor", "rig", ".beads", "x.db") + ` bd create --title=x`, false},
		{"rig store via inline BD_DB", polecatRole, b.worktree, `BD_DB=` + filepath.Join(b.rigDir, "mayor", "rig", ".beads", "x.db") + ` bd create --title=x`, false},
		{"rig store via BEADS_DB at the town root", polecatRole, b.town, `BEADS_DB=` + filepath.Join(b.rigDir, "mayor", "rig", ".beads", "x.db") + ` bd create --title=x`, false},
		{"town via BEADS_DB after a then", polecatRole, b.worktree, `if true; then BEADS_DB=` + filepath.Join(b.town, ".beads", "x.db") + ` bd create --title=x; fi`, true},
		{"town via relative BEADS_DB", polecatRole, b.worktree, `BEADS_DB=../../../../.beads/x.db bd create --title=x`, true},
		{"town via BEADS_DB inside bash -c", polecatRole, b.worktree, `bash -c "BEADS_DB=` + filepath.Join(b.town, ".beads", "x.db") + ` bd create --title=x"`, true},
		{"BEADS_DB rig wins over BD_DB town", polecatRole, b.worktree, `BEADS_DB=` + filepath.Join(b.rigDir, "mayor", "rig", ".beads", "x.db") + ` BD_DB=` + filepath.Join(b.town, ".beads", "x.db") + ` bd create --title=x`, false},
		{"BD_DB read when BEADS_DB is empty", polecatRole, b.worktree, `BEADS_DB= BD_DB=` + filepath.Join(b.town, ".beads", "x.db") + ` bd create --title=x`, true},
		{"unresolvable BEADS_DB", polecatRole, b.worktree, `BEADS_DB="$NOPE" bd create --title=x`, true},
		{"--db rig outranks BEADS_DB town", polecatRole, b.worktree, `BEADS_DB=` + filepath.Join(b.town, ".beads", "x.db") + ` bd --db ` + filepath.Join(b.rigDir, "mayor", "rig", ".beads", "x.db") + ` create --title=x`, false},
		{"--repo rig outranks BD_DB town", polecatRole, b.worktree, `BD_DB=` + filepath.Join(b.town, ".beads", "x.db") + ` bd create --title=x --repo ` + b.rigDir, false},
		{"BEADS_DB rig with BEADS_DIR town", polecatRole, b.worktree, `BEADS_DB=` + filepath.Join(b.rigDir, "mayor", "rig", ".beads", "x.db") + ` BEADS_DIR=` + filepath.Join(b.town, ".beads") + ` bd create --title=x`, true},
		// Repeated assignments apply in order, so the last BEADS_DIR is the one
		// bd reads.
		{"last inline BEADS_DIR town", polecatRole, b.worktree, `BEADS_DIR=` + filepath.Join(b.rigDir, "mayor", "rig", ".beads") + ` BEADS_DIR=` + filepath.Join(b.town, ".beads") + ` bd create --title=x`, true},
		{"last inline BEADS_DIR rig", polecatRole, b.worktree, `BEADS_DIR=` + filepath.Join(b.town, ".beads") + ` BEADS_DIR=` + filepath.Join(b.rigDir, "mayor", "rig", ".beads") + ` bd create --title=x`, false},
		{"--global in the worktree", polecatRole, b.worktree, `bd --global create --title=x`, false},
		{"--repo on a bd list", polecatRole, b.worktree, `bd list --repo ` + b.town, false},
		{"cd town then -C worktree", polecatRole, b.worktree, `cd ` + b.town + ` && bd -C ` + b.worktree + ` create --title=x`, false},
		{"-C town inside bash -c", polecatRole, b.worktree, `bash -c "bd -C ` + b.town + ` create --title=x"`, true},
		{"cd town then back then create", polecatRole, b.worktree, `cd ` + b.town + `; cd ` + b.worktree + `; bd create --title=x`, false},
		// A cd in a subshell, pipeline or background job runs in a child
		// process, so the create after it files from the original directory.
		{"subshell cd then create", polecatRole, b.worktree, `(cd ` + b.town + `) && bd create --title=x`, false},
		{"pipeline cd then create", polecatRole, b.worktree, `cd ` + b.town + ` | cat; bd create --title=x`, false},
		{"background cd then create", polecatRole, b.worktree, `cd ` + b.town + ` & bd create --title=x`, false},
		{"failed cd list then create", polecatRole, b.worktree, `cd ` + filepath.Join(b.town, "nope") + ` && cd ` + b.town + `; bd create --title=x`, false},
		// A brace group is not a subshell: its cd is the shell's.
		{"brace group cd then create", polecatRole, b.worktree, `{ cd ` + b.town + ` ; } ; bd create --title=x`, true},
		{"create inside bash -c", polecatRole, b.town, `bash -c 'bd create --title=x'`, true},
		{"cd town inside bash -c", polecatRole, b.worktree, `bash -c "cd ` + b.town + ` && bd create --title=x"`, true},
		{"create in a command substitution", polecatRole, b.town, `echo $(bd create --title=x --silent)`, true},
		{"cd town inside a substitution", polecatRole, b.worktree, `echo $(cd ` + b.town + `; bd create --title=x)`, true},
		{"substitution cd does not leak", polecatRole, b.worktree, `echo $(cd ` + b.town + `); bd create --title=x`, false},
		{"bash -c cd does not leak", polecatRole, b.worktree, `bash -c "cd ` + b.town + `"; bd create --title=x`, false},
		// Past the nesting cap the guard refuses only a command whose nested
		// text may hold the invocation it judges; deep nesting of an unrelated
		// command is not a reason to block it.
		{"create past the nesting cap", polecatRole, b.town, `echo $(echo $(echo $(echo $(bd create --title=x))))`, true},
		{"unrelated command past the nesting cap", polecatRole, b.town, `echo $(echo $(echo $(echo $(echo hi))))`, false},
		{"create in an if", polecatRole, b.town, `if true; then bd create --title=x; fi`, true},
		{"unresolvable cd then create", polecatRole, b.worktree, `cd "$NOPE" && bd create --title=x`, true},
		{"unresolvable cd then no create", polecatRole, b.worktree, `cd "$NOPE" && bd list`, false},
		{"BEADS_DIR names the town store", polecatRole, b.worktree, `BEADS_DIR=` + filepath.Join(b.town, ".beads") + ` bd create --title=x`, true},
		{"BEADS_DIR names the rig store at the town root", polecatRole, b.town, `BEADS_DIR=` + filepath.Join(b.rigDir, "mayor", "rig", ".beads") + ` bd create --title=x`, false},
		{"relative BEADS_DIR at the town root", polecatRole, b.town, `BEADS_DIR=.beads bd create --title=x`, true},
		{"relative BEADS_DIR in the worktree", polecatRole, b.worktree, `BEADS_DIR=.beads bd create --title=x`, false},
		{"town root list", polecatRole, b.town, `bd list`, false},
		{"town root show", polecatRole, b.town, `bd show hq-abc`, false},
		{"town root comment", polecatRole, b.town, `bd comments add hq-abc "create"`, false},
		{"town root gt escalate", polecatRole, b.town, `gt escalate "bd create"`, false},
		{"crew at town root", "gastown/crew/alice", b.town, `bd create --title=x`, false},
		{"no role, town root", "", b.town, `bd create --title=x`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			stderr, err := b.run(t, tt.role, tt.cwd, tt.command)
			if !tt.blocked {
				if err != nil || stderr != "" {
					t.Fatalf("want allowed, got err=%v stderr=%q", err, stderr)
				}
				return
			}
			if err == nil {
				t.Fatalf("want blocked, got allowed")
			}
			if !strings.Contains(stderr, bdCreateStoreRefusal) {
				t.Errorf("stderr lacks the refusal line %q:\n%s", bdCreateStoreRefusal, stderr)
			}
		})
	}
}

// TestTapGuardBdCreateStoreProcessEnvDB pins BEADS_DB and BD_DB read from the
// process env, where the guard hook itself runs, and an inline assignment
// replacing the env's value for the same name.
func TestTapGuardBdCreateStoreProcessEnvDB(t *testing.T) {
	t.Parallel()
	b := newBdStoreTown(t)
	townDB := filepath.Join(b.town, ".beads", "x.db")
	rigDB := filepath.Join(b.rigDir, "mayor", "rig", ".beads", "x.db")
	tests := []struct {
		name    string
		env     map[string]string
		command string
		blocked bool
	}{
		{"BEADS_DB town", map[string]string{"BEADS_DB": townDB}, `bd create --title=x`, true},
		{"BD_DB town", map[string]string{"BD_DB": townDB}, `bd create --title=x`, true},
		{"BEADS_DB rig", map[string]string{"BEADS_DB": rigDB}, `bd create --title=x`, false},
		{"BD_DB rig", map[string]string{"BD_DB": rigDB}, `bd create --title=x`, false},
		{"BEADS_DB rig over BD_DB town", map[string]string{"BEADS_DB": rigDB, "BD_DB": townDB}, `bd create --title=x`, false},
		{"inline BEADS_DB rig replaces env town", map[string]string{"BEADS_DB": townDB}, `BEADS_DB=` + rigDB + ` bd create --title=x`, false},
		{"inline BEADS_DB town replaces env rig", map[string]string{"BEADS_DB": rigDB}, `BEADS_DB=` + townDB + ` bd create --title=x`, true},
		{"empty inline BEADS_DB unsets env town", map[string]string{"BEADS_DB": townDB}, `BEADS_DB= bd create --title=x`, false},
		{"--db rig outranks env BEADS_DB town", map[string]string{"BEADS_DB": townDB}, `bd --db ` + rigDB + ` create --title=x`, false},
		{"BEADS_DB town with BEADS_DIR rig", map[string]string{"BEADS_DB": townDB, "BEADS_DIR": filepath.Join(b.rigDir, "mayor", "rig", ".beads")}, `bd create --title=x`, true},
		{"BEADS_DB rig with BEADS_DIR town", map[string]string{"BEADS_DB": rigDB, "BEADS_DIR": filepath.Join(b.town, ".beads")}, `bd create --title=x`, true},
		{"BD_DB town with BEADS_DIR rig", map[string]string{"BD_DB": townDB, "BEADS_DIR": filepath.Join(b.rigDir, "mayor", "rig", ".beads")}, `bd create --title=x`, true},
		{"BD_DB rig with BEADS_DIR town", map[string]string{"BD_DB": rigDB, "BEADS_DIR": filepath.Join(b.town, ".beads")}, `bd create --title=x`, true},
		{"inline BEADS_DIR rig replaces env town", map[string]string{"BEADS_DIR": filepath.Join(b.town, ".beads")}, `BEADS_DIR=` + filepath.Join(b.rigDir, "mayor", "rig", ".beads") + ` bd create --title=x`, false},
		{"inline BEADS_DIR town replaces env rig", map[string]string{"BEADS_DIR": filepath.Join(b.rigDir, "mayor", "rig", ".beads")}, `BEADS_DIR=` + filepath.Join(b.town, ".beads") + ` bd create --title=x`, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			payload, err := json.Marshal(map[string]any{
				"tool_name":  "Bash",
				"cwd":        b.worktree,
				"tool_input": map[string]string{"command": tt.command},
			})
			if err != nil {
				t.Fatal(err)
			}
			env := map[string]string{"GT_ROLE": polecatRole, "HOME": b.town}
			for k, v := range tt.env {
				env[k] = v
			}
			var stderr strings.Builder
			err = tapGuardBdCreateStore(strings.NewReader(string(payload)), &stderr, fakeGuardProcess(env, ""))
			if !tt.blocked {
				if err != nil || stderr.Len() != 0 {
					t.Fatalf("want allowed, got err=%v stderr=%q", err, stderr.String())
				}
				return
			}
			if err == nil || !strings.Contains(stderr.String(), bdCreateStoreRefusal) {
				t.Fatalf("want blocked, got err=%v stderr=%q", err, stderr.String())
			}
		})
	}
}

// TestTapGuardBdCreateStoreDashCOutranksEnvDB pins that bd -C decides the
// store: bd sets BEADS_DIR from -C and reads it before BEADS_DB/BD_DB, so a
// rig-valued database variable, inline or in the process env, cannot make a
// -C town create look like a rig one.
func TestTapGuardBdCreateStoreDashCOutranksEnvDB(t *testing.T) {
	t.Parallel()
	b := newBdStoreTown(t)
	townDB := filepath.Join(b.town, ".beads", "x.db")
	rigDB := filepath.Join(b.rigDir, "mayor", "rig", ".beads", "x.db")
	rigBeads := filepath.Join(b.rigDir, "mayor", "rig", ".beads")
	tests := []struct {
		name    string
		env     map[string]string
		command string
		blocked bool
	}{
		{"inline BEADS_DB rig, -C town", nil, `BEADS_DB=` + rigDB + ` bd -C ` + b.town + ` create --title=x`, true},
		{"inline BD_DB rig, -C town", nil, `BD_DB=` + rigDB + ` bd -C ` + b.town + ` create --title=x`, true},
		{"inline BEADS_DB rig, --directory town", nil, `BEADS_DB=` + rigDB + ` bd --directory ` + b.town + ` create --title=x`, true},
		{"inline BEADS_DB rig, -C town after the verb", nil, `BEADS_DB=` + rigDB + ` bd create --title=x -C ` + b.town, true},
		{"process BEADS_DB rig, -C town", map[string]string{"BEADS_DB": rigDB}, `bd -C ` + b.town + ` create --title=x`, true},
		{"process BD_DB rig, -C town", map[string]string{"BD_DB": rigDB}, `bd -C ` + b.town + ` create --title=x`, true},
		{"inline BEADS_DIR rig, -C town", nil, `BEADS_DIR=` + rigBeads + ` bd -C ` + b.town + ` create --title=x`, true},
		{"process BEADS_DIR rig, -C town", map[string]string{"BEADS_DIR": rigBeads}, `bd -C ` + b.town + ` create --title=x`, true},
		{"inline BEADS_DB town, -C rig dir", nil, `BEADS_DB=` + townDB + ` bd -C ` + b.rigDir + ` create --title=x`, false},
		{"process BEADS_DB town, -C rig dir", map[string]string{"BEADS_DB": townDB}, `bd -C ` + b.rigDir + ` create --title=x`, false},
		{"inline BEADS_DIR town, -C rig dir", nil, `BEADS_DIR=` + filepath.Join(b.town, ".beads") + ` bd -C ` + b.rigDir + ` create --title=x`, false},
		{"inline BEADS_DB rig, no -C", nil, `BEADS_DB=` + rigDB + ` bd create --title=x`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			payload, err := json.Marshal(map[string]any{
				"tool_name":  "Bash",
				"cwd":        b.worktree,
				"tool_input": map[string]string{"command": tt.command},
			})
			if err != nil {
				t.Fatal(err)
			}
			env := map[string]string{"GT_ROLE": polecatRole, "HOME": b.home}
			for k, v := range tt.env {
				env[k] = v
			}
			var stderr strings.Builder
			err = tapGuardBdCreateStore(strings.NewReader(string(payload)), &stderr, fakeGuardProcess(env, ""))
			if !tt.blocked {
				if err != nil || stderr.Len() != 0 {
					t.Fatalf("want allowed, got err=%v stderr=%q", err, stderr.String())
				}
				return
			}
			if err == nil || !strings.Contains(stderr.String(), bdCreateStoreRefusal) {
				t.Fatalf("want blocked, got err=%v stderr=%q", err, stderr.String())
			}
		})
	}
}

// TestTapGuardBdCreateStoreEnvPrefixAndExport pins the store selectors a
// command sets through env, through an export earlier on the same line, or
// through a bare assignment. bd reads the exported and env-prefixed values, so
// each names the store it files into; a bare assignment is not exported and
// bd never sees it.
func TestTapGuardBdCreateStoreEnvPrefixAndExport(t *testing.T) {
	t.Parallel()
	b := newBdStoreTown(t)
	townDB := filepath.Join(b.town, ".beads", "x.db")
	rigDB := filepath.Join(b.rigDir, "mayor", "rig", ".beads", "x.db")
	townBeads := filepath.Join(b.town, ".beads")
	rigBeads := filepath.Join(b.rigDir, "mayor", "rig", ".beads")
	tests := []struct {
		name    string
		command string
		blocked bool
	}{
		{"env BEADS_DB town", `env BEADS_DB=` + townDB + ` bd create --title=x`, true},
		{"env BD_DB town", `env BD_DB=` + townDB + ` bd create --title=x`, true},
		{"env BEADS_DIR town", `env BEADS_DIR=` + townBeads + ` bd create --title=x`, true},
		{"env -u then BEADS_DB town", `env -u HOME BEADS_DB=` + townDB + ` bd create --title=x`, true},
		{"command-prefixed env BEADS_DB town", `command env BEADS_DB=` + townDB + ` bd create --title=x`, true},
		{"env BEADS_DB rig", `env BEADS_DB=` + rigDB + ` bd create --title=x`, false},
		{"env BEADS_DB town, -C rig dir", `env BEADS_DB=` + townDB + ` bd -C ` + b.rigDir + ` create --title=x`, false},
		{"export BEADS_DB town", `export BEADS_DB=` + townDB + ` && bd create --title=x`, true},
		{"export BD_DB town", `export BD_DB=` + townDB + `; bd create --title=x`, true},
		{"export BEADS_DIR town", `export BEADS_DIR=` + townBeads + ` && bd create --title=x`, true},
		{"export BEADS_DB rig", `export BEADS_DB=` + rigDB + ` && bd create --title=x`, false},
		{"export BEADS_DB town, -C rig dir", `export BEADS_DB=` + townDB + ` && bd -C ` + b.rigDir + ` create --title=x`, false},
		{"export BEADS_DB town, then a rig create", `export BEADS_DB=` + townDB + ` && bd -C ` + rigBeads + ` create --title=x`, false},
		{"export BEADS_DIR rig, export BEADS_DB town", `export BEADS_DIR=` + rigBeads + ` && export BEADS_DB=` + townDB + ` && bd create --title=x`, true},
		{"bare BEADS_DB town is not exported", `BEADS_DB=` + townDB + `; bd create --title=x`, false},
		{"bare BEADS_DB town on the line of a later export", `BEADS_DB=` + townDB + `; export BD_DB=` + rigDB + `; bd create --title=x`, false},
		{"inline BEADS_DB rig over export town", `export BEADS_DB=` + townDB + ` && BEADS_DB=` + rigDB + ` bd create --title=x`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			payload, err := json.Marshal(map[string]any{
				"tool_name":  "Bash",
				"cwd":        b.worktree,
				"tool_input": map[string]string{"command": tt.command},
			})
			if err != nil {
				t.Fatal(err)
			}
			env := map[string]string{"GT_ROLE": polecatRole, "HOME": b.home}
			var stderr strings.Builder
			err = tapGuardBdCreateStore(strings.NewReader(string(payload)), &stderr, fakeGuardProcess(env, ""))
			if !tt.blocked {
				if err != nil || stderr.Len() != 0 {
					t.Fatalf("want allowed, got err=%v stderr=%q", err, stderr.String())
				}
				return
			}
			if err == nil || !strings.Contains(stderr.String(), bdCreateStoreRefusal) {
				t.Fatalf("want blocked, got err=%v stderr=%q", err, stderr.String())
			}
		})
	}
}

// TestTapGuardBdCreateStoreDashCTownSubdir pins that bd -C resolves the store
// the way bd does: from the -C directory up to the nearest .beads. A town
// subdirectory with no .beads of its own files into the town store, so a
// create there is refused; a worktree subdirectory resolves to the rig's.
func TestTapGuardBdCreateStoreDashCTownSubdir(t *testing.T) {
	t.Parallel()
	b := newBdStoreTown(t)
	tests := []struct {
		name    string
		command string
		blocked bool
	}{
		{"-C town subdir without .beads", `bd -C ` + filepath.Join(b.town, "mayor") + ` create --title=x`, true},
		{"--directory town subdir without .beads", `bd --directory=` + filepath.Join(b.town, "mayor") + ` create --title=x`, true},
		{"-C town subdir, create after the verb", `bd create --title=x -C ` + filepath.Join(b.town, "mayor"), true},
		{"-C worktree subdir without .beads", `bd -C ` + filepath.Join(b.worktree, "internal") + ` create --title=x`, false},
		{"-C rig dir", `bd -C ` + b.rigDir + ` create --title=x`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			stderr, err := b.run(t, polecatRole, b.worktree, tt.command)
			if !tt.blocked {
				if err != nil || stderr != "" {
					t.Fatalf("want allowed, got err=%v stderr=%q", err, stderr)
				}
				return
			}
			if err == nil || !strings.Contains(stderr, bdCreateStoreRefusal) {
				t.Fatalf("want blocked, got err=%v stderr=%q", err, stderr)
			}
		})
	}
}

// failingReader is a hook payload the guard cannot read, as a closed stdin is.
type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("read failed") }

// TestTapGuardBdCreateStoreFailsOpenOnUnreadableContext pins the guard's
// early returns: when the command or the directory it would judge cannot be
// read, the guard allows the call rather than block a command it knows
// nothing about.
func TestTapGuardBdCreateStoreFailsOpenOnUnreadableContext(t *testing.T) {
	t.Parallel()
	b := newBdStoreTown(t)

	payload := func(t *testing.T, fields map[string]any) string {
		t.Helper()
		data, err := json.Marshal(fields)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	polecat := fakeGuardProcess(map[string]string{"GT_ROLE": polecatRole, "HOME": b.town}, "")

	tests := []struct {
		name string
		r    io.Reader
		proc guardProcess
	}{
		{"unreadable payload", failingReader{}, polecat},
		{"malformed payload", strings.NewReader("{"), polecat},
		{"payload without a command", strings.NewReader(payload(t, map[string]any{"cwd": b.town})), polecat},
		{
			"payload without cwd and no working directory",
			strings.NewReader(payload(t, map[string]any{
				"tool_name":  "Bash",
				"tool_input": map[string]string{"command": "bd create --title=x"},
			})),
			fakeGuardProcess(map[string]string{"GT_ROLE": polecatRole, "HOME": b.town}, ""),
		},
		{
			"cwd outside any town",
			strings.NewReader(payload(t, map[string]any{
				"cwd":        t.TempDir(),
				"tool_name":  "Bash",
				"tool_input": map[string]string{"command": "bd create --title=x"},
			})),
			polecat,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var stderr strings.Builder
			if err := tapGuardBdCreateStore(tt.r, &stderr, tt.proc); err != nil || stderr.Len() != 0 {
				t.Fatalf("want allowed, got err=%v stderr=%q", err, stderr.String())
			}
		})
	}
}

// TestBdDirectoryFlagParse pins what the -C/--directory reader returns for the
// token shapes a bd create can carry, apart from the store judgement on top.
func TestBdDirectoryFlagParse(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		segment []string
		want    string
		wantOK  bool
	}{
		{"no flag", []string{"bd", "create", "--title=x"}, "", false},
		{"separate value", []string{"bd", "-C", "/a", "create"}, "/a", true},
		{"glued value", []string{"bd", "create", "-C/a"}, "/a", true},
		{"equals value", []string{"bd", "create", "-C=/a"}, "/a", true},
		{"long equals value", []string{"bd", "create", "--directory=/a"}, "/a", true},
		{"last wins", []string{"bd", "-C", "/a", "create", "-C", "/b"}, "/b", true},
		{"flag with no value", []string{"bd", "create", "-C"}, "", true},
		{"glued -C is a --title value", []string{"bd", "create", "--title", "-C/a"}, "", false},
		{"bare -C is a --title value", []string{"bd", "create", "--title", "-C", "/a"}, "", false},
		{"-C after a --title value", []string{"bd", "create", "--title", "-C/x", "-C", "/a"}, "/a", true},
		{"-C is a --db value", []string{"bd", "--db", "-C/a", "create"}, "", false},
		{"--directory value spelled like a flag", []string{"bd", "--directory", "--title", "create"}, "--title", true},
		{"after the -- terminator", []string{"bd", "create", "--", "-C", "/a"}, "", false},
		{"value flag as the last token", []string{"bd", "create", "--title"}, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, ok := bdDirectoryFlag(tt.segment)
			if got != tt.want || ok != tt.wantOK {
				t.Errorf("bdDirectoryFlag(%q) = (%q, %v), want (%q, %v)", tt.segment, got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

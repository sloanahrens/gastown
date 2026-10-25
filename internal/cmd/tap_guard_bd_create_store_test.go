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
		// A nested shell inherits the process env, so the store it selects
		// reaches the bd create it runs.
		{"BEADS_DB town into bash -c", map[string]string{"BEADS_DB": townDB}, `bash -c 'bd create --title=x'`, true},
		{"BEADS_DB rig into bash -c", map[string]string{"BEADS_DB": rigDB}, `bash -c 'bd create --title=x'`, false},
		{"BEADS_DB town into a bare here-string", map[string]string{"BEADS_DB": townDB}, `bash <<< bd create --title=x`, true},
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

// TestTapGuardBdCreateStoreDashCOutranksEnvDB pins which selector decides the
// store when bd -C meets a database one: -C sets BEADS_DIR and bd reads it
// before BEADS_DB/BD_DB, so a rig-valued database variable, inline or in the
// process env, cannot make a -C town create look like a rig one; an explicit
// --db or --repo outranks -C, so it alone decides where the bead lands.
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
		// An explicit --db or --repo outranks -C: bd acts on the flag, so the
		// directory -C changed to no longer decides the store.
		{"-C town with --db rig", nil, `bd -C ` + b.town + ` --db ` + rigDB + ` create --title=x`, false},
		{"-C rig dir with --db town", nil, `bd -C ` + b.rigDir + ` --db ` + townDB + ` create --title=x`, true},
		{"-C town with --repo rig", nil, `bd -C ` + b.town + ` create --title=x --repo ` + b.rigDir, false},
		{"-C rig dir with --repo town", nil, `bd -C ` + b.rigDir + ` create --title=x --repo ` + b.town, true},
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
		{"export town, create, then export rig: the create still files to town", `export BEADS_DB=` + townDB + `; bd create --title=x; export BEADS_DB=` + rigDB, true},
		{"export rig, create, then export town: the create files to the rig", `export BEADS_DB=` + rigDB + `; bd create --title=x; export BEADS_DB=` + townDB, false},
		{"bare town, exported without a value", `BEADS_DB=` + townDB + `; export BEADS_DB; bd create --title=x`, true},
		{"export town, unset, create", `export BEADS_DB=` + townDB + `; unset BEADS_DB; bd create --title=x`, false},
		{"grep names BEADS_DB; it sets nothing", `grep BEADS_DB README.md; bd create --title=x`, false},
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

// TestTapGuardBdCreateStoreNestedSelectors pins gt-norvx: a bd create run by a
// shell the line starts files into the store the line selects for it. The
// selector reaches that shell as the segment's own inline assignment, as the
// value an export earlier on the line leaves behind, or as the process env
// (TestTapGuardBdCreateStoreProcessEnvDB), so `BEADS_DB=<town db> bash -c 'bd
// create'` files into the town however the line spells it, and the same line
// under the rig's database stays allowed. A here-string body is script text as
// well: quoted it is one word, and written across the operator's words the
// shell word-splits it, so the guard reads every word after the operator.
func TestTapGuardBdCreateStoreNestedSelectors(t *testing.T) {
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
		{"inline BEADS_DB town into bash -c", `BEADS_DB=` + townDB + ` bash -c 'bd create --title=x'`, true},
		{"inline BEADS_DB rig into bash -c", `BEADS_DB=` + rigDB + ` bash -c 'bd create --title=x'`, false},
		{"inline BEADS_DIR town into bash -c", `BEADS_DIR=` + townBeads + ` bash -c 'bd create --title=x'`, true},
		{"inline BEADS_DIR rig into bash -c", `BEADS_DIR=` + rigBeads + ` bash -c 'bd create --title=x'`, false},
		{"inline BD_DB town into sh -c", `BD_DB=` + townDB + ` sh -c 'bd create --title=x'`, true},
		{"inline BD_DB rig into sh -c", `BD_DB=` + rigDB + ` sh -c 'bd create --title=x'`, false},
		{"exported BEADS_DB town into bash -c", `export BEADS_DB=` + townDB + `; bash -c 'bd create --title=x'`, true},
		{"exported BEADS_DB rig into bash -c", `export BEADS_DB=` + rigDB + `; bash -c 'bd create --title=x'`, false},
		{"exported BEADS_DIR town into bash -c", `export BEADS_DIR=` + townBeads + `; bash -c 'bd create --title=x'`, true},
		{"exported BEADS_DIR rig into bash -c", `export BEADS_DIR=` + rigBeads + `; bash -c 'bd create --title=x'`, false},
		{"exported BEADS_DB town into a bare here-string", `export BEADS_DB=` + townDB + `; bash <<< bd create --title=x`, true},
		// A bare here-string body is word-split, so the words after the
		// operator are the script, with or without a backslash-newline
		// between the operator and the first word.
		{"inline BEADS_DB town into a bare here-string", `BEADS_DB=` + townDB + ` bash <<< bd create --title=x`, true},
		{"inline BEADS_DB rig into a bare here-string", `BEADS_DB=` + rigDB + ` bash <<< bd create --title=x`, false},
		{"inline BEADS_DIR town into a bare here-string", `BEADS_DIR=` + townBeads + ` bash <<< bd create --title=x`, true},
		{"inline BEADS_DIR rig into a bare here-string", `BEADS_DIR=` + rigBeads + ` bash <<< bd create --title=x`, false},
		{"inline BD_DB town into a bare here-string", `BD_DB=` + townDB + ` sh <<< bd create --title=x`, true},
		{"backslash-newline before the bare body", `BEADS_DB=` + townDB + ` bash <<< \
bd create --title=x`, true},
		{"quoted here-string body", `BEADS_DB=` + townDB + ` bash <<< 'bd create --title=x'`, true},
		// The here-string is read as the text it feeds, not as a bd create by
		// its own name: a body that files nothing stays allowed either way.
		{"bare here-string body that files nothing", `BEADS_DB=` + townDB + ` bash <<< echo hi`, false},
		{"here-string body under the rig store", `BEADS_DB=` + rigDB + ` bash <<< bd create --title=x`, false},
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
		{"-C directory with no .beads above it", `bd -C ` + filepath.Join(t.TempDir(), "nowhere") + ` create --title=x`, true},
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

// TestTapGuardBdCreateStoreLauncherChdir pins gt-m5bb4: env -C/--chdir and
// sudo -D/--chdir run the command behind them in that directory, so bd create
// resolves its store from there the way bd -C does. A relative value resolves
// against the shell's directory, bd -C still overrides the launcher, and a
// value the guard cannot place is refused.
//
// Each blocked case names the refusal it must give, so a create refused for the
// wrong reason (a directory that never reached the town, an unplaceable
// launcher value) fails the case instead of passing as blocked (gt-p911n).
func TestTapGuardBdCreateStoreLauncherChdir(t *testing.T) {
	t.Parallel()
	b := newBdStoreTown(t)
	upToTown := filepath.Join("..", "..", "..", "..")
	tests := []struct {
		name    string
		command string
		reason  string // the refusal the guard must give; "" for allowed
	}{
		{"env -C town", `env -C ` + b.town + ` bd create --title=x`, bdCreateTownRefusal},
		{"env -C town, glued", `env -C` + b.town + ` bd create --title=x`, bdCreateTownRefusal},
		{"env --chdir town", `env --chdir ` + b.town + ` bd create --title=x`, bdCreateTownRefusal},
		{"env --chdir= town", `env --chdir=` + b.town + ` bd create --title=x`, bdCreateTownRefusal},
		{"env -i -C town", `env -i -C ` + b.town + ` bd create --title=x`, bdCreateTownRefusal},
		{"env -C town subdir without .beads", `env -C ` + filepath.Join(b.town, "mayor") + ` bd create --title=x`, bdCreateTownRefusal},
		{"env -C relative up to town", `env -C ` + upToTown + ` bd create --title=x`, bdCreateTownRefusal},
		{"env -C rig dir", `env -C ` + b.rigDir + ` bd create --title=x`, ""},
		{"env -C worktree", `env -C ` + b.worktree + ` bd create --title=x`, ""},
		{"env -C town, bd -C back to the rig", `env -C ` + b.town + ` bd -C ` + b.rigDir + ` create --title=x`, ""},
		{"env -C town over an inline rig BEADS_DIR", `env -C ` + b.town + ` BEADS_DIR=` + filepath.Join(b.rigDir, "mayor", "rig", ".beads") + ` bd create --title=x`, ""},
		{"env -C rig with inline town BEADS_DIR", `env -C ` + b.rigDir + ` BEADS_DIR=` + filepath.Join(b.town, ".beads") + ` bd create --title=x`, bdCreateTownRefusal},
		{"env -C unexpanded variable", `env -C $TOWN_DIR bd create --title=x`, bdCreateLauncherDirUnplaceable},
		{"env -C value from an assignment on the line", `TOWN_DIR=` + b.town + ` env -C $TOWN_DIR bd create --title=x`, bdCreateTownRefusal},
		{"env -C nowhere", `env -C ` + filepath.Join(t.TempDir(), "nowhere") + ` bd create --title=x`, bdCreateNoStore},
		{"cd town then env -C .", `cd ` + b.town + ` && env -C . bd create --title=x`, bdCreateTownRefusal},
		{"sudo -D town", `sudo -D ` + b.town + ` bd create --title=x`, bdCreateTownRefusal},
		{"sudo --chdir town", `sudo --chdir ` + b.town + ` bd create --title=x`, bdCreateTownRefusal},
		{"sudo --chdir= town", `sudo --chdir=` + b.town + ` bd create --title=x`, bdCreateTownRefusal},
		{"sudo -D rig", `sudo -D ` + b.rigDir + ` bd create --title=x`, ""},
		{"sudo --chdir rig", `sudo --chdir ` + b.rigDir + ` bd create --title=x`, ""},
		{"env -C town, nested shell", `env -C ` + b.town + ` bash -c 'bd create --title=x'`, bdCreateTownRefusal},
		{"env -C rig, nested shell", `env -C ` + b.rigDir + ` bash -c 'bd create --title=x'`, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			stderr, err := b.run(t, polecatRole, b.worktree, tt.command)
			if tt.reason == "" {
				if err != nil || stderr != "" {
					t.Fatalf("want allowed, got err=%v stderr=%q", err, stderr)
				}
				return
			}
			if err == nil || !strings.Contains(stderr, bdCreateStoreRefusal) || !strings.Contains(stderr, tt.reason) {
				t.Fatalf("want blocked with %q, got err=%v stderr=%q", tt.reason, err, stderr)
			}
		})
	}
}

// TestTapGuardBdCreateStoreUnreadableDirRefused pins that a directory the
// walk cannot stat is refused, not skipped: the .beads bd finds there might be
// the town's. The fixture is a path through a regular file, whose stat fails
// with ENOTDIR rather than not-exist. A missing .beads is not an error and the
// walk goes on.
func TestTapGuardBdCreateStoreUnreadableDirRefused(t *testing.T) {
	t.Parallel()
	b := newBdStoreTown(t)
	file := filepath.Join(b.worktree, "file.txt")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	stderr, err := b.run(t, polecatRole, b.worktree, `bd -C `+file+` create --title=x`)
	if err == nil || !strings.Contains(stderr, bdCreateStoreRefusal) || !strings.Contains(stderr, bdCreateStoreUnreadable) {
		t.Fatalf("want the unreadable-directory refusal, got err=%v stderr=%q", err, stderr)
	}

	stderr, err = b.run(t, polecatRole, b.worktree, `bd -C `+filepath.Join(b.worktree, "internal")+` create --title=x`)
	if err != nil || stderr != "" {
		t.Fatalf("want a worktree subdirectory without .beads allowed, got err=%v stderr=%q", err, stderr)
	}
}

// TestTapGuardBdCreateStoreShellEnvFollowsLine pins what bd reads from its
// environment once the commands on a line have run in order, starting from the
// process env. A store change the guard cannot follow to the bd create (a
// subshell, a branch, set -a, read) is refused, not guessed.
func TestTapGuardBdCreateStoreShellEnvFollowsLine(t *testing.T) {
	t.Parallel()
	b := newBdStoreTown(t)
	townDB := filepath.Join(b.town, ".beads", "x.db")
	rigDB := filepath.Join(b.rigDir, "mayor", "rig", ".beads", "x.db")
	tests := []struct {
		name    string
		procDB  string // BEADS_DB in the process env; "" for none
		command string
		blocked bool
	}{
		{"process town, unset before the create", townDB, `unset BEADS_DB; bd create --title=x`, false},
		{"process town, bare rig: the process export carries the new value", townDB, `BEADS_DB=` + rigDB + `; bd create --title=x`, false},
		{"process town, export -n before the create", townDB, `export -n BEADS_DB; bd create --title=x`, false},
		{"process rig, bare town exported without a value", rigDB, `BEADS_DB=` + townDB + `; export BEADS_DB; bd create --title=x`, true},
		{"subshell export of rig is not followed", "", `(export BEADS_DB=` + rigDB + `); bd create --title=x`, true},
		{"export after a failed && is not followed", "", `false && export BEADS_DB=` + rigDB + `; bd create --title=x`, true},
		{"export after cd && is not followed", "", `cd ` + b.worktree + ` && export BEADS_DB=` + rigDB + ` && bd create --title=x`, true},
		{"set -a before a bare town assignment", "", `set -a; BEADS_DB=` + townDB + `; bd create --title=x`, true},
		{"read into BEADS_DB", "", `read BEADS_DB; bd create --title=x`, true},
		{"set -e names no store variable", "", `set -e; bd create --title=x`, false},
		{"process town, inline rig overrides the process value", townDB, `BEADS_DB=` + rigDB + ` bd create --title=x`, false},
		{"export town reaches a bash -c payload", "", `export BEADS_DB=` + townDB + `; bash -c 'bd create --title=x'`, true},
		{"export town reaches a command substitution", "", `export BEADS_DB=` + townDB + `; echo $(bd create --title=x)`, true},
		{"inline town reaches the bash -c it prefixes", "", `BEADS_DB=` + townDB + ` bash -c 'bd create --title=x'`, true},
		{"process town reaches a bash -c payload", townDB, `bash -c 'bd create --title=x'`, true},
		{"export rig reaches a bash -c payload", "", `export BEADS_DB=` + rigDB + `; bash -c 'bd create --title=x'`, false},
		{"bare town is not exported to a bash -c payload", "", `BEADS_DB=` + townDB + `; bash -c 'bd create --title=x'`, false},
		{"unset inside the payload clears the exported town", "", `export BEADS_DB=` + townDB + `; bash -c 'unset BEADS_DB; bd create --title=x'`, false},
		{"export town inside the payload", "", `bash -c 'export BEADS_DB=` + townDB + `; bd create --title=x'`, true},
		{"unfollowed store change reaches a bash -c payload", "", `read BEADS_DB; bash -c 'bd create --title=x'`, true},
		{"unfollowed store change inside a bash -c payload", "", `bash -c 'read BEADS_DB; bd create --title=x'`, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			env := map[string]string{"GT_ROLE": polecatRole, "HOME": b.home}
			if tt.procDB != "" {
				env["BEADS_DB"] = tt.procDB
			}
			payload, err := json.Marshal(map[string]any{
				"tool_name":  "Bash",
				"cwd":        b.worktree,
				"tool_input": map[string]string{"command": tt.command},
			})
			if err != nil {
				t.Fatal(err)
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

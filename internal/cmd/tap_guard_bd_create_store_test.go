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
	return bdStoreTown{town: town, rigDir: rigDir, worktree: worktree, storeLink: storeLink}
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
	proc := fakeGuardProcess(map[string]string{"GT_ROLE": role, "HOME": b.town}, "")
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
		{"--repo rig dir outranks --db town", polecatRole, b.worktree, `bd --db ` + filepath.Join(b.town, ".beads", "x.db") + ` create --title=x --repo ` + b.rigDir, false},
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

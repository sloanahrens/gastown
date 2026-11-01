package main

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/testdb"
	"github.com/steveyegge/gastown/internal/testutil"
)

// The recorder runs bd only through an injected child, so the hermetic harness keeps the test
// binary off the live town's state, as every package that can reach it must.
func TestMain(m *testing.M) {
	os.Exit(testutil.HermeticMain(m))
}

// tempDir returns a temp directory whose own path holds no symlink. The guard refuses a scratch
// behind one, and a Mac's temp root is reached through /var, so a test that wants a clear scratch
// resolves its root first.
func tempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve the temp directory %s: %v", dir, err)
	}
	return dir
}

// safeCwd is the working directory runIn is given by a test that is not about the guard: a temp
// directory whose parents hold no beads workspace.
func safeCwd(t *testing.T) func() (string, error) {
	t.Helper()
	return cwdAt(tempDir(t))
}

// cwdAt is a working directory the process was started in dir.
func cwdAt(dir string) func() (string, error) {
	return func() (string, error) { return dir, nil }
}

// testScratchPort is the port the throwaway Dolt server of a test child is named by: not a port a
// town serves on, so a test that is not about the endpoint is not refused by it.
const testScratchPort = "13307"

// testDoltHost is the name a test child reaches this machine's Dolt server on inside its container.
const testDoltHost = "host.docker.internal"

// The two spellings of one build's `bd version` line: the host's build stamps its branch before the
// commit and a cross-compiled build of the same commit does not (gt-p85pw.117).
const (
	forkHostVersion      = "bd version 1.2.2 (688ff7d2: master@688ff7d249e9) schema<=66\n"
	forkContainerVersion = "bd version 1.2.2 (688ff7d2: 688ff7d249e9) schema<=66\n"
)

// scratchServerEnv is the recording environment of the child bd a test drives unless the test is
// about the Dolt endpoint: a throwaway server named the way testutil names one, by a port and no
// host (gt-p85pw.115).
func scratchServerEnv() []string {
	return []string{"PATH=/usr/bin", "BEADS_DOLT_SERVER_PORT=" + testScratchPort}
}

// testChildEnv is the environment a child inside the container gets for env, built by the production
// filter, so the guard is driven by the environment a real container would be given (gt-p85pw.117).
func testChildEnv(env []string) []string {
	return endpointEnv(env)
}

// answerVersion returns run with the container's bd version answered, since the guard proves the
// build before it asks anything else (gt-p85pw.117).
func answerVersion(run Runner) Runner {
	return func(dir string, args []string) ([]byte, int, error) {
		if len(args) == 1 && args[0] == "version" {
			return []byte(forkContainerVersion), 0, nil
		}
		return run(dir, args)
	}
}

// atWork resolves the workspace bd makes under the mounted directory.
func atWork(string, []string) (string, error) { return filepath.Join(workDir, ".beads"), nil }

// noWorkspace answers as bd does in a fresh container: it resolves no workspace at all.
func noWorkspace(string, []string) (string, error) { return "", nil }

// testChild returns the child bd a test drives: its runs are run's with the container's version
// answered, its environment is the one a container gets, where answers for bd where, and the host bd
// is the build the container's is.
func testChild(run Runner, where Resolver) childBD {
	return childWith(run, scratchServerEnv(), where)
}

// childWith is testChild with the recording environment a test is about named.
func childWith(run Runner, env []string, where Resolver) childBD {
	return childBD{
		Run:         answerVersion(run),
		Env:         func(string) []string { return testChildEnv(env) },
		Where:       where,
		HostVersion: func() ([]byte, error) { return []byte(forkHostVersion), nil },
	}
}

// childWithRawEnv is childWith with env handed to the resolver as it stands, so a test drives a pin
// the container filter would drop and proves the guard decides by the workspace resolved, never by
// the pin (gt-p85pw.117).
func childWithRawEnv(run Runner, env []string, where Resolver) childBD {
	return childBD{
		Run:         answerVersion(run),
		Env:         func(string) []string { return env },
		Where:       where,
		HostVersion: func() ([]byte, error) { return []byte(forkHostVersion), nil },
	}
}

// initMakesWorkspace returns run with the step a real bd init takes when it exits 0: the .beads
// directory in the directory it runs in, which is the workspace the guard checks on disk before it
// trusts bd's answer (gt-p85pw.115).
func initMakesWorkspace(run Runner) Runner {
	return func(dir string, args []string) ([]byte, int, error) {
		out, exit, err := run(dir, args)
		if err != nil || exit != 0 || len(args) == 0 || args[0] != "init" {
			return out, exit, err
		}
		return out, exit, errors.Join(err, os.MkdirAll(filepath.Join(dir, ".beads"), 0o755))
	}
}

// guardBD is the bd the guard tests drive: every run answers exit 0, and an init makes the .beads
// directory in the directory it runs in, as bd init does. A noWorkspace guardBD answers the init
// without making anything, which is a bd that writes nowhere the recorder can see.
type guardBD struct {
	dirs        []string
	args        [][]string
	noWorkspace bool
}

func (g *guardBD) Run(dir string, args []string) ([]byte, int, error) {
	g.dirs = append(g.dirs, dir)
	g.args = append(g.args, args)
	if args[0] == "init" && !g.noWorkspace {
		if err := os.MkdirAll(filepath.Join(dir, ".beads"), 0o755); err != nil {
			return nil, 0, err
		}
	}
	return nil, 0, nil
}

// childFor returns the child factory runIn takes for a child that answers every call the same way.
func childFor(child childBD) func(options) childBD {
	return func(options) childBD { return child }
}

// passingChild is the child bd a runIn test drives when the guard is not what the test is about:
// every run answers exit 0, an init makes the workspace the guard checks for, and bd where reports
// the workspace bd makes under the mounted directory.
func passingChild(bd *fakeBD) childBD {
	return testChild(initMakesWorkspace(bd.Run), atWork)
}

// TestGuardRunRefusesWhatItCannotCheck: a working directory the process cannot name, or a marker it
// cannot read, refuses the run rather than being taken for a safe tree.
func TestGuardRunRefusesWhatItCannotCheck(t *testing.T) {
	t.Parallel()
	t.Run("a working directory it cannot name", func(t *testing.T) {
		t.Parallel()
		unnamed := func() (string, error) { return "", errors.New("getwd: i/o error") }
		err := guardRun(unnamed, filepath.Join(tempDir(t), "scratch"), testChild(okRun, atWork))
		if err == nil || !strings.Contains(err.Error(), "i/o error") {
			t.Fatalf("guardRun with no working directory = %v, want the getwd failure reported", err)
		}
	})
	t.Run("a marker it cannot read", func(t *testing.T) {
		t.Parallel()
		// A marker symlinked to itself fails the stat with ELOOP, which is neither a workspace nor a
		// missing marker.
		tree := tempDir(t)
		if err := os.Symlink(".beads", filepath.Join(tree, ".beads")); err != nil {
			t.Fatalf("make the marker loop: %v", err)
		}
		err := guardRun(cwdAt(tree), filepath.Join(tempDir(t), "scratch"), testChild(okRun, atWork))
		if err == nil || !strings.Contains(err.Error(), "check for a beads workspace") {
			t.Fatalf("guardRun over an unreadable marker = %v, want the read failure reported", err)
		}
	})
}

// TestGuardRunPassesAScratchThatOwnsItsWorkspace: bd resolves the workspace under the mounted
// directory, so a scratch whose answer lands there passes, and the workspace the guard made for the
// check is dropped and removed again (gt-p85pw.115, gt-p85pw.117).
func TestGuardRunPassesAScratchThatOwnsItsWorkspace(t *testing.T) {
	t.Parallel()
	bd := &guardBD{}
	scratch := filepath.Join(tempDir(t), "scratch")
	if err := guardRun(safeCwd(t), scratch, testChild(bd.Run, atWork)); err != nil {
		t.Fatalf("guardRun with a scratch that owns its workspace = %v, want it accepted", err)
	}
	if len(bd.args) != 2 || bd.args[0][0] != "init" || bd.args[1][0] != "sql" {
		t.Fatalf("bd ran %v, want the scratch's init and the drop of the database it made", bd.args)
	}
	if name := initName(t, bd.args[0]); !strings.HasPrefix(name, testdb.MintPrefix) {
		t.Errorf("the scratch workspace's database is %q, want one under the test prefix %q", name, testdb.MintPrefix)
	}
	for i, dir := range bd.dirs {
		if dir != scratch {
			t.Errorf("call %v ran in %s, want the scratch %s", bd.args[i], dir, scratch)
		}
	}
	if _, err := os.Stat(filepath.Join(scratch, ".beads")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the scratch workspace stat = %v, want it removed once the guard passes", err)
	}
}

// TestGuardRunPassesAWorkingDirectoryBeneathAWorkspace: work lives under the town, so a recorder
// started beneath a workspace passes as long as bd in the container resolves a workspace under the
// mounted directory (gt-p85pw.116).
func TestGuardRunPassesAWorkingDirectoryBeneathAWorkspace(t *testing.T) {
	t.Parallel()
	town := tempDir(t)
	townWorkspace := filepath.Join(town, ".beads")
	mustWrite(t, filepath.Join(townWorkspace, "metadata.json"), `{"dolt_database":"hq"}`)
	work := filepath.Join(town, "work")
	if err := os.Mkdir(work, 0o755); err != nil {
		t.Fatalf("make the working directory: %v", err)
	}
	bd := &guardBD{}
	scratch := filepath.Join(work, "scratch")
	if err := guardRun(cwdAt(work), scratch, testChild(bd.Run, atWork)); err != nil {
		t.Fatalf("guardRun beneath a workspace = %v, want it accepted", err)
	}
	if len(bd.args) != 2 || bd.args[0][0] != "init" || bd.args[1][0] != "sql" {
		t.Fatalf("bd ran %v, want the scratch's init and the drop of the database it made", bd.args)
	}
	for i, dir := range bd.dirs {
		if dir != scratch {
			t.Errorf("call %v ran in %s, want the scratch %s", bd.args[i], dir, scratch)
		}
	}
}

// TestGuardRunRefusesAWorkingDirectoryThatIsAWorkspaceRoot: a recorder whose working directory is
// the workspace root itself is refused before any bd call, since that is where bd init would rewrite
// the store the recorder did not make. A root is either a marker the directory holds -- the .beads
// tree or the metadata.json bd keeps beside it -- or one it reaches through a symlink, which the
// guard resolves before it looks (gt-p85pw.116).
func TestGuardRunRefusesAWorkingDirectoryThatIsAWorkspaceRoot(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		root     func(t *testing.T) (cwd, marker string) // marker is the store file the refusal must leave untouched
		resolved bool                                    // the refusal also names the root the working directory resolves to
	}{
		{"the .beads it holds", func(t *testing.T) (string, string) {
			tree := tempDir(t)
			metadata := filepath.Join(tree, ".beads", "metadata.json")
			mustWrite(t, metadata, `{"dolt_database":"hq"}`)
			return tree, metadata
		}, false},
		{"the metadata.json it holds", func(t *testing.T) (string, string) {
			tree := tempDir(t)
			metadata := filepath.Join(tree, "metadata.json")
			mustWrite(t, metadata, `{"dolt_database":"hq"}`)
			return tree, metadata
		}, false},
		{"a working directory behind a symlink to a root", func(t *testing.T) (string, string) {
			root := tempDir(t)
			metadata := filepath.Join(root, ".beads", "metadata.json")
			mustWrite(t, metadata, `{"dolt_database":"hq"}`)
			link := filepath.Join(tempDir(t), "clean-looking")
			if err := os.Symlink(root, link); err != nil {
				t.Fatalf("make the symlink: %v", err)
			}
			return link, metadata
		}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cwd, marker := tc.root(t)
			before := readFile(t, marker)
			bd := &guardBD{}
			err := guardRun(cwdAt(cwd), filepath.Join(tempDir(t), "scratch"), testChild(bd.Run, atWork))
			if err == nil || !strings.Contains(err.Error(), "workspace root") {
				t.Fatalf("guardRun in %s = %v, want it refused", tc.name, err)
			}
			if tc.resolved {
				real, rerr := filepath.EvalSymlinks(cwd)
				if rerr != nil {
					t.Fatalf("resolve %s: %v", cwd, rerr)
				}
				if !strings.Contains(err.Error(), real) {
					t.Errorf("guardRun error = %q, want the root it resolves to, %s, named", err, real)
				}
			}
			if len(bd.args) != 0 {
				t.Errorf("bd ran %v, want no bd run attempted", bd.args)
			}
			if after := readFile(t, marker); after != before {
				t.Errorf("%s = %q after the refused run, want %q", marker, after, before)
			}
		})
	}
}

// TestGuardRunResolvesARelativeScratchAgainstTheWorkingDirectory: a relative -scratch names the
// directory under the working directory, and the pass still needs bd to resolve a workspace inside
// it; the same working directory is refused when bd answers with the town's workspace (gt-p85pw.116).
func TestGuardRunResolvesARelativeScratchAgainstTheWorkingDirectory(t *testing.T) {
	t.Parallel()
	town := tempDir(t)
	townWorkspace := filepath.Join(town, ".beads")
	mustWrite(t, filepath.Join(townWorkspace, "metadata.json"), `{"dolt_database":"hq"}`)
	work := filepath.Join(town, "work")
	if err := os.Mkdir(work, 0o755); err != nil {
		t.Fatalf("make the working directory: %v", err)
	}
	tests := []struct {
		name  string
		where Resolver
		want  string // empty: the guard accepts
	}{
		{"bd resolves a workspace inside the mounted directory", atWork, ""},
		{"bd resolves no workspace yet", noWorkspace, ""},
		{"bd resolves a workspace outside it", func(string, []string) (string, error) {
			return "/town/.beads", nil
		}, "outside /work"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			bd := &guardBD{}
			child := testChild(bd.Run, tc.where)
			err := guardRun(cwdAt(work), "scratch", child)
			if tc.want != "" {
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("guardRun over %s = %v, want an error naming %q", tc.name, err, tc.want)
				}
				return
			}
			if err != nil {
				t.Fatalf("guardRun over %s = %v, want it accepted", tc.name, err)
			}
			want := filepath.Join(work, "scratch")
			if len(bd.dirs) == 0 || bd.dirs[0] != want {
				t.Errorf("bd ran in %v, want the scratch resolved under the working directory %s", bd.dirs, want)
			}
		})
	}
}

// TestGuardRunRefusesTheTownDoltServer: the scratch's bd init creates a database, so an environment
// that aims it at the town's Dolt server, or at no server the guard can vouch for, is refused before
// any bd call, and the scratch directory is not even made (gt-p85pw.115). Only the BEADS_DOLT_ names
// reach the container, so a server named under the fork's GT_DOLT_ names is no server at all and is
// refused as one (gt-p85pw.117).
func TestGuardRunRefusesTheTownDoltServer(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		env  []string
		want string
	}{
		{"the production port", []string{"BEADS_DOLT_SERVER_PORT=3307"}, "town's Dolt server"},
		{"the production port under bd's other name", []string{"BEADS_DOLT_PORT=3307"}, "town's Dolt server"},
		{"the production port under a name the container never sees", []string{"GT_DOLT_PORT=3307"}, "no Dolt server is named"},
		{"a host that is not this machine", []string{"BEADS_DOLT_SERVER_HOST=dolt.example", "BEADS_DOLT_SERVER_PORT=" + testScratchPort}, "not this machine"},
		{"a port that is not a port", []string{"BEADS_DOLT_SERVER_PORT=dolt"}, "does not name a port"},
		{"no server named at all", nil, "no Dolt server is named"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			bd := &guardBD{}
			child := childWith(bd.Run, tc.env, atWork)
			scratch := filepath.Join(tempDir(t), "scratch")
			err := guardRun(safeCwd(t), scratch, child)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("guardRun with %s = %v, want an error naming %q", tc.name, err, tc.want)
			}
			if len(bd.args) != 0 {
				t.Errorf("bd ran %v, want the town's server refused before any bd call", bd.args)
			}
			if _, statErr := os.Stat(scratch); !errors.Is(statErr, fs.ErrNotExist) {
				t.Errorf("scratch %s stat = %v, want no directory made for a refused server", scratch, statErr)
			}
		})
	}
}

// TestGuardRunRefusesThePortTheTownItselfNames: a town may serve Dolt on a port other than the
// production default, and the endpoint it names is refused as well, so the guard does not rest on
// the default alone (gt-p85pw.115).
func TestGuardRunRefusesThePortTheTownItselfNames(t *testing.T) {
	t.Parallel()
	town := tempDir(t)
	mustWrite(t, filepath.Join(town, "mayor", "town.json"), `{"name":"temp","dolt":{"port":13307}}`)
	bd := &guardBD{}
	child := childWith(bd.Run, []string{"BEADS_DOLT_SERVER_PORT=13307"}, atWork)
	err := guardRun(cwdAt(town), filepath.Join(town, "scratch"), child)
	if err == nil || !strings.Contains(err.Error(), "port 13307") {
		t.Fatalf("guardRun in a town that serves Dolt on 13307 = %v, want its port refused", err)
	}
	if len(bd.args) != 0 {
		t.Errorf("bd ran %v, want the town's server refused before any bd call", bd.args)
	}
}

// TestGuardRunRefusesAScratchItsInitMadeNoWorkspaceIn: bd where's answer is bd's own report, so a bd
// that answers with the mounted workspace but wrote nothing to the scratch is refused on the marker
// it did not make. A marker above the scratch is the town's workspace, which the scratch does not
// own, and a marker it cannot read is a refusal rather than an absent one (gt-p85pw.115).
func TestGuardRunRefusesAScratchItsInitMadeNoWorkspaceIn(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		tree func(t *testing.T) string // the scratch to guard
		want string
	}{
		{"no workspace anywhere above it", func(t *testing.T) string {
			return filepath.Join(tempDir(t), "scratch")
		}, "cannot prove the scratch owns"},
		{"the town's workspace above it", func(t *testing.T) string {
			town := tempDir(t)
			mustWrite(t, filepath.Join(town, ".beads", "metadata.json"), `{"dolt_database":"hq"}`)
			return filepath.Join(town, "scratch")
		}, "which the scratch does not own"},
		{"a marker it cannot read", func(t *testing.T) string {
			scratch := filepath.Join(tempDir(t), "scratch")
			if err := os.MkdirAll(scratch, 0o755); err != nil {
				t.Fatalf("make the scratch: %v", err)
			}
			// A self-symlink fails the stat with ELOOP, which is neither a workspace nor a missing
			// marker.
			if err := os.Symlink(".beads", filepath.Join(scratch, ".beads")); err != nil {
				t.Fatalf("make the marker loop: %v", err)
			}
			return scratch
		}, "check for the workspace"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			bd := &guardBD{noWorkspace: true} // answers exit 0 to init, and makes nothing
			err := guardRun(safeCwd(t), tc.tree(t), testChild(bd.Run, atWork))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("guardRun over an init that made nothing = %v, want an error naming %q", err, tc.want)
			}
			if len(bd.args) != 1 || bd.args[0][0] != "init" {
				t.Errorf("bd ran %v, want only the init that made nothing", bd.args)
			}
		})
	}
}

// TestGuardRunRefusesAWorkspaceTheContainerResolvesOutsideWork: a container whose bd resolves a
// workspace outside the mounted directory is refused before the scratch's init runs, and the
// workspace bd walked up to is left untouched (gt-p85pw.117).
func TestGuardRunRefusesAWorkspaceTheContainerResolvesOutsideWork(t *testing.T) {
	t.Parallel()
	town := tempDir(t)
	metadata := filepath.Join(town, ".beads", "metadata.json")
	mustWrite(t, metadata, `{"dolt_database":"hq"}`)
	before := readFile(t, metadata)
	bd := &guardBD{}
	child := testChild(bd.Run, func(string, []string) (string, error) { return "/town/.beads", nil })
	err := guardRun(safeCwd(t), filepath.Join(town, "scratch"), child)
	if err == nil || !strings.Contains(err.Error(), "outside /work") {
		t.Fatalf("guardRun over a container that resolves the town's workspace = %v, want it refused", err)
	}
	if len(bd.args) != 0 {
		t.Errorf("bd ran %v, want no bd run after a refused answer", bd.args)
	}
	if after := readFile(t, metadata); after != before {
		t.Errorf("%s = %q after the refused run, want %q", metadata, after, before)
	}
}

// TestGuardRunRefusesAResolverThatDoesNotAnswer: an answer the recorder cannot use as proof of a
// workspace inside workDir is a refusal, not a guess (gt-p85pw.117).
func TestGuardRunRefusesAResolverThatDoesNotAnswer(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		where Resolver
		want  string
	}{
		{"an error", func(string, []string) (string, error) {
			return "", errors.New("bd where: exit status 1")
		}, "ask bd which workspace"},
		{"a path that is not absolute", func(string, []string) (string, error) { return ".beads", nil }, "outside /work"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			bd := &guardBD{}
			child := testChild(bd.Run, tc.where)
			err := guardRun(safeCwd(t), filepath.Join(tempDir(t), "scratch"), child)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("guardRun over %s = %v, want an error naming %q", tc.name, err, tc.want)
			}
			if len(bd.args) != 0 {
				t.Errorf("bd ran %v, want no bd run after a refused answer", bd.args)
			}
		})
	}
}

// TestGuardRunRefusesAScratchBehindASymlink: bd follows symlinks, so a scratch reached through one
// is refused before bd runs, whatever the resolver would have answered (gt-p85pw.115).
func TestGuardRunRefusesAScratchBehindASymlink(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		scratch func(t *testing.T) string
	}{
		{"the scratch is a symlink", func(t *testing.T) string {
			link := filepath.Join(tempDir(t), "scratch")
			if err := os.Symlink(tempDir(t), link); err != nil {
				t.Fatalf("make the symlink: %v", err)
			}
			return link
		}},
		{"a parent of the scratch is a symlink", func(t *testing.T) string {
			link := filepath.Join(tempDir(t), "town")
			if err := os.Symlink(tempDir(t), link); err != nil {
				t.Fatalf("make the symlink: %v", err)
			}
			return filepath.Join(link, "scratch")
		}},
		{"the scratch is behind a symlink loop", func(t *testing.T) string {
			loop := filepath.Join(tempDir(t), "loop")
			if err := os.Symlink(loop, loop); err != nil {
				t.Fatalf("make the symlink loop: %v", err)
			}
			return filepath.Join(loop, "scratch")
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			bd := &guardBD{}
			resolved := false
			child := testChild(bd.Run, func(string, []string) (string, error) {
				resolved = true
				return filepath.Join(workDir, ".beads"), nil
			})
			err := guardRun(safeCwd(t), tc.scratch(t), child)
			if err == nil || !strings.Contains(err.Error(), "symlink") {
				t.Fatalf("guardRun over %s = %v, want the symlink refused", tc.name, err)
			}
			if resolved || len(bd.args) != 0 {
				t.Errorf("bd ran %v and the resolver was asked %v, want a symlinked scratch refused before either", bd.args, resolved)
			}
		})
	}
}

// TestGuardRunRefusesAWorkingDirectoryItCannotResolve: a working directory behind a symlink loop
// has no resolved form, so the guard refuses instead of naming a tree it could not read.
func TestGuardRunRefusesAWorkingDirectoryItCannotResolve(t *testing.T) {
	t.Parallel()
	loop := filepath.Join(tempDir(t), "loop")
	if err := os.Symlink(loop, loop); err != nil {
		t.Fatalf("make the symlink loop: %v", err)
	}
	err := guardRun(cwdAt(loop), filepath.Join(tempDir(t), "scratch"), testChild(okRun, atWork))
	if err == nil || !strings.Contains(err.Error(), "resolve") {
		t.Fatalf("guardRun from an unresolvable working directory = %v, want the resolve failure reported", err)
	}
}

// TestGuardRunRefusesAScratchPathItCannotCheck: a path whose component the OS will not stat is an
// error, never a tree taken for one without a symlink in it.
func TestGuardRunRefusesAScratchPathItCannotCheck(t *testing.T) {
	t.Parallel()
	scratch := filepath.Join(tempDir(t), strings.Repeat("a", 300), "scratch")
	err := guardRun(safeCwd(t), scratch, testChild(okRun, atWork))
	if err == nil || !strings.Contains(err.Error(), "check") {
		t.Fatalf("guardRun over a path it cannot stat = %v, want the check failure reported", err)
	}
}

// TestGuardRunReportsAFailedScratchWorkspace: an init that fails, or a cleanup that cannot finish,
// fails the run rather than passing a scratch whose workspace was never proved.
func TestGuardRunReportsAFailedScratchWorkspace(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		verb string
		want string
	}{
		{"the init", "init", "workspace of its own"},
		{"the cleanup", "sql", "clean up the scratch workspace"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			bd := &fakeBD{out: func(args []string) ([]byte, int, error) {
				if args[0] == tc.verb {
					return nil, 1, nil
				}
				return nil, 0, nil
			}}
			err := guardRun(safeCwd(t), filepath.Join(tempDir(t), "scratch"), testChild(initMakesWorkspace(bd.Run), atWork))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("guardRun over a failed %s = %v, want an error naming %q", tc.name, err, tc.want)
			}
		})
	}
}

// TestGuardRunRefusesAScratchItCannotMake: the guard names the scratch directory it cannot make as
// the failure, rather than reporting a workspace it never asked bd about.
func TestGuardRunRefusesAScratchItCannotMake(t *testing.T) {
	t.Parallel()
	file := filepath.Join(tempDir(t), "file")
	mustWrite(t, file, "not a directory\n")
	err := guardRun(safeCwd(t), filepath.Join(file, "scratch"), testChild(okRun, atWork))
	if err == nil || !strings.Contains(err.Error(), "make the scratch directory") {
		t.Fatalf("guardRun with a scratch under a file = %v, want the directory it cannot make named", err)
	}
}

// TestRunRefusesAScratchDirectoryItCannotMake: fixture copies need a scratch directory, and when it
// cannot be made the command stops with the failure reported, before bd runs.
func TestRunRefusesAScratchDirectoryItCannotMake(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	bd := &fakeBD{out: okBD}
	args := []string{"-fixture", writeFixture(t, fixtureIDs...), "-scratch", ""}
	if code := runIn(safeCwd(t), childFor(passingChild(bd)), args, &stdout, &stderr); code != 1 {
		t.Errorf("run with an empty -scratch = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "make scratch directory") {
		t.Errorf("stderr = %q, want the scratch directory failure reported", stderr.String())
	}
	if len(bd.args) != 0 {
		t.Errorf("bd ran %v for a scratch directory that was never made", bd.args)
	}
}

// TestRunRefusesAMissingFixture: without -fixture there is nothing for bd to run in, so the command
// stops before it records anything.
func TestRunRefusesAMissingFixture(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	if code := runIn(safeCwd(t), childFor(passingChild(&fakeBD{out: okBD})), nil, &stdout, &stderr); code != 2 {
		t.Errorf("run with no -fixture = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "-fixture is required") {
		t.Errorf("stderr = %q, want the missing -fixture named", stderr.String())
	}
}

// TestRunRefusesAFixtureFileThatIsNotThere: a -fixture that names no file stops the run before any
// database is made.
func TestRunRefusesAFixtureFileThatIsNotThere(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	bd := &fakeBD{out: okBD}
	missing := filepath.Join(tempDir(t), "missing.jsonl")
	args := []string{"-fixture", missing, "-scratch", filepath.Join(tempDir(t), "scratch")}
	if code := runIn(safeCwd(t), childFor(passingChild(bd)), args, &stdout, &stderr); code != 2 {
		t.Errorf("run with a missing fixture file = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "open the fixture") {
		t.Errorf("stderr = %q, want the fixture failure reported", stderr.String())
	}
	if inits := callIndices(bd, "init"); len(inits) != 1 {
		t.Errorf("bd ran %v, want only the guard's init before the fixture is read", bd.args)
	}
}

// TestRunRefusesAFixtureMissingABead: a fixture file that lacks a bead the table names stops the
// run before any database is made, naming the bead.
func TestRunRefusesAFixtureMissingABead(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	fixture := writeFixture(t, fixtureID, fixtureParent)
	args := []string{"-fixture", fixture, "-scratch", filepath.Join(tempDir(t), "scratch")}
	if code := runIn(safeCwd(t), childFor(passingChild(&fakeBD{out: okBD})), args, &stdout, &stderr); code != 2 {
		t.Errorf("run over a fixture missing %s = %d, want 2", fixtureDepID, code)
	}
	if !strings.Contains(stderr.String(), fixtureDepID) {
		t.Errorf("stderr = %q, want the missing bead named", stderr.String())
	}
}

// TestRunRefusesAScratchWhoseContainerResolvesAnotherWorkspace: a container whose bd resolves a
// workspace outside the mounted directory is refused before anything is recorded, and that
// workspace is left untouched (gt-p85pw.115, gt-p85pw.117).
func TestRunRefusesAScratchWhoseContainerResolvesAnotherWorkspace(t *testing.T) {
	t.Parallel()
	town := tempDir(t)
	metadata := filepath.Join(town, ".beads", "metadata.json")
	mustWrite(t, metadata, `{"dolt_database":"hq"}`)
	before := readFile(t, metadata)
	bd := &fakeBD{out: okBD}
	child := testChild(initMakesWorkspace(bd.Run), func(string, []string) (string, error) { return "/town/.beads", nil })
	scratch := filepath.Join(town, "scratch")
	out := filepath.Join(tempDir(t), "golden")
	var stdout, stderr bytes.Buffer
	args := []string{"-fixture", writeFixture(t, fixtureIDs...), "-scratch", scratch, "-out", out}
	if code := runIn(safeCwd(t), childFor(child), args, &stdout, &stderr); code != 1 {
		t.Fatalf("run over a container that resolves the town's workspace = %d, want 1\nstderr: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "outside /work") {
		t.Errorf("stderr = %q, want the workspace outside /work named", stderr.String())
	}
	if len(bd.args) != 0 {
		t.Errorf("bd ran %v, want no bd run after a refused answer", bd.args)
	}
	if after := readFile(t, metadata); after != before {
		t.Errorf("%s = %q after the refused run, want %q", metadata, after, before)
	}
	if entries, err := os.ReadDir(out); err == nil && len(entries) > 0 {
		t.Errorf("goldens written under a workspace the recorder refused: %v", entries)
	}
}

// TestRunRecordsAScratchUnderAWorkspaceThatOwnsItsOwn: the production shape is a scratch under a
// tree that holds a workspace, recorded from a directory that is in none. The scratch makes a
// workspace of its own there, so the recording runs, and the store above it is left untouched
// (gt-p85pw.115).
func TestRunRecordsAScratchUnderAWorkspaceThatOwnsItsOwn(t *testing.T) {
	t.Parallel()
	town := tempDir(t)
	metadata := filepath.Join(town, ".beads", "metadata.json")
	mustWrite(t, metadata, `{"dolt_database":"hq"}`)
	before := readFile(t, metadata)
	bd := &fakeBD{out: echoBD}
	scratch := filepath.Join(town, "scratch")
	out := filepath.Join(tempDir(t), "golden")
	var stdout, stderr bytes.Buffer
	args := []string{"-fixture", writeFixture(t, fixtureIDs...), "-scratch", scratch, "-out", out}
	if code := runIn(safeCwd(t), childFor(passingChild(bd)), args, &stdout, &stderr); code != 0 {
		t.Fatalf("run with a scratch under a workspace that owns its own = %d, want 0\nstderr: %s", code, stderr.String())
	}
	goldens, err := os.ReadDir(out)
	if err != nil || len(goldens) == 0 {
		t.Fatalf("goldens under %s = %d entries, %v, want the rows recorded", out, len(goldens), err)
	}
	if after := readFile(t, metadata); after != before {
		t.Errorf("%s = %q after the recording, want %q", metadata, after, before)
	}
	if _, err := os.Stat(filepath.Join(scratch, ".beads")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the scratch workspace stat = %v, want it removed once the guard passes", err)
	}
}

// TestRunRefusesASymlinkedScratch: bd follows the symlinks on its way to a directory, so a -scratch
// whose lexical path never passes a workspace but whose symlink leads into one is refused too
// (gt-p85pw.103, gt-p85pw.115). Nothing is recorded and the workspace is untouched.
func TestRunRefusesASymlinkedScratch(t *testing.T) {
	t.Parallel()
	tree := tempDir(t)
	metadata := filepath.Join(tree, ".beads", "metadata.json")
	mustWrite(t, metadata, `{"dolt_database":"hq"}`)
	before := readFile(t, metadata)
	inside := filepath.Join(tree, "inside")
	if err := os.Mkdir(inside, 0o755); err != nil {
		t.Fatalf("make the directory the link leads to: %v", err)
	}
	link := filepath.Join(tempDir(t), "clean-looking")
	if err := os.Symlink(inside, link); err != nil {
		t.Fatalf("make the symlink: %v", err)
	}
	bd := &fakeBD{out: okBD}
	out := filepath.Join(tempDir(t), "golden")
	var stdout, stderr bytes.Buffer
	args := []string{
		"-fixture", writeFixture(t, fixtureIDs...),
		"-scratch", filepath.Join(link, "scratch"),
		"-out", out,
	}
	if code := runIn(safeCwd(t), childFor(passingChild(bd)), args, &stdout, &stderr); code != 1 {
		t.Fatalf("run with -scratch behind a symlink = %d, want 1\nstderr: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "symlink") {
		t.Errorf("stderr = %q, want the symlink named", stderr.String())
	}
	if len(bd.args) != 0 {
		t.Errorf("bd ran %v, want no bd run attempted for a symlinked scratch", bd.args)
	}
	if after := readFile(t, metadata); after != before {
		t.Errorf("%s = %q after the refused run, want %q", metadata, after, before)
	}
	if entries, err := os.ReadDir(out); err == nil && len(entries) > 0 {
		t.Errorf("goldens written under a workspace: %v", entries)
	}
}

// TestRunRefusesAWorkingDirectoryThatIsAWorkspaceRoot: a recorder started in a workspace root is
// where the incident began, so the run stops before bd runs and writes nothing (gt-p85pw.116).
func TestRunRefusesAWorkingDirectoryThatIsAWorkspaceRoot(t *testing.T) {
	t.Parallel()
	tree := tempDir(t)
	metadata := filepath.Join(tree, ".beads", "metadata.json")
	mustWrite(t, metadata, `{"dolt_database":"hq"}`)
	before := readFile(t, metadata)
	bd := &fakeBD{out: okBD}
	out := filepath.Join(tempDir(t), "golden")
	var stdout, stderr bytes.Buffer
	args := []string{
		"-fixture", writeFixture(t, fixtureIDs...),
		"-scratch", filepath.Join(tempDir(t), "scratch"),
		"-out", out,
	}
	if code := runIn(cwdAt(tree), childFor(passingChild(bd)), args, &stdout, &stderr); code != 1 {
		t.Fatalf("run from a working directory that is a workspace root = %d, want 1\nstderr: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "workspace root") {
		t.Errorf("stderr = %q, want the workspace root named", stderr.String())
	}
	if len(bd.args) != 0 {
		t.Errorf("bd ran %v, want no bd run attempted", bd.args)
	}
	if after := readFile(t, metadata); after != before {
		t.Errorf("%s = %q after the refused run, want %q", metadata, after, before)
	}
	if entries, err := os.ReadDir(out); err == nil && len(entries) > 0 {
		t.Errorf("goldens written under a workspace: %v", entries)
	}
}

// TestRunNamesARelativeScratchByTheWorkingDirectory: docker mounts each copy by its host path, so a
// relative -scratch is made absolute against the working directory the run started in, and every bd
// call runs in a copy under that directory, never under a path the process's own directory would
// resolve (gt-p85pw.117).
func TestRunNamesARelativeScratchByTheWorkingDirectory(t *testing.T) {
	t.Parallel()
	town := tempDir(t)
	bd := &fakeBD{out: echoBD}
	out := filepath.Join(tempDir(t), "golden")
	var stdout, stderr bytes.Buffer
	args := []string{"-fixture", writeFixture(t, fixtureIDs...), "-scratch", "scratch", "-out", out}
	if code := runIn(cwdAt(town), childFor(passingChild(bd)), args, &stdout, &stderr); code != 0 {
		t.Fatalf("run with a relative scratch = %d, want 0\nstderr: %s", code, stderr.String())
	}
	under := filepath.Join(town, "scratch")
	ran := 0
	for _, dir := range bd.dirs {
		if dir == "" {
			continue
		}
		ran++
		if !isWithin(under, dir) {
			t.Errorf("bd ran in %s, want a copy under the scratch %s named from the working directory", dir, under)
		}
	}
	if ran == 0 {
		t.Fatal("bd ran in no copy, want the rows run in one")
	}
}

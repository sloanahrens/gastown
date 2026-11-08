package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/callercwd"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/lock"
	"github.com/steveyegge/gastown/internal/workspace"
)

// writeCallerTown creates a town whose gastown rig exists but whose crew/eagle
// worktree does not, as in the pod: the town is mounted, the session-owned
// worktree is not.
func writeCallerTown(t *testing.T) (townRoot, callerPath string) {
	t.Helper()
	townRoot = t.TempDir()
	writeTownRoot(t, townRoot)
	if err := os.MkdirAll(filepath.Join(townRoot, "gastown", "crew"), 0755); err != nil {
		t.Fatalf("mkdir crew: %v", err)
	}
	return townRoot, filepath.Join(townRoot, "gastown", "crew", "eagle")
}

// writeTownRoot makes dir a town root: the primary marker the resolvers look
// for.
func writeTownRoot(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "mayor"), 0755); err != nil {
		t.Fatalf("mkdir mayor: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "mayor", "town.json"), []byte(`{"type":"town"}`), 0644); err != nil {
		t.Fatalf("write town.json: %v", err)
	}
}

// TestGtPrimeWithCallerCwd pins the pod target's prime: running in a directory
// that lacks the caller's crew path, with GT_CALLER_CWD naming that path, prime
// resolves that crew seat's identity and runs its external tools in the
// caller's directory; and gt done, which runs git there, refuses because the
// directory is not present.
//
//testpolicy:allow parallel — GT_CALLER_CWD is the input under test, which t.Setenv shares with the whole process
func TestGtPrimeWithCallerCwd(t *testing.T) {
	townRoot, callerPath := writeCallerTown(t)
	//testpolicy:allow no-env — GT_CALLER_CWD is the input under test
	t.Setenv(callercwd.EnvCallerCwd, callerPath)

	cwd, gotTown, err := resolvePrimeWorkspace()
	if err != nil {
		t.Fatalf("resolvePrimeWorkspace: %v", err)
	}
	if gotTown != townRoot {
		t.Fatalf("resolvePrimeWorkspace town = %q, want %q", gotTown, townRoot)
	}
	if cwd != callerPath {
		t.Fatalf("resolvePrimeWorkspace cwd = %q, want the caller's directory %q", cwd, callerPath)
	}

	info, err := GetRoleWithContext(cwd, gotTown)
	if err != nil {
		t.Fatalf("GetRoleWithContext: %v", err)
	}
	if info.Role != RoleCrew || info.Rig != "gastown" || info.Polecat != "eagle" {
		t.Fatalf("role = %s/%s/%s, want gastown/crew/eagle", info.Rig, info.Role, info.Polecat)
	}
	if got := info.ActorString(); got != "gastown/crew/eagle" {
		t.Fatalf("actor = %q, want gastown/crew/eagle", got)
	}

	// prime's external tools run through the fake runner, and every call it
	// sees must be made in the caller's directory, not this process's.
	f := &fakePrimeRunner{answers: map[string]string{
		primeKVListCall:    `{}`,
		primeMailCheckCall: "\n",
	}}
	p, _, _ := newFakePrimeTools(f)
	ctx := RoleContext{Role: info.Role, Rig: info.Rig, Polecat: info.Polecat, TownRoot: gotTown, WorkDir: cwd}
	p.memoryInject(ctx, cwd, nil)
	p.mailInject(cwd)
	if len(f.calls) == 0 {
		t.Fatalf("prime's external tools did not run; want the memories and mail calls")
	}
	for _, call := range f.calls {
		if call.workDir != callerPath {
			t.Errorf("prime ran %q in %q, want the caller's directory %q", call.line, call.workDir, callerPath)
		}
	}

	t.Run("gt prime refuses the absent caller directory before any check is skipped", func(t *testing.T) {
		err := runPrime(primeCmd, nil)
		if err == nil {
			t.Fatalf("runPrime = nil, want a refusal: the worktree check and the identity lock cannot run for %s", callerPath)
		}
		want := fmt.Sprintf("the caller's directory %s is not present here", callerPath)
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("runPrime error = %q, want it to name %q", err, want)
		}
	})

	t.Run("gt done refuses the absent caller directory", func(t *testing.T) {
		err := runDone(nil, nil)
		if err == nil {
			t.Fatalf("runDone = nil, want the not-present refusal")
		}
		want := fmt.Sprintf("the caller's directory %s is not present here", callerPath)
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("runDone error = %q, want it to name %q", err, want)
		}
	})
}

// TestCommandsRequireTheCallerDirectory pins the guard on the callers that run
// git or write in the caller's worktree: each refuses an absent directory with
// the specific error instead of acting on this process's directory.
//
//testpolicy:allow parallel — GT_CALLER_CWD is the input under test, which t.Setenv shares with the whole process
func TestCommandsRequireTheCallerDirectory(t *testing.T) {
	callerPath := filepath.Join(t.TempDir(), "crew", "eagle")
	//testpolicy:allow no-env — GT_CALLER_CWD is the input under test
	t.Setenv(callercwd.EnvCallerCwd, callerPath)

	cases := []struct {
		name string
		run  func() error
	}{
		{name: "gt commit", run: func() error { return runCommit(nil, nil) }},
		{name: "gt crew", run: func() error { _, err := detectCrewFromCwd(); return err }},
		{name: "gt mol step done", run: func() error { return runMoleculeStepDone(nil, []string{"gt-abc.1"}) }},
	}
	want := fmt.Sprintf("the caller's directory %s is not present here", callerPath)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.run()
			if err == nil {
				t.Fatalf("%s = nil, want the not-present refusal", tc.name)
			}
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("%s error = %q, want it to name %q", tc.name, err, want)
			}
		})
	}
}

// TestIdentityReadsRefuseAnUnusableCallerDirectory pins that the identity
// call sites return the variable's refusal instead of falling back to the
// process directory.
//
//testpolicy:allow parallel — GT_CALLER_CWD is the input under test, which t.Setenv shares with the whole process
func TestIdentityReadsRefuseAnUnusableCallerDirectory(t *testing.T) {
	//testpolicy:allow no-env — GT_CALLER_CWD is the input under test
	t.Setenv(callercwd.EnvCallerCwd, "gastown/crew/eagle")

	// --role is what runAgentsResolve needs before it reads the directory; the
	// flag is set through its own flag set so the value is the command's.
	if err := agentsResolveCmd.Flags().Set("role", "crew"); err != nil {
		t.Fatalf("set --role: %v", err)
	}
	t.Cleanup(func() { _ = agentsResolveCmd.Flags().Set("role", "") })

	cases := []struct {
		name string
		run  func() error
	}{
		{name: "gt prime", run: func() error { _, _, err := resolvePrimeWorkspace(); return err }},
		{name: "gt bead", run: func() error {
			return withBeadVerbs(&cobra.Command{}, func(*beadVerbs) error { return nil })
		}},
		{name: "gt agents resolve", run: func() error { return runAgentsResolve(&cobra.Command{}, nil) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.run()
			if err == nil {
				t.Fatalf("%s = nil, want the variable's refusal", tc.name)
			}
			if !strings.Contains(err.Error(), callercwd.EnvCallerCwd) {
				t.Fatalf("%s error = %q, want it to name %s", tc.name, err, callercwd.EnvCallerCwd)
			}
		})
	}
}

// TestBeadVerbsRefuseAnUnusableCallerDirectory pins the identity read: a
// GT_CALLER_CWD that cannot name a directory is an error, not a silent fall
// back to this process's directory.
//
//testpolicy:allow parallel — GT_CALLER_CWD is the input under test, which t.Setenv shares with the whole process
func TestBeadVerbsRefuseAnUnusableCallerDirectory(t *testing.T) {
	t.Run("unusable", func(t *testing.T) {
		//testpolicy:allow no-env — GT_CALLER_CWD is the input under test
		t.Setenv(callercwd.EnvCallerCwd, "gastown/crew/eagle")

		bv, err := newBeadVerbs(&cobra.Command{})
		if err == nil {
			t.Fatalf("newBeadVerbs = %+v, want the variable's refusal", bv)
		}
		if !strings.Contains(err.Error(), callercwd.EnvCallerCwd) {
			t.Fatalf("newBeadVerbs error = %q, want it to name %s", err, callercwd.EnvCallerCwd)
		}
	})
}

// TestPrimeRefusesAnUnreadableCallerDirectory pins the refusal when the caller
// directory cannot be read for a reason other than absence (here a component
// of the path is a file): a failed check is never read as absent.
//
//testpolicy:allow parallel — GT_CALLER_CWD is the input under test, which t.Setenv shares with the whole process
func TestPrimeRefusesAnUnreadableCallerDirectory(t *testing.T) {
	townRoot := t.TempDir()
	writeTownRoot(t, townRoot)
	// gastown is a file, so statting anything below it is ENOTDIR: neither a
	// usable directory nor one that is merely absent.
	if err := os.WriteFile(filepath.Join(townRoot, "gastown"), []byte("not a directory"), 0644); err != nil {
		t.Fatalf("write gastown file: %v", err)
	}
	//testpolicy:allow no-env — GT_CALLER_CWD is the input under test
	t.Setenv(callercwd.EnvCallerCwd, filepath.Join(townRoot, "gastown", "crew", "eagle"))

	err := runPrime(&cobra.Command{}, nil)
	if err == nil {
		t.Fatalf("runPrime = nil, want the unreadable-directory refusal")
	}
	if !strings.Contains(err.Error(), "checking the caller's directory") {
		t.Fatalf("runPrime error = %q, want the stat failure", err)
	}
}

// TestPrimeRefusesAWorktreeWithoutGitMetadata pins prime's worktree check for a
// caller directory that is present here: metadata that is missing is a
// refusal, not a skip.
//
//testpolicy:allow parallel — GT_CALLER_CWD is the input under test, which t.Setenv shares with the whole process
func TestPrimeRefusesAWorktreeWithoutGitMetadata(t *testing.T) {
	_, callerPath := writeCallerTown(t)
	if err := os.MkdirAll(callerPath, 0755); err != nil {
		t.Fatalf("mkdir caller directory: %v", err)
	}
	//testpolicy:allow no-env — GT_CALLER_CWD is the input under test
	t.Setenv(callercwd.EnvCallerCwd, callerPath)

	err := runPrime(&cobra.Command{}, nil)
	if err == nil {
		t.Fatalf("runPrime = nil, want the worktree integrity refusal")
	}
	if !strings.Contains(err.Error(), "missing .git metadata") {
		t.Fatalf("runPrime error = %q, want the missing-metadata refusal", err)
	}
}

// TestHookIntegrityFailsClosed pins the hook integrity guard: an absent (or
// unreadable) directory is a FAILURE that blocks, never a silent pass. The
// guard reads the real process directory through an injected getwd, so the
// test never depends on where it runs.
//
//testpolicy:allow parallel — GT_CALLER_CWD is the input under test in one row, which t.Setenv shares with the whole process
func TestHookIntegrityFailsClosed(t *testing.T) {
	townRoot := t.TempDir()
	writeTownRoot(t, townRoot)
	crewWorktree := filepath.Join(townRoot, "gastown", "crew", "eagle")

	cases := []struct {
		name  string
		getwd func() (string, error)
	}{
		{name: "absent directory", getwd: func() (string, error) { return filepath.Join(crewWorktree, "gastown"), nil }},
		{name: "a file, not a directory", getwd: func() (string, error) { return filepath.Join(townRoot, "mayor", "town.json"), nil }},
		{name: "unreadable path", getwd: func() (string, error) {
			return filepath.Join(crewWorktree, "gastown", "x"), nil
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := hookWorktreeIntegrityIn(tc.getwd, workspace.Find)
			if err == nil {
				t.Fatalf("hookWorktreeIntegrityIn = nil, want the integrity refusal (fail closed)")
			}
		})
	}

	t.Run("a town lookup failure blocks", func(t *testing.T) {
		err := hookWorktreeIntegrityIn(
			func() (string, error) { return townRoot, nil },
			func(string) (string, error) { return "", fmt.Errorf("unreadable town tree") },
		)
		if err == nil {
			t.Fatalf("hookWorktreeIntegrityIn = nil, want the town-lookup failure (fail closed)")
		}
	})

	// A variable naming a present, valid worktree must not turn the absent
	// process directory's failure into a pass.
	t.Run("the caller variable does not open the guard", func(t *testing.T) {
		valid := filepath.Join(townRoot, "gastown", "crew", "valid")
		if err := os.MkdirAll(valid, 0755); err != nil {
			t.Fatalf("mkdir valid worktree: %v", err)
		}
		writeGitMetadata(t, valid)
		//testpolicy:allow no-env — GT_CALLER_CWD is the input under test
		t.Setenv(callercwd.EnvCallerCwd, valid)

		err := hookWorktreeIntegrityIn(func() (string, error) { return crewWorktree, nil }, workspace.Find)
		if err == nil {
			t.Fatalf("hookWorktreeIntegrityIn = nil, want the absent worktree refused despite GT_CALLER_CWD")
		}
	})
}

// writeGitMetadata makes dir a valid regular clone: a .git directory with the
// HEAD that worktree integrity requires.
func writeGitMetadata(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0644); err != nil {
		t.Fatalf("write HEAD: %v", err)
	}
}

// TestSetupPrimeSessionRefusesAnAbsentWorkDir is the identity lock's fail-closed
// case: when the work directory is absent in this process, setupPrimeSession
// does not skip the lock, it refuses. A lock nobody took lets two sessions share
// an identity. The lock's liveness is injected, so the test never depends on the
// host's processes.
func TestSetupPrimeSessionRefusesAnAbsentWorkDir(t *testing.T) {
	t.Parallel()
	workDir := filepath.Join(t.TempDir(), "no", "such", "worktree")

	ctx := RoleContext{Role: RoleCrew, Rig: "gastown", Polecat: "eagle", TownRoot: workDir, WorkDir: workDir}
	info := RoleInfo{Role: RoleCrew, Rig: "gastown", Polecat: "eagle"}

	err := setupPrimeSession(ctx, info, lock.NewWithLiveness(workDir, liveTable))
	if err == nil || !strings.Contains(err.Error(), "is not present here") {
		t.Fatalf("setupPrimeSession with an absent work directory = %v, want a refusal naming that it is not present", err)
	}
	if _, statErr := os.Stat(workDir); statErr == nil {
		t.Errorf("setupPrimeSession created %s while refusing", workDir)
	}
}

// TestIdentityCollisionUsesInjectedLiveness pins that a held identity lock is
// reported. Liveness is injected, so the verdict never depends on a real
// process being alive.
func TestIdentityCollisionUsesInjectedLiveness(t *testing.T) {
	t.Parallel()
	workDir := t.TempDir()
	writeHeldLock(t, workDir)

	ctx := RoleContext{Role: RoleCrew, Rig: "gastown", Polecat: "eagle", TownRoot: workDir, WorkDir: workDir}
	err := claimIdentityLock(lock.NewWithLiveness(workDir, liveTable), ctx)
	if err == nil {
		t.Fatalf("claimIdentityLock = nil, want the identity collision")
	}
	if !strings.Contains(err.Error(), "cannot claim identity") {
		t.Fatalf("claimIdentityLock error = %q, want the collision refusal", err)
	}
}

// TestSetupPrimeSessionRefusesAnIdentityCollision pins that prime's session
// setup propagates the collision: with the caller directory present, a held
// lock is an error, not a silent overwrite.
func TestSetupPrimeSessionRefusesAnIdentityCollision(t *testing.T) {
	t.Parallel()
	workDir := t.TempDir()
	writeHeldLock(t, workDir)

	ctx := RoleContext{Role: RoleCrew, Rig: "gastown", Polecat: "eagle", TownRoot: workDir, WorkDir: workDir}
	info := RoleInfo{Role: RoleCrew, Rig: "gastown", Polecat: "eagle"}

	err := setupPrimeSession(ctx, info, lock.NewWithLiveness(workDir, liveTable))
	if err == nil {
		t.Fatalf("setupPrimeSession = nil, want the identity collision")
	}
	if !strings.Contains(err.Error(), "cannot claim identity") {
		t.Fatalf("setupPrimeSession error = %q, want the collision refusal", err)
	}
}

// liveTable reports the process the lock names as alive with the recorded
// start token, the way the real table reads a running holder.
func liveTable(int) (string, bool) { return "start-token", true }

// writeHeldLock writes an identity lock for a pid that no real host is running,
// so only a scripted table can call it alive.
func writeHeldLock(t *testing.T, workDir string) {
	t.Helper()
	runtimeDir := filepath.Join(workDir, ".runtime")
	if err := os.MkdirAll(runtimeDir, 0755); err != nil {
		t.Fatalf("mkdir .runtime: %v", err)
	}
	held, err := json.Marshal(lock.LockInfo{
		PID:        999999999,
		Start:      "start-token",
		AcquiredAt: time.Now(),
		SessionID:  "other-agent",
	})
	if err != nil {
		t.Fatalf("marshal lock: %v", err)
	}
	if err := os.WriteFile(filepath.Join(runtimeDir, "agent.lock"), held, 0644); err != nil {
		t.Fatalf("write lock: %v", err)
	}
}

// TestGuardsIgnoreTheCallerCwd is the guard table: for each guard that resolves
// the working directory, GT_CALLER_CWD set to a permitted directory while the
// process is in a forbidden one must leave the verdict exactly as it was with
// the variable unset. A guard that started reading the variable would flip
// each row.
//
//testpolicy:allow parallel — GT_CALLER_CWD is the input under test, which t.Setenv shares with the whole process
func TestGuardsIgnoreTheCallerCwd(t *testing.T) {
	towns := t.TempDir()
	forbiddenTown := filepath.Join(towns, "forbidden")
	writeTownRoot(t, forbiddenTown)
	if err := os.MkdirAll(filepath.Join(forbiddenTown, "gastown", "crew"), 0755); err != nil {
		t.Fatalf("mkdir forbidden town rig: %v", err)
	}
	// A crew worktree with no .git metadata: the hook integrity check blocks it.
	forbiddenCrew := filepath.Join(forbiddenTown, "gastown", "crew", "eagle")
	if err := os.MkdirAll(forbiddenCrew, 0755); err != nil {
		t.Fatalf("mkdir forbidden worktree: %v", err)
	}

	// Permitted: a directory outside any town where each guard would allow.
	permitted := t.TempDir()

	// The production hook path: the same entry the hook commands call, with the
	// process directory seam pointed at the forbidden worktree. The variable is
	// set around these calls, so a guard that read it would flip the verdict.
	inForbiddenCrew := func(run func() error) error {
		prev := hookWorktreeIntegrityGetwd
		//testpolicy:allow no-global-swap — the production getwd seam of the hook integrity guard, restored below
		hookWorktreeIntegrityGetwd = func() (string, error) { return forbiddenCrew, nil }
		//testpolicy:allow no-global-swap — restores the seam swapped above
		defer func() { hookWorktreeIntegrityGetwd = prev }()
		return run()
	}
	runHookIntegrity := func() error {
		return inForbiddenCrew(ensureCurrentHookWorktreeIntegrity)
	}
	runHookShowCommand := func() error {
		return inForbiddenCrew(func() error { return runHookShow(nil, nil) })
	}
	runHookCommand := func() error {
		return inForbiddenCrew(func() error { return runHook(nil, []string{"gt-abc"}) })
	}

	runDangerous := func() error {
		goTree := t.TempDir()
		if err := os.WriteFile(filepath.Join(goTree, "go.mod"), []byte("module example.com/gated\n"), 0644); err != nil {
			t.Fatalf("write go.mod: %v", err)
		}
		proc := fakeGuardProcess(nil, goTree)
		// A busy host holds the whole-module suite start, and a busy host is
		// what makes the verdict depend on the directory the guard reads.
		proc.cpuIdle = func() (float64, bool) { return 10, true }
		payload := `{"tool_name":"Bash","tool_input":{"command":"make test"}}`
		return tapGuardDangerous(strings.NewReader(payload), io.Discard, proc)
	}

	runPRWorkflow := func() error {
		// A crew worktree is an agent context, which blocks a PR command.
		proc := fakeGuardProcess(nil, forbiddenCrew)
		payload := `{"tool_name":"Bash","tool_input":{"command":"gh pr create --title x"}}`
		return tapGuardPRWorkflow(strings.NewReader(payload), io.Discard, proc)
	}

	// The hermetic harness leaves GT_CALLER_CWD unset, so the baseline runs
	// with the variable unset.
	baseline := map[string]error{
		"hook integrity":    runHookIntegrity(),
		"gt hook show":      runHookShowCommand(),
		"gt hook <bead>":    runHookCommand(),
		"dangerous-command": runDangerous(),
		"pr-workflow":       runPRWorkflow(),
	}
	//testpolicy:allow no-env — GT_CALLER_CWD is the input under test
	t.Setenv(callercwd.EnvCallerCwd, permitted)

	for _, tc := range []struct {
		name string
		run  func() error
	}{
		{name: "hook integrity", run: runHookIntegrity},
		{name: "gt hook show", run: runHookShowCommand},
		{name: "gt hook <bead>", run: runHookCommand},
		{name: "dangerous-command", run: runDangerous},
		{name: "pr-workflow", run: runPRWorkflow},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if baseline[tc.name] == nil {
				t.Fatalf("%s allowed the forbidden process directory with the variable unset; the row needs a blocking baseline", tc.name)
			}
			got := tc.run()
			if got == nil {
				t.Fatalf("%s allowed the forbidden process directory once GT_CALLER_CWD named a permitted one; the guard read the variable", tc.name)
			}
			if got.Error() != baseline[tc.name].Error() {
				t.Fatalf("%s verdict changed with GT_CALLER_CWD set: %q, want %q", tc.name, got, baseline[tc.name])
			}
		})
	}
}

// TestHookIntegrityReadsTheProcessDirectory pins the production seam itself, not
// only the verdict behind an injected one: the hook integrity check's directory
// source IS os.Getwd, so a change that points it at GT_CALLER_CWD (directly or
// through a wrapper) fails here even though the guard table injects its own.
// With the variable naming an absent path and the real seam in place, the
// verdict is about the process directory and never names the variable's path.
//
//testpolicy:allow parallel — GT_CALLER_CWD is the input under test, which t.Setenv shares with the whole process
func TestHookIntegrityReadsTheProcessDirectory(t *testing.T) {
	if reflect.ValueOf(hookWorktreeIntegrityGetwd).Pointer() != reflect.ValueOf(os.Getwd).Pointer() {
		t.Fatalf("hookWorktreeIntegrityGetwd is not os.Getwd: the hook integrity guard must read the real directory")
	}
	absent := filepath.Join(t.TempDir(), "not", "here")
	//testpolicy:allow no-env — GT_CALLER_CWD is the input under test
	t.Setenv(callercwd.EnvCallerCwd, absent)
	if err := ensureCurrentHookWorktreeIntegrity(); err != nil && strings.Contains(err.Error(), absent) {
		t.Fatalf("the hook integrity guard named the variable's path %s: %v", absent, err)
	}
}

// TestCallerCwdReachesTheConvertedCommands pins the commands whose directory
// read moved behind internal/callercwd (gt-yjzepk.2): prime, handoff, mail
// identity and hooks install. With GT_CALLER_CWD unset each acts on this
// process's directory, as before the seam; with it set each acts on the
// caller's directory instead. The handoff read runs through its git client
// seam, so the test builds no repository and starts no process.
//
//testpolicy:allow parallel — GT_CALLER_CWD is the input under test, which t.Setenv shares with the whole process
func TestCallerCwdReachesTheConvertedCommands(t *testing.T) {
	processDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("process directory: %v", err)
	}

	townRoot, callerPath := writeCallerTown(t)
	if err := os.MkdirAll(callerPath, 0755); err != nil {
		t.Fatalf("mkdir caller directory: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(callerPath, ".beads"), 0755); err != nil {
		t.Fatalf("mkdir caller beads: %v", err)
	}
	//testpolicy:allow no-env — BEADS_DIR is held empty so the cwd walk is the input under test
	t.Setenv("BEADS_DIR", "")

	var handoffDir string
	previousGit := handoffGitFor
	//testpolicy:allow no-global-swap — the git client seam collectGitState reads, restored below
	handoffGitFor = func(dir string) handoffGit {
		handoffDir = dir
		return fakeHandoffGit{notRepo: true}
	}
	//testpolicy:allow no-global-swap — restores the seam swapped above
	t.Cleanup(func() { handoffGitFor = previousGit })

	rigCrew := filepath.Join(townRoot, "gastown", "crew")

	// The hermetic harness leaves GT_CALLER_CWD unset, so the baseline shows
	// each command on this process's directory, as it was before the seam.
	if cwd, _, err := resolvePrimeWorkspace(); err == nil && cwd != processDir {
		t.Fatalf("gt prime with the variable unset read %q, want the process directory %q", cwd, processDir)
	}
	if got, err := findLocalBeadsDir(); err == nil && got == callerPath {
		t.Fatalf("findLocalBeadsDir with the variable unset read the fixture %q; it must read this process's directory", got)
	}
	if got := collectGitState(); got != "" {
		t.Fatalf("collectGitState with the variable unset returned %q for a scripted non-repository", got)
	}
	if handoffDir != processDir {
		t.Fatalf("gt handoff with the variable unset read git in %q, want the process directory %q", handoffDir, processDir)
	}
	if targets, err := determineTargets(townRoot, constants.RoleCrew, false, []string{constants.RoleCrew}); err != nil {
		t.Fatalf("gt hooks install with the variable unset: %v", err)
	} else if len(targets) == 1 && targets[0] == rigCrew {
		t.Fatalf("gt hooks install with the variable unset resolved the caller's rig %q; it must read this process's directory", rigCrew)
	}

	//testpolicy:allow no-env — GT_CALLER_CWD is the input under test
	t.Setenv(callercwd.EnvCallerCwd, callerPath)

	cwd, gotTown, err := resolvePrimeWorkspace()
	if err != nil || cwd != callerPath || gotTown != townRoot {
		t.Fatalf("gt prime = (%q, %q, %v), want the caller's seat (%q, %q)", cwd, gotTown, err, callerPath, townRoot)
	}
	if got, err := findLocalBeadsDir(); err != nil || got != callerPath {
		t.Fatalf("findLocalBeadsDir = %q, %v, want the caller's directory %q", got, err, callerPath)
	}
	if got := collectGitState(); got != "" {
		t.Fatalf("collectGitState returned %q for a scripted non-repository", got)
	}
	if handoffDir != callerPath {
		t.Fatalf("gt handoff read git in %q, want the caller's directory %q", handoffDir, callerPath)
	}
	targets, err := determineTargets(townRoot, constants.RoleCrew, false, []string{constants.RoleCrew})
	if err != nil || len(targets) != 1 || targets[0] != rigCrew {
		t.Fatalf("gt hooks install targets = %v, %v, want the caller's rig crew settings %q", targets, err, rigCrew)
	}
}

// TestConvertedCommandsRefuseAnAbsentCallerDirectory drives the refusal branch
// each Require added by gt-yjzepk.2 opens: the command reads the caller's
// directory before the state collection, the mail or the pane, so an absent
// one is refused rather than replaced by this process's.
//
//testpolicy:allow parallel — GT_CALLER_CWD is the input under test, which t.Setenv shares with the whole process
func TestConvertedCommandsRefuseAnAbsentCallerDirectory(t *testing.T) {
	callerPath := filepath.Join(t.TempDir(), "crew", "eagle")
	//testpolicy:allow no-env — GT_CALLER_CWD is the input under test
	t.Setenv(callercwd.EnvCallerCwd, callerPath)

	cases := []struct {
		name string
		run  func() error
	}{
		{name: "gt handoff", run: func() error {
			return handoffSession(sessionHandoff{}, handoffSteps{})
		}},
		{name: "gt handoff --auto", run: runHandoffAuto},
		{name: "gt handoff --cycle", run: runHandoffCycle},
		{name: "gt hooks install", run: func() error {
			return runHooksInstall(&cobra.Command{}, []string{"pr-workflow-guard"})
		}},
	}
	want := fmt.Sprintf("the caller's directory %s is not present here", callerPath)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.run()
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("%s = %v, want the not-present refusal %q", tc.name, err, want)
			}
		})
	}
}

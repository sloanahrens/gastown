package polecat

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/rig"
	"github.com/steveyegge/gastown/internal/tmux/tmuxfake"
	"github.com/steveyegge/gastown/internal/util"
)

// agentShowJSON is the agent bead the bd stand-ins answer `show` with: an
// idle polecat whose fields parse the way a real agent bead's do.
// withCleanup=false leaves cleanup_status out, as for a polecat that never
// reported one.
func agentShowJSON(id string, withCleanup bool) string {
	desc := `agent\n\nrole_type: polecat\nagent_state: idle\nhook_bead: null\n`
	if withCleanup {
		desc += `cleanup_status: clean\n`
	}
	desc += `active_mr: null\nbranch: polecat/toast/gt-work@abc123`
	return fmt.Sprintf(`[{"id":%q,"title":"agent","issue_type":"agent","description":"%s"}]`+"\n", id, desc)
}

// bdCommand is the bd subcommand in args: the first argument that is not a
// flag, the way the shell stubs these replace picked it.
func bdCommand(args []string) (cmd string, rest []string) {
	for i, a := range args {
		if !strings.HasPrefix(a, "--") {
			return a, args[i+1:]
		}
	}
	return "", nil
}

// fakeBd answers a Manager's bd calls in process, in place of a bd stub put
// on PATH (which no parallel test may do). It records every call's argv.
type fakeBd struct {
	mu     sync.Mutex
	answer func(cmd string, args []string) string
	// answerCall, when set, answers instead of answer and also sees the
	// call's directory and environment.
	answerCall func(c beads.BDCall) string
	calls      []beads.BDCall
}

func (f *fakeBd) run(_ context.Context, c beads.BDCall) ([]byte, []byte, error) {
	cmd, rest := bdCommand(c.Args)
	f.mu.Lock()
	f.calls = append(f.calls, beads.BDCall{Dir: c.Dir, Env: append([]string(nil), c.Env...), Args: append([]string(nil), c.Args...)})
	answer, answerCall := f.answer, f.answerCall
	f.mu.Unlock()
	if cmd == "version" {
		// The --allow-stale probe: answering nothing keeps argv unprefixed.
		return nil, nil, nil
	}
	var out string
	if answerCall != nil {
		out = answerCall(c)
	} else {
		out = answer(cmd, rest)
	}
	if out == bdMissing {
		return nil, nil, &exec.Error{Name: "bd", Err: exec.ErrNotFound}
	}
	if out == "" && (cmd == "list" || cmd == "query") && slices.Contains(c.Args, "--json") {
		// The fork's --json list/query prints "[]" for no rows, never nothing;
		// gastown treats empty output from a --json call as a failed read.
		out = "[]"
	}
	if msg, failed := strings.CutPrefix(out, bdFailure); failed {
		return nil, []byte(msg), bdExit{1}
	}
	return []byte(out), nil, nil
}

// bdFailure marks an answer as bd's stderr on a failed call (exit 1).
const bdFailure = "\x00fail:"

// bdMissing answers as if there were no bd on PATH at all.
const bdMissing = "\x00missing"

// newMissingBd is a host with no bd installed.
func newMissingBd() *fakeBd {
	return &fakeBd{answer: func(string, []string) string { return bdMissing }}
}

// bdExit is a bd exit status, as *exec.ExitError carries one.
type bdExit struct{ code int }

func (e bdExit) Error() string { return fmt.Sprintf("exit status %d", e.code) }
func (e bdExit) ExitCode() int { return e.code }

// noDatabaseAnswer is what the real bd says, on stderr with exit 1, to any
// call in a directory with no beads database.
const noDatabaseAnswer = bdFailure + "Error: no beads database found\n" +
	"Hint: run 'bd where' to inspect the resolved workspace, or 'bd init' to create a new database\n" +
	"      or set BEADS_DIR to point to your .beads directory\n"

// newNoDatabaseBd is bd in a rig with no beads database, what a test with no
// bd set up used to reach through the real bd on PATH.
func newNoDatabaseBd() *fakeBd {
	return &fakeBd{answer: func(string, []string) string { return noDatabaseAnswer }}
}

// become switches f to answer like other from now on, as a test that put a
// different bd stub first on PATH mid-test did.
func (f *fakeBd) become(other *fakeBd) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answer = other.answer
}

// argvs returns every recorded call but the --allow-stale capability probe,
// joined with spaces, as the shell stubs logged them.
func (f *fakeBd) argvs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.calls))
	for _, c := range f.calls {
		if cmd, _ := bdCommand(c.Args); cmd == "version" {
			continue
		}
		out = append(out, strings.Join(c.Args, " "))
	}
	return out
}

// envValue is key's value in env, and whether it is set.
func envValue(env []string, key string) (string, bool) {
	for i := len(env) - 1; i >= 0; i-- {
		if v, ok := strings.CutPrefix(env[i], key+"="); ok {
			return v, true
		}
	}
	return "", false
}

// createAnswer is the stubs' `create` reply: the requested --id, open.
func createAnswer(args []string) string {
	id := "mock-1"
	for _, a := range args {
		if v, ok := strings.CutPrefix(a, "--id="); ok {
			id = v
		}
	}
	return fmt.Sprintf(`{"id":%q,"status":"open","created_at":"2025-01-01T00:00:00Z"}`+"\n", id)
}

// showID is the issue ID a `show` call names.
func showID(args []string) string {
	for _, a := range args {
		if !strings.HasPrefix(a, "--") {
			return a
		}
	}
	return ""
}

// newAgentBd is the bd AddWithOptions and friends need: creates succeed,
// writes succeed silently, and every show is an idle agent bead.
// withCleanup=false omits cleanup_status from that bead.
func newAgentBd(withCleanup bool) *fakeBd {
	return &fakeBd{answer: func(cmd string, args []string) string {
		switch cmd {
		case "create":
			return createAnswer(args)
		case "show":
			return agentShowJSON(showID(args), withCleanup)
		}
		return ""
	}}
}

// newEmptyBd is a bd with no issues: show and list answer an empty array.
func newEmptyBd() *fakeBd {
	return &fakeBd{answer: func(cmd string, _ []string) string {
		if cmd == "show" || cmd == "list" {
			return "[]\n"
		}
		return ""
	}}
}

// newTestManager is NewManager with a fake bd, a fake tmux when tm is
// non-nil, and git answered by w (a fresh, empty world when nil). Nothing it
// builds reads PATH, reaches a tmux server or runs git.
//
// With a fake bd, the rig's own config reads (the rig identity bead) go to it
// too, and when the rig's beads directory exists the types sentinel is
// written there: beads.EnsureCustomTypes runs bd itself, outside any runner,
// unless the sentinel says the custom types are already configured.
func newTestManager(r *rig.Rig, w *world, tm sessionProbe, bd *fakeBd) *Manager {
	var run beads.BDRunner
	if bd != nil {
		run = bd.run
		if r.BDRunner == nil {
			r.BDRunner = run
		}
		markTypesConfigured(beads.ResolveBeadsDir(r.Path))
	}
	if w == nil {
		w = newWorld()
	}
	m := newManager(r, w.repo(r.Path), tm, run)
	m.gits = w.opener()
	// rig.EnsureLocalExcludePatterns runs git itself, so the test writes its
	// patterns into the world's exclude file directly.
	m.ensureExcludes = func(worktreePath string) error { return writeLocalExcludes(w, worktreePath) }
	// util.CheckDiskSpace runs diskutil on macOS; the disk is never full.
	m.diskSpace = func(string) (util.DiskSpaceLevel, string, error) { return util.DiskSpaceOK, "", nil }
	return m
}

// markTypesConfigured writes the custom-types sentinel into an existing
// beads directory.
func markTypesConfigured(beadsDir string) {
	if info, err := os.Stat(beadsDir); err != nil || !info.IsDir() {
		return
	}
	_ = os.WriteFile(filepath.Join(beadsDir, ".gt-types-configured"), []byte(beads.TypeConfigSentinelValue()+"\n"), 0o644)
}

// fakeProbe is a tmux for the Manager: tmuxfake's sessions, plus the agent
// liveness and pane pid answers isSessionProcessDead asks for.
type fakeProbe struct {
	*tmuxfake.Server

	mu       sync.Mutex
	alive    map[string]bool
	aliveErr error
	probed   []string
	pids     map[string]string
}

// testEpoch is the fixed start of every fake clock in this package.
var testEpoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func newFakeProbe() *fakeProbe {
	return &fakeProbe{
		Server: tmuxfake.New(clockwork.NewFakeClockAt(testEpoch)),
		alive:  map[string]bool{},
		pids:   map[string]string{},
	}
}

func (f *fakeProbe) IsAgentAliveChecked(session string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.probed = append(f.probed, session)
	if f.aliveErr != nil {
		return false, f.aliveErr
	}
	return f.alive[session], nil
}

func (f *fakeProbe) GetPanePID(target string) (string, error) {
	if ok, _ := f.HasSession(target); !ok {
		return "", fmt.Errorf("can't find pane: %s", target)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pids[target], nil
}

func (f *fakeProbe) setAlive(session string, alive bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.alive[session] = alive
}

func (f *fakeProbe) probedSessions() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.probed...)
}

// driveClock advances clk by step every time a goroutine blocks on it, until
// done delivers a value. It fails the test if nothing blocks within 10 s.
func driveClock[T any](t *testing.T, clk *clockwork.FakeClock, step time.Duration, done <-chan T) T {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for {
		blocked := make(chan error, 1)
		go func() { blocked <- clk.BlockUntilContext(ctx, 1) }()
		select {
		case v := <-done:
			return v
		case err := <-blocked:
			if err != nil {
				t.Fatalf("nothing blocked on the fake clock: %v", err)
			}
			clk.Advance(step)
		}
	}
}

// recorded returns a copy of every call f has answered.
func (f *fakeBd) recorded() []beads.BDCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]beads.BDCall(nil), f.calls...)
}

// localExcludePatterns mirrors rig.gasTownLocalExcludePatterns: what
// rig.EnsureLocalExcludePatterns writes into a worktree's info/exclude.
var localExcludePatterns = []string{
	".runtime/", ".claude/", ".opencode/", ".logs/", "__pycache__/", "state.json",
	"CLAUDE.md", "CLAUDE.local.md", "GEMINI.md", ".beads/",
}

// writeLocalExcludes is rig.EnsureLocalExcludePatterns for a checkout in w.
func writeLocalExcludes(w *world, worktreePath string) error {
	path, err := w.ExcludePath(worktreePath)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(strings.Join(localExcludePatterns, "\n")+"\n"), 0o644)
}

// Hermetic test harness (gt-lwi).
//
// gastown's tests frequently run from a worktree INSIDE a live Gas Town
// (~/gt): every polecat runs `go test ./...` from its own worktree. Three
// leak vectors have each caused production incidents:
//
//  1. Dolt: tests (or gt/bd subprocesses they spawn) connect to the
//     production Dolt server on :3307 and create testdb_*/beads_t* orphan
//     databases (hq-det, hq-4zrq).
//  2. Events: path resolution via workspace.FindFromCwd walks up out of the
//     sandbox and appends fixture events to the live ~/gt/.events.jsonl
//     (gt-x9o).
//  3. Ambient env: the polecat session exports GT_*/BD_* variables that
//     tests and their subprocesses silently inherit.
//
// This file provides the systemic fix: a process-wide harness for TestMain
// (StartHermetic/Finish, or the HermeticMain convenience wrapper) plus
// per-test helpers (HermeticTest, ScratchTown). The harness:
//
//   - scrubs all GT_*/BD_*/BEADS_* variables from the process environment,
//     so every subprocess inherits a clean env;
//   - redirects HOME, CLAUDE_CONFIG_DIR, XDG_* and GT_TOWN_ROOT into a
//     throwaway sandbox containing a minimal marker town;
//   - poisons GT_DOLT_PORT/BEADS_DOLT_PORT so any code path that tries to
//     reach a Dolt server without opting in via WithDolt fails fast with
//     "connection refused" instead of silently mutating production :3307;
//   - sets GT_TEST_HERMETIC=1, which gt subprocesses honor by suppressing
//     cwd-resolved event writes (see internal/events);
//   - snapshots the real town (if any) at startup and diffs it at Finish —
//     the tripwire: new files, databases, or unattributable events in the
//     live town fail the run even when every test passed.
package testutil

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/workspace"
)

// HermeticEnvVar marks a process tree as running under the hermetic test
// harness. gt subprocesses honor it (internal/events suppresses cwd-resolved
// writes when it is "1").
const HermeticEnvVar = "GT_TEST_HERMETIC"

// poisonDoltPort is an unroutable TCP port assigned to GT_DOLT_PORT and
// BEADS_DOLT_PORT by the harness. Port 1 is reserved and never carries a
// listener, so stray Dolt connections fail fast instead of reaching the
// production server on :3307. WithDolt overrides it with a real container
// port.
const poisonDoltPort = "1"

// doltPassthroughVars are preserved across the env scrub when an outer test
// runner has already provided an ephemeral Dolt server (GT_TEST_EXTERNAL_DOLT=1).
var doltPassthroughVars = []string{
	"GT_DOLT_PORT",
	"GT_DOLT_HOST",
	"BEADS_DOLT_PORT",
	"BEADS_DOLT_SERVER_HOST",
	"GT_TEST_EXTERNAL_DOLT",
}

// Hermetic holds the state of an active hermetic harness. Create one with
// StartHermetic (typically from TestMain) and call Finish after m.Run().
type Hermetic struct {
	// SandboxDir is the throwaway root holding home/, town/ and claude/.
	SandboxDir string
	// HomeDir is the sandbox HOME.
	HomeDir string
	// TownRoot is a minimal sandbox town (mayor/town.json present). Point
	// subprocess cwds and *To-style calls here when a town is needed.
	TownRoot string
	// RealTownRoot is the live town the test process is running inside, or
	// "" when not inside one. The tripwire watches it.
	RealTownRoot string

	cfg  hermeticConfig
	snap *townSnapshot
}

type hermeticConfig struct {
	dolt bool
}

// HermeticOption configures StartHermetic/HermeticMain.
type HermeticOption func(*hermeticConfig)

// WithDolt starts a shared ephemeral Dolt container for the package
// (EnsureDoltContainerForTestMain) and routes GT_DOLT_PORT/BEADS_DOLT_PORT to
// it. Without this option those variables stay poisoned and Dolt-touching
// code fails fast. When Docker is unavailable the harness warns and
// continues; Dolt-dependent tests then skip or fail on the poisoned port.
func WithDolt() HermeticOption {
	return func(c *hermeticConfig) { c.dolt = true }
}

// HermeticMain is the standard TestMain body:
//
//	func TestMain(m *testing.M) {
//		os.Exit(testutil.HermeticMain(m, testutil.WithDolt()))
//	}
//
// Packages that need extra setup around m.Run() (tmux sockets, flags) use
// StartHermetic/Finish directly instead.
func HermeticMain(m *testing.M, opts ...HermeticOption) int {
	h, err := StartHermetic(opts...)
	if err != nil {
		fmt.Fprintf(os.Stderr, "hermetic harness setup failed: %v\n", err)
		return 1
	}
	return h.Finish(m.Run())
}

// StartHermetic sandboxes the test process as described in the package
// comment and snapshots the surrounding live town for the tripwire. It
// mutates process-wide state (env, temp dirs) and is meant to be called once,
// from TestMain, before m.Run().
func StartHermetic(opts ...HermeticOption) (*Hermetic, error) {
	h := &Hermetic{}
	for _, opt := range opts {
		opt(&h.cfg)
	}

	// Identify the live town BEFORE scrubbing env or redirecting anything.
	if root, err := workspace.FindFromCwd(); err == nil && root != "" {
		h.RealTownRoot = root
	} else if root := os.Getenv("GT_TOWN_ROOT"); root != "" {
		if ok, _ := workspace.IsWorkspace(root); ok {
			h.RealTownRoot = root
		}
	}
	if h.RealTownRoot != "" {
		h.snap = snapshotTown(h.RealTownRoot)
	}

	// An outer harness (e.g. a test that runs `go test` as a subprocess) may
	// have provided an ephemeral Dolt server already; keep its routing.
	externalDolt := os.Getenv("GT_TEST_EXTERNAL_DOLT") == "1"

	scrubProcessEnv(externalDolt)

	sandbox, err := os.MkdirTemp("", "gt-hermetic-")
	if err != nil {
		return nil, fmt.Errorf("creating sandbox: %w", err)
	}
	h.SandboxDir = sandbox
	h.HomeDir = filepath.Join(sandbox, "home")
	h.TownRoot = filepath.Join(sandbox, "town")

	claudeDir := filepath.Join(sandbox, "claude")
	for _, dir := range []string{h.HomeDir, claudeDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("creating sandbox dir: %w", err)
		}
	}
	if err := writeSandboxTown(h.TownRoot); err != nil {
		return nil, err
	}
	if err := writeSandboxGitConfig(h.HomeDir); err != nil {
		return nil, err
	}

	// Redirecting HOME moves the Go toolchain's default cache locations
	// (GOPATH=$HOME/go etc.), which would make any `go` invocation from a
	// test re-download the module cache. Pin the current effective values
	// before HOME changes.
	preserveGoEnv()

	setenvs := map[string]string{
		"HOME":              h.HomeDir,
		"USERPROFILE":       h.HomeDir, // Windows HOME
		"CLAUDE_CONFIG_DIR": claudeDir,
		"XDG_CONFIG_HOME":   filepath.Join(h.HomeDir, ".config"),
		"XDG_CACHE_HOME":    filepath.Join(h.HomeDir, ".cache"),
		"XDG_STATE_HOME":    filepath.Join(h.HomeDir, ".state"),
		"GT_TOWN_ROOT":      h.TownRoot,
		HermeticEnvVar:      "1",
	}
	if !externalDolt {
		setenvs["GT_DOLT_PORT"] = poisonDoltPort
		setenvs["BEADS_DOLT_PORT"] = poisonDoltPort
	}
	for k, v := range setenvs {
		if err := os.Setenv(k, v); err != nil {
			return nil, fmt.Errorf("setting %s: %w", k, err)
		}
	}

	if h.cfg.dolt {
		// Replaces the poisoned port vars with the container's mapped port.
		if err := EnsureDoltContainerForTestMain(); err != nil {
			fmt.Fprintf(os.Stderr,
				"hermetic harness: Dolt container unavailable (%v); Dolt-dependent tests will skip or fail fast\n", err)
		}
	}

	return h, nil
}

// Finish tears down the sandbox and runs the tripwire against the live town
// snapshot. It returns the exit code for os.Exit: the m.Run() code, forced to
// 1 when the tripwire detects that tests leaked state into the live town.
func (h *Hermetic) Finish(code int) int {
	// No-op when no container was started; also covers containers started
	// lazily by tests via RequireDoltContainer.
	TerminateDoltContainer()
	if h.SandboxDir != "" {
		_ = os.RemoveAll(h.SandboxDir)
	}

	if h.snap != nil {
		if leaks := h.snap.diff(); len(leaks) > 0 {
			fmt.Fprintf(os.Stderr, "\n%s\n", strings.Repeat("=", 72))
			fmt.Fprintf(os.Stderr, "HERMETIC TRIPWIRE: tests leaked state into the live town at %s\n", h.snap.root)
			for _, l := range leaks {
				fmt.Fprintf(os.Stderr, "  - %s\n", l)
			}
			fmt.Fprintf(os.Stderr, "Tests must never write to a real town. Route writes through the\n")
			fmt.Fprintf(os.Stderr, "sandbox (testutil.Hermetic.TownRoot / ScratchTown) instead.\n")
			fmt.Fprintf(os.Stderr, "%s\n", strings.Repeat("=", 72))
			if code == 0 {
				code = 1
			}
		}
	}
	return code
}

// scrubProcessEnv removes every GT_*, BD_* and BEADS_* variable from the
// process environment so tests and their subprocesses cannot inherit live
// town context from the invoking agent session. When keepDolt is true the
// Dolt passthrough variables survive (an outer runner provided the server).
func scrubProcessEnv(keepDolt bool) {
	keep := map[string]bool{}
	if keepDolt {
		for _, k := range doltPassthroughVars {
			keep[k] = true
		}
	}
	for _, kv := range os.Environ() {
		name, _, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		if !strings.HasPrefix(name, "GT_") && !strings.HasPrefix(name, "BD_") && !strings.HasPrefix(name, "BEADS_") {
			continue
		}
		if keep[name] {
			continue
		}
		_ = os.Unsetenv(name)
	}
}

// writeSandboxGitConfig gives the sandbox HOME a deterministic git identity,
// since redirecting HOME hides the developer's ~/.gitconfig and git commands
// in tests would otherwise fail with "Please tell me who you are".
func writeSandboxGitConfig(home string) error {
	cfg := `[user]
	name = Hermetic Test
	email = hermetic@test.invalid
[init]
	defaultBranch = main
[commit]
	gpgsign = false
[tag]
	gpgsign = false
`
	if err := os.WriteFile(filepath.Join(home, ".gitconfig"), []byte(cfg), 0o644); err != nil {
		return fmt.Errorf("writing sandbox gitconfig: %w", err)
	}
	return nil
}

// preserveGoEnv pins GOPATH/GOCACHE/GOMODCACHE to their current effective
// values (asking the go tool) before HOME is redirected, so `go` invocations
// from tests keep using the real build and module caches. Best-effort: when
// the go tool is unavailable, nothing is pinned.
func preserveGoEnv() {
	vars := []string{"GOPATH", "GOCACHE", "GOMODCACHE"}
	var missing []string
	for _, v := range vars {
		if os.Getenv(v) == "" {
			missing = append(missing, v)
		}
	}
	if len(missing) == 0 {
		return
	}
	goBin, err := exec.LookPath("go")
	if err != nil {
		return
	}
	out, err := exec.Command(goBin, append([]string{"env"}, missing...)...).Output() //nolint:gosec // fixed args
	if err != nil {
		return
	}
	values := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	if len(values) != len(missing) {
		return
	}
	for i, v := range missing {
		if values[i] != "" {
			_ = os.Setenv(v, values[i])
		}
	}
}

// writeSandboxTown creates a minimal valid town (mayor/town.json marker) at
// root so workspace detection succeeds for code pointed at the sandbox.
func writeSandboxTown(root string) error {
	if err := os.MkdirAll(filepath.Join(root, "mayor"), 0o755); err != nil {
		return fmt.Errorf("creating sandbox town: %w", err)
	}
	town := `{"type":"town","version":2,"name":"hermetic-sandbox"}` + "\n"
	if err := os.WriteFile(filepath.Join(root, "mayor", "town.json"), []byte(town), 0o644); err != nil {
		return fmt.Errorf("writing sandbox town.json: %w", err)
	}
	return nil
}

// HermeticTest applies the hermetic env treatment to a single test: scrubs
// GT_*/BD_*/BEADS_* variables, redirects HOME/CLAUDE_CONFIG_DIR/GT_TOWN_ROOT
// into a per-test sandbox town, and poisons the Dolt port vars. Everything is
// restored via t.Cleanup. Returns the sandbox town root.
//
// Because it mutates process-wide env, it must not be combined with
// t.Parallel. Prefer the TestMain harness (HermeticMain) for whole packages;
// use this for spot isolation in packages that don't need it globally.
func HermeticTest(t testing.TB) string {
	t.Helper()

	// Save and restore the full environment: t.Setenv cannot unset variables,
	// and the scrub needs to remove them entirely.
	saved := os.Environ()
	t.Cleanup(func() {
		os.Clearenv()
		for _, kv := range saved {
			if name, val, ok := strings.Cut(kv, "="); ok {
				_ = os.Setenv(name, val)
			}
		}
	})

	scrubProcessEnv(os.Getenv("GT_TEST_EXTERNAL_DOLT") == "1")

	sandbox := t.TempDir()
	home := filepath.Join(sandbox, "home")
	town := filepath.Join(sandbox, "town")
	claudeDir := filepath.Join(sandbox, "claude")
	for _, dir := range []string{home, claudeDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("creating sandbox dir: %v", err)
		}
	}
	if err := writeSandboxTown(town); err != nil {
		t.Fatal(err)
	}
	if err := writeSandboxGitConfig(home); err != nil {
		t.Fatal(err)
	}
	preserveGoEnv()

	for k, v := range map[string]string{
		"HOME":              home,
		"USERPROFILE":       home,
		"CLAUDE_CONFIG_DIR": claudeDir,
		"GT_TOWN_ROOT":      town,
		HermeticEnvVar:      "1",
	} {
		if err := os.Setenv(k, v); err != nil {
			t.Fatalf("setting %s: %v", k, err)
		}
	}
	if os.Getenv("GT_TEST_EXTERNAL_DOLT") != "1" {
		_ = os.Setenv("GT_DOLT_PORT", poisonDoltPort)
		_ = os.Setenv("BEADS_DOLT_PORT", poisonDoltPort)
	}

	return town
}

// ScratchTown creates a minimal town in a temp dir and chdirs the test into
// it, so code that resolves the town root from cwd (workspace.FindFromCwd)
// lands in the scratch town instead of walking up into a live one. The
// original working directory is restored via t.Cleanup (t.Chdir). Returns the
// scratch town root.
//
// t.Chdir is process-wide: tests using ScratchTown must not use t.Parallel.
func ScratchTown(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := writeSandboxTown(root); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)
	return root
}

// townSnapshot records the observable state of a live town so the tripwire
// can detect test pollution. It intentionally tracks only additions:
// concurrent legitimate agents in a busy town create lock files and append
// events, so removals and lock-file churn are ignored.
type townSnapshot struct {
	root       string
	eventsSize int64
	entries    map[string]bool // relative paths, e.g. ".dolt-data/testdb_x"
}

// watchedSubdirs are town-root subdirectories whose direct children are
// snapshotted. .dolt-data catches orphan test databases (each database is a
// directory); .beads catches stray tracker files.
var watchedSubdirs = []string{".beads", ".dolt-data"}

func snapshotTown(root string) *townSnapshot {
	s := &townSnapshot{root: root, entries: map[string]bool{}}
	if fi, err := os.Stat(filepath.Join(root, ".events.jsonl")); err == nil {
		s.eventsSize = fi.Size()
	}
	for name := range listDir(root) {
		s.entries[name] = true
	}
	for _, sub := range watchedSubdirs {
		for name := range listDir(filepath.Join(root, sub)) {
			s.entries[filepath.ToSlash(filepath.Join(sub, name))] = true
		}
	}
	return s
}

// diff re-snapshots the town and returns a description of every leak: new
// non-lock entries, and appended events not attributable to the town's known
// actors (concurrent legitimate agents keep writing events while tests run,
// so growth alone is not a failure).
func (s *townSnapshot) diff() []string {
	var leaks []string

	after := snapshotTown(s.root)
	var added []string
	for name := range after.entries {
		if !s.entries[name] && !strings.HasSuffix(name, ".lock") {
			added = append(added, name)
		}
	}
	sort.Strings(added)
	for _, name := range added {
		leaks = append(leaks, fmt.Sprintf("new entry: %s", name))
	}

	if after.eventsSize > s.eventsSize {
		leaks = append(leaks, suspiciousAppendedEvents(s.root, s.eventsSize)...)
	}

	return leaks
}

// suspiciousAppendedEvents reads .events.jsonl from offset and flags events
// whose actor does not belong to the town (first path segment neither a rig
// from mayor/rigs.json nor a built-in town-level actor). Fixture actors like
// "myr/mycat" (the gt-x9o incident) are caught; concurrent legitimate agents
// pass.
func suspiciousAppendedEvents(root string, offset int64) []string {
	f, err := os.Open(filepath.Join(root, ".events.jsonl")) //nolint:gosec // path derives from detected town root
	if err != nil {
		return nil
	}
	defer f.Close() //nolint:errcheck // read-only
	if _, err := f.Seek(offset, 0); err != nil {
		return nil
	}

	known := knownActorPrefixes(root)
	var leaks []string
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var ev struct {
			Actor string `json:"actor"`
			Type  string `json:"type"`
		}
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			leaks = append(leaks, fmt.Sprintf("unparseable event appended to .events.jsonl: %.120s", line))
			continue
		}
		prefix, _, _ := strings.Cut(strings.TrimSuffix(ev.Actor, "/"), "/")
		if !known[prefix] {
			leaks = append(leaks, fmt.Sprintf("event with unknown actor %q (type %s) appended to .events.jsonl", ev.Actor, ev.Type))
		}
	}
	return leaks
}

// builtinActorPrefixes are town-level actors that are always legitimate.
var builtinActorPrefixes = []string{
	"mayor", "overseer", "deacon", "daemon", "convoy", "town", "gt", "boot", "human", "crew",
}

func knownActorPrefixes(root string) map[string]bool {
	known := map[string]bool{}
	for _, p := range builtinActorPrefixes {
		known[p] = true
	}
	data, err := os.ReadFile(filepath.Join(root, "mayor", "rigs.json")) //nolint:gosec // path derives from detected town root
	if err != nil {
		return known
	}
	var rigs struct {
		Rigs map[string]json.RawMessage `json:"rigs"`
	}
	if err := json.Unmarshal(data, &rigs); err != nil {
		return known
	}
	for name := range rigs.Rigs {
		known[name] = true
	}
	return known
}

// listDir returns the names of dir's direct children, or an empty map when
// the directory cannot be read.
func listDir(dir string) map[string]bool {
	names := map[string]bool{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return names
	}
	for _, e := range entries {
		names[e.Name()] = true
	}
	return names
}

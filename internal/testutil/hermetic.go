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
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/events"
	"github.com/steveyegge/gastown/internal/feed"
	"github.com/steveyegge/gastown/internal/krc"
	"github.com/steveyegge/gastown/internal/tmux"
	"github.com/steveyegge/gastown/internal/workspace"
)

// liveTownResolver is one in-process town-root resolver the harness probes at
// setup.
type liveTownResolver struct {
	name    string
	resolve func(dir string) string
}

// liveTownResolvers are the in-process town-root resolvers the harness probes
// at setup. A resolver reachable from a test binary that is NOT listed here
// still has to route its refusal through workspace.RefuseForbiddenRoot
// (gt-dr664).
var liveTownResolvers = []liveTownResolver{
	{"workspace.Find", func(dir string) string {
		root, _ := workspace.Find(dir)
		return root
	}},
	{"beads.FindTownRoot", beads.FindTownRoot},
}

// assertLiveTownRefused fails harness setup when an in-process resolver still
// reaches the live town after the scrub and redirect. Refusing loudly (a panic
// from workspace.RefuseForbiddenRoot) is the pass condition here — it is the
// guard working — so anything that returns a root inside the live town is a
// leak and fails the whole run at TestMain, before a test body can write
// (gt-dr664).
func assertLiveTownRefused(dir string) error {
	var leaks []string
	for _, r := range liveTownResolvers {
		root, refusedLoudly := probeResolver(r.resolve, dir)
		if refusedLoudly || root == "" || !workspace.IsForbiddenRoot(root) {
			continue
		}
		leaks = append(leaks, fmt.Sprintf("  - %s resolved %s", r.name, root))
	}
	if len(leaks) == 0 {
		return nil
	}
	return fmt.Errorf("in-process town-root resolvers reached the live town at %s:\n%s\n\n"+
		"Route the resolver's refusal through workspace.RefuseForbiddenRoot so tests fail\n"+
		"loudly instead of opening production beads and its Dolt server (gt-dr664)",
		workspace.ForbiddenTownRoot(), strings.Join(leaks, "\n"))
}

// probeResolver calls a resolver that may refuse loudly; refusedLoudly reports
// that it panicked instead of returning a root.
func probeResolver(resolve func(dir string) string, dir string) (root string, refusedLoudly bool) {
	defer func() {
		if r := recover(); r != nil {
			refusedLoudly = true
		}
	}()
	return resolve(dir), false
}

// AllowLiveTmuxEnv opts a test process out of the tmux socket isolation
// StartHermetic applies below, for the rare case that deliberately needs the
// live/default tmux socket. See gt-yav3. Re-exported from internal/tmux so
// both the hermetic harness and tmux.NewTmux's own guard read one source of
// truth.
const AllowLiveTmuxEnv = tmux.AllowLiveTmuxEnv

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
	// StartDir is the working directory when the harness started, which is
	// inside RealTownRoot when a test binary runs from a worktree in it.
	StartDir string
	// TmuxSocket is the isolated per-process tmux socket (-L flag) this
	// harness bound via tmux.SetDefaultSocket, or "" when tmux isn't
	// installed or isolation was bypassed via AllowLiveTmuxEnv.
	TmuxSocket string
	// LiveTmuxSocket is the socket of the tmux server this test process was
	// launched inside (from the ambient $TMUX), or "" when it was not started
	// from a tmux pane. Finish tripwires on test sessions found there.
	LiveTmuxSocket string

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
	// An outer harness (nested `go test` runs) may have set the forbidden
	// root already, which blinds FindFromCwd to it — inherit it in that case
	// so the guard and tripwire survive nesting.
	h.StartDir, _ = os.Getwd() //nolint:errcheck // "" just disables the startup probe
	if root, err := workspace.FindFromCwd(); err == nil && root != "" {
		h.RealTownRoot = root
	} else if root := os.Getenv(workspace.EnvForbiddenTownRoot); root != "" {
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

	// Capture before the scrub below strips it: AllowLiveTmuxEnv is a
	// BEADS_* var, so scrubProcessEnv always removes it regardless of
	// externalDolt. Restored immediately after the scrub (see below) so both
	// isolateTmuxSocket() and tmux.NewTmux()'s own guard — which reads the
	// same var later in the process lifetime, from inside test bodies — see
	// the caller's real opt-out instead of an env that was already wiped
	// before anything checked it. Without this, BEADS_TEST_ALLOW_LIVE_TMUX=1
	// was a documented but dead opt-out (gt-yav3 MR1 bounce).
	allowLiveTmux := os.Getenv(AllowLiveTmuxEnv) == "1"

	// Same reason as allowLiveTmux: scrubInheritedTmuxVars removes this, and
	// the tripwire in Finish compares against the server the test process was
	// itself launched inside. Captured here, before the scrub.
	h.LiveTmuxSocket = tmux.SocketFromEnv()

	scrubProcessEnv(externalDolt)
	scrubInheritedTmuxVars()

	if allowLiveTmux {
		if err := os.Setenv(AllowLiveTmuxEnv, "1"); err != nil {
			return nil, fmt.Errorf("restoring %s: %w", AllowLiveTmuxEnv, err)
		}
	}

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
	// (GOPATH=$HOME/go etc.) AND its go env file ($GOENV, itself under
	// $HOME), which would make any `go` invocation from a test re-download
	// the module cache and, on a host with a keg-only icu4c, lose the cgo
	// flags. Pin the current effective values before HOME changes.
	if err := preserveGoEnv(); err != nil {
		return nil, err
	}

	setenvs := map[string]string{
		"HOME":              h.HomeDir,
		"USERPROFILE":       h.HomeDir, // Windows HOME
		"CLAUDE_CONFIG_DIR": claudeDir,
		"XDG_CONFIG_HOME":   filepath.Join(h.HomeDir, ".config"),
		"XDG_CACHE_HOME":    filepath.Join(h.HomeDir, ".cache"),
		"XDG_STATE_HOME":    filepath.Join(h.HomeDir, ".state"),
		"GT_TOWN_ROOT":      h.TownRoot,
		HermeticEnvVar:      "1",
		// bd auto-starts a dolt sql-server when the configured port is
		// unreachable; under the harness that would leak server processes
		// (observed during gt-lwi verification). bd honors this variable.
		"BEADS_DOLT_AUTO_START": "0",
		BeadsCircuitDirEnv:      sandboxCircuitDir(h.HomeDir),
	}
	if h.RealTownRoot != "" {
		// Workspace resolution (workspace.Find and gt subprocesses) refuses
		// to resolve to the live town, so cwd walk-up from a package dir
		// inside it behaves as "no workspace" instead of touching production.
		setenvs[workspace.EnvForbiddenTownRoot] = h.RealTownRoot
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

	h.TmuxSocket = isolateTmuxSocket()

	// The env scrub above only reaches resolvers that consult it. Probe them
	// from the package's own directory — the one cwd guaranteed to sit inside
	// the live worktree — so a resolver that ignores the guard fails here
	// rather than mid-test.
	if h.RealTownRoot != "" {
		if err := assertLiveTownRefused(h.StartDir); err != nil {
			return nil, err
		}
	}

	return h, nil
}

// isolateTmuxSocket binds the package-level tmux default socket (see
// tmux.SetDefaultSocket) to an isolated per-process name so any test in this
// binary that constructs tmux.NewTmux() lands on a throwaway server instead
// of a live town's production socket (gt-yav3: a `go test` run inside a live
// polecat worktree previously inherited GT_TOWN_SOCKET and created real
// sessions — including ones named like polecats — on the town's tmux
// server, flapping zombie/capacity readings and risking a phantom-session
// auto-nuke). Returns the socket name bound, or "" when tmux isn't
// installed or AllowLiveTmuxEnv opts out.
func isolateTmuxSocket() string {
	if os.Getenv(AllowLiveTmuxEnv) == "1" {
		return ""
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		return ""
	}
	socket := fmt.Sprintf("gt-test-%d", os.Getpid())
	tmux.SetDefaultSocket(socket)
	return socket
}

// terminateDoltContainer is TerminateDoltContainer, indirected so a test can
// force the cleanup-failure branch below without starting a real container.
var terminateDoltContainer = TerminateDoltContainer

// Finish tears down the sandbox and runs the tripwire against the live town
// snapshot. It returns the exit code for os.Exit: the m.Run() code, forced to
// 1 when the tripwire detects that tests leaked state into the live town, or
// when the shared Dolt container fails to terminate.
func (h *Hermetic) Finish(code int) int {
	// No-op when no container was started; also covers containers started
	// lazily by tests via RequireDoltContainer.
	if err := terminateDoltContainer(); err != nil {
		fmt.Fprintf(os.Stderr, "\n%s\n", strings.Repeat("=", 72))
		fmt.Fprintf(os.Stderr, "HERMETIC TRIPWIRE: shared Dolt container failed to terminate: %v\n", err)
		fmt.Fprintf(os.Stderr, "A container that fails to terminate keeps running and holding\n")
		fmt.Fprintf(os.Stderr, "memory on the shared Docker VM (gt-p98h, gt-n5g6). Investigate\n")
		fmt.Fprintf(os.Stderr, "rather than re-running: repeated leaks exhaust it town-wide.\n")
		fmt.Fprintf(os.Stderr, "%s\n", strings.Repeat("=", 72))
		if code == 0 {
			code = 1
		}
	}
	if h.SandboxDir != "" {
		_ = os.RemoveAll(h.SandboxDir)
	}
	if h.TmuxSocket != "" {
		_ = exec.Command("tmux", "-L", h.TmuxSocket, "kill-server").Run() //nolint:errcheck // best-effort cleanup
		_ = os.Remove(filepath.Join(tmux.SocketDir(), h.TmuxSocket))
	}

	if h.LiveTmuxSocket != "" {
		if leaks := liveTmuxTestSessions(h.LiveTmuxSocket); len(leaks) > 0 {
			fmt.Fprintf(os.Stderr, "\n%s\n", strings.Repeat("=", 72))
			fmt.Fprintf(os.Stderr, "HERMETIC TRIPWIRE: tests created %d session(s) on the live tmux server %q\n",
				len(leaks), h.LiveTmuxSocket)
			for _, l := range leaks {
				fmt.Fprintf(os.Stderr, "  - %s\n", l)
			}
			fmt.Fprintf(os.Stderr, "A session named gt-test-* on the live server reads as a phantom polecat\n")
			fmt.Fprintf(os.Stderr, "in the roster and is a target for auto-nuke (gt-2bj). Name a session\n")
			fmt.Fprintf(os.Stderr, "socket (tmux -L gt-test-<something>) instead of using the inherited one.\n")
			fmt.Fprintf(os.Stderr, "%s\n", strings.Repeat("=", 72))
			if code == 0 {
				code = 1
			}
		}
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

// bdTelemetryOff is what every bd spawned under the harness must see. With
// HOME redirected to the sandbox there is no user config saying `bd metrics
// off`, so bd resolves telemetry ENABLED by default: it forks the platform
// machine-id probe on the cold cache, writes event files under the sandbox
// and spawns detached send-metrics children at the real endpoint. Both
// switches are telemetry-only: BD_DISABLE_METRICS turns the collector off
// and BD_DISABLE_EVENT_FLUSH stops the detached flusher, which bd gates
// independently of the collector (beads GH#5712). BEADS_TEST_MODE is
// deliberately not used here — it is a general test-mode switch in the
// beads SDK (testdb_ minting among other things) that the packages needing
// it set for themselves. Set after the scrub (which removes BD_*) and re-set
// by every scrub (gt-wcq2).
var bdTelemetryOff = map[string]string{
	"BD_DISABLE_METRICS":     "1",
	"BD_DISABLE_EVENT_FLUSH": "1",
}

// BeadsCircuitDirEnv is bd's override for the directory its Dolt
// circuit-breaker state files live in (beads internal/storage/dolt/circuit.go,
// testCircuitBreakerDirEnv). Test-spawned bd must not write the real
// $TMPDIR/beads-circuit, which every production bd start reads in full
// (gt-tkz7, be-0v4).
const BeadsCircuitDirEnv = "BEADS_TEST_CIRCUIT_DIR"

// sandboxCircuitDir is where a sandbox's bd keeps circuit-breaker state. The
// directory is created so bd never depends on the sandbox layout for it.
func sandboxCircuitDir(home string) string {
	dir := filepath.Join(home, ".cache", "beads-circuit")
	_ = os.MkdirAll(dir, 0o755)
	return dir
}

// scrubProcessEnv removes every GT_*, BD_* and BEADS_* variable from the
// process environment so tests and their subprocesses cannot inherit live
// town context from the invoking agent session. When keepDolt is true the
// Dolt passthrough variables survive (an outer runner provided the server).
// It leaves bd's telemetry switched off (bdTelemetryOff).
func scrubProcessEnv(keepDolt bool) {
	defer func() {
		for k, v := range bdTelemetryOff {
			_ = os.Setenv(k, v)
		}
	}()
	// DockerTestsEnv is the caller's opt-in to container-backed tests, not
	// live-town context; it is a GT_* variable only by naming convention.
	// Without this, `GT_TEST_DOCKER=1 go test` would be wiped here before
	// the WithDolt option or any RequireDoltContainer call could read it,
	// and the opt-in would be a documented but dead switch (same shape as
	// the AllowLiveTmuxEnv bounce, gt-yav3).
	keep := map[string]bool{DockerTestsEnv: true}
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

// liveTmuxVars are the session-identity variables an agent shell exports and
// tmux honors ahead of its own default socket.
var liveTmuxVars = []string{"TMUX", "TMUX_PANE", "TMUX_TMPDIR"}

// scrubInheritedTmuxVars drops the invoking agent's tmux identity, so a bare
// `tmux` in a test or its subprocess cannot reach the live town server. No
// socket-scoped wrapper covers a shell-out; the scrub does (gt-2bj).
func scrubInheritedTmuxVars() {
	for _, v := range liveTmuxVars {
		_ = os.Unsetenv(v)
	}
}

// liveTmuxTestSessions returns the gt-test-* sessions running on the given
// socket, each with the pane pid and cwd that identify what left it behind.
// Phantoms live under two minutes, so a name alone is not evidence (gt-2bj).
func liveTmuxTestSessions(socket string) []string {
	tm := tmux.NewTmuxWithSocket(socket)
	names, err := tm.ListSessions()
	if err != nil {
		return nil
	}
	var leaks []string
	for _, name := range names {
		if !strings.HasPrefix(name, "gt-test-") {
			continue
		}
		var evidence []string
		if pid, err := tm.GetPanePID(name); err == nil && pid != "" {
			evidence = append(evidence, "pane_pid="+pid)
		}
		if cwd, err := tm.PaneCurrentPath(name); err == nil && cwd != "" {
			evidence = append(evidence, "cwd="+cwd)
		}
		sort.Strings(evidence)
		if len(evidence) == 0 {
			leaks = append(leaks, name)
			continue
		}
		leaks = append(leaks, fmt.Sprintf("%s (%s)", name, strings.Join(evidence, " ")))
	}
	sort.Strings(leaks)
	return leaks
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

// goEnvCarryVars are the cache-location variables preserveGoEnv pins to
// their current effective values before the harness redirects HOME, so `go`
// invocations from tests keep using the real build and module caches
// instead of a cold cache under the throwaway sandbox.
var goEnvCarryVars = []string{"GOPATH", "GOCACHE", "GOMODCACHE"}

// preserveGoEnv pins GOENV to the go tool's current env file, and
// goEnvCarryVars to their current effective values, before HOME is
// redirected.
//
// GOENV is what actually carries a host's cgo flags: on a host with a
// keg-only icu4c, CGO_CPPFLAGS/CGO_LDFLAGS live in the go env FILE ($GOENV,
// itself under $HOME), not the process environment, and the redirect would
// otherwise point `go env` at an empty file under the sandbox HOME, losing
// them from every `go` subprocess a test spawns (gt-mjll). Pinning the file
// pointer, rather than exporting the flags themselves, leaves the process
// environment untouched, so a caller like buildGTBinary's brew-icu4c
// fallback still runs on a host whose env file lacks them.
//
// Fails on every error except a missing go tool: a test process with no
// `go` on PATH cannot build anything that would notice the loss, but a `go
// env` failure with the tool present means the carry did not happen, and
// dropping it silently resurfaces much later as an obscure compile error.
//
// Trade-off: pinning GOENV carries the whole file, not just the cgo flags —
// GOFLAGS, GOPROXY, GOPRIVATE and GOTOOLCHAIN included — into every `go`
// subprocess a test spawns, which is broader than the cache-location carry
// it replaces. Accepted: those are the same values a developer's own shell
// already sees outside the harness.
func preserveGoEnv() error {
	goBin, err := exec.LookPath("go")
	if err != nil {
		return nil
	}

	if os.Getenv("GOENV") == "" {
		out, err := exec.Command(goBin, "env", "GOENV").Output()
		if err != nil {
			return fmt.Errorf("asking the go tool for GOENV: %w", err)
		}
		if goEnvPath := strings.TrimSpace(string(out)); goEnvPath != "" {
			if err := os.Setenv("GOENV", goEnvPath); err != nil {
				return fmt.Errorf("pinning GOENV: %w", err)
			}
		}
	}

	var missing []string
	for _, v := range goEnvCarryVars {
		if os.Getenv(v) == "" {
			missing = append(missing, v)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	// -json: values are space-bearing flag strings and may legitimately be
	// empty, so splitting output on newlines would be lossy.
	args := append([]string{"env", "-json"}, missing...)
	out, err := exec.Command(goBin, args...).Output() //nolint:gosec // fixed args
	if err != nil {
		return fmt.Errorf("asking the go tool for %s: %w", strings.Join(missing, ", "), err)
	}
	values := map[string]string{}
	if err := json.Unmarshal(out, &values); err != nil {
		return fmt.Errorf("parsing `go env -json %s`: %w", strings.Join(missing, " "), err)
	}
	for _, v := range missing {
		if values[v] == "" {
			continue
		}
		if err := os.Setenv(v, values[v]); err != nil {
			return fmt.Errorf("carrying %s into the hermetic environment: %w", v, err)
		}
	}
	return nil
}

// WithFailingGoOnPath points PATH at a stand-in `go` that runs but always
// exits nonzero, so a `go env` call made by code under test returns an error
// instead of hitting the "no go on PATH" branch several callers treat as a
// silent no-op. Restored via t.Setenv's automatic cleanup. Exported so
// packages that pin gt-mjll's loud-failure path in their own tests (e.g.
// internal/cmd's cgo probe) share this setup instead of reimplementing it.
func WithFailingGoOnPath(t testing.TB) {
	t.Helper()
	binDir := t.TempDir()
	name := "go"
	script := "#!/bin/sh\nexit 1\n"
	if runtime.GOOS == "windows" {
		name = "go.bat"
		script = "@exit /b 1\r\n"
	}
	if err := os.WriteFile(filepath.Join(binDir, name), []byte(script), 0o755); err != nil {
		t.Fatalf("writing stand-in go: %v", err)
	}
	t.Setenv("PATH", binDir)
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
	if err := preserveGoEnv(); err != nil {
		t.Fatal(err)
	}

	testEnvs := map[string]string{
		"HOME":                  home,
		"USERPROFILE":           home,
		"CLAUDE_CONFIG_DIR":     claudeDir,
		"GT_TOWN_ROOT":          town,
		HermeticEnvVar:          "1",
		"BEADS_DOLT_AUTO_START": "0",
		BeadsCircuitDirEnv:      sandboxCircuitDir(home),
	}
	startDir, _ := os.Getwd() //nolint:errcheck // "" just disables the startup probe
	realRoot := ""
	if root, err := workspace.FindFromCwd(); err == nil && root != "" {
		realRoot = root
		testEnvs[workspace.EnvForbiddenTownRoot] = root
	}
	for k, v := range testEnvs {
		if err := os.Setenv(k, v); err != nil {
			t.Fatalf("setting %s: %v", k, err)
		}
	}
	if os.Getenv("GT_TEST_EXTERNAL_DOLT") != "1" {
		_ = os.Setenv("GT_DOLT_PORT", poisonDoltPort)
		_ = os.Setenv("BEADS_DOLT_PORT", poisonDoltPort)
	}
	if realRoot != "" {
		if err := assertLiveTownRefused(startDir); err != nil {
			t.Fatal(err)
		}
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
// non-lock, non-atomic-write-temp, non-rig-explained entries, and appended
// events not attributable to the town's known actors (concurrent legitimate
// agents keep writing events while tests run, so growth alone is not a
// failure).
//
// The atomic-temp exemption is fails-open by construction (gt-lqri): a
// .tmp-suffix match cannot distinguish a temp written to a watched directory
// from one abandoned there, and the suffix alone forgives both. diff()
// tolerates a new .tmp entry only when its base matches a known atomic-write
// sibling (atomicTempLeak) — the transients the exemption was added for — and
// reports any other .tmp as a leak.
func (s *townSnapshot) diff() []string {
	var leaks []string

	after := snapshotTown(s.root)
	rigs := rigNames(s.root)
	var added []string
	for name := range after.entries {
		if s.entries[name] || strings.HasSuffix(name, ".lock") || explainedByRig(name, rigs) {
			continue
		}
		if isAtomicWriteTemp(name) {
			// .~ names are unforgeable — the temp pattern is
			// .~<file>.<random> — and stay tolerated as-is (gt-wdr). A
			// .tmp suffix, by contrast, is a fails-open match: it is
			// tolerated only when the base is a known atomic-write
			// sibling, so an abandoned or leaked .tmp is reported (gt-lqri).
			base := filepath.Base(name)
			if strings.HasPrefix(base, ".~") || atomicTempLeak(base) {
				continue
			}
		}
		added = append(added, name)
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

// explainedByRig reports whether an added town entry is the rig worktree or
// Dolt data directory for a rig now registered in mayor/rigs.json —
// legitimate concurrent operator onboarding (gt-bd79: `gt-lwi`'s
// before/after snapshot cannot otherwise distinguish a new rig directory
// like `hm` or `.dolt-data/hm` from test-leaked state in the same window),
// not test leakage. A rig's worktree and Dolt data both live under the rig's
// name — at the town root and under .dolt-data/ respectively — so matching
// the entry's base name against the registered rig set covers both.
func explainedByRig(name string, rigs map[string]bool) bool {
	if len(rigs) == 0 {
		return false
	}
	return rigs[filepath.Base(name)]
}

// isAtomicWriteTemp reports whether name is a transient atomic-write temp
// file: bd's JSONL export at .beads/.~issues.jsonl.<random>, or a plain
// write-temp-then-rename sibling like .events.jsonl.tmp (internal/krc's
// prune rewrite of the raw events log races the snapshot diff the same way,
// gt-hotx). Town tooling routinely writes via create-tmp-then-rename, so any
// concurrent invocation by any agent during a test window creates and then
// removes one of these — indistinguishable from the .lock churn already
// tolerated below (gt-wdr, the file-entry analog of gt-ro0's event-actor
// exemption).
//
// gt-lqri: the .tmp suffix alone is a fails-open exemption — a test that
// abandons a .tmp file, or tooling that hard-crashes before renaming, looks
// identical to a live atomic write, and the suffix match forgives both. The
// .~ prefix does not have this ambiguity (the OS temp pattern
// .~<file>.<random> is unforgeable by accident), so diff() keeps it
// unconditional; .tmp entries pass diff() only when atomicTempLeak()
// cross-checks them against the live target set, and fails closed otherwise.
func isAtomicWriteTemp(name string) bool {
	base := filepath.Base(name)
	return strings.HasPrefix(base, ".~") || strings.HasSuffix(base, ".tmp")
}

// atomicWriteTemps maps each live target file on the snapshot's watched
// surface to the exact suffixes its atomic writers append while building a
// replacement, before renaming it over the target. .feed.jsonl has two
// independent writers with different suffixes — krc's periodic prune
// (replaceWithLines, gt-hotx) and the feed curator's separate truncate
// rotation — so both must be listed, or the writer that isn't gets its
// crash residue reported as a leak (exactly the bug this map replaced:
// hardcoding a single ".tmp" sibling per file missed the curator's
// ".truncate.tmp"). Suffixes are the producers' own exported constants
// rather than re-typed literals, so a renamed suffix breaks the build here
// instead of silently going stale.
var atomicWriteTemps = map[string][]string{
	events.EventsFile:      {krc.ReplaceTempSuffix},
	feed.FeedFile:          {krc.ReplaceTempSuffix, feed.TruncateTempSuffix},
	krc.AutoPruneStateFile: {krc.ReplaceTempSuffix},
}

// atomicTempPrefixes are CreateTemp patterns that produce temps on the
// watched surface: beads.WriteRoutes uses os.CreateTemp(beadsDir,
// beads.RoutesTempPrefix+"*.tmp") in .beads, and os.CreateTemp splices its
// random suffix at the "*" in the pattern — so the temp's name
// (.routes-<random>.tmp) is not derivable from routes.jsonl, which is why
// the match is a prefix, not a sibling of a target name.
var atomicTempPrefixes = []string{
	beads.RoutesTempPrefix,
}

// atomicTempLeak reports whether a new .tmp-suffix entry is a known atomic-
// write temp rather than a leaked file. The exemption is fails-open by
// construction — a suffix match cannot tell a .tmp written to a watched
// directory from a .tmp abandoned there — so diff() tolerates a .tmp entry
// only when its base names a known temp (a target file plus one of its
// producers' exact suffixes, or a known CreateTemp prefix); a .tmp with no
// live target (a test that wrote a temp and never renamed it) is reported
// as a leak instead of being silently forgiven.
func atomicTempLeak(base string) bool {
	for file, suffixes := range atomicWriteTemps {
		for _, suffix := range suffixes {
			if base == file+suffix { // e.g. .feed.jsonl.truncate.tmp
				return true
			}
		}
	}
	if strings.HasSuffix(base, ".tmp") {
		for _, prefix := range atomicTempPrefixes {
			if strings.HasPrefix(base, prefix) { // e.g. .routes-12345.tmp
				return true
			}
		}
	}
	return false
}

// suspiciousAppendedEvents reads .events.jsonl from offset and flags events
// whose actor does not belong to the town (first path segment neither a rig
// from mayor/rigs.json nor a built-in town-level actor). Fixture actors like
// "myr/mycat" (the gt-x9o incident) are caught; concurrent legitimate agents
// pass.
//
// gt-5few: offset is a byte position recorded by Stat at snapshot time, but
// the town's agents append to the live file whenever they like, so a line can
// be partially present at either end of the window being scanned:
//
//   - head — Stat ran mid-append, so offset points into a line and the scan
//     starts with that line's tail. This is what the refinery kept hitting:
//     the "unparseable event" fragments in the gt-5few comments are real
//     live-town lines missing their leading `{"ts":` (7 bytes).
//   - tail — a writer is mid-append at scan time, leaving a last line with no
//     trailing newline.
//
// Both fragments fail to parse and were reported as leaked state, though the
// writer was a concurrent legitimate agent. A truncation says nothing about
// the actor that produced the line, so partial lines are skipped at both
// ends; every complete appended line is still checked in full.
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
	r := bufio.NewReader(f)
	if !atLineStart(f, offset) {
		// The snapshot boundary landed inside a line, so that line was at
		// least partly written before the snapshot. Discard the remainder
		// and start at the next line boundary.
		if _, err := r.ReadString('\n'); err != nil {
			return nil // nothing in the window but the partial line
		}
	}
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			// err is io.EOF (or a read failure). Any bytes returned had no
			// trailing newline: a write in flight at scan time, not an
			// appended event with an actor to judge.
			return leaks
		}
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			leaks = append(leaks, appendedEventLeaks(trimmed, known)...)
		}
	}
}

// atLineStart reports whether offset is a line boundary: the start of the
// file, or immediately after a newline. Unknown positions are treated as
// boundaries so the scan degrades to the pre-gt-5few behavior.
func atLineStart(f *os.File, offset int64) bool {
	if offset <= 0 {
		return true
	}
	var prev [1]byte
	if _, err := f.ReadAt(prev[:], offset-1); err != nil {
		return true
	}
	return prev[0] == '\n'
}

// appendedEventLeaks classifies one complete appended line: malformed JSON, or
// an event whose actor prefix the town does not know.
func appendedEventLeaks(line string, known map[string]bool) []string {
	var ev struct {
		Actor string `json:"actor"`
		Type  string `json:"type"`
	}
	if err := json.Unmarshal([]byte(line), &ev); err != nil {
		return []string{fmt.Sprintf("unparseable event appended to .events.jsonl: %.120s", line)}
	}
	prefix, _, _ := strings.Cut(strings.TrimSuffix(ev.Actor, "/"), "/")
	if !known[prefix] {
		return []string{fmt.Sprintf("event with unknown actor %q (type %s) appended to .events.jsonl", ev.Actor, ev.Type)}
	}
	return nil
}

// builtinActorPrefixes are town-level actors that are always legitimate.
//
// gt-9pn: this list was previously enumerated entirely by hand and was
// missing a legitimate actor twice (gt-ro0 "unknown", gt-kvc "dog") — each
// time a false positive that could have blocked a real merge. Most of these
// entries are no longer maintained by hand alone: internal/cmd's
// TestDetectActorOutputsToleratedByTripwire iterates every internal/cmd.Role
// (the enum backing detectActor(), the function that actually writes most
// agent-originated actor values) and fails if RoleInfo.ActorString() ever
// produces a value not in this list — so the two sides can no longer
// silently drift apart the way they did before.
//
// "mayor", "deacon", "witness", "refinery", "polecat", "crew", "dog",
// "unknown" are the bare (no-rig) actor strings for their respective Roles.
//
// gt-jna (CRITICAL regression in gt-9pn): RoleBoot has TWO independent,
// both-legitimate actor-construction paths that gt-9pn wrongly assumed were
// one and the same:
//   - RoleInfo.ActorString() (internal/cmd/role.go) returns "deacon-boot" —
//     the beads-attribution form, matches BD_ACTOR for Boot's `bd` calls.
//   - getAgentIdentity() (internal/cmd/prime.go), used by emitSessionEvent
//     to set the actor on every session_start event Boot's `gt prime` emits,
//     returns bare "boot" — the hook/agent-identity form, matches GT_ROLE's
//     compound "deacon/boot" root and Boot's session/hook identity elsewhere.
//   - gt-9pn's TestDetectActorOutputsToleratedByTripwire only cross-checks
//     ActorString(), so it never saw getAgentIdentity()'s "boot" and the fix
//     dropped a live, high-volume (~90s cadence) actor value — reproducing
//     exactly the kind of false positive it was built to eliminate. Both
//     "boot" and "deacon-boot" must stay tolerated; see
//     TestGetAgentIdentityOutputsToleratedByTripwire (role_actor_tripwire_test.go)
//     for the cross-check covering this second construction path.
//
// The remaining four are not derivable from internal/cmd.Role because they
// come from other construction paths, verified directly against source:
//   - "overseer": the fallback in detectSender() (internal/cmd/mail_identity.go)
//     used as the mail actor when no agent identity resolves.
//   - "gt": literal actor for town-infrastructure events with no owning
//     agent (internal/cmd/up.go, polecat_spawn.go, down.go).
//   - "daemon": literal actor for daemon-originated events, e.g. mass-death
//     detection (internal/daemon/daemon.go).
//   - "convoy": convoyNotifyFrom() (internal/cmd/convoy.go, also inlined at
//     internal/refinery/engineer.go) builds "convoy/<convoy-id>" as the
//     --from actor for a convoy's completion-notification mail — confirmed
//     live in ~/gt/.events.jsonl while verifying this change (gt-9pn), which
//     is exactly the kind of dynamically-built actor a plain string search
//     for "convoy" as a whole value misses.
//
// "town" and "human" remain removed (gt-9pn, re-verified gt-jna): neither a
// repo-wide search for them as a literal actor value nor for a "<prefix>/"+
// id-style builder (the pattern that caught "convoy" above) found a code
// path that ever writes them as an event actor. If one is ever needed, the
// tripwire's leak report will name the exact actor to add back — that is the
// point of deriving this list instead of guessing at it.
var builtinActorPrefixes = []string{
	"mayor", "deacon", "boot", "deacon-boot", "witness", "refinery", "polecat",
	"crew", "dog", "unknown", "overseer", "gt", "daemon", "convoy",
}

// BuiltinActorPrefixes returns a copy of the always-legitimate town-level
// actor prefixes. It exists so other packages (e.g. internal/cmd's
// TestDetectActorOutputsToleratedByTripwire) can cross-check their own
// actor-construction logic against the tripwire's tolerances without
// duplicating this list.
func BuiltinActorPrefixes() []string {
	return append([]string(nil), builtinActorPrefixes...)
}

func knownActorPrefixes(root string) map[string]bool {
	known := map[string]bool{}
	for _, p := range builtinActorPrefixes {
		known[p] = true
	}
	for name := range rigNames(root) {
		known[name] = true
	}
	return known
}

// rigNames returns the set of rig names registered in the town's
// mayor/rigs.json, or an empty set when the file is missing or unparseable.
func rigNames(root string) map[string]bool {
	names := map[string]bool{}
	data, err := os.ReadFile(filepath.Join(root, "mayor", "rigs.json")) //nolint:gosec // path derives from detected town root
	if err != nil {
		return names
	}
	var rigs struct {
		Rigs map[string]json.RawMessage `json:"rigs"`
	}
	if err := json.Unmarshal(data, &rigs); err != nil {
		return names
	}
	for name := range rigs.Rigs {
		names[name] = true
	}
	return names
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

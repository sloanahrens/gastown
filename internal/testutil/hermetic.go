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
//   - forbids workspace resolution from reaching the live town the binary
//     runs inside, and refuses to start when an in-process resolver still
//     reaches it (gt-dr664).
//
// The harness never reads the live town's state: whether tests leaked into it
// is `gt doctor`'s test-leaks check, so a run's verdict never depends on what
// the town is doing while it runs (gt-ik4a1.3).
package testutil

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
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
func (h *harnessHost) assertLiveTownRefused(dir string) error {
	var leaks []string
	for _, r := range h.resolvers {
		root, refusedLoudly := probeResolver(r.resolve, dir)
		if refusedLoudly || root == "" || !h.forbidden(root) {
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
		getenv(h.env, workspace.EnvForbiddenTownRoot), strings.Join(leaks, "\n"))
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
	// "" when not inside one. Workspace resolution refuses it.
	RealTownRoot string
	// StartDir is the working directory when the harness started, which is
	// inside RealTownRoot when a test binary runs from a worktree in it.
	StartDir string
	// TmuxSocket is the isolated per-process tmux socket (-L flag) this
	// harness bound via tmux.SetDefaultSocket, or "" when tmux isn't
	// installed or isolation was bypassed via AllowLiveTmuxEnv.
	TmuxSocket string

	host *harnessHost // nil is processHost
	cfg  hermeticConfig
	// refusedGitLog is where WithoutGit's refusing git records calls; ""
	// without WithoutGit.
	refusedGitLog string
}

type hermeticConfig struct {
	dolt  bool
	noGit bool
}

// HermeticOption configures StartHermetic/HermeticMain.
type HermeticOption func(*hermeticConfig)

// WithDolt starts a shared ephemeral Dolt container for the package
// (EnsureDoltContainerForTestMain) and routes GT_DOLT_PORT/BEADS_DOLT_PORT to
// it. Without this option those variables stay poisoned and Dolt-touching
// code fails fast. Without the GT_TEST_DOCKER=1 opt-in the harness warns and
// continues, and Dolt-dependent tests skip; with it, a container that will
// not start fails StartHermetic.
func WithDolt() HermeticOption {
	return func(c *hermeticConfig) { c.dolt = true }
}

// WithoutGit puts a git on PATH that refuses to run, ahead of the real one.
// A package listed in internal/testpolicy/gitfree.txt passes it from its
// unit-tier TestMain, so a git process started anywhere in that tier, even
// from production code, fails instead of running (docs/testing.md, "Seams
// for external tools"). The refusing git also records the call, and Finish
// fails the run when any call was recorded: production code that tolerates
// a git failure would otherwise swallow the refusal and let the test pass.
// The integration tier's TestMain does not pass it.
func WithoutGit() HermeticOption {
	return func(c *hermeticConfig) { c.noGit = true }
}

// noGitMessage is what the refusing git writes to stderr.
const noGitMessage = "git: this package's unit tier runs no git (internal/testpolicy/gitfree.txt); use gitfake or canned output, or move the test to the integration tier"

// refusedGitLog is the file in the refusing git's directory that records
// each refused call.
const refusedGitLog = "refused.log"

// installRefusingGit writes the refusing git into dir and puts dir first on
// PATH. It returns the refused-call log's path, or "" where no refusing git
// is installed (Windows).
func (h *harnessHost) installRefusingGit(dir string) (string, error) {
	if runtime.GOOS == "windows" {
		return "", nil
	}
	log, err := writeRefusingGit(dir)
	if err != nil {
		return "", err
	}
	return log, h.env.Setenv("PATH", dir+string(os.PathListSeparator)+getenv(h.env, "PATH"))
}

// writeRefusingGit writes a git into dir that appends its working directory
// and arguments to dir/refused.log, prints noGitMessage and exits 1, and
// returns the log's path.
func writeRefusingGit(dir string) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("creating refusing-git dir: %w", err)
	}
	log := filepath.Join(dir, refusedGitLog)
	script := "#!/bin/sh\n" +
		"printf '%s\\tgit %s\\n' \"$PWD\" \"$*\" >> " + shellQuote(log) + "\n" +
		"echo \"" + noGitMessage + "\" >&2\n" +
		"exit 1\n"
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o755); err != nil { //nolint:gosec // G306: the refusing git must be executable
		return "", fmt.Errorf("writing refusing git: %w", err)
	}
	return log, nil
}

// shellQuote quotes s as one single-quoted sh word.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// failOnRefusedGit reports every call the refusing git recorded in log to w
// and returns code, forced to 1 from 0, when there was any. A missing log means no
// call was refused; an unreadable one fails the run, since it cannot show
// there was none.
func failOnRefusedGit(code int, log string, w io.Writer) int {
	if log == "" {
		return code
	}
	data, err := os.ReadFile(log)
	if errors.Is(err, os.ErrNotExist) {
		return code
	}
	if err != nil {
		fmt.Fprintf(w, "\nHERMETIC TRIPWIRE: cannot read the refusing git's log %s: %v\n", log, err)
		return 1
	}
	if len(data) == 0 {
		return code
	}
	calls := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	fmt.Fprintf(w, "\n%s\n", strings.Repeat("=", 72))
	fmt.Fprintf(w, "HERMETIC TRIPWIRE: the unit tier started git %d time(s) under WithoutGit\n", len(calls))
	for _, c := range calls {
		fmt.Fprintf(w, "  - %s\n", c)
	}
	fmt.Fprintf(w, "The refusal fails the run even when the code under test tolerated the git\n")
	fmt.Fprintf(w, "error. Answer git through a fake (gitfake, canned output) or move the test\n")
	fmt.Fprintf(w, "to the integration tier (docs/testing.md, \"Seams for external tools\").\n")
	fmt.Fprintf(w, "%s\n", strings.Repeat("=", 72))
	if code == 0 {
		code = 1
	}
	return code
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
// comment and forbids resolving the surrounding live town. It
// mutates process-wide state (env, temp dirs) and is meant to be called once,
// from TestMain, before m.Run().
func StartHermetic(opts ...HermeticOption) (*Hermetic, error) {
	return processHost().startHermetic(opts...)
}

// startHermetic is StartHermetic on host.
func (host *harnessHost) startHermetic(opts ...HermeticOption) (*Hermetic, error) {
	h := &Hermetic{host: host}
	for _, opt := range opts {
		opt(&h.cfg)
	}

	// Identify the live town BEFORE scrubbing env or redirecting anything.
	// An outer harness (nested `go test` runs) may have set the forbidden
	// root already, which blinds FindFromCwd to it — inherit it in that case
	// so the guard survives nesting.
	h.StartDir, _ = host.getwd() //nolint:errcheck // "" just disables the startup probe
	if root, err := host.findTown(); err == nil && root != "" {
		h.RealTownRoot = root
	} else if root := getenv(host.env, workspace.EnvForbiddenTownRoot); root != "" {
		h.RealTownRoot = root
	} else if root := getenv(host.env, "GT_TOWN_ROOT"); root != "" {
		if ok, _ := host.isWorkspace(root); ok {
			h.RealTownRoot = root
		}
	}

	// An outer harness (e.g. a test that runs `go test` as a subprocess) may
	// have provided an ephemeral Dolt server already; keep its routing.
	externalDolt := getenv(host.env, "GT_TEST_EXTERNAL_DOLT") == "1"

	// Capture before the scrub below strips it: AllowLiveTmuxEnv is a
	// BEADS_* var, so scrubProcessEnv always removes it regardless of
	// externalDolt. Restored immediately after the scrub (see below) so both
	// isolateTmuxSocket() and tmux.NewTmux()'s own guard — which reads the
	// same var later in the process lifetime, from inside test bodies — see
	// the caller's real opt-out instead of an env that was already wiped
	// before anything checked it. Without this, BEADS_TEST_ALLOW_LIVE_TMUX=1
	// was a documented but dead opt-out (gt-yav3 MR1 bounce).
	allowLiveTmux := getenv(host.env, AllowLiveTmuxEnv) == "1"

	scrubEnv(host.env, externalDolt)
	scrubTmuxVars(host.env)

	if allowLiveTmux {
		if err := host.env.Setenv(AllowLiveTmuxEnv, "1"); err != nil {
			return nil, fmt.Errorf("restoring %s: %w", AllowLiveTmuxEnv, err)
		}
	}

	sandbox, err := os.MkdirTemp(host.tempDir, "gt-hermetic-")
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
	if err := host.preserveGoEnv(); err != nil {
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
		if err := host.env.Setenv(k, v); err != nil {
			return nil, fmt.Errorf("setting %s: %w", k, err)
		}
	}
	if h.cfg.dolt {
		// Replaces the poisoned port vars with the container's mapped port.
		if err := host.ensureDolt(); err != nil {
			if dockerTestsEnabled(host.env) {
				// Opted in: the run wants the container coverage, so a
				// missing container fails the package rather than letting
				// its container tests skip.
				_ = os.RemoveAll(sandbox)
				return nil, fmt.Errorf("%s=1 opted in to the container-backed tests, but the Dolt container is unavailable: %w", DockerTestsEnv, err)
			}
			fmt.Fprintf(host.stderr,
				"hermetic harness: Dolt container unavailable (%v); Dolt-dependent tests will skip\n", err)
		}
	}

	if h.cfg.noGit {
		log, err := host.installRefusingGit(filepath.Join(sandbox, "nogit"))
		if err != nil {
			return nil, err
		}
		h.refusedGitLog = log
	}

	h.TmuxSocket = host.isolateTmuxSocket()

	// The env scrub above only reaches resolvers that consult it. Probe them
	// from the package's own directory — the one cwd guaranteed to sit inside
	// the live worktree — so a resolver that ignores the guard fails here
	// rather than mid-test.
	if h.RealTownRoot != "" {
		if err := host.assertLiveTownRefused(h.StartDir); err != nil {
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
func (h *harnessHost) isolateTmuxSocket() string {
	if getenv(h.env, AllowLiveTmuxEnv) == "1" {
		return ""
	}
	if _, err := h.lookPath("tmux"); err != nil {
		return ""
	}
	socket := fmt.Sprintf("gt-test-%d", h.pid)
	h.setTmuxSocket(socket)
	return socket
}

// Finish tears down the sandbox. It returns the exit code for os.Exit: the
// m.Run() code, forced to 1 when the shared Dolt container's catalog guard
// finds a database created or dropped while tests ran (ErrDoltCatalogChanged),
// when the container fails to terminate, or when WithoutGit's refusing git was
// started.
func (h *Hermetic) Finish(code int) int {
	host := h.host
	if host == nil {
		host = processHost()
	}
	// No-op when no container was started; also covers containers started
	// lazily by tests via RequireDoltContainer.
	if err := host.terminateDolt(); errors.Is(err, ErrDoltCatalogChanged) {
		fmt.Fprintf(host.stderr, "\n%s\n", strings.Repeat("=", 72))
		fmt.Fprintf(host.stderr, "DOLT CATALOG GUARD: %v\n", err)
		fmt.Fprintf(host.stderr, "%s\n", strings.Repeat("=", 72))
		if code == 0 {
			code = 1
		}
	} else if err != nil {
		fmt.Fprintf(host.stderr, "\n%s\n", strings.Repeat("=", 72))
		fmt.Fprintf(host.stderr, "HERMETIC TRIPWIRE: shared Dolt container failed to terminate: %v\n", err)
		fmt.Fprintf(host.stderr, "A container that fails to terminate keeps running and holding\n")
		fmt.Fprintf(host.stderr, "memory on the shared Docker VM (gt-p98h, gt-n5g6). Investigate\n")
		fmt.Fprintf(host.stderr, "rather than re-running: repeated leaks exhaust it town-wide.\n")
		fmt.Fprintf(host.stderr, "%s\n", strings.Repeat("=", 72))
		if code == 0 {
			code = 1
		}
	}
	// Read before the sandbox that holds it is removed.
	code = failOnRefusedGit(code, h.refusedGitLog, host.stderr)
	if h.SandboxDir != "" {
		_ = os.RemoveAll(h.SandboxDir)
	}
	if h.TmuxSocket != "" {
		_, _ = host.run("tmux", "-L", h.TmuxSocket, "kill-server") //nolint:errcheck // best-effort cleanup
		_ = os.Remove(filepath.Join(host.tmuxSocketDir(), h.TmuxSocket))
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

// scrubEnv removes every GT_*, BD_* and BEADS_* variable from env so tests
// and their subprocesses cannot inherit live town context from the invoking
// agent session. When keepDolt is true the Dolt passthrough variables survive
// (an outer runner provided the server). It leaves bd's telemetry switched
// off (bdTelemetryOff).
func scrubEnv(env environment, keepDolt bool) {
	defer func() {
		for k, v := range bdTelemetryOff {
			_ = env.Setenv(k, v)
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
	for _, kv := range env.Environ() {
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
		_ = env.Unsetenv(name)
	}
}

// liveTmuxVars are the session-identity variables an agent shell exports and
// tmux honors ahead of its own default socket.
var liveTmuxVars = []string{"TMUX", "TMUX_PANE", "TMUX_TMPDIR"}

// scrubTmuxVars drops the invoking agent's tmux identity from env, so a bare
// `tmux` in a test or its subprocess cannot reach the live town server. No
// socket-scoped wrapper covers a shell-out; the scrub does (gt-2bj).
func scrubTmuxVars(env environment) {
	for _, v := range liveTmuxVars {
		_ = env.Unsetenv(v)
	}
}

// writeSandboxGitConfig gives the sandbox HOME a deterministic git identity,
// since redirecting HOME hides the developer's ~/.gitconfig and git commands
// in tests would otherwise fail with "Please tell me who you are".
//
// It also points init.templateDir at a minimal template: an empty hooks
// directory and info/exclude, without git's stock template's fourteen
// *.sample hooks and description. Every repo a test inits or clones would
// otherwise carry those fifteen inert files, and creating, copying and
// deleting them is a large share of the filesystem work git-heavy packages
// do (gt-22hdp.13).
func writeSandboxGitConfig(home string) error {
	template := filepath.Join(home, ".git-template")
	if err := os.MkdirAll(filepath.Join(template, "hooks"), 0o755); err != nil {
		return fmt.Errorf("creating sandbox git template: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(template, "info"), 0o755); err != nil {
		return fmt.Errorf("creating sandbox git template: %w", err)
	}
	exclude := "# git ls-files --others --exclude-from=.git/info/exclude\n# Lines that start with '#' are comments.\n"
	if err := os.WriteFile(filepath.Join(template, "info", "exclude"), []byte(exclude), 0o644); err != nil {
		return fmt.Errorf("writing sandbox git template: %w", err)
	}
	cfg := `[user]
	name = Hermetic Test
	email = hermetic@test.invalid
[init]
	defaultBranch = main
	templateDir = ` + template + `
[commit]
	gpgsign = false
[tag]
	gpgsign = false
[core]
	fsync = none
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
func (h *harnessHost) preserveGoEnv() error {
	goBin, err := h.lookPath("go")
	if err != nil {
		return nil
	}

	if getenv(h.env, "GOENV") == "" {
		out, err := h.run(goBin, "env", "GOENV")
		if err != nil {
			return fmt.Errorf("asking the go tool for GOENV: %w", err)
		}
		if goEnvPath := strings.TrimSpace(string(out)); goEnvPath != "" {
			if err := h.env.Setenv("GOENV", goEnvPath); err != nil {
				return fmt.Errorf("pinning GOENV: %w", err)
			}
		}
	}

	var missing []string
	for _, v := range goEnvCarryVars {
		if getenv(h.env, v) == "" {
			missing = append(missing, v)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	// -json: values are space-bearing flag strings and may legitimately be
	// empty, so splitting output on newlines would be lossy.
	args := append([]string{"env", "-json"}, missing...)
	out, err := h.run(goBin, args...)
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
		if err := h.env.Setenv(v, values[v]); err != nil {
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
		os.Clearenv() //testpolicy:allow prod-no-setenv — restores the environment HermeticTest rewrote for this test
		for _, kv := range saved {
			if name, val, ok := strings.Cut(kv, "="); ok {
				_ = os.Setenv(name, val) //testpolicy:allow prod-no-setenv — restores the environment HermeticTest rewrote for this test
			}
		}
	})
	return processHost().hermeticTest(t)
}

// hermeticTest is HermeticTest's treatment of host's environment, which the
// caller restores.
func (h *harnessHost) hermeticTest(t testing.TB) string {
	t.Helper()
	scrubEnv(h.env, getenv(h.env, "GT_TEST_EXTERNAL_DOLT") == "1")

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
	if err := h.preserveGoEnv(); err != nil {
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
	startDir, _ := h.getwd() //nolint:errcheck // "" just disables the startup probe
	realRoot := ""
	if root, err := h.findTown(); err == nil && root != "" {
		realRoot = root
		testEnvs[workspace.EnvForbiddenTownRoot] = root
	}
	for k, v := range testEnvs {
		if err := h.env.Setenv(k, v); err != nil {
			t.Fatalf("setting %s: %v", k, err)
		}
	}
	if getenv(h.env, "GT_TEST_EXTERNAL_DOLT") != "1" {
		_ = h.env.Setenv("GT_DOLT_PORT", poisonDoltPort)
		_ = h.env.Setenv("BEADS_DOLT_PORT", poisonDoltPort)
	}
	if realRoot != "" {
		if err := h.assertLiveTownRefused(startDir); err != nil {
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

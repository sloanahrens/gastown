package testutil

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/workspace"
)

// makeFakeTown builds a minimal "live town" fixture: marker file, rigs.json
// with one known rig, watched subdirectories, and an events log.
func makeFakeTown(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "mayor"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(root, "mayor", "town.json"), `{"type":"town","version":2,"name":"fake"}`)
	writeFile(t, filepath.Join(root, "mayor", "rigs.json"), `{"version":1,"rigs":{"gastown":{}}}`)
	for _, sub := range []string{".beads", ".dolt-data"} {
		if err := os.MkdirAll(filepath.Join(root, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(root, ".events.jsonl"),
		`{"ts":"2026-09-08T00:00:00Z","source":"gt","type":"boot","actor":"mayor","visibility":"feed"}`+"\n")
	return root
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// goEnvSet makes preserveGoEnv a no-op on a fake harness: everything it
// would ask the go tool for is already set.
var goEnvSet = []string{"GOENV=/dev/go/env", "GOPATH=/dev/go", "GOCACHE=/dev/cache", "GOMODCACHE=/dev/go/pkg/mod"}

func TestHermeticTest_ScrubsAndRedirects(t *testing.T) {
	t.Parallel()
	f := newFakeHarness(t, append([]string{"GT_ROLE=gastown/polecats/flint", "BD_ACTOR=someone", "BEADS_DB=gt", "HOME=/home/dev"}, goEnvSet...)...)

	town := f.hermeticTest(t)

	for _, v := range []string{"GT_ROLE", "BD_ACTOR", "BEADS_DB"} {
		if got, ok := f.env.LookupEnv(v); ok {
			t.Errorf("%s survived the scrub: %q", v, got)
		}
	}
	if home := f.env.get("HOME"); home == "/home/dev" || home == "" {
		t.Errorf("HOME not redirected: %q", home)
	}
	for k, want := range map[string]string{
		"GT_DOLT_PORT":          poisonDoltPort,
		"BEADS_DOLT_PORT":       poisonDoltPort,
		HermeticEnvVar:          "1",
		"GT_TOWN_ROOT":          town,
		"BEADS_DOLT_AUTO_START": "0",
	} {
		if got := f.env.get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if ok, _ := workspace.IsWorkspace(town); !ok {
		t.Errorf("sandbox town %q is not a valid workspace", town)
	}
	if _, ok := f.env.LookupEnv(workspace.EnvForbiddenTownRoot); ok {
		t.Error("a forbidden root was set with no live town around")
	}
}

// With an outer runner's Dolt (GT_TEST_EXTERNAL_DOLT=1) the per-test scrub
// keeps its routing instead of poisoning it.
func TestHermeticTest_KeepsExternalDolt(t *testing.T) {
	t.Parallel()
	f := newFakeHarness(t, append([]string{"GT_TEST_EXTERNAL_DOLT=1", "GT_DOLT_PORT=4400", "BEADS_DOLT_PORT=4400"}, goEnvSet...)...)
	f.hermeticTest(t)
	if got := f.env.get("GT_DOLT_PORT"); got != "4400" {
		t.Errorf("GT_DOLT_PORT = %q, want the external server's 4400", got)
	}
}

// TestStartHermetic_IsolatesTmuxSocketByDefault guards the isolation half of
// gt-yav3: without an explicit opt-out, StartHermetic must bind a throwaway
// per-process tmux socket rather than leaving the default (town) socket in
// force, so tests that construct tmux.Tmux land on a private server. Finish
// kills that server, with the tmux it found at start, when one was started:
// otherwise it runs no tmux. With no tmux installed there is nothing to bind.
func TestStartHermetic_IsolatesTmuxSocketByDefault(t *testing.T) {
	t.Parallel()
	f := newFakeHarness(t, goEnvSet...)
	h, err := f.startHermetic()
	if err != nil {
		t.Fatalf("StartHermetic: %v", err)
	}
	if h.TmuxSocket != "gt-test-4242" || f.socket != "gt-test-4242" {
		t.Errorf("TmuxSocket = %q, bound %q; want the per-process gt-test-4242", h.TmuxSocket, f.socket)
	}
	socket := filepath.Join(f.tmuxSocketDir(), "gt-test-4242")
	if err := os.WriteFile(socket, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	h.Finish(0)
	if !slices.Contains(f.commands(), "/fake/bin/tmux -L gt-test-4242 kill-server") {
		t.Errorf("Finish did not kill the isolated server: %q", f.commands())
	}
	if _, err := os.Lstat(socket); err == nil {
		t.Errorf("Finish left the isolated server's socket %s", socket)
	}

	f = newFakeHarness(t, goEnvSet...)
	h, err = f.startHermetic()
	if err != nil {
		t.Fatalf("StartHermetic: %v", err)
	}
	h.Finish(0)
	for _, c := range f.commands() {
		if strings.Contains(c, "tmux") {
			t.Errorf("Finish ran %q with no server started, want no tmux", c)
		}
	}

	f = newFakeHarness(t, goEnvSet...).noTool("tmux")
	if h, err := f.startHermetic(); err != nil || h.TmuxSocket != "" || f.socket != "" {
		t.Errorf("without tmux: %v, socket %q, bound %q; want none", err, h.TmuxSocket, f.socket)
	}
}

// TestStartHermetic_AllowLiveTmuxSurvivesScrub guards the gt-yav3 MR1 bounce:
// AllowLiveTmuxEnv (BEADS_TEST_ALLOW_LIVE_TMUX) is itself a BEADS_* variable,
// so scrubProcessEnv used to wipe it before anything ever checked it —
// isolateTmuxSocket() here, and tmux.NewTmux()'s own live-socket guard later
// in the process lifetime — making the documented opt-out permanently dead.
// StartHermetic must restore the caller's opt-out immediately after the scrub
// so both call sites see it.
func TestStartHermetic_AllowLiveTmuxSurvivesScrub(t *testing.T) {
	t.Parallel()
	f := newFakeHarness(t, append([]string{AllowLiveTmuxEnv + "=1"}, goEnvSet...)...)

	h, err := f.startHermetic()
	if err != nil {
		t.Fatalf("StartHermetic: %v", err)
	}
	if got := f.env.get(AllowLiveTmuxEnv); got != "1" {
		t.Errorf("%s = %q after StartHermetic, want \"1\" (the opt-out did not survive the scrub)", AllowLiveTmuxEnv, got)
	}
	if h.TmuxSocket != "" || f.socket != "" {
		t.Errorf("TmuxSocket = %q, bound %q; want none — AllowLiveTmuxEnv=1 must bypass tmux socket isolation", h.TmuxSocket, f.socket)
	}
}

// The scrub removes the invoking agent's GT_*/BD_*/BEADS_* context and its
// tmux identity, poisons the Dolt ports, and redirects HOME and the config
// dirs into the sandbox.
func TestStartHermetic_ScrubsAndRedirects(t *testing.T) {
	t.Parallel()
	f := newFakeHarness(t, append([]string{
		"GT_ROLE=gastown/polecats/topaz", "BD_ACTOR=someone", "BEADS_DIR=/live/.beads",
		"TMUX=/private/tmp/tmux-501/gt-town,123,0", "TMUX_PANE=%3", "HOME=/home/dev", "PATH=/usr/bin",
	}, goEnvSet...)...)

	h, err := f.startHermetic()
	if err != nil {
		t.Fatalf("StartHermetic: %v", err)
	}
	for _, k := range []string{"GT_ROLE", "BD_ACTOR", "BEADS_DIR", "TMUX", "TMUX_PANE"} {
		if v, ok := f.env.LookupEnv(k); ok {
			t.Errorf("%s survived the scrub: %q", k, v)
		}
	}
	for k, want := range map[string]string{
		"HOME":              h.HomeDir,
		"CLAUDE_CONFIG_DIR": filepath.Join(h.SandboxDir, "claude"),
		"XDG_CONFIG_HOME":   filepath.Join(h.HomeDir, ".config"),
		"GT_TOWN_ROOT":      h.TownRoot,
		"GT_DOLT_PORT":      poisonDoltPort,
		"BEADS_DOLT_PORT":   poisonDoltPort,
		HermeticEnvVar:      "1",
		"PATH":              "/usr/bin",
	} {
		if got := f.env.get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if data, err := os.ReadFile(filepath.Join(h.HomeDir, ".gitconfig")); err != nil || !strings.Contains(string(data), "name = Hermetic Test") {
		t.Errorf("sandbox gitconfig: %v", err)
	}
	h.Finish(0)
	if _, err := os.Stat(h.SandboxDir); !os.IsNotExist(err) {
		t.Errorf("Finish left the sandbox: %v", err)
	}
}

// WithoutGit puts a refusing git first on PATH.
func TestStartHermetic_WithoutGitPutsRefusingGitFirst(t *testing.T) {
	t.Parallel()
	f := newFakeHarness(t, append([]string{"PATH=/usr/bin"}, goEnvSet...)...)
	h, err := f.startHermetic(WithoutGit())
	if err != nil {
		t.Fatalf("StartHermetic: %v", err)
	}
	defer h.Finish(0)
	dir := filepath.Join(h.SandboxDir, "nogit")
	if got := f.env.get("PATH"); got != dir+string(os.PathListSeparator)+"/usr/bin" {
		t.Errorf("PATH = %q, want the refusing git's dir first", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "git")); err != nil {
		t.Errorf("no refusing git written: %v", err)
	}
}

// Started inside a live town, the harness forbids resolving it, never reads
// it, and refuses to start when an in-process resolver still
// reaches it (gt-dr664).
func TestStartHermetic_LiveTown(t *testing.T) {
	t.Parallel()
	town, inner := liveTownFixture(t)
	under := func(root string) bool {
		return root == town || strings.HasPrefix(root, town+string(filepath.Separator))
	}

	f := newFakeHarness(t, goEnvSet...)
	f.findTown = func() (string, error) { return town, nil }
	f.getwd = func() (string, error) { return inner, nil }
	f.forbidden = under
	f.resolvers = []liveTownResolver{{"loud", func(string) string { panic(workspace.ErrForbiddenTownRoot) }}}
	h, err := f.startHermetic()
	if err != nil {
		t.Fatalf("StartHermetic inside a live town: %v", err)
	}
	if h.RealTownRoot != town {
		t.Errorf("RealTownRoot = %q, want the live town", h.RealTownRoot)
	}
	if got := f.env.get(workspace.EnvForbiddenTownRoot); got != town {
		t.Errorf("%s = %q, want the live town", workspace.EnvForbiddenTownRoot, got)
	}
	// What the town does while tests run is gt doctor's to judge, not the
	// run's (gt-ik4a1.3): a new database there does not fail it.
	writeFile(t, filepath.Join(town, ".dolt-data", "testdb_leak"), "")
	if code := h.Finish(0); code != 0 {
		t.Errorf("Finish after a town change = %d, stderr %q; want the run's own verdict", code, f.stderr.String())
	}

	f = newFakeHarness(t, goEnvSet...)
	f.findTown = func() (string, error) { return town, nil }
	f.forbidden = under
	f.resolvers = []liveTownResolver{{"leaky", func(string) string { return town }}}
	if _, err := f.startHermetic(); err == nil || !strings.Contains(err.Error(), "leaky") {
		t.Errorf("StartHermetic with a leaking resolver = %v, want a refusal naming it", err)
	}
}

// TestStartHermetic_DockerOptInSurvivesScrub: DockerTestsEnv is a GT_*
// variable, so the scrub would wipe the opt-in before WithDolt or
// RequireDoltContainer could see it. It must survive both the TestMain
// harness and the per-test HermeticTest scrub.
func TestStartHermetic_DockerOptInSurvivesScrub(t *testing.T) {
	t.Parallel()
	f := newFakeHarness(t, append([]string{DockerTestsEnv + "=1"}, goEnvSet...)...)

	h, err := f.startHermetic()
	if err != nil {
		t.Fatalf("StartHermetic: %v", err)
	}
	defer h.Finish(0)

	if !dockerTestsEnabled(f.env) {
		t.Errorf("%s did not survive StartHermetic's scrub", DockerTestsEnv)
	}
	f.hermeticTest(t)
	if !dockerTestsEnabled(f.env) {
		t.Errorf("%s did not survive HermeticTest's scrub", DockerTestsEnv)
	}
	// And the scrub still removes ordinary GT_* context.
	_ = f.env.Setenv("GT_ROLE", "gastown/polecats/topaz")
	scrubEnv(f.env, false)
	if _, ok := f.env.LookupEnv("GT_ROLE"); ok {
		t.Error("scrubEnv kept GT_ROLE")
	}
	if !dockerTestsEnabled(f.env) {
		t.Errorf("scrubEnv removed %s", DockerTestsEnv)
	}
}

// With an outer runner's Dolt (GT_TEST_EXTERNAL_DOLT=1) the scrub keeps its
// routing variables, and only then.
func TestScrubEnv_KeepsDoltPassthroughOnlyWhenAsked(t *testing.T) {
	t.Parallel()
	entries := []string{"GT_DOLT_PORT=4400", "GT_DOLT_HOST=h", "BEADS_DOLT_PORT=4400", "BEADS_DOLT_SERVER_HOST=h", "GT_TEST_EXTERNAL_DOLT=1", "GT_ROLE=x"}
	keep := newMapEnv(entries...)
	scrubEnv(keep, true)
	drop := newMapEnv(entries...)
	scrubEnv(drop, false)
	for _, k := range doltPassthroughVars {
		if _, ok := keep.LookupEnv(k); !ok {
			t.Errorf("keepDolt dropped %s", k)
		}
		if _, ok := drop.LookupEnv(k); ok {
			t.Errorf("without keepDolt %s survived", k)
		}
	}
	if _, ok := keep.LookupEnv("GT_ROLE"); ok {
		t.Error("keepDolt kept GT_ROLE")
	}
}

// TestStartHermetic_WithDoltWithoutOptIn: with the container opt-in unset,
// a TestMain that asks for Dolt must still start (no error), with the port
// left poisoned so container-dependent tests skip — the contract daemon's and
// convoy's TestMains rely on, and the reason a bare `go test` of those
// packages passes in seconds without Docker.
func TestStartHermetic_WithDoltWithoutOptIn(t *testing.T) {
	t.Parallel()
	f := newFakeHarness(t, goEnvSet...)

	h, err := f.startHermetic(WithDolt())
	if err != nil {
		t.Fatalf("StartHermetic(WithDolt) without the opt-in must not fail: %v", err)
	}
	defer h.Finish(0)

	if got := f.env.get("GT_DOLT_PORT"); got != poisonDoltPort {
		t.Errorf("GT_DOLT_PORT = %q, want the poison port %q", got, poisonDoltPort)
	}
	if !strings.Contains(f.stderr.String(), "Dolt-dependent tests will skip") {
		t.Errorf("stderr = %q, want the skip warning", f.stderr.String())
	}
}

// TestStartHermetic_WithDoltOptedInFailsWithoutContainer: once GT_TEST_DOCKER=1
// opts in, a container that will not start fails the package's TestMain
// instead of letting every container test skip on the empty port — an opt-in
// run that loses its coverage must not read as green.
func TestStartHermetic_WithDoltOptedInFailsWithoutContainer(t *testing.T) {
	t.Parallel()
	f := newFakeHarness(t, append([]string{DockerTestsEnv + "=1"}, goEnvSet...)...)
	f.ensureDolt = func() error { return errors.New("simulated: Docker not available") }

	h, err := f.startHermetic(WithDolt())
	if err == nil {
		h.Finish(0)
		t.Fatal("StartHermetic(WithDolt) with the opt-in set and no container succeeded; want an error")
	}
	if !strings.Contains(err.Error(), "simulated: Docker not available") || !strings.Contains(err.Error(), DockerTestsEnv) {
		t.Errorf("StartHermetic error = %q, want it to carry the cause and name %s", err, DockerTestsEnv)
	}
}

// TestFinish_DoltTerminationFailureFailsLoud pins the fix for gt-p98h/gt-n5g6:
// a Dolt container that fails to terminate must fail the run, not vanish
// silently and keep holding memory on the shared Docker VM until it's
// noticed hours later.
func TestFinish_DoltTerminationFailureFailsLoud(t *testing.T) {
	t.Parallel()
	f := newFakeHarness(t)
	f.terminateDolt = func() error { return errors.New("simulated: container still running") }

	if code := (&Hermetic{host: f.harnessHost}).Finish(0); code != 1 {
		t.Errorf("Finish(0) with a termination error = %d, want 1 (forced failure)", code)
	}
	if !strings.Contains(f.stderr.String(), "HERMETIC TRIPWIRE: shared Dolt container failed to terminate") {
		t.Errorf("Finish stderr = %q, want it to name the termination tripwire", f.stderr.String())
	}
}

// A catalog-guard failure at teardown fails the run under its own banner,
// which names the database, rather than the termination tripwire's.
func TestFinish_DoltCatalogGuardFailsLoud(t *testing.T) {
	t.Parallel()
	f := newFakeHarness(t)
	f.terminateDolt = func() error {
		return catalogViolations([]string{"gt_test", "information_schema", "mysql", "beads"}, nil, nil)
	}

	if code := (&Hermetic{host: f.harnessHost}).Finish(0); code != 1 {
		t.Errorf("Finish(0) with a catalog-guard error = %d, want 1", code)
	}
	for _, want := range []string{"DOLT CATALOG GUARD", `database "beads" was created`} {
		if !strings.Contains(f.stderr.String(), want) {
			t.Errorf("Finish stderr = %q, want it to contain %q", f.stderr.String(), want)
		}
	}
}

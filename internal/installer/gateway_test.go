package installer

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// testGatewayTemplate is a unit with two homes to render and a trailing newline,
// which the files the step writes must keep as exactly one.
const testGatewayTemplate = "<?xml version=\"1.0\"?>\n<string>__HOME__/.config/openshell/gateway.toml</string>\n"

// testGateway is the step's world: a temporary home and checkout, and a
// recording runner in place of launchctl, so no case runs a process.
type testGateway struct {
	Gateway
	root      string
	out       strings.Builder
	errOut    strings.Builder
	execs     [][]string
	loaded    bool
	failsBoot bool
}

func newTestGateway(t *testing.T) *testGateway {
	t.Helper()
	root := t.TempDir()
	g := &testGateway{root: root}
	g.Gateway = Gateway{
		C:       &Contract{Stdout: &g.out, Stderr: &g.errOut},
		RigDir:  filepath.Join(root, "rig"),
		Home:    filepath.Join(root, "home"),
		SelfDir: filepath.Join(root, "bin", "gt-installer"),
	}
	g.C.Exec = func(name string, args ...string) ([]byte, error) {
		g.execs = append(g.execs, append([]string{name}, args...))
		switch {
		case len(args) > 0 && args[0] == "print" && !g.loaded:
			return nil, errors.New("no such service")
		case len(args) > 0 && args[0] == "bootstrap" && g.failsBoot:
			return nil, errors.New("bootstrap failed")
		}
		return nil, nil
	}
	return g
}

func (g *testGateway) toml() string {
	return filepath.Join(g.Home, ".config", "openshell", "gateway.toml")
}

func (g *testGateway) rigTemplate() string {
	return filepath.Join(g.RigDir, "scripts", "launchd", gatewayTemplateName)
}

func (g *testGateway) plist() string {
	return filepath.Join(g.Home, "Library", "LaunchAgents", gatewayLabel+".plist")
}

func (g *testGateway) domain() string { return fmt.Sprintf("gui/%d", os.Getuid()) }

func (g *testGateway) lines() []string {
	trimmed := strings.TrimSuffix(g.out.String(), "\n")
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "\n")
}

// warnings is stderr split into the lines a reader sees.
func (g *testGateway) warnings() []string {
	trimmed := strings.TrimSuffix(g.errOut.String(), "\n")
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "\n")
}

// prepared writes the operator's gateway.toml and the checkout's template, the
// two files every step past the first two checks needs.
func (g *testGateway) prepared(t *testing.T) {
	t.Helper()
	writeFileAt(t, g.toml(), "")
	writeFileAt(t, g.rigTemplate(), testGatewayTemplate)
}

func writeFileAt(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A home without the operator's gateway.toml has nothing to start the agent
// with: both modes say they skipped and touch nothing.
func TestGatewaySkipsWithoutTheOperatorsConfig(t *testing.T) {
	t.Parallel()
	for _, mode := range []GatewayMode{GatewayInstall, GatewayDryRun} {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()
			g := newTestGateway(t)

			g.Step(mode)

			if want := []string{"[install-gt] gateway: skipped, no " + g.toml()}; !reflect.DeepEqual(g.lines(), want) {
				t.Errorf("stdout = %q, want %q", g.lines(), want)
			}
			if g.execs != nil {
				t.Errorf("ran %q, want no command", g.execs)
			}
			if _, err := os.Stat(g.plist()); err == nil {
				t.Error("wrote a unit with no gateway.toml")
			}
		})
	}
}

// Without a template there is nothing to render: one WARNING names it and
// nothing is written or loaded.
func TestGatewayWithoutATemplate(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		selfDir func(g *testGateway) string
		setup   func(t *testing.T, g *testGateway)
		want    func(g *testGateway) string
	}{
		{
			name:    "a program that cannot name its directory keeps the checkout's path",
			selfDir: func(*testGateway) string { return "" },
			want:    func(g *testGateway) string { return g.rigTemplate() },
		},
		{
			name:    "a relative program directory is no fallback",
			selfDir: func(*testGateway) string { return "bin" },
			want:    func(g *testGateway) string { return g.rigTemplate() },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			g := newTestGateway(t)
			g.SelfDir = tc.selfDir(g)
			writeFileAt(t, g.toml(), "")

			g.Step(GatewayDryRun)

			if want := []string{"[install-gt] WARNING: gateway: no template at " + tc.want(g)}; !reflect.DeepEqual(g.warnings(), want) {
				t.Errorf("stderr = %q, want %q", g.warnings(), want)
			}
			if g.lines() != nil {
				t.Errorf("stdout = %q, want nothing", g.lines())
			}
			if _, err := os.Stat(g.plist()); err == nil {
				t.Error("wrote a unit from no template")
			}
		})
	}
}

// The program's own directory is the fallback when the checkout carries no
// template, and that copy installs like any other.
func TestGatewayFallsBackToTheProgramsOwnTemplate(t *testing.T) {
	t.Parallel()
	g := newTestGateway(t)
	writeFileAt(t, g.toml(), "")
	writeFileAt(t, filepath.Join(g.SelfDir, "launchd", gatewayTemplateName), testGatewayTemplate)

	g.Step(GatewayInstall)

	if _, err := os.Stat(g.plist()); err != nil {
		t.Fatalf("no unit written from the fallback template: %v", err)
	}
	if g.warnings() != nil {
		t.Errorf("stderr = %q, want nothing", g.warnings())
	}
}

// A dry run prints what an install would do, in the script's two lines, and
// still asks launchd: a unit that is already loaded has nothing to bootstrap.
func TestGatewayDryRun(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		setup  func(t *testing.T, g *testGateway)
		lines  func(g *testGateway) []string
		loaded bool
	}{
		{
			name: "a unit to write and an agent to load",
			lines: func(g *testGateway) []string {
				domain, plist := g.domain(), g.plist()
				return []string{
					"[install-gt] gateway: would write " + plist,
					"[install-gt] gateway: would run: launchctl bootstrap " + domain + " " + plist,
				}
			},
		},
		{
			name: "a loaded agent with a stale unit has both steps to do",
			setup: func(t *testing.T, g *testGateway) {
				writeFileAt(t, g.plist(), "stale\n")
			},
			loaded: true,
			lines: func(g *testGateway) []string {
				plist := g.plist()
				return []string{"[install-gt] gateway: would write " + plist}
			},
		},
		{
			name: "a loaded agent with a current unit has nothing to do",
			setup: func(t *testing.T, g *testGateway) {
				rendered, err := g.render(g.rigTemplate())
				if err != nil {
					t.Fatal(err)
				}
				writeFileAt(t, g.plist(), rendered)
			},
			loaded: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			g := newTestGateway(t)
			g.prepared(t)
			g.loaded = tc.loaded
			if tc.setup != nil {
				tc.setup(t, g)
			}
			before, err := os.ReadFile(g.plist())
			if err != nil {
				before = nil
			}

			g.Step(GatewayDryRun)

			var want []string
			if tc.lines != nil {
				want = tc.lines(g)
			}
			if !reflect.DeepEqual(g.lines(), want) {
				t.Errorf("stdout = %q, want %q", g.lines(), want)
			}
			if !reflect.DeepEqual(g.execs, [][]string{{"launchctl", "print", g.domain() + "/" + gatewayLabel}}) {
				t.Errorf("ran %q, want only the launchd lookup", g.execs)
			}
			after, err := os.ReadFile(g.plist())
			if (err == nil) != (before != nil) || string(after) != string(before) {
				t.Errorf("the unit changed: before %q, after %q", before, after)
			}
		})
	}
}

// The checkout being installed owns the template; the program's copy is only
// the fallback.
func TestGatewayPrefersTheCheckoutsTemplate(t *testing.T) {
	t.Parallel()
	g := newTestGateway(t)
	g.prepared(t)
	writeFileAt(t, filepath.Join(g.SelfDir, "launchd", gatewayTemplateName), "the program's copy\n")

	g.Step(GatewayInstall)

	got, err := os.ReadFile(g.plist())
	if err != nil {
		t.Fatalf("unit: %v", err)
	}
	if !strings.Contains(string(got), "<string>") {
		t.Errorf("unit = %q, want the checkout's template", got)
	}
}

// A template that exists but cannot be read is one WARNING: nothing is written
// and nothing is loaded.
func TestGatewayCannotRenderTheTemplate(t *testing.T) {
	t.Parallel()
	g := newTestGateway(t)
	g.Files = Files{
		IsFile:   func(string) bool { return true },
		ReadFile: func(string) ([]byte, error) { return nil, errors.New("permission denied") },
	}

	g.Step(GatewayInstall)

	want := []string{"[install-gt] WARNING: gateway: could not render " + g.rigTemplate()}
	if !reflect.DeepEqual(g.warnings(), want) {
		t.Errorf("stderr = %q, want %q", g.warnings(), want)
	}
	if g.execs != nil {
		t.Errorf("ran %q, want no command", g.execs)
	}
}

// A second install over a current, loaded unit has nothing to do: no write, no
// bootstrap, no line.
func TestGatewayLeavesACurrentLoadedUnitAlone(t *testing.T) {
	t.Parallel()
	g := newTestGateway(t)
	g.prepared(t)
	g.loaded = true
	rendered, err := g.render(g.rigTemplate())
	if err != nil {
		t.Fatal(err)
	}
	writeFileAt(t, g.plist(), rendered)

	g.Step(GatewayInstall)

	if g.lines() != nil {
		t.Errorf("stdout = %q, want nothing", g.lines())
	}
	if want := [][]string{{"launchctl", "print", g.domain() + "/" + gatewayLabel}}; !reflect.DeepEqual(g.execs, want) {
		t.Errorf("ran %q, want only the launchd lookup", g.execs)
	}
	got, err := os.ReadFile(g.plist())
	if err != nil {
		t.Fatalf("unit: %v", err)
	}
	if string(got) != rendered {
		t.Errorf("unit = %q, want it untouched at %q", got, rendered)
	}
}

// A real install renders the unit with the escaped home, makes the log
// directory the unit writes to, and bootstraps the agent it just wrote.
func TestGatewayInstallsTheUnit(t *testing.T) {
	t.Parallel()
	g := newTestGateway(t)
	g.Home = filepath.Join(g.root, "a&b<c home")
	g.prepared(t)

	g.Step(GatewayInstall)

	got, err := os.ReadFile(g.plist())
	if err != nil {
		t.Fatalf("unit: %v", err)
	}
	want := "<?xml version=\"1.0\"?>\n<string>" + g.root +
		"/a&amp;b&lt;c home/.config/openshell/gateway.toml</string>\n"
	if string(got) != want {
		t.Errorf("unit = %q, want %q", got, want)
	}
	if info, err := os.Stat(filepath.Join(g.Home, "gt", ".runtime", "openshell")); err != nil || !info.IsDir() {
		t.Errorf("log directory: %v", err)
	}
	domain, plist := g.domain(), g.plist()
	wantLines := []string{
		"[install-gt] gateway: wrote " + plist,
		"[install-gt] gateway: bootstrapped " + gatewayLabel,
	}
	if !reflect.DeepEqual(g.lines(), wantLines) {
		t.Errorf("stdout = %q, want %q", g.lines(), wantLines)
	}
	wantExecs := [][]string{
		{"launchctl", "print", domain + "/" + gatewayLabel},
		{"launchctl", "bootstrap", domain, plist},
	}
	if !reflect.DeepEqual(g.execs, wantExecs) {
		t.Errorf("ran %q, want %q", g.execs, wantExecs)
	}
	if g.warnings() != nil {
		t.Errorf("stderr = %q, want nothing", g.warnings())
	}
}

// A loaded agent keeps its old unit until the operator kickstarts it: the step
// rewrites the file, never bootstraps, and says how to restart it.
func TestGatewayLoadedAgentKeepsItsOldUnit(t *testing.T) {
	t.Parallel()
	g := newTestGateway(t)
	g.prepared(t)
	g.loaded = true
	writeFileAt(t, g.plist(), "stale\n")

	g.Step(GatewayInstall)

	got, err := os.ReadFile(g.plist())
	if err != nil {
		t.Fatalf("unit: %v", err)
	}
	if strings.Contains(string(got), "stale") {
		t.Error("the stale unit was left in place")
	}
	plist := g.plist()
	wantLines := []string{
		"[install-gt] gateway: wrote " + plist,
		"[install-gt] gateway: " + gatewayLabel + " is loaded and keeps its old unit until: launchctl kickstart -k " +
			g.domain() + "/" + gatewayLabel,
	}
	if !reflect.DeepEqual(g.lines(), wantLines) {
		t.Errorf("stdout = %q, want %q", g.lines(), wantLines)
	}
	if want := [][]string{{"launchctl", "print", g.domain() + "/" + gatewayLabel}}; !reflect.DeepEqual(g.execs, want) {
		t.Errorf("ran %q, want no bootstrap", g.execs)
	}
}

// A bootstrap that fails leaves the unit in place and is one WARNING: the
// install does not fail, and nothing is kickstarted.
func TestGatewayBootstrapFailure(t *testing.T) {
	t.Parallel()
	g := newTestGateway(t)
	g.prepared(t)
	g.failsBoot = true

	g.Step(GatewayInstall)

	if _, err := os.Stat(g.plist()); err != nil {
		t.Errorf("the unit was not kept: %v", err)
	}
	want := []string{"[install-gt] WARNING: gateway: launchctl bootstrap " + g.domain() + " " + g.plist() + " failed"}
	if !reflect.DeepEqual(g.warnings(), want) {
		t.Errorf("stderr = %q, want %q", g.warnings(), want)
	}
	if wantLines := []string{"[install-gt] gateway: wrote " + g.plist()}; !reflect.DeepEqual(g.lines(), wantLines) {
		t.Errorf("stdout = %q, want %q", g.lines(), wantLines)
	}
}

// A directory the step cannot make stops it with one WARNING naming the
// LaunchAgents directory: no unit, no bootstrap, no install failure. The log
// directory is created by the same call and has its own case.
func TestGatewayCannotCreateItsDirectory(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		fails func(g *testGateway, path string) bool
	}{
		{
			name:  "the LaunchAgents directory",
			fails: func(g *testGateway, path string) bool { return path == filepath.Dir(g.plist()) },
		},
		{
			name: "the log directory",
			fails: func(g *testGateway, path string) bool {
				return path == filepath.Join(g.Home, "gt", ".runtime", "openshell")
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			g := newTestGateway(t)
			g.prepared(t)
			g.Files.MkdirAll = func(path string) error {
				if tc.fails(g, path) {
					return errors.New("read-only")
				}
				return os.MkdirAll(path, 0o755)
			}

			g.Step(GatewayInstall)

			want := []string{"[install-gt] WARNING: gateway: cannot create " + filepath.Dir(g.plist())}
			if !reflect.DeepEqual(g.warnings(), want) {
				t.Errorf("stderr = %q, want %q", g.warnings(), want)
			}
			if g.lines() != nil {
				t.Errorf("stdout = %q, want nothing", g.lines())
			}
			if _, err := os.Stat(g.plist()); err == nil {
				t.Error("wrote a unit into a directory it could not make")
			}
			for _, exec := range g.execs {
				if len(exec) > 1 && exec[1] == "bootstrap" {
					t.Errorf("ran %q, want no bootstrap", exec)
				}
			}
		})
	}
}

// A unit that cannot be written is one WARNING, leaves no temporary file
// behind, and is never bootstrapped.
func TestGatewayCannotWriteTheUnit(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		files func() Files
	}{
		{
			name: "the temporary file",
			files: func() Files {
				return Files{WriteFile: func(string, []byte, os.FileMode) error { return errors.New("disk full") }}
			},
		},
		{
			name: "the rename into place",
			files: func() Files {
				return Files{Rename: func(string, string) error { return errors.New("read-only") }}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			g := newTestGateway(t)
			g.prepared(t)
			g.Files = tc.files()

			g.Step(GatewayInstall)

			want := []string{"[install-gt] WARNING: gateway: cannot write " + g.plist()}
			if !reflect.DeepEqual(g.warnings(), want) {
				t.Errorf("stderr = %q, want %q", g.warnings(), want)
			}
			if g.lines() != nil {
				t.Errorf("stdout = %q, want nothing", g.lines())
			}
			entries, err := os.ReadDir(filepath.Dir(g.plist()))
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				t.Errorf("left %q behind", entry.Name())
			}
			for _, exec := range g.execs {
				if len(exec) > 1 && exec[1] == "bootstrap" {
					t.Errorf("ran %q, want no bootstrap", exec)
				}
			}
		})
	}
}

// The no-op path runs the gateway step before its receipt, as the script does:
// a clean no-op still writes and bootstraps the agent (gt-tpg0p.9).
func TestNoopRunsTheGatewayStep(t *testing.T) {
	t.Parallel()
	tr := newTestRun(t)
	tr.resolved(installed)
	tr.Repo.IsAncestor = ancestors([2]string{full, installed})
	tr.C.Exec = func(name string, args ...string) ([]byte, error) {
		tr.execs = append(tr.execs, append([]string{name}, args...))
		if name == "launchctl" && len(args) > 0 && args[0] == "print" {
			return nil, errors.New("no such service")
		}
		return nil, nil
	}
	writeFileAt(t, filepath.Join(tr.Paths.Home, ".config", "openshell", "gateway.toml"), "")
	writeFileAt(t, filepath.Join(tr.Paths.RigDir, "scripts", "launchd", gatewayTemplateName), testGatewayTemplate)
	plist := filepath.Join(tr.Paths.Home, "Library", "LaunchAgents", gatewayLabel+".plist")

	exit, ended := tr.Noop()

	if !ended || exit != ExitOK {
		t.Fatalf("Noop() = (%d, %v), want (%d, true)", exit, ended, ExitOK)
	}
	if _, err := os.Stat(plist); err != nil {
		t.Errorf("no unit written: %v", err)
	}
	want := []string{
		"[install-gt] Installed " + installed + " already contains " + full + "; nothing to do.",
		"[install-gt] gateway: wrote " + plist,
		"[install-gt] gateway: bootstrapped " + gatewayLabel,
		"install-gt: RESULT noop " + full + " " + installed + " already-installed",
	}
	if !reflect.DeepEqual(tr.lines(), want) {
		t.Errorf("stdout = %q, want %q", tr.lines(), want)
	}
	wantExecs := [][]string{
		{"launchctl", "print", fmt.Sprintf("gui/%d", os.Getuid()) + "/" + gatewayLabel},
		{"launchctl", "bootstrap", fmt.Sprintf("gui/%d", os.Getuid()), plist},
	}
	if !reflect.DeepEqual(tr.execs, wantExecs) {
		t.Errorf("ran %q, want %q", tr.execs, wantExecs)
	}
}

// A gateway step that fails reports false, so the install can fail with it; a
// clean install, and a gateway that was never configured, report true.
func TestGatewayStepReportsItsFailure(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		setup     func(g *testGateway)
		failsBoot bool
		want      bool
	}{
		{
			name:  "a clean install reports true",
			setup: func(g *testGateway) { g.prepared(t) },
			want:  true,
		},
		{
			name:      "a failed bootstrap reports false",
			setup:     func(g *testGateway) { g.prepared(t) },
			failsBoot: true,
			want:      false,
		},
		{
			name:  "a missing template reports false",
			setup: func(g *testGateway) { writeFileAt(t, g.toml(), "") },
			want:  false,
		},
		{
			name:  "a gateway that was never configured reports true",
			setup: func(g *testGateway) {},
			want:  true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			g := newTestGateway(t)
			g.failsBoot = tc.failsBoot
			tc.setup(g)
			if got := g.Step(GatewayInstall); got != tc.want {
				t.Errorf("Step(GatewayInstall) = %v, want %v", got, tc.want)
			}
		})
	}
}

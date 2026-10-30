package tiersweep

import (
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// fakeClock is a clock a fake command advances, so a tier's elapsed time is the
// fake's to say.
type fakeClock struct{ at time.Time }

func (c *fakeClock) now() time.Time { return c.at }

// rig is one Run over fakes: the repository, the commands and the clock.
type rig struct {
	repo *fakeRepo
	tier *fakeTier
	// shellOut is what the shell-tier runner prints, shellCode its exit code.
	shellOut  string
	shellCode int
	// goTestTakes is how long each go test (a gt slot run) advances the clock.
	goTestTakes time.Duration
	clock       *fakeClock
	logDir      string
	skip        string
	trigger     string
	// timingsFile is TIER_SWEEP_TIMINGS, empty when the run names none.
	timingsFile string
	// writes and renames record the file seams a persistent timings file goes
	// through, in the order they ran.
	writes  []string
	renames [][2]string
}

func newRig(t *testing.T) *rig {
	t.Helper()
	return &rig{
		repo:   baseRepo(),
		tier:   &fakeTier{head: "abc123"},
		clock:  &fakeClock{at: time.Date(2026, time.October, 30, 0, 0, 0, 0, time.UTC)},
		logDir: t.TempDir(),
	}
}

// run calls Run with args and returns the exit code, stdout and stderr.
func (r *rig) run(args ...string) (code int, stdout, stderr string) {
	var out, errw bytes.Buffer
	exec := func(dir string, env []string, argv []string, w io.Writer) int {
		if len(argv) > 2 && argv[1] == "run" && argv[2] == shellPkg {
			fmt.Fprint(w, r.shellOut)
			return r.shellCode
		}
		if argv[0] == "gt" {
			r.clock.at = r.clock.at.Add(r.goTestTakes)
		}
		return r.tier.exec(dir, env, argv, w)
	}
	env := map[string]string{
		EnvLogDir: r.logDir, EnvSkip: r.skip, EnvTrigger: r.trigger, EnvTimeout: "7m",
		EnvTimings: r.timingsFile,
	}
	code = Run(args, &out, &errw, Deps{
		Host: Host{
			Env:       func(name string) string { return env[name] },
			MkdirTemp: func(string, string) (string, error) { return r.logDir, nil },
			MkdirAll:  func(string, fs.FileMode) error { return nil },
			WriteFile: func(name string, data []byte, perm fs.FileMode) error {
				r.writes = append(r.writes, name)
				return os.WriteFile(name, data, perm)
			},
			Rename: func(oldpath, newpath string) error {
				r.renames = append(r.renames, [2]string{oldpath, newpath})
				return os.Rename(oldpath, newpath)
			},
		},
		Exec:    exec,
		Src:     r.repo,
		Environ: []string{"PATH=/fake/bin"},
		Now:     r.clock.now,
	})
	return code, out.String(), errw.String()
}

// shellLine is the shell-tier runner's summary line with the verdict given.
func shellLine(verdict string) string {
	return "tier-sweep: shell " + verdict + " passed=2 failed=0 skipped=0 (logs /l) in 3s\n"
}

// oneTagged makes the root module hold one integration-tagged package, ./r.
func oneTagged(r *fakeRepo) {
	r.lists[". *_test.go"] = []string{"r/r_test.go"}
	r.files["r/r_test.go"] = taggedTest
}

// TestSweepTierOrderAndExit: the tiers run in the order given, a RED tier makes
// the exit 1 without stopping the later ones, and an unknown tier exits 2 after
// the earlier lines are printed and prints no summary line of its own.
func TestSweepTierOrderAndExit(t *testing.T) {
	t.Parallel()
	prefixes := func(out string) []string {
		var tiers []string
		for _, l := range strings.Split(strings.TrimSuffix(out, "\n"), "\n") {
			if l != "" {
				tiers = append(tiers, strings.Fields(l)[1])
			}
		}
		return tiers
	}
	cases := []struct {
		name      string
		args      []string
		shellCode int
		shellOut  string
		wantCode  int
		wantTiers []string
		wantErr   string
	}{
		{
			name: "no tier named runs shell, integration and race in that order",
			args: nil, shellOut: shellLine("GREEN"),
			wantCode: 0, wantTiers: []string{"shell", "integration", "race"},
		},
		{
			name: "the order given is the order run",
			args: []string{"race", "shell"}, shellOut: shellLine("GREEN"),
			wantCode: 0, wantTiers: []string{"race", "shell"},
		},
		{
			name: "a RED tier makes the exit 1 and the later tiers still run",
			args: []string{"shell", "race"}, shellOut: shellLine("RED"), shellCode: 1,
			wantCode: 1, wantTiers: []string{"shell", "race"},
		},
		{
			name:     "an unknown tier exits 2 after the earlier lines and prints no line of its own",
			args:     []string{"race", "bogus", "shell"},
			wantCode: 2, wantTiers: []string{"race"},
			wantErr: "tier-sweep: unknown tier bogus (want shell, integration or race)",
		},
		{
			name: "a RED tier before an unknown one is still printed, and the exit is 2",
			args: []string{"shell", "bogus"}, shellOut: shellLine("RED"), shellCode: 1,
			wantCode: 2, wantTiers: []string{"shell"},
			wantErr: "unknown tier bogus",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newRig(t)
			oneTagged(r.repo)
			r.shellOut, r.shellCode = tc.shellOut, tc.shellCode
			code, stdout, stderr := r.run(tc.args...)
			if code != tc.wantCode {
				t.Errorf("exit code = %d, want %d (stderr %q)", code, tc.wantCode, stderr)
			}
			if got := prefixes(stdout); !reflect.DeepEqual(got, tc.wantTiers) {
				t.Errorf("tiers printed = %v, want %v (stdout %q)", got, tc.wantTiers, stdout)
			}
			if tc.wantErr != "" && !strings.Contains(stderr, tc.wantErr) {
				t.Errorf("stderr = %q, want it to contain %q", stderr, tc.wantErr)
			}
			if strings.Contains(stdout, "unknown tier") {
				t.Errorf("the unknown-tier line reached stdout: %q", stdout)
			}
		})
	}
}

// TestSweepElapsedTimeFromTheClock: a tier's line ends with the time the clock
// moved while it ran, in the daemon's rounded style, never the real clock.
func TestSweepElapsedTimeFromTheClock(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.goTestTakes = 2*time.Minute + 14*time.Second
	_, stdout, _ := r.run("race")
	want := fmt.Sprintf("tier-sweep: race GREEN passed=1 failed=0 skipped=0 (logs %s) in 2m14s\n", r.logDir)
	if stdout != want {
		t.Errorf("stdout = %q, want %q", stdout, want)
	}
}

// TestSweepRootFlag: --root puts every command in that directory, and a bare
// --root, or an empty one, is a usage error that runs nothing.
func TestSweepRootFlag(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	if code, _, stderr := r.run("--root", "/repo", "race"); code != 0 {
		t.Fatalf("exit code = %d, stderr %q", code, stderr)
	}
	// The fake exec records the directory the rooted exec passed it.
	if got := r.tier.calls[0].dir; got != "/repo" {
		t.Errorf("race ran in %q, want /repo", got)
	}
	for _, args := range [][]string{{"--root"}, {"--root", ""}, {"--root="}} {
		r := newRig(t)
		code, stdout, stderr := r.run(args...)
		if code != 2 || stdout != "" || !strings.Contains(stderr, "--root needs a directory") || !strings.Contains(stderr, "usage:") {
			t.Errorf("%q: code %d, stdout %q, stderr %q, want exit 2 with the usage line", args, code, stdout, stderr)
		}
		if len(r.tier.calls) != 0 {
			t.Errorf("%q ran commands: %v", args, r.tier.calls)
		}
	}
}

// TestSweepLogDirFailureExitsTwo: a log directory that cannot be made stops the
// run before any tier.
func TestSweepLogDirFailureExitsTwo(t *testing.T) {
	t.Parallel()
	var out, errw bytes.Buffer
	code := Run([]string{"race"}, &out, &errw, Deps{
		Host: Host{
			Env:       func(string) string { return "" },
			MkdirTemp: func(string, string) (string, error) { return "", fmt.Errorf("disk full") },
			MkdirAll:  func(string, fs.FileMode) error { return nil },
		},
		Exec: func(string, []string, []string, io.Writer) int { t.Error("a command ran"); return 0 },
		Now:  func() time.Time { return time.Date(2026, time.October, 30, 0, 0, 0, 0, time.UTC) },
	})
	if code != 2 || out.Len() != 0 || !strings.Contains(errw.String(), "disk full") {
		t.Errorf("code %d, stdout %q, stderr %q, want exit 2, no line and the cause", code, out.String(), errw.String())
	}
}

// TestSweepGitSource: with no Source injected, the listings come from
// `git ls-files` in the directory asked about, and a failing git is an error,
// never an empty list.
func TestSweepGitSource(t *testing.T) {
	t.Parallel()
	var calls [][]string
	var dirs []string
	g := gitSource{
		exec: func(dir string, env []string, argv []string, out io.Writer) int {
			dirs = append(dirs, dir)
			calls = append(calls, argv)
			if argv[len(argv)-1] == "*/boom" {
				fmt.Fprintln(out, "fatal: not a git repository")
				return 128
			}
			fmt.Fprint(out, "a/x_test.go\n\nb/y_test.go\n")
			return 0
		},
		read: func(name string) ([]byte, error) { return []byte("read " + name), nil },
	}
	files, err := g.LsFiles("tracker", "*_test.go")
	if err != nil || !reflect.DeepEqual(files, []string{"a/x_test.go", "b/y_test.go"}) {
		t.Errorf("LsFiles = %v, %v", files, err)
	}
	if !reflect.DeepEqual(calls[0], []string{"git", "ls-files", "--", "*_test.go"}) || dirs[0] != "tracker" {
		t.Errorf("ran %v in %q", calls[0], dirs[0])
	}
	if _, err := g.LsFiles(".", "*/boom"); err == nil || !strings.Contains(err.Error(), "not a git repository") {
		t.Errorf("a failing git returned %v, want an error carrying its output", err)
	}
	if data, _ := g.ReadFile("f"); string(data) != "read f" {
		t.Errorf("ReadFile = %q", data)
	}
}

// TestSweepCaseTable runs the cases the old sweep script's shell test pinned
// through Run, one subtest per case, over the fakes: no go, gt or git runs.
// Each case reads the command's stdout line and, where the script did, its
// stderr and the commands the fake saw.
func TestSweepCaseTable(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		tier     string
		setup    func(r *rig)
		wantLine string // a fragment of the tier's stdout line
		wantCode int
		wantRuns []string
		// wantErr are fragments stderr must hold. The progress and SKIPPED lines
		// are asserted as text because the operator and the daemon's log read
		// them as written (the script's wording); the summary on stdout is the
		// byte-for-byte contract (internal/daemon tierSweepSummaryRE).
		wantErr []string
		// noErr are names stderr must not mention: a module with no tagged
		// package must not appear in the log at all.
		noErr    []string
		noRecord bool // the integration tier recorded nothing; by default it records once
	}{
		{
			name: "green tier ends with its elapsed time", tier: "integration",
			setup:    func(r *rig) { oneTagged(r.repo); r.goTestTakes = 45 * time.Second },
			wantLine: "integration GREEN passed=1 failed=0 skipped=0 (logs LOG) in 45s",
			wantRuns: []string{". ./r"},
		},
		{
			name: "red tier carries the time after the names and the log marker", tier: "integration",
			setup: func(r *rig) {
				oneTagged(r.repo)
				r.tier.codes = map[string]int{". ./r": 1}
				r.goTestTakes = 75 * time.Second
			},
			wantLine: "integration RED passed=0 failed=1 skipped=0 failed: ./r (logs LOG) in 1m15s",
			wantCode: 1, wantRuns: []string{". ./r"},
		},
		{
			name: "an empty tier is RED with no names (gt-tqxdd)", tier: "integration",
			setup:    func(r *rig) {},
			wantLine: "integration RED passed=0 failed=0 skipped=0 (logs LOG) in 0s",
			wantCode: 1,
		},
		{
			name: "a nested module's package runs inside that module", tier: "integration",
			setup:    func(r *rig) { setTracker(r.repo) },
			wantLine: "integration GREEN passed=1 failed=0 skipped=0",
			wantRuns: []string{"tracker ./store/pgtest"},
		},
		{
			name: "a nested failure names its module and module-relative path", tier: "integration",
			setup: func(r *rig) {
				setTracker(r.repo)
				r.tier.codes = map[string]int{"tracker ./store/pgtest": 1}
			},
			wantLine: "failed: ./tracker/store/pgtest",
			wantCode: 1, wantRuns: []string{"tracker ./store/pgtest"},
		},
		{
			name: "a root failure stays named beside a passing nested module", tier: "integration",
			setup: func(r *rig) {
				setTracker(r.repo)
				r.repo.lists[". *_test.go"] = append(r.repo.lists[". *_test.go"], "r/r_test.go")
				r.repo.files["r/r_test.go"] = taggedTest
				r.tier.codes = map[string]int{". ./r": 1}
			},
			wantLine: "integration RED passed=1 failed=1 skipped=0 failed: ./r", wantCode: 1,
		},
		{
			name: "a nested package takes the root list's skip entry", tier: "integration",
			setup:    func(r *rig) { setTracker(r.repo); r.skip = "integration:./tracker/store/pgtest" },
			wantLine: "integration GREEN passed=0 failed=0 skipped=1",
			wantErr:  []string{"tier-sweep: integration ./tracker/store/pgtest SKIPPED (TIER_SWEEP_SKIP)"},
		},
		{
			name: "a module with no tagged package runs nothing and says nothing", tier: "integration",
			setup: func(r *rig) {
				r.repo.lists[". */go.mod"] = []string{"other/go.mod"}
				r.repo.lists["other *_test.go"] = []string{"plain_test.go"}
				r.repo.files["other/plain_test.go"] = plainTest
				oneTagged(r.repo)
			},
			wantLine: "integration GREEN passed=1", wantRuns: []string{". ./r"},
			noErr: []string{"other"},
		},
		{
			name: "a tagged file at the module root runs in that module", tier: "integration",
			setup: func(r *rig) {
				r.repo.lists[". */go.mod"] = []string{"tracker/go.mod"}
				r.repo.lists[". *_test.go"] = []string{"tracker/root_test.go"}
				r.repo.lists["tracker *_test.go"] = []string{"root_test.go"}
				r.repo.files["tracker/root_test.go"] = taggedTest
			},
			wantLine: "integration GREEN passed=1", wantRuns: []string{"tracker ./"},
			wantErr: []string{"tier-sweep: integration ./tracker ..."},
		},
		{
			name: "a root failure survives a module that reports nothing", tier: "integration",
			setup: func(r *rig) {
				r.repo.lists[". */go.mod"] = []string{"other/go.mod"}
				r.repo.lists["other *_test.go"] = []string{"plain_test.go"}
				r.repo.files["other/plain_test.go"] = plainTest
				oneTagged(r.repo)
				r.tier.codes = map[string]int{". ./r": 1}
			},
			wantLine: "integration RED passed=0 failed=1 skipped=0 failed: ./r", wantCode: 1,
		},
		{
			name: "a nested failure precedes the untagged group", tier: "integration",
			setup: func(r *rig) {
				setTracker(r.repo)
				r.repo.files[dockerList] = "internal/beads\n"
				r.tier.codes = map[string]int{"tracker ./store/pgtest": 1, ". ./internal/beads": 1}
			},
			wantLine: "failed: ./tracker/store/pgtest untagged:./internal/beads", wantCode: 1, noRecord: true,
		},
		{
			name: "a file ending in go.mod is not a module", tier: "integration",
			setup: func(r *rig) {
				r.repo.lists[". */go.mod"] = []string{"extra/notgo.mod"}
				r.repo.lists[". *_test.go"] = []string{"extra/x_test.go"}
				r.repo.files["extra/x_test.go"] = taggedTest
			},
			wantLine: "integration GREEN passed=1", wantRuns: []string{". ./extra"},
		},
		{
			name: "a trigger that reports a package not due skips it", tier: "integration",
			setup: func(r *rig) {
				r.repo.files[dockerList] = "internal/beads\ninternal/testutil\n"
				r.tier.skipsOut = "integration-docker:./internal/beads\n"
			},
			wantLine: "integration GREEN passed=1 failed=0 skipped=1",
			wantRuns: []string{". ./internal/testutil"},
			wantErr:  []string{"tier-sweep: integration-docker ./internal/beads SKIPPED (TIER_SWEEP_SKIP)"},
		},
		{
			name: "a passing container run records", tier: "integration",
			setup:    func(r *rig) { r.repo.files[dockerList] = "internal/beads\n" },
			wantLine: "integration GREEN passed=1",
		},
		{
			name: "a red container run does not record", tier: "integration",
			setup: func(r *rig) {
				r.repo.files[dockerList] = "internal/beads\n"
				r.tier.codes = map[string]int{". ./internal/beads": 1}
			},
			wantLine: "integration RED passed=0 failed=1", wantCode: 1, noRecord: true,
		},
		{
			name: "a trigger that cannot run leaves every package running", tier: "integration",
			setup: func(r *rig) {
				r.repo.files[dockerList] = "internal/beads\n"
				r.tier.skipsOut = "tier-sweep: TIER_SWEEP_TRIGGER: skips integration-docker failed (boom); every container package runs\n"
				r.tier.skipsCode = 1
			},
			wantLine: "integration GREEN passed=1", wantRuns: []string{". ./internal/beads"},
			wantErr: []string{"every container package runs"},
		},
		{
			name: "a record that fails leaves the tier GREEN", tier: "integration",
			setup: func(r *rig) {
				r.repo.files[dockerList] = "internal/beads\n"
				r.tier.recordCode = 1
			},
			wantLine: "integration GREEN passed=1",
			wantErr:  []string{"tiertrigger: record: boom"},
		},
		{
			name: "TIER_SWEEP_TRIGGER=0 reaches the trigger", tier: "integration",
			setup:    func(r *rig) { r.repo.files[dockerList] = "internal/beads\n"; r.trigger = "0" },
			wantLine: "integration GREEN passed=1", wantRuns: []string{". ./internal/beads"},
		},
		{
			name: "the shell tier's line is the runner's, unchanged", tier: "shell",
			setup:    func(r *rig) { r.shellOut = shellLine("GREEN") },
			wantLine: "tier-sweep: shell GREEN passed=2 failed=0 skipped=0 (logs /l) in 3s",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newRig(t)
			tc.setup(r)
			code, stdout, stderr := r.run(tc.tier)
			if code != tc.wantCode {
				t.Errorf("exit code = %d, want %d (stdout %q, stderr %q)", code, tc.wantCode, stdout, stderr)
			}
			want := strings.ReplaceAll(tc.wantLine, "LOG", r.logDir)
			if strings.Count(stdout, "\n") != 1 || !strings.Contains(stdout, want) {
				t.Errorf("stdout = %q, want one line containing %q", stdout, want)
			}
			if tc.wantRuns != nil && !reflect.DeepEqual(r.tier.testRuns(), tc.wantRuns) {
				t.Errorf("go test runs = %v, want %v", r.tier.testRuns(), tc.wantRuns)
			}
			wantRecords := 1
			if tc.noRecord {
				wantRecords = 0
			}
			if got := len(r.tier.triggerCalls("record")); tc.tier == "integration" && got != wantRecords {
				t.Errorf("record calls = %d, want %d", got, wantRecords)
			}
			for _, w := range tc.wantErr {
				if !strings.Contains(stderr, w) {
					t.Errorf("stderr = %q, want it to contain %q", stderr, w)
				}
			}
			for _, n := range tc.noErr {
				if strings.Contains(stderr, n) {
					t.Errorf("stderr = %q, want no %q", stderr, n)
				}
			}
			if strings.Contains(stdout, "SKIPPED") || strings.Contains(stdout, " ...") {
				t.Errorf("progress reached stdout: %q", stdout)
			}
		})
	}
}

// TestSweepRootTestdataPinned: the script's gap, kept by the port: a root-level
// testdata/ package is not excluded from the tagged list, and the line counts it.
func TestSweepRootTestdataPinned(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.repo.lists[". *_test.go"] = []string{"testdata/x_test.go"}
	r.repo.files["testdata/x_test.go"] = taggedTest
	_, stdout, _ := r.run("integration")
	if !strings.Contains(stdout, "integration GREEN passed=1") || !reflect.DeepEqual(r.tier.testRuns(), []string{". ./testdata"}) {
		t.Errorf("stdout %q, runs %v: a root testdata package should run", stdout, r.tier.testRuns())
	}
}

// productionRun runs Run with NO Src injected, as the command does, over a fake
// Exec that records every call and a ReadFile that records every name. git
// ls-files answers from lists (keyed by the directory it ran in); every other
// command succeeds with no output. It returns the code, stdout and stderr.
type productionRun struct {
	dirs      []string // the directory each git ls-files ran in
	reads     []string // each name ReadFile was asked for
	tierDirs  []string // the directory each go test ran in
	gitExit   int
	listings  map[string]string // dir -> git ls-files output
	fileTexts map[string]string // name -> contents
}

func (p *productionRun) run(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	logDir := t.TempDir()
	var out, errw bytes.Buffer
	code = Run(args, &out, &errw, Deps{
		Host: Host{
			Env: func(name string) string {
				if name == EnvLogDir {
					return logDir
				}
				return ""
			},
			MkdirTemp: func(string, string) (string, error) { return logDir, nil },
			MkdirAll:  func(string, fs.FileMode) error { return nil },
		},
		Exec: func(dir string, env []string, argv []string, w io.Writer) int {
			switch {
			case argv[0] == "git" && argv[1] == "ls-files":
				p.dirs = append(p.dirs, dir)
				if p.gitExit != 0 {
					fmt.Fprintln(w, "fatal: not a git repository")
					return p.gitExit
				}
				fmt.Fprint(w, p.listings[dir])
			case argv[0] == "gt":
				p.tierDirs = append(p.tierDirs, dir)
			}
			return 0
		},
		ReadFile: func(name string) ([]byte, error) {
			p.reads = append(p.reads, name)
			text, ok := p.fileTexts[name]
			if !ok {
				return nil, fmt.Errorf("no such file: %s", name)
			}
			return []byte(text), nil
		},
		Environ: []string{"PATH=/fake/bin"},
		Now:     func() time.Time { return time.Date(2026, time.October, 30, 0, 0, 0, 0, time.UTC) },
	})
	return code, out.String(), errw.String()
}

// TestSweepProductionWiringHonorsRoot: with no Src injected, --root reaches the
// git listing (the directory each ls-files ran in, a nested module's included)
// and ReadFile (every name read is under the root), and the tier's go test runs
// from the root.
func TestSweepProductionWiringHonorsRoot(t *testing.T) {
	t.Parallel()
	const root = "/the/repo"
	// The fake git answers by the directory it ran in: the root's one listing is
	// the same for every pattern, so it holds the tagged test file and no go.mod.
	p := &productionRun{
		listings: map[string]string{root: "r/r_test.go\n"},
		fileTexts: map[string]string{
			root + "/r/r_test.go":   taggedTest,
			root + "/" + dockerList: noDocker,
		},
	}
	code, stdout, stderr := p.run(t, "--root", root, "integration")
	if code != 0 || !strings.Contains(stdout, "integration GREEN passed=1") {
		t.Fatalf("code %d, stdout %q, stderr %q, want GREEN passed=1", code, stdout, stderr)
	}
	if len(p.dirs) == 0 {
		t.Fatal("git ls-files never ran")
	}
	for _, d := range p.dirs {
		if d != root {
			t.Errorf("git ls-files ran in %q, want the root %q", d, root)
		}
	}
	if len(p.reads) == 0 {
		t.Fatal("no file was read")
	}
	for _, name := range p.reads {
		if !strings.HasPrefix(name, root+"/") {
			t.Errorf("ReadFile(%q) is outside the root %q", name, root)
		}
	}
	if !reflect.DeepEqual(p.tierDirs, []string{root}) {
		t.Errorf("go test ran in %v, want only the root", p.tierDirs)
	}
}

// TestSweepFailingGitListingIsRed: a root that is not a git repository (git
// ls-files fails) is a RED integration tier with no names and exit 1, never an
// empty list read as a clean tier, and the cause reaches stderr.
func TestSweepFailingGitListingIsRed(t *testing.T) {
	t.Parallel()
	p := &productionRun{gitExit: 128}
	code, stdout, stderr := p.run(t, "--root", "/not/a/repo", "integration")
	if code != 1 || !strings.Contains(stdout, "integration RED passed=0 failed=0 skipped=0") {
		t.Errorf("code %d, stdout %q, want exit 1 and a RED line with no counts", code, stdout)
	}
	if strings.Contains(stdout, "failed:") {
		t.Errorf("stdout %q names failures; the tier ran nothing", stdout)
	}
	if !strings.Contains(stderr, "not a git repository") {
		t.Errorf("stderr = %q, want the git error in it", stderr)
	}
	if len(p.tierDirs) != 0 {
		t.Errorf("go test ran in %v, want nothing run", p.tierDirs)
	}
}

// TestSweepDefaultsReadFileToTheDisk: a Deps with no Src and no ReadFile still
// reads the repository's files, from under the root on disk, and does not panic.
func TestSweepDefaultsReadFileToTheDisk(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	for name, text := range map[string]string{"r/r_test.go": taggedTest, dockerList: noDocker} {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	logDir := t.TempDir()
	var out, errw bytes.Buffer
	code := Run([]string{"--root", root, "integration"}, &out, &errw, Deps{
		Host: Host{
			Env: func(name string) string {
				if name == EnvLogDir {
					return logDir
				}
				return ""
			},
			MkdirTemp: func(string, string) (string, error) { return logDir, nil },
			MkdirAll:  func(string, fs.FileMode) error { return nil },
		},
		Exec: func(dir string, env []string, argv []string, w io.Writer) int {
			if argv[0] == "git" && argv[1] == "ls-files" && argv[len(argv)-1] == "*_test.go" {
				fmt.Fprintln(w, "r/r_test.go")
			}
			return 0
		},
		Now: func() time.Time { return time.Date(2026, time.October, 30, 0, 0, 0, 0, time.UTC) },
	})
	if code != 0 || !strings.Contains(out.String(), "integration GREEN passed=1") {
		t.Errorf("code %d, stdout %q, stderr %q, want GREEN passed=1", code, out.String(), errw.String())
	}
}

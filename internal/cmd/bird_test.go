package cmd

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/steveyegge/gastown/internal/bird"
)

// birdRunnerForTest is a Runner over a fresh state file, with the seams a test
// keeps off the machine: no environment, no process table, no exec.
func birdRunnerForTest(t *testing.T, env ...string) *bird.Runner {
	t.Helper()
	return &bird.Runner{
		Registry: bird.New(t.TempDir()),
		Claude:   filepath.Join(t.TempDir(), "claude"),
		Terminal: bird.Terminal{Stdin: true, Stdout: true},
		Environ:  func() []string { return env },
		PID:      func() int { return 4242 },
		Exec:     func(string, []string, []string) error { return nil },
	}
}

// birdSetupForTest is the setup the commands read, answered from a temporary
// home and the town it is given, so no test reads this machine's town.
func birdSetupForTest(t *testing.T, town string) (birdSetup, string) {
	t.Helper()
	home := filepath.Join(t.TempDir(), "home")
	return birdSetup{
		home:   func() (string, error) { return home, nil },
		find:   func() (string, error) { return town, nil },
		guard:  func(string, string) error { return nil },
		isTTY:  func(*os.File) bool { return false },
		claude: func(h, _ string) (string, error) { return filepath.Join(h, "bin", "claude"), nil },
		label:  func(string, string, string) error { return nil },
	}, home
}

func TestBirdStartExits127WhenClaudeIsNotThere(t *testing.T) {
	t.Parallel()
	r := birdRunnerForTest(t)

	err := runBirdStart(r, nil)
	if code := exitCodeForError(err); code != 127 {
		t.Fatalf("gt bird start with no claude exits %d (%v), want 127", code, err)
	}
	if !strings.Contains(err.Error(), r.Claude) {
		t.Errorf("error %q does not name the binary %q", err, r.Claude)
	}
}

func TestBirdBindExits2ForABeadItCannotRead(t *testing.T) {
	t.Parallel()
	r := birdRunnerForTest(t, "CLAUDE_AGENT_NAME=wren")

	err := runBirdBind(r, "not a bead")
	if code := exitCodeForError(err); code != 2 {
		t.Fatalf("gt bird bind with an unreadable bead exits %d (%v), want 2", code, err)
	}
}

func TestBirdBindSaysWhenTheSessionHasNoName(t *testing.T) {
	t.Parallel()
	err := runBirdBind(birdRunnerForTest(t), "gt-abc")
	if err == nil || !strings.Contains(err.Error(), "CLAUDE_AGENT_NAME") {
		t.Fatalf("gt bird bind outside a bird = %v, want an error naming CLAUDE_AGENT_NAME", err)
	}
	if code := exitCodeForError(err); code != 1 {
		t.Errorf("exit = %d, want 1 for a failure with no status of its own", code)
	}
}

func TestBirdListPrintsTheLiveBirds(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	if err := runBirdList(birdRunnerForTest(t), &out); err != nil {
		t.Fatalf("gt bird list: %v", err)
	}
	if got := strings.TrimSpace(out.String()); got != "no live birds" {
		t.Errorf("gt bird list printed %q, want %q", got, "no live birds")
	}
}

// TestBirdRunnerReadsTheTownTheHomeAndTheClaude pins the wiring the commands
// drive: the town's state file, the operator's home and the real claude.
func TestBirdRunnerReadsTheTownTheHomeAndTheClaude(t *testing.T) {
	t.Parallel()
	town := filepath.Join(t.TempDir(), "town")
	setup, home := birdSetupForTest(t, town)

	r, err := setup.runner(true)
	if err != nil {
		t.Fatalf("runner: %v", err)
	}
	if want := filepath.Join(town, ".runtime", "agents", "state.json"); r.Registry.Path != want {
		t.Errorf("registry path = %q, want %q", r.Registry.Path, want)
	}
	if want := filepath.Join(home, "bin", "claude"); r.Claude != want {
		t.Errorf("claude = %q, want %q", r.Claude, want)
	}
	if r.Home != home {
		t.Errorf("home = %q, want %q", r.Home, home)
	}
	if r.Terminal != (bird.Terminal{}) {
		t.Errorf("terminal = %+v, want the streams the setup says are not terminals", r.Terminal)
	}
}

// TestBirdRunnerFallsBackToTheHomeTown: a start outside any town records into
// ~/gt, which is where the sessions inside one record.
func TestBirdRunnerFallsBackToTheHomeTown(t *testing.T) {
	t.Parallel()
	setup, home := birdSetupForTest(t, "")
	setup.find = func() (string, error) { return "", errors.New("not in a Gas Town workspace") }

	r, err := setup.runner(false)
	if err != nil {
		t.Fatalf("runner: %v", err)
	}
	if want := filepath.Join(home, "gt", ".runtime", "agents", "state.json"); r.Registry.Path != want {
		t.Errorf("registry path = %q, want %q", r.Registry.Path, want)
	}
}

// TestBirdRunnerRefusesWhatItCannotRead: neither a home the process cannot name
// nor a root the harness forbids becomes a registry to write into.
func TestBirdRunnerRefusesWhatItCannotRead(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		set   func(*birdSetup)
		wants string
	}{
		{
			name:  "a home that cannot be read",
			set:   func(s *birdSetup) { s.home = func() (string, error) { return "", errors.New("HOME is unset") } },
			wants: "home directory",
		},
		{
			name: "a live town the harness forbids",
			set: func(s *birdSetup) {
				s.guard = func(what, dir string) error { return errors.New(what + " refused " + dir) }
			},
			wants: "refused",
		},
		{
			name: "a bird_claude setting that cannot be read",
			set: func(s *birdSetup) {
				s.claude = func(string, string) (string, error) { return "", errors.New("bird_claude unreadable") }
			},
			wants: "bird_claude unreadable",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			setup, _ := birdSetupForTest(t, filepath.Join(t.TempDir(), "town"))
			tc.set(&setup)
			r, err := setup.runner(true)
			if err == nil || !strings.Contains(err.Error(), tc.wants) {
				t.Fatalf("runner = %+v, %v; want an error naming %q", r, err, tc.wants)
			}
		})
	}
}

// TestBirdLeafCarriesASetupFailure: a command whose setup cannot build a
// runner never reaches the leaf, and says why.
func TestBirdLeafCarriesASetupFailure(t *testing.T) {
	t.Parallel()
	setup, _ := birdSetupForTest(t, filepath.Join(t.TempDir(), "town"))
	setup.home = func() (string, error) { return "", errors.New("HOME is unset") }
	ran := false

	err := birdLeaf(setup, func(*bird.Runner) error {
		ran = true
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "home directory") {
		t.Fatalf("birdLeaf = %v, want the setup's failure", err)
	}
	if ran {
		t.Error("the leaf ran without a runner")
	}
}

// TestBirdListIgnoresABirdClaudeItCannotRead: a list never execs claude, so a
// bird_claude setting that cannot be read must not stop it. The leaf is the one
// gt bird list runs, with the production claude path behind it.
func TestBirdListIgnoresABirdClaudeItCannotRead(t *testing.T) {
	t.Parallel()
	town := filepath.Join(t.TempDir(), "town")
	writeBirdTownSettings(t, town, `{"type":`)
	setup, _ := birdSetupForTest(t, town)
	setup.claude = birdClaudePath
	var out bytes.Buffer
	ran := false

	err := birdLeaf(setup, func(r *bird.Runner) error {
		ran = true
		return runBirdList(r, &out)
	})
	if err != nil {
		t.Fatalf("gt bird list with an unreadable bird_claude: %v", err)
	}
	if !ran {
		t.Fatal("gt bird list did not run")
	}
	if got := strings.TrimSpace(out.String()); got != "no live birds" {
		t.Errorf("gt bird list printed %q, want %q", got, "no live birds")
	}
}

// TestBirdStartLeafRefusesABirdClaudeItCannotRead: a start execs the claude the
// town names, so a setting it cannot read stops the start before the leaf runs.
func TestBirdStartLeafRefusesABirdClaudeItCannotRead(t *testing.T) {
	t.Parallel()
	town := filepath.Join(t.TempDir(), "town")
	writeBirdTownSettings(t, town, `{"type":`)
	setup, _ := birdSetupForTest(t, town)
	setup.claude = birdClaudePath
	ran := false

	err := birdStartLeaf(setup, func(*bird.Runner) error {
		ran = true
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "bird_claude") {
		t.Fatalf("birdStartLeaf = %v, want an error naming bird_claude", err)
	}
	if ran {
		t.Error("the start ran without a claude it could read")
	}
}

// TestBirdStartPassesClaudesOwnFlagsThrough: a run that is not a session hands
// claude every argument as it was written, --name, --resume and --help too, so
// an alias of claude behaves as claude does.
func TestBirdStartPassesClaudesOwnFlagsThrough(t *testing.T) {
	t.Parallel()
	r := birdRunnerForTest(t)
	r.Terminal = bird.Terminal{}
	r.Stat = runnableClaude
	var argv []string
	r.Exec = func(_ string, a, _ []string) error {
		argv = slices.Clone(a)
		return nil
	}
	args := []string{"-p", "--name", "robin", "--resume", "abc", "--help"}

	if err := runBirdStart(r, args); err != nil {
		t.Fatalf("runBirdStart: %v", err)
	}
	if got := argv[1:]; !slices.Equal(got, args) {
		t.Errorf("claude was given %q, want its own arguments %q", got, args)
	}
}

// writeBirdTownSettings writes the town's settings file, where bird_claude is
// read from.
func writeBirdTownSettings(t *testing.T, town, body string) {
	t.Helper()
	dir := filepath.Join(town, "settings")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestBirdStartExecsTheClaudeTheTownNames drives the production claude path
// into the exec a start makes: a configured bird_claude is the claude that
// runs, and an unset one keeps ~/.local/bin/claude.
func TestBirdStartExecsTheClaudeTheTownNames(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		settings string
		want     func(home string) string
	}{
		{
			name:     "an unset bird_claude execs ~/.local/bin/claude",
			settings: `{"type":"town-settings","version":1}`,
			want:     func(home string) string { return filepath.Join(home, ".local", "bin", "claude") },
		},
		{
			name:     "a configured absolute path is the claude that runs",
			settings: `{"type":"town-settings","version":1,"bird_claude":"/opt/claude/bin/claude"}`,
			want:     func(string) string { return "/opt/claude/bin/claude" },
		},
		{
			name:     "a path under ~/ is read from the operator's home",
			settings: `{"type":"town-settings","version":1,"bird_claude":"~/tools/claude"}`,
			want:     func(home string) string { return filepath.Join(home, "tools", "claude") },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town := filepath.Join(t.TempDir(), "town")
			writeBirdTownSettings(t, town, tc.settings)
			setup, home := birdSetupForTest(t, town)
			setup.claude = birdClaudePath
			r, err := setup.runner(true)
			if err != nil {
				t.Fatalf("runner: %v", err)
			}
			if want := tc.want(home); r.Claude != want {
				t.Errorf("runner claude = %q, want %q", r.Claude, want)
			}
			r.Stat = runnableClaude
			var execed string
			r.Exec = func(path string, _, _ []string) error {
				execed = path
				return nil
			}

			if err := runBirdStart(r, []string{"-p", "hello"}); err != nil {
				t.Fatalf("gt bird start: %v", err)
			}
			if want := tc.want(home); execed != want {
				t.Errorf("gt bird start execed %q, want %q", execed, want)
			}
		})
	}
}

// TestBirdClaudePathRefusesASettingItCannotUse: a relative bird_claude would
// exec a name looked up from the working directory, and a settings file that
// does not parse hides the setting, so neither falls back to the default.
func TestBirdClaudePathRefusesASettingItCannotUse(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		settings string
	}{
		{name: "a relative path", settings: `{"type":"town-settings","version":1,"bird_claude":"bin/claude"}`},
		{name: "a settings file that does not parse", settings: `{"type":`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town := filepath.Join(t.TempDir(), "town")
			writeBirdTownSettings(t, town, tc.settings)
			home := filepath.Join(t.TempDir(), "home")

			got, err := birdClaudePath(home, town)
			if err == nil || !strings.Contains(err.Error(), "bird_claude") {
				t.Fatalf("birdClaudePath = %q, %v; want an error naming bird_claude", got, err)
			}
		})
	}
}

// TestBirdClaudePathReadsAMissingSettingsAsUnset: a town that never wrote its
// settings execs ~/.local/bin/claude, and reading them writes nothing.
func TestBirdClaudePathReadsAMissingSettingsAsUnset(t *testing.T) {
	t.Parallel()
	town := filepath.Join(t.TempDir(), "town")
	home := filepath.Join(t.TempDir(), "home")

	got, err := birdClaudePath(home, town)
	if err != nil {
		t.Fatalf("birdClaudePath: %v", err)
	}
	if want := filepath.Join(home, ".local", "bin", "claude"); got != want {
		t.Errorf("birdClaudePath = %q, want %q", got, want)
	}
	if _, err := os.Stat(filepath.Join(town, "settings")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("reading the settings left a settings directory behind (stat err %v)", err)
	}
}

// runnableClaude is the stat of a claude that is there and can run, read from
// an in-memory file system so no executable is created.
func runnableClaude(string) (fs.FileInfo, error) {
	return fs.Stat(fstest.MapFS{"claude": {Mode: 0o755}}, "claude")
}

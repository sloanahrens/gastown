package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/exectax"
)

// fake is a host with every effect scripted: it records the commands and the
// waits, answers from the test's own directory, and starts no process.
type fake struct {
	goos     string
	env      map[string]string
	tmuxPath string
	lookErr  error
	exe      string
	exeErr   error
	wd       string
	wdErr    error
	pid      int

	work    string
	tempErr error

	// hasSession is the verdict of each tmux has-session call, in order; the
	// last verdict repeats once the script runs out.
	hasSession []error
	// runFunc scripts individual commands; nil makes every command succeed.
	runFunc func(command) error

	commands []command
	sleeps   []time.Duration

	probeRes exectax.Result
	probeErr error

	// clock is what now returns, advanced by clockStep on every call.
	clock     time.Time
	clockStep time.Duration
}

// newFake is a fake ready to run on the darwin pane path.
func newFake(t *testing.T) *fake {
	t.Helper()
	return &fake{
		goos:     "darwin",
		tmuxPath: "/opt/homebrew/bin/tmux",
		exe:      "/var/folders/exe/testtiming",
		wd:       "/repo",
		pid:      4242,
		work:     t.TempDir(),
	}
}

func (f *fake) host() host {
	return host{
		goos:       f.goos,
		getenv:     func(k string) string { return f.env[k] },
		lookPath:   func(string) (string, error) { return f.tmuxPath, f.lookErr },
		executable: func() (string, error) { return f.exe, f.exeErr },
		getwd:      func() (string, error) { return f.wd, f.wdErr },
		pid:        f.pid,
		tempDir:    func(string) (string, error) { return f.work, f.tempErr },
		run:        f.run,
		sleep:      func(d time.Duration) { f.sleeps = append(f.sleeps, d) },
		now:        f.now,
		probe:      func(context.Context) (exectax.Result, error) { return f.probeRes, f.probeErr },
	}
}

func (f *fake) run(_ context.Context, c command) error {
	f.commands = append(f.commands, c)
	if containsArg(c.args, "has-session") {
		err := error(nil)
		if len(f.hasSession) > 0 {
			err = f.hasSession[0]
			if len(f.hasSession) > 1 {
				f.hasSession = f.hasSession[1:]
			}
		}
		return err
	}
	if f.runFunc != nil {
		return f.runFunc(c)
	}
	return nil
}

func (f *fake) now() time.Time {
	t := f.clock
	f.clock = f.clock.Add(f.clockStep)
	return t
}

// names is the commands run, as "name arg..." lines, for a whole-sequence
// comparison.
func (f *fake) names() []string {
	out := make([]string, len(f.commands))
	for i, c := range f.commands {
		out[i] = strings.TrimSpace(c.name + " " + strings.Join(c.args, " "))
	}
	return out
}

// scriptedExit is a run error carrying an exit status, the way a real command
// that exited non-zero reports one, with no process behind it.
type scriptedExit int

func (e scriptedExit) Error() string { return "exit status " + strconv.Itoa(int(e)) }
func (e scriptedExit) ExitCode() int { return int(e) }

func containsArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

// write puts a file in dir, as the pane would have.
func write(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestPkgsOrDefault(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		args []string
		want []string
	}{
		{"no packages runs the module", nil, []string{"./..."}},
		{"the recipe's separator alone runs the module", []string{"--"}, []string{"./..."}},
		{"the separator is dropped", []string{"--", "./internal/style"}, []string{"./internal/style"}},
		{"packages pass through in order", []string{"./internal/a", "./internal/b"}, []string{"./internal/a", "./internal/b"}},
		{"only a leading separator is dropped", []string{"./internal/a", "--"}, []string{"./internal/a", "--"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := pkgsOrDefault(tt.args)
			if strings.Join(got, " ") != strings.Join(tt.want, " ") {
				t.Errorf("pkgsOrDefault(%q) = %q, want %q", tt.args, got, tt.want)
			}
		})
	}
}

func TestShellQuote(t *testing.T) {
	t.Parallel()
	tests := []struct{ in, want string }{
		{"plain", `'plain'`},
		{"a b", `'a b'`},
		{"", `''`},
		{"it's", `'it'\''s'`},
		{`"$HOME"`, `'"$HOME"'`},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			t.Parallel()
			if got := shellQuote(tt.in); got != tt.want {
				t.Errorf("shellQuote(%q) = %s, want %s", tt.in, got, tt.want)
			}
		})
	}
}

func TestTargetSeconds(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		value   string
		want    int
		wantErr bool
	}{
		{"unset takes the default", "", defaultTarget, false},
		{"a number is the target", "5", 5, false},
		{"zero is a target", "0", 0, false},
		{"a word is refused", "abc", 0, true},
		{"a negative is refused", "-1", 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := targetSeconds(func(string) string { return tt.value })
			if (err != nil) != tt.wantErr {
				t.Fatalf("targetSeconds(%q) error = %v, want error %v", tt.value, err, tt.wantErr)
			}
			if !tt.wantErr && got != tt.want {
				t.Errorf("targetSeconds(%q) = %d, want %d", tt.value, got, tt.want)
			}
		})
	}
}

// TestRunTiming_Verdicts pins every verdict the outer process reaches from
// what the pane left: the probe's red line, the tier's exit code, the target
// and the two ways the pane fails to report.
func TestRunTiming_Verdicts(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		env     map[string]string
		probe   string // "" leaves no probe file
		done    string // "" leaves no done file
		want    int
		wantOut []string
		wantErr string
		// wantWaits is how many result polls sleep: one per failed poll.
		wantWaits int
	}{
		{
			name:  "a taxed pane within the target passes",
			probe: "80",
			done:  "0 60",
			want:  0,
			wantOut: []string{
				"probe: 80 ms per new executable (taxed pane expected >= 50)",
				"unit tier: 60s (target 90s), go test exit 0; log: {work}/log",
			},
		},
		{
			name:  "the probe at the red line is taxed, not exempt",
			probe: "50",
			done:  "0 10",
			want:  0,
			wantOut: []string{
				"probe: 50 ms per new executable (taxed pane expected >= 50)",
				"unit tier: 10s (target 90s), go test exit 0; log: {work}/log",
			},
		},
		{
			name:  "an exempt pane proves nothing",
			probe: "49",
			done:  "0 10",
			want:  3,
			wantOut: []string{
				"probe: 49 ms per new executable (taxed pane expected >= 50)",
				"unit tier: 10s (target 90s), go test exit 0; log: {work}/log",
			},
			wantErr: "pane is EXEMPT (probe 49 ms); this measurement does not prove the target",
		},
		{
			name:  "the tier's own failure is the exit code",
			probe: "80",
			done:  "2 12",
			want:  2,
			wantOut: []string{
				"probe: 80 ms per new executable (taxed pane expected >= 50)",
				"unit tier: 12s (target 90s), go test exit 2; log: {work}/log",
			},
		},
		{
			name:  "a tier over the target fails",
			probe: "80",
			done:  "0 120",
			want:  1,
			wantOut: []string{
				"probe: 80 ms per new executable (taxed pane expected >= 50)",
				"unit tier: 120s (target 90s), go test exit 0; log: {work}/log",
			},
			wantErr: "test-timing: over target",
		},
		{
			name:  "TARGET_SECONDS judges the tier",
			env:   map[string]string{"TARGET_SECONDS": "5"},
			probe: "80",
			done:  "0 5",
			want:  0,
			wantOut: []string{
				"probe: 80 ms per new executable (taxed pane expected >= 50)",
				"unit tier: 5s (target 5s), go test exit 0; log: {work}/log",
			},
		},
		{
			name:      "a pane that never reports is abandoned",
			probe:     "80",
			want:      2,
			wantErr:   "no result after 30 min",
			wantWaits: resultChecks - 1,
		},
		{
			name:    "a pane with no probe proves nothing",
			done:    "0 10",
			want:    2,
			wantErr: "the pane recorded no probe",
		},
		{
			name:    "a probe that is not a number is refused",
			probe:   "abc",
			done:    "0 10",
			want:    2,
			wantErr: `the pane's probe "abc" is not a number of milliseconds`,
		},
		{
			name:    "a malformed result is refused",
			probe:   "80",
			done:    "zero",
			want:    2,
			wantErr: `the pane's result "zero" is not "<exit> <seconds>"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFake(t)
			f.env = tt.env
			if tt.probe != "" {
				write(t, f.work, probeFile, tt.probe)
			}
			if tt.done != "" {
				write(t, f.work, doneFile, tt.done)
			}
			var stdout, stderr strings.Builder
			got := runWith(context.Background(), f.host(), nil, &stdout, &stderr)
			if got != tt.want {
				t.Errorf("exit code = %d, want %d (stdout %q, stderr %q)", got, tt.want, stdout.String(), stderr.String())
			}
			wantOut := ""
			for _, line := range tt.wantOut {
				wantOut += strings.ReplaceAll(line, "{work}", f.work) + "\n"
			}
			if stdout.String() != wantOut {
				t.Errorf("stdout = %q, want %q", stdout.String(), wantOut)
			}
			if tt.wantErr != "" && !strings.Contains(stderr.String(), tt.wantErr) {
				t.Errorf("stderr = %q, want it to contain %q", stderr.String(), tt.wantErr)
			}
			if tt.wantErr == "" && stderr.String() != "" {
				t.Errorf("stderr = %q, want nothing", stderr.String())
			}
			var waits int
			for _, d := range f.sleeps {
				if d == resultInterval {
					waits++
				}
			}
			if waits != tt.wantWaits {
				t.Errorf("result polls that slept = %d, want %d", waits, tt.wantWaits)
			}
		})
	}
}

func TestRunTiming_NotDarwin(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	f.goos = "linux"
	var stdout, stderr strings.Builder
	if got := runWith(context.Background(), f.host(), nil, &stdout, &stderr); got != 2 {
		t.Errorf("exit code = %d, want 2", got)
	}
	if !strings.Contains(stderr.String(), "test-timing: macOS only") {
		t.Errorf("stderr = %q, want the macOS-only refusal", stderr.String())
	}
	if len(f.commands) != 0 {
		t.Errorf("ran %q, want nothing on a host without launchd", f.names())
	}
}

// TestRunTiming_ServerStart pins the launchd and tmux calls in order,
// including the quoted line the pane's shell receives.
func TestRunTiming_ServerStart(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	write(t, f.work, probeFile, "80")
	write(t, f.work, doneFile, "0 60")
	var stdout, stderr strings.Builder
	pkgs := []string{"./internal/style", "./internal/a b", "it's"}
	if got := runWith(context.Background(), f.host(), pkgs, &stdout, &stderr); got != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr %q)", got, stderr.String())
	}
	line := "'/var/folders/exe/testtiming' '-pane-run' '-work' '" + f.work + "' '-repo' '/repo' './internal/style' './internal/a b' 'it'\\''s'"
	want := []string{
		"launchctl submit -l com.gastown.test-timing.4242 -- /opt/homebrew/bin/tmux -L gt-test-timing-4242 new-session -d -s timing",
		"/opt/homebrew/bin/tmux -L gt-test-timing-4242 has-session -t timing",
		"launchctl remove com.gastown.test-timing.4242",
		"/opt/homebrew/bin/tmux -L gt-test-timing-4242 has-session -t timing",
		"/opt/homebrew/bin/tmux -L gt-test-timing-4242 send-keys -t timing " + line + " Enter",
		"/opt/homebrew/bin/tmux -L gt-test-timing-4242 kill-server",
		"launchctl remove com.gastown.test-timing.4242",
	}
	got := f.names()
	if len(got) != len(want) {
		t.Fatalf("ran %d commands, want %d:\n%s", len(got), len(want), strings.Join(got, "\n"))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("command %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestRunTiming_WaitsForServer pins the ready poll: a server that answers on
// the third call costs two intervals, and no more checks follow.
func TestRunTiming_WaitsForServer(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	f.hasSession = []error{errors.New("no server yet"), errors.New("still no server"), nil}
	write(t, f.work, probeFile, "80")
	write(t, f.work, doneFile, "0 10")
	var stdout, stderr strings.Builder
	if got := runWith(context.Background(), f.host(), nil, &stdout, &stderr); got != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr %q)", got, stderr.String())
	}
	var ready int
	for _, d := range f.sleeps {
		if d == readyInterval {
			ready++
		}
	}
	if ready != 2 {
		t.Errorf("ready waits = %d, want 2", ready)
	}
	var checks int
	for _, c := range f.commands {
		if containsArg(c.args, "has-session") {
			checks++
		}
	}
	if checks != 4 { // three in the poll, one after the job is removed
		t.Errorf("has-session calls = %d, want 4", checks)
	}
}

// TestRunTiming_ServerNeverStarts checks the poll's give-up and that the
// launchd job is removed and the server killed on the way out.
func TestRunTiming_ServerNeverStarts(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	f.hasSession = []error{errors.New("no server")}
	var stdout, stderr strings.Builder
	if got := runWith(context.Background(), f.host(), nil, &stdout, &stderr); got != 2 {
		t.Errorf("exit code = %d, want 2", got)
	}
	if !strings.Contains(stderr.String(), "never came up") {
		t.Errorf("stderr = %q, want the server refusal", stderr.String())
	}
	var ready int
	for _, d := range f.sleeps {
		if d == readyInterval {
			ready++
		}
	}
	if ready != readyChecks-1 {
		t.Errorf("ready waits = %d, want %d", ready, readyChecks-1)
	}
	if !containsArg(f.commands[len(f.commands)-2].args, "kill-server") {
		t.Errorf("cleanup did not kill the server: %q", f.names())
	}
	if !containsArg(f.commands[len(f.commands)-1].args, "remove") {
		t.Errorf("cleanup did not remove the launchd job: %q", f.names())
	}
}

func TestRunTiming_SetupErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		setup   func(*fake)
		wantErr string
	}{
		{"tmux is not installed", func(f *fake) { f.lookErr = errors.New("not found") }, "test-timing: tmux: not found"},
		{"the binary's path is unknown", func(f *fake) { f.exeErr = errors.New("no path") }, "test-timing: no path"},
		{"the working directory is unknown", func(f *fake) { f.wdErr = errors.New("gone") }, "test-timing: gone"},
		{"no work directory", func(f *fake) { f.tempErr = errors.New("no space") }, "test-timing: no space"},
		{"launchctl submit fails", func(f *fake) {
			f.runFunc = func(c command) error {
				if c.name == launchctlBin {
					return errors.New("operation not permitted")
				}
				return nil
			}
		}, "test-timing: launchctl submit: operation not permitted"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFake(t)
			tt.setup(f)
			var stdout, stderr strings.Builder
			if got := runWith(context.Background(), f.host(), nil, &stdout, &stderr); got != 2 {
				t.Errorf("exit code = %d, want 2 (stderr %q)", got, stderr.String())
			}
			if !strings.Contains(stderr.String(), tt.wantErr) {
				t.Errorf("stderr = %q, want it to contain %q", stderr.String(), tt.wantErr)
			}
		})
	}
}

// TestRunPane_RunsTheTier pins what the pane writes and the command it runs:
// the probe's median, the budget invocation over the given packages in the
// module root, and the result line.
func TestRunPane_RunsTheTier(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	f.probeRes = exectax.Result{Median: 61 * time.Millisecond}
	f.clockStep = 47 * time.Second
	var stdout, stderr strings.Builder
	args := []string{paneFlag, "-work", f.work, "-repo", "/repo", "--", "./internal/style", "./internal/beads/..."}
	if got := runWith(context.Background(), f.host(), args, &stdout, &stderr); got != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr %q)", got, stderr.String())
	}
	if got := readFile(t, filepath.Join(f.work, probeFile)); got != "61" {
		t.Errorf("probe file = %q, want the median \"61\"", got)
	}
	if got := readFile(t, filepath.Join(f.work, doneFile)); got != "0 47\n" {
		t.Errorf("done file = %q, want \"0 47\\n\"", got)
	}
	if len(f.commands) != 1 {
		t.Fatalf("ran %d commands, want the tier's one:\n%s", len(f.commands), strings.Join(f.names(), "\n"))
	}
	c := f.commands[0]
	if c.name != "go" || c.dir != "/repo" {
		t.Errorf("tier command = %q in %q, want go in /repo", c.name, c.dir)
	}
	wantArgs := "run ./internal/testpolicy/cmd/budget -- -count=1 ./internal/style ./internal/beads/..."
	if got := strings.Join(c.args, " "); got != wantArgs {
		t.Errorf("tier args = %q, want %q", got, wantArgs)
	}
	if want := "GT_TEST_DOCKER=0 GOFLAGS=-p=8"; strings.Join(c.env, " ") != want {
		t.Errorf("tier env = %q, want %q", c.env, want)
	}
	if c.stdout == nil || c.stderr == nil {
		t.Errorf("tier output is not sent to the log file")
	}
	if _, err := os.Stat(filepath.Join(f.work, logFile)); err != nil {
		t.Errorf("log file: %v", err)
	}
}

// TestRunPane_ExitCodes pins how a tier that failed reaches the outer
// process: go test's code when the run reported one, 127 when it could not
// start (the shell's code for that).
func TestRunPane_ExitCodes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		runErr   error
		wantDone string
		wantLog  string
	}{
		{"go test's own code", scriptedExit(2), "2 0\n", ""},
		{"a run that never started", errors.New("exec: \"go\": executable file not found"), "127 0\n", "test-timing: exec: \"go\": executable file not found"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFake(t)
			f.probeRes = exectax.Result{Median: 61 * time.Millisecond}
			f.runFunc = func(command) error { return tt.runErr }
			var stdout, stderr strings.Builder
			if got := runWith(context.Background(), f.host(), []string{paneFlag, "-work", f.work, "-repo", "/repo"}, &stdout, &stderr); got != 0 {
				t.Fatalf("pane exit code = %d, want 0: the outer process judges (stderr %q)", got, stderr.String())
			}
			if got := readFile(t, filepath.Join(f.work, doneFile)); got != tt.wantDone {
				t.Errorf("done file = %q, want %q", got, tt.wantDone)
			}
			if tt.wantLog != "" {
				if got := readFile(t, filepath.Join(f.work, logFile)); !strings.Contains(got, tt.wantLog) {
					t.Errorf("log = %q, want it to contain %q", got, tt.wantLog)
				}
			}
		})
	}
}

// TestRunPane_ProbeFails checks that a failed probe leaves no probe file (so
// the outer process refuses to judge the run) and still reports the tier.
func TestRunPane_ProbeFails(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	f.probeErr = errors.New("exec sample-0: permission denied")
	var stdout, stderr strings.Builder
	if got := runWith(context.Background(), f.host(), []string{paneFlag, "-work", f.work, "-repo", "/repo"}, &stdout, &stderr); got != 0 {
		t.Fatalf("pane exit code = %d, want 0 (stderr %q)", got, stderr.String())
	}
	if _, err := os.Stat(filepath.Join(f.work, probeFile)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("probe file exists, want none: %v", err)
	}
	if got := readFile(t, filepath.Join(f.work, doneFile)); got != "0 0\n" {
		t.Errorf("done file = %q, want the tier's result %q", got, "0 0\n")
	}
	if got := readFile(t, filepath.Join(f.work, logFile)); !strings.Contains(got, "test-timing: probe: exec sample-0: permission denied") {
		t.Errorf("log = %q, want the probe error", got)
	}
}

func TestRunPane_NeedsWorkAndRepo(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	var stdout, stderr strings.Builder
	if got := runWith(context.Background(), f.host(), []string{paneFlag}, &stdout, &stderr); got != 2 {
		t.Errorf("exit code = %d, want 2", got)
	}
	if !strings.Contains(stderr.String(), "needs -work and -repo") {
		t.Errorf("stderr = %q, want the usage refusal", stderr.String())
	}
	if len(f.commands) != 0 {
		t.Errorf("ran %q, want nothing", f.names())
	}
}

func TestRunPane_DefaultsPkgs(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	var stdout, stderr strings.Builder
	if got := runWith(context.Background(), f.host(), []string{paneFlag, "-work", f.work, "-repo", "/repo"}, &stdout, &stderr); got != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr %q)", got, stderr.String())
	}
	if len(f.commands) != 1 {
		t.Fatalf("ran %d commands, want one", len(f.commands))
	}
	if got := strings.Join(f.commands[0].args, " "); got != "run ./internal/testpolicy/cmd/budget -- -count=1 ./..." {
		t.Errorf("tier args = %q, want the module-wide default", got)
	}
}

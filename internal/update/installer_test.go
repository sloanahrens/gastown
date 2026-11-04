package update

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/installer"
)

// runCall is one command a fake RunFunc received.
type runCall struct {
	dir  string
	env  []string
	name string
	args []string
}

// fakeRun records every command it is given and answers from answer.
type fakeRun struct {
	calls  []runCall
	answer func(call runCall) (string, int, error)
}

func (f *fakeRun) run(_ context.Context, dir string, env []string, name string, args ...string) (string, int, error) {
	call := runCall{dir: dir, env: env, name: name, args: args}
	f.calls = append(f.calls, call)
	return f.answer(call)
}

// envIndex returns the position of kv in env, or -1.
func envIndex(env []string, kv string) int {
	for i, got := range env {
		if got == kv {
			return i
		}
	}
	return -1
}

func TestParseInstallerResult(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		output string
		want   InstallResult
		ok     bool
	}{
		{
			name:   "installed with no reason",
			output: "install-gt: RESULT installed abc123 def456 -\n",
			want:   InstallResult{Event: "installed", Commit: "abc123", Prev: "def456"},
			ok:     true,
		},
		{
			name:   "noop already installed",
			output: "install-gt: RESULT noop abc123 def456 already-installed\n",
			want:   InstallResult{Event: "noop", Commit: "abc123", Prev: "def456", Reason: "already-installed"},
			ok:     true,
		},
		{
			name:   "failed build",
			output: "install-gt: RESULT failed abc123 def456 build-failed\n",
			want:   InstallResult{Event: "failed", Commit: "abc123", Prev: "def456", Reason: "build-failed"},
			ok:     true,
		},
		{
			name:   "refused with an absent prev",
			output: "install-gt: RESULT refused abc123 - dirty\n",
			want:   InstallResult{Event: "refused", Commit: "abc123", Reason: "dirty"},
			ok:     true,
		},
		{
			name:   "two result lines: the last wins",
			output: "install-gt: RESULT installed aaa bbb -\nmake: nothing to be done\ninstall-gt: RESULT failed ccc ddd build-failed\n",
			want:   InstallResult{Event: "failed", Commit: "ccc", Prev: "ddd", Reason: "build-failed"},
			ok:     true,
		},
		{
			name:   "a result line with too few fields",
			output: "install-gt: RESULT installed abc123\n",
			ok:     false,
		},
		{
			name:   "a later unreadable result line wins",
			output: "install-gt: RESULT installed aaa bbb -\ninstall-gt: RESULT - ccc ddd dirty\n",
			ok:     false,
		},
		{
			name:   "no result line",
			output: "make: nothing to be done for 'gt-installer'\n",
			ok:     false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, ok := ParseInstallerResult(tt.output)
			if ok != tt.ok {
				t.Fatalf("ParseInstallerResult(%q) ok = %v, want %v", tt.output, ok, tt.ok)
			}
			if got != tt.want {
				t.Errorf("ParseInstallerResult(%q) = %+v, want %+v", tt.output, got, tt.want)
			}
		})
	}
}

func TestParseInstallerResultDashEvent(t *testing.T) {
	t.Parallel()
	got, ok := ParseInstallerResult("install-gt: RESULT - abc123 def456 dirty\n")
	if ok {
		t.Errorf("ParseInstallerResult ok = true, want false: %+v", got)
	}
	if got.Event != "" {
		t.Errorf("Event = %q, want empty", got.Event)
	}
}

func TestClassifyInstall(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		r    InstallResult
		want StepResult
	}{
		{
			name: "installed at exit 0",
			r:    InstallResult{Event: "installed", Exit: int(installer.ExitOK)},
			want: ResultOK,
		},
		{
			name: "noop at exit 0",
			r:    InstallResult{Event: "noop", Reason: "already-installed", Exit: int(installer.ExitOK)},
			want: ResultOK,
		},
		{
			name: "busy lock at exit 3 is deferred",
			r:    InstallResult{Event: "refused", Reason: "lock-busy", Exit: int(installer.ExitBusy)},
			want: ResultDeferred,
		},
		{
			name: "failed at exit 1",
			r:    InstallResult{Event: "failed", Reason: "build-failed", Exit: int(installer.ExitFailed)},
			want: ResultFailed,
		},
		{
			name: "refused at exit 2",
			r:    InstallResult{Event: "refused", Reason: "dirty", Exit: int(installer.ExitRefused)},
			want: ResultFailed,
		},
		{
			name: "a failed event at exit 0 is not ok",
			r:    InstallResult{Event: "failed", Reason: "marker-write", Exit: int(installer.ExitOK)},
			want: ResultFailed,
		},
		{
			name: "a refused event at exit 0 is not ok",
			r:    InstallResult{Event: "refused", Reason: "dirty", Exit: int(installer.ExitOK)},
			want: ResultFailed,
		},
		{
			name: "an unknown event at exit 0 is not ok",
			r:    InstallResult{Event: "surprise", Exit: int(installer.ExitOK)},
			want: ResultFailed,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := ClassifyInstall(tt.r); got != tt.want {
				t.Errorf("ClassifyInstall(%+v) = %q, want %q", tt.r, got, tt.want)
			}
		})
	}
}

func TestExecInstallerRunsMakeThenTheInstaller(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	fake := &fakeRun{answer: func(call runCall) (string, int, error) {
		if call.name == "make" {
			return "built gt-installer\n", 0, nil
		}
		return "install-gt: RESULT installed abc123 def456 -\n", 0, nil
	}}
	ex := &ExecInstaller{
		RepoRoot: root,
		Run:      fake.run,
		ExtraEnv: []string{"GT_POLECAT_IMAGE_TRIM=1"},
	}

	res, err := ex.Install(context.Background(), "abc123")
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if len(fake.calls) != 2 {
		t.Fatalf("Run calls = %d, want 2: %+v", len(fake.calls), fake.calls)
	}

	build := fake.calls[0]
	if want := (runCall{dir: root, name: "make", args: []string{"gt-installer"}}); !reflect.DeepEqual(build, want) {
		t.Errorf("first call = %+v, want %+v", build, want)
	}

	install := fake.calls[1]
	if want := filepath.Join(root, "gt-installer"); install.name != want {
		t.Errorf("installer name = %q, want %q", install.name, want)
	}
	if install.dir != root {
		t.Errorf("installer dir = %q, want %q", install.dir, root)
	}
	if want := []string{"--sha", "abc123", "--source", "gt-update"}; !reflect.DeepEqual(install.args, want) {
		t.Errorf("installer args = %v, want %v", install.args, want)
	}

	lockAt := envIndex(install.env, "INSTALL_GT_LOCK_WAIT=60")
	if lockAt < 0 {
		t.Errorf("environment has no INSTALL_GT_LOCK_WAIT=60: %v", install.env)
	}
	rigAt := envIndex(install.env, "INSTALL_GT_RIG_DIR="+root)
	if rigAt < 0 {
		t.Errorf("environment has no INSTALL_GT_RIG_DIR=%s", root)
	}
	extraAt := envIndex(install.env, "GT_POLECAT_IMAGE_TRIM=1")
	if extraAt < 0 {
		t.Errorf("environment has no ExtraEnv entry: %v", install.env)
	}
	if lockAt >= 0 && rigAt >= 0 && extraAt >= 0 {
		if !(lockAt < rigAt && rigAt < extraAt) {
			t.Errorf("environment order = lock %d, rig %d, extra %d; want lock before rig before extra",
				lockAt, rigAt, extraAt)
		}
	}

	if res.Event != "installed" || res.Commit != "abc123" || res.Prev != "def456" || res.Exit != 0 {
		t.Errorf("Install = %+v, want installed abc123 def456 exit 0", res)
	}
}

func TestExecInstallerErrors(t *testing.T) {
	t.Parallel()
	t.Run("a failed make is an error with the output tail and the installer is not run", func(t *testing.T) {
		t.Parallel()
		fake := &fakeRun{answer: func(runCall) (string, int, error) {
			return "compile: undefined: boom\n", 2, nil
		}}
		ex := &ExecInstaller{RepoRoot: t.TempDir(), Run: fake.run}

		_, err := ex.Install(context.Background(), "abc123")
		if err == nil {
			t.Fatal("Install: no error, want the make failure")
		}
		if !strings.Contains(err.Error(), "compile: undefined: boom") {
			t.Errorf("error %q does not carry the output tail", err)
		}
		if len(fake.calls) != 1 {
			t.Errorf("Run calls = %d, want 1: the installer must not run after a failed build", len(fake.calls))
		}
	})

	t.Run("a make that cannot start names the run error", func(t *testing.T) {
		t.Parallel()
		fake := &fakeRun{answer: func(runCall) (string, int, error) {
			return "", -1, errors.New("exec: make: executable file not found in $PATH")
		}}
		ex := &ExecInstaller{RepoRoot: t.TempDir(), Run: fake.run}

		_, err := ex.Install(context.Background(), "abc123")
		if err == nil {
			t.Fatal("Install: no error, want the make start failure")
		}
		if !strings.Contains(err.Error(), "executable file not found") {
			t.Errorf("error %q does not name the run error", err)
		}
		if len(fake.calls) != 1 {
			t.Errorf("Run calls = %d, want 1: the installer must not run after a build that did not start", len(fake.calls))
		}
	})

	t.Run("a run with no RESULT line names its run error", func(t *testing.T) {
		t.Parallel()
		fake := &fakeRun{answer: func(call runCall) (string, int, error) {
			if call.name == "make" {
				return "", 0, nil
			}
			return "", -1, errors.New("signal: killed")
		}}
		ex := &ExecInstaller{RepoRoot: t.TempDir(), Run: fake.run}

		_, err := ex.Install(context.Background(), "abc123")
		if err == nil {
			t.Fatal("Install: no error, want the missing result line")
		}
		if !strings.Contains(err.Error(), "signal: killed") {
			t.Errorf("error %q does not name the run error", err)
		}
	})

	t.Run("a run with no RESULT line is an error with the tail", func(t *testing.T) {
		t.Parallel()
		fake := &fakeRun{answer: func(call runCall) (string, int, error) {
			if call.name == "make" {
				return "", 0, nil
			}
			return "installing\nswap: permission denied\n", 1, nil
		}}
		ex := &ExecInstaller{RepoRoot: t.TempDir(), Run: fake.run}

		_, err := ex.Install(context.Background(), "abc123")
		if err == nil {
			t.Fatal("Install: no error, want the missing result line")
		}
		if !strings.Contains(err.Error(), "swap: permission denied") {
			t.Errorf("error %q does not carry the output tail", err)
		}
	})

	t.Run("the tail is the last 20 lines", func(t *testing.T) {
		t.Parallel()
		lines := make([]string, 25)
		for i := range lines {
			lines[i] = fmt.Sprintf("noise-%02d", i)
		}
		output := strings.Join(lines, "\n") + "\n"
		fake := &fakeRun{answer: func(runCall) (string, int, error) {
			return output, 3, nil
		}}
		ex := &ExecInstaller{RepoRoot: t.TempDir(), Run: fake.run}

		_, err := ex.Install(context.Background(), "abc123")
		if err == nil {
			t.Fatal("Install: no error, want the make failure")
		}
		if !strings.Contains(err.Error(), "noise-24") {
			t.Errorf("error %q does not carry the last line", err)
		}
		if strings.Contains(err.Error(), "noise-04") {
			t.Errorf("error %q carries output older than the last 20 lines", err)
		}
	})
}

func TestExecInstallerKeepsTheVerdictWhenTheRunErrors(t *testing.T) {
	t.Parallel()
	fake := &fakeRun{answer: func(call runCall) (string, int, error) {
		if call.name == "make" {
			return "", 0, nil
		}
		// A RunFunc may report an error for a nonzero status; a printed RESULT
		// line still decides the verdict.
		return "install-gt: RESULT installed abc123 def456 -\n", 0, errors.New("wait: reported an error")
	}}
	ex := &ExecInstaller{RepoRoot: t.TempDir(), Run: fake.run}

	res, err := ex.Install(context.Background(), "abc123")
	if err != nil {
		t.Fatalf("Install: %v, want nil: the installer named a result", err)
	}
	if res.Event != "installed" || res.Commit != "abc123" || res.Exit != 0 {
		t.Errorf("Install = %+v, want installed abc123 exit 0", res)
	}
}

// TestExecRunNamesACommandThatCannotStart drives the production RunFunc's
// error return. A bare name goes through exec's PATH lookup, which fails
// before any process starts, so the unit tier stays hermetic.
func TestExecRunNamesACommandThatCannotStart(t *testing.T) {
	t.Parallel()
	_, exit, err := ExecRun(context.Background(), t.TempDir(), nil, "gt-update-no-such-command")
	if !errors.Is(err, exec.ErrNotFound) {
		t.Fatalf("ExecRun error = %v, want exec.ErrNotFound in its chain", err)
	}
	if exit != -1 {
		t.Errorf("ExecRun exit = %d, want -1", exit)
	}
}

func TestExecInstallerEnvironmentKeepsPathAndHome(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	var installerEnv []string
	fake := &fakeRun{answer: func(call runCall) (string, int, error) {
		if call.name == "make" {
			return "", 0, nil
		}
		installerEnv = call.env
		return "install-gt: RESULT noop abc123 def456 already-installed\n", 0, nil
	}}
	ex := &ExecInstaller{RepoRoot: root, Run: fake.run, ExtraEnv: []string{"EXTRA=1"}}

	if _, err := ex.Install(context.Background(), "abc123"); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if installerEnv == nil {
		t.Fatal("the installer ran with a nil environment, which inherits instead of carrying the process environment")
	}

	present := make(map[string]bool, len(installerEnv))
	for _, kv := range installerEnv {
		present[kv] = true
	}
	// The process environment is what the installer would have inherited; the
	// test asserts it is copied into the run rather than replaced, which is
	// exactly the behaviour that dropped PATH and HOME before.
	for _, kv := range os.Environ() {
		key, _, _ := strings.Cut(kv, "=")
		if key == "INSTALL_GT_LOCK_WAIT" || key == "INSTALL_GT_RIG_DIR" {
			continue // the wrapper sets its own value
		}
		if !present[kv] {
			t.Errorf("environment dropped %s from the process environment", key)
		}
	}
	//testpolicy:allow no-host-env — the process environment is the value under test
	path, ok := os.LookupEnv("PATH")
	if !ok {
		t.Fatal("PATH is unset in this test process, so the test cannot show it is kept")
	}
	if !present["PATH="+path] {
		t.Errorf("environment has no PATH=%s", path)
	}
	//testpolicy:allow no-host-env — the process environment is the value under test
	home, ok := os.LookupEnv("HOME")
	if !ok {
		t.Fatal("HOME is unset in this test process, so the test cannot show it is kept")
	}
	if !present["HOME="+home] {
		t.Errorf("environment has no HOME=%s", home)
	}
	for _, kv := range []string{"INSTALL_GT_LOCK_WAIT=60", "INSTALL_GT_RIG_DIR=" + root, "EXTRA=1"} {
		if !present[kv] {
			t.Errorf("environment has no %s", kv)
		}
	}
}

// TestInstallDetailTailKeepsTheInstallersLastLines drives a failed run through
// the verdict and the detail installDetail writes: the installer's own error
// line must survive into the step's detail, or the operator has to re-run the
// installer by hand to see it.
func TestInstallDetailTailKeepsTheInstallersLastLines(t *testing.T) {
	t.Parallel()
	fake := &fakeRun{answer: func(call runCall) (string, int, error) {
		if call.name == "make" {
			return "", 0, nil
		}
		return "installing abc123\n" +
			"swap: permission denied\n" +
			"install-gt: RESULT refused abc123 def456 dirty\n", 2, nil
	}}
	ex := &ExecInstaller{RepoRoot: t.TempDir(), Run: fake.run}

	res, err := ex.Install(context.Background(), "abc123")
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	result := ClassifyInstall(res)
	if result != ResultFailed {
		t.Fatalf("ClassifyInstall(%+v) = %q, want %q", res, result, ResultFailed)
	}
	detail := installDetail(res, result, nil)
	if !strings.Contains(detail, "swap: permission denied") {
		t.Errorf("install Detail = %q, want it to carry the installer's error line", detail)
	}
	if !strings.Contains(detail, "dirty") {
		t.Errorf("install Detail = %q, want it to keep the installer's reason", detail)
	}
}

// TestInstallDetailTailBoundsLinesAndTotal keeps the tail readable: five lines
// at most, each cut at 200 characters, and the whole tail at 1000.
func TestInstallDetailTailBoundsLinesAndTotal(t *testing.T) {
	t.Parallel()
	output := make([]string, 0, 51)
	for i := 0; i < 50; i++ {
		output = append(output, fmt.Sprintf("line-%02d %s", i, strings.Repeat("x", 300)))
	}
	output = append(output, "install-gt: RESULT failed abc123 def456 build-failed")
	fake := &fakeRun{answer: func(call runCall) (string, int, error) {
		if call.name == "make" {
			return "", 0, nil
		}
		return strings.Join(output, "\n") + "\n", 1, nil
	}}
	ex := &ExecInstaller{RepoRoot: t.TempDir(), Run: fake.run}

	res, err := ex.Install(context.Background(), "abc123")
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	result := ClassifyInstall(res)
	if result != ResultFailed {
		t.Fatalf("ClassifyInstall(%+v) = %q, want %q", res, result, ResultFailed)
	}
	detail := installDetail(res, result, nil)
	_, tail, found := strings.Cut(detail, "lines of output:\n")
	if !found {
		t.Fatalf("install Detail = %q, want an output tail", detail)
	}

	lines := strings.Split(tail, "\n")
	if len(lines) > 5 {
		t.Errorf("tail has %d lines, want at most 5: %q", len(lines), tail)
	}
	for _, line := range lines {
		if n := len([]rune(line)); n > 200 {
			t.Errorf("tail line is %d characters, want at most 200: %q", n, line)
		}
	}
	if n := len([]rune(tail)); n > 1000 {
		t.Errorf("tail is %d characters, want at most 1000", n)
	}
	if !strings.Contains(tail, "line-49 ") {
		t.Errorf("tail = %q, want the installer's last line", tail)
	}
	if strings.Contains(tail, "line-45 ") {
		t.Errorf("tail = %q, want no line older than the last 5", tail)
	}
}

// TestInstallDetailTailSuccessUnchanged leaves a successful install's detail
// as it was: no tail, so the state file does not grow on every good run.
func TestInstallDetailTailSuccessUnchanged(t *testing.T) {
	t.Parallel()
	fake := &fakeRun{answer: func(call runCall) (string, int, error) {
		if call.name == "make" {
			return "", 0, nil
		}
		return "building abc123\n" +
			"install-gt: RESULT installed abc123 def456 -\n", 0, nil
	}}
	ex := &ExecInstaller{RepoRoot: t.TempDir(), Run: fake.run}

	res, err := ex.Install(context.Background(), "abc123")
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	result := ClassifyInstall(res)
	if result != ResultOK {
		t.Fatalf("ClassifyInstall(%+v) = %q, want %q", res, result, ResultOK)
	}
	if detail := installDetail(res, result, nil); detail != "" {
		t.Errorf("install Detail = %q, want empty: a successful install's detail is unchanged", detail)
	}
}

// TestInstallDetailTailBusyUnchanged leaves a busy result's detail as the
// installer's reason: a deferred run is retried, so its detail names the wait.
func TestInstallDetailTailBusyUnchanged(t *testing.T) {
	t.Parallel()
	fake := &fakeRun{answer: func(call runCall) (string, int, error) {
		if call.name == "make" {
			return "", 0, nil
		}
		return "waiting for the install lock\n" +
			"install-gt: RESULT refused abc123 - lock-busy\n", 3, nil
	}}
	ex := &ExecInstaller{RepoRoot: t.TempDir(), Run: fake.run}

	res, err := ex.Install(context.Background(), "abc123")
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	result := ClassifyInstall(res)
	if result != ResultDeferred {
		t.Fatalf("ClassifyInstall(%+v) = %q, want %q", res, result, ResultDeferred)
	}
	if detail := installDetail(res, result, nil); detail != "lock-busy" {
		t.Errorf("install Detail = %q, want %q: a busy result gets no tail", detail, "lock-busy")
	}
}

// TestInstallDetailTailBoundsErrorPathLines keeps an error path's output tail
// to the last five lines, the bound a failed install's detail has. The make
// path keeps its 20 lines; this covers the run's own error paths.
func TestInstallDetailTailBoundsErrorPathLines(t *testing.T) {
	t.Parallel()
	lines := make([]string, 25)
	for i := range lines {
		lines[i] = fmt.Sprintf("noise-%02d", i)
	}
	output := strings.Join(lines, "\n") + "\n"
	tests := []struct {
		name   string
		answer func(call runCall) (string, int, error)
	}{
		{
			name: "a run that errors",
			answer: func(call runCall) (string, int, error) {
				if call.name == "make" {
					return "", 0, nil
				}
				return output, -1, errors.New("signal: killed")
			},
		},
		{
			name: "a run that exits with no RESULT line",
			answer: func(call runCall) (string, int, error) {
				if call.name == "make" {
					return "", 0, nil
				}
				return output, 1, nil
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fake := &fakeRun{answer: tt.answer}
			ex := &ExecInstaller{RepoRoot: t.TempDir(), Run: fake.run}

			_, err := ex.Install(context.Background(), "abc123")
			if err == nil {
				t.Fatal("Install: no error, want the missing result line")
			}
			if !strings.Contains(err.Error(), "noise-24") {
				t.Errorf("error %q does not carry the last line", err)
			}
			if strings.Contains(err.Error(), "noise-19") {
				t.Errorf("error %q carries output older than the last 5 lines", err)
			}
		})
	}
}

// TestInstallDetailTailTotalCapBindsAtOneThousand puts the RESULT line first,
// so a failed run's tail is five full-length lines and the whole-tail cap is
// what limits it. Without that cap the tail is 1004 characters.
func TestInstallDetailTailTotalCapBindsAtOneThousand(t *testing.T) {
	t.Parallel()
	output := []string{"install-gt: RESULT failed abc123 def456 build-failed"}
	for i := 0; i < 50; i++ {
		output = append(output, fmt.Sprintf("line-%02d %s", i, strings.Repeat("x", 300)))
	}
	fake := &fakeRun{answer: func(call runCall) (string, int, error) {
		if call.name == "make" {
			return "", 0, nil
		}
		return strings.Join(output, "\n") + "\n", 1, nil
	}}
	ex := &ExecInstaller{RepoRoot: t.TempDir(), Run: fake.run}

	res, err := ex.Install(context.Background(), "abc123")
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	result := ClassifyInstall(res)
	if result != ResultFailed {
		t.Fatalf("ClassifyInstall(%+v) = %q, want %q", res, result, ResultFailed)
	}
	detail := installDetail(res, result, nil)
	_, tail, found := strings.Cut(detail, "lines of output:\n")
	if !found {
		t.Fatalf("install Detail = %q, want an output tail", detail)
	}
	if n := len([]rune(tail)); n != 1000 {
		t.Errorf("tail is %d characters, want exactly 1000: the whole-tail cap binds", n)
	}
}

// TestInstallDetailTailStripsControlCharacters keeps escape sequences and bells
// the installer prints out of a failed detail, so the state file, `gt update
// status` and the escalation show the text and not live terminal control.
func TestInstallDetailTailStripsControlCharacters(t *testing.T) {
	t.Parallel()
	fake := &fakeRun{answer: func(call runCall) (string, int, error) {
		if call.name == "make" {
			return "", 0, nil
		}
		return "\x1b[31mswap: permission denied\x1b[0m\a\n" +
			"install-gt: RESULT failed abc123 def456 build\x1b[0m-failed\n", 1, nil
	}}
	ex := &ExecInstaller{RepoRoot: t.TempDir(), Run: fake.run}

	res, err := ex.Install(context.Background(), "abc123")
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	result := ClassifyInstall(res)
	if result != ResultFailed {
		t.Fatalf("ClassifyInstall(%+v) = %q, want %q", res, result, ResultFailed)
	}
	// Each control character is dropped; the printable text around it stays.
	detail := installDetail(res, result, nil)
	want := "build[0m-failed\n" +
		"last 2 lines of output:\n" +
		"[31mswap: permission denied[0m\n" +
		"install-gt: RESULT failed abc123 def456 build[0m-failed"
	if detail != want {
		t.Errorf("install Detail = %q, want %q", detail, want)
	}
}

// TestInstallDetailStripsControlCharactersOnEveryVerdict keeps a control
// character out of the step's detail whatever the verdict: the reason is
// installer output on the OK and busy paths as well as the failed one.
func TestInstallDetailStripsControlCharactersOnEveryVerdict(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		exit   int
		output string
		want   string
	}{
		{
			name:   "a failed verdict keeps its tail",
			exit:   1,
			output: "install-gt: RESULT failed abc123 def456 build\x1b[0m-failed\n",
			want:   "build[0m-failed\nlast 1 lines of output:\ninstall-gt: RESULT failed abc123 def456 build[0m-failed",
		},
		{
			name:   "a busy verdict keeps its reason alone",
			exit:   3,
			output: "install-gt: RESULT refused abc123 - lock-busy\x1b\n",
			want:   "lock-busy",
		},
		{
			name:   "an OK verdict keeps its reason alone",
			exit:   0,
			output: "install-gt: RESULT installed abc123 def456 done\a\n",
			want:   "done",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fake := &fakeRun{answer: func(call runCall) (string, int, error) {
				if call.name == "make" {
					return "", 0, nil
				}
				return tt.output, tt.exit, nil
			}}
			ex := &ExecInstaller{RepoRoot: t.TempDir(), Run: fake.run}

			res, err := ex.Install(context.Background(), "abc123")
			if err != nil {
				t.Fatalf("Install: %v", err)
			}
			result := ClassifyInstall(res)
			if detail := installDetail(res, result, nil); detail != tt.want {
				t.Errorf("install Detail = %q, want %q", detail, tt.want)
			}
		})
	}
}

// TestInstallDetailTailSkipsControlOnlyLines keeps a line that is only control
// characters out of a failed tail, so it is not counted as a line of output.
func TestInstallDetailTailSkipsControlOnlyLines(t *testing.T) {
	t.Parallel()
	fake := &fakeRun{answer: func(call runCall) (string, int, error) {
		if call.name == "make" {
			return "", 0, nil
		}
		return "first line\n\x1b\a\x01\n" +
			"install-gt: RESULT failed abc123 def456 build-failed\n", 1, nil
	}}
	ex := &ExecInstaller{RepoRoot: t.TempDir(), Run: fake.run}

	res, err := ex.Install(context.Background(), "abc123")
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	result := ClassifyInstall(res)
	want := "build-failed\n" +
		"last 2 lines of output:\n" +
		"first line\n" +
		"install-gt: RESULT failed abc123 def456 build-failed"
	if detail := installDetail(res, result, nil); detail != want {
		t.Errorf("install Detail = %q, want %q", detail, want)
	}
}

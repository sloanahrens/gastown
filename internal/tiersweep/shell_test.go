package tiersweep

import (
	"fmt"
	"io"
	"reflect"
	"testing"
	"time"
)

// fakeShell is the Exec a shell-tier test injects. It records each command,
// writes out as the runner's output and returns code. Nothing runs.
type fakeShell struct {
	out   string
	code  int
	calls []tierCall
}

func (f *fakeShell) exec(dir string, env []string, argv []string, out io.Writer) int {
	f.calls = append(f.calls, tierCall{dir: dir, env: env, argv: argv})
	fmt.Fprint(out, f.out)
	return f.code
}

// shellSweep is a Sweep over fake for one shell-tier run, logging into a fresh
// temporary directory. The clock stands still, so every elapsed time reads 0s.
func shellSweep(t *testing.T, fake *fakeShell) *Sweep {
	t.Helper()
	at := time.Date(2026, time.October, 30, 0, 0, 0, 0, time.UTC)
	return &Sweep{
		Opts:   Options{LogDir: t.TempDir()},
		Exec:   fake.exec,
		Stderr: io.Discard,
		Now:    func() time.Time { return at },
	}
}

func TestShellCommandLine(t *testing.T) {
	fake := &fakeShell{}
	s := shellSweep(t, fake)
	s.Shell()
	want := []string{"go", "run", "./internal/shelltier/cmd/shelltier", "--all", "--logdir", s.Opts.LogDir}
	if len(fake.calls) != 1 {
		t.Fatalf("shell ran %d commands, want 1: %v", len(fake.calls), fake.calls)
	}
	if got := fake.calls[0]; got.dir != "." || !reflect.DeepEqual(got.argv, want) {
		t.Errorf("shell ran in %q as %q, want in \".\" as %q", got.dir, got.argv, want)
	}
}

func TestShellReturnsTheRunnersLine(t *testing.T) {
	cases := []struct {
		name string
		// out is the runner's output; %[1]s is the log directory it prints.
		out string
		// want is the line Shell returns; %[1]s is the log directory.
		want string
		code int
		red  bool
	}{
		{
			name: "green",
			out:  "tier-sweep: shell scripts/a_test.sh ...\ntier-sweep: shell GREEN passed=3 failed=0 skipped=0 (logs %[1]s) in 2m14s\n",
			want: "tier-sweep: shell GREEN passed=3 failed=0 skipped=0 (logs %[1]s) in 2m14s",
			code: 0,
		},
		{
			name: "red",
			out:  "tier-sweep: shell scripts/b_test.sh ...\ntier-sweep: shell RED passed=2 failed=1 skipped=0 failed: scripts/b_test.sh (logs %[1]s) in 1m3s\n",
			want: "tier-sweep: shell RED passed=2 failed=1 skipped=0 failed: scripts/b_test.sh (logs %[1]s) in 1m3s",
			code: 1,
			red:  true,
		},
		{
			name: "output after the summary",
			out:  "tier-sweep: shell GREEN passed=1 failed=0 skipped=0 (logs %[1]s) in 9s\nshelltier: trailing note\n",
			want: "tier-sweep: shell GREEN passed=1 failed=0 skipped=0 (logs %[1]s) in 9s",
			code: 0,
		},
		{
			name: "last summary line wins",
			out:  "tier-sweep: shell GREEN passed=1 failed=0 skipped=0 (logs %[1]s) in 9s\ntier-sweep: shell RED passed=0 failed=1 skipped=0 failed: scripts/c_test.sh (logs %[1]s) in 9s\n",
			want: "tier-sweep: shell RED passed=0 failed=1 skipped=0 failed: scripts/c_test.sh (logs %[1]s) in 9s",
			code: 1,
			red:  true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fake := &fakeShell{code: c.code}
			s := shellSweep(t, fake)
			fake.out = fmt.Sprintf(c.out, s.Opts.LogDir)
			line, red := s.Shell()
			want := fmt.Sprintf(c.want, s.Opts.LogDir)
			if line != want || red != c.red {
				t.Errorf("Shell() = %q, %v; want %q, %v", line, red, want, c.red)
			}
		})
	}
}

func TestShellWithoutSummaryLineIsRed(t *testing.T) {
	cases := []struct {
		name string
		out  string
		code int
		// want is the line Shell returns; %s is the log directory.
		want string
	}{
		{
			name: "printed nothing",
			out:  "",
			code: 0,
			want: "tier-sweep: shell RED passed=0 failed=0 skipped=0 (logs %s) in 0s",
		},
		{
			name: "exit 0 with only announce lines",
			out:  "tier-sweep: shell scripts/a_test.sh ...\n",
			code: 0,
			want: "tier-sweep: shell RED passed=0 failed=1 skipped=0 failed: shelltier-command (logs %s) in 0s",
		},
		{
			name: "usage error, exit 2",
			out:  "usage: shelltier [--all] [--root dir] [--logdir dir] [--budget duration]\n",
			code: 2,
			want: "tier-sweep: shell RED passed=0 failed=1 skipped=0 failed: shelltier-command (logs %s) in 0s",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fake := &fakeShell{out: c.out, code: c.code}
			s := shellSweep(t, fake)
			line, red := s.Shell()
			want := fmt.Sprintf(c.want, s.Opts.LogDir)
			if line != want || !red {
				t.Errorf("Shell() = %q, %v; want %q, true", line, red, want)
			}
		})
	}
}

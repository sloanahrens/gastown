package tiersweep

import (
	"fmt"
	"io"
	"reflect"
	"testing"
	"time"
)

// raceSweep is a Sweep over fake for one race run, logging into a fresh
// temporary directory. The clock stands still, so every elapsed time reads 0s.
func raceSweep(t *testing.T, fake *fakeTier) *Sweep {
	t.Helper()
	at := time.Date(2026, time.October, 30, 0, 0, 0, 0, time.UTC)
	return &Sweep{
		Opts:   Options{Timeout: "7m", LogDir: t.TempDir()},
		Exec:   fake.exec,
		Stderr: io.Discard,
		Now:    func() time.Time { return at },
	}
}

func TestRaceCommandLine(t *testing.T) {
	fake := &fakeTier{}
	s := raceSweep(t, fake)
	s.Race()
	want := []string{
		"gt", "slot", "run", "--", "env", "GT_TEST_DOCKER=1",
		"go", "test", "-count=1", "-timeout", "7m",
		"-race", "-tags", "integration",
		"-run", "^TestCommandTreeWalkIsReadOnlyUnderParallelTests$",
		"./internal/cmd",
	}
	if len(fake.calls) != 1 {
		t.Fatalf("race ran %d commands, want 1: %v", len(fake.calls), fake.calls)
	}
	if got := fake.calls[0]; got.dir != "." || !reflect.DeepEqual(got.argv, want) {
		t.Errorf("race ran in %q as %q, want in \".\" as %q", got.dir, got.argv, want)
	}
}

func TestRaceSummary(t *testing.T) {
	cases := []struct {
		name string
		code int
		// want is the summary line; %s is the log directory the sweep writes.
		want string
		red  bool
	}{
		{
			name: "pass",
			code: 0,
			want: "tier-sweep: race GREEN passed=1 failed=0 skipped=0 (logs %s) in 0s",
		},
		{
			name: "fail",
			code: 1,
			want: "tier-sweep: race RED passed=0 failed=1 skipped=0 failed: ./internal/cmd (logs %s) in 0s",
			red:  true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fake := &fakeTier{codes: map[string]int{". ./internal/cmd": c.code}}
			s := raceSweep(t, fake)
			line, red := s.Race()
			want := fmt.Sprintf(c.want, s.Opts.LogDir)
			if line != want || red != c.red {
				t.Errorf("Race() = %q, %v; want %q, %v", line, red, want, c.red)
			}
		})
	}
}

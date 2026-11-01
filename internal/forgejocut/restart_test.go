package forgejocut

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/forgejoprov"
)

// restartRunner is step 7's Runner seam: the bd landing count answers Output, the
// gt daemon restart answers Run, and the two are recorded apart so a test says
// which child ran.
type restartRunner struct {
	count    string
	countErr error
	runErr   error
	counts   [][]string
	runs     [][]string
}

func (f *restartRunner) Output(argv []string) ([]byte, error) {
	f.counts = append(f.counts, argv)
	return []byte(f.count), f.countErr
}

func (f *restartRunner) Run(argv []string) error {
	f.runs = append(f.runs, argv)
	return f.runErr
}

// RunWithToken is not this step's child: no step 7 child carries the admin token,
// so a call here is a bug and is recorded as one.
func (f *restartRunner) RunWithToken(argv []string, _ forgejoprov.Secret) error {
	f.runs = append(f.runs, argv)
	return errors.New("step 7 handed a child the admin token")
}

// restartCutover is a cutover of the rig at queueRoot, dry or not, logging into
// the slice the test reads.
func restartCutover(dry bool) (*Cutover, *[]string) {
	var log []string
	opts := &Options{
		Rig: "gastown", TownRoot: townRoot, RigRoot: queueRoot,
		GitHubSlug: "acme/rig", GT: "gt", DryRun: dry,
	}
	return &Cutover{Opts: opts, Log: func(line string) { log = append(log, line) }}, &log
}

// restartWanted is step 7's restart argv: the script's env words, the gt binary
// and the subcommand.
var restartWanted = []string{"env", "GT_TOWN_ROOT=" + townRoot, "GT_ROOT=" + townRoot, "gt", "daemon", "restart"}

// The restart runs only on an empty landing queue: a queued landing and an
// unreadable queue both skip it and say why. Restarting regardless turns the two
// skip rows red.
func TestRestartOnlyWhenNoLanding(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		count       string
		countErr    error
		wantRestart bool
		wantInLog   []string
	}{
		{
			name: "empty queue restarts", count: "0\n", wantRestart: true,
			wantInLog: []string{"+ env GT_TOWN_ROOT=/town GT_ROOT=/town gt daemon restart"},
		},
		{
			name: "a queued landing skips", count: "1\n",
			wantInLog: []string{
				"gastown has 1 open gt:ready-to-land bead(s); NOT restarting the daemon",
				"When the queue is empty run: gt daemon restart",
			},
		},
		{
			name: "several queued landings skip", count: "3\n",
			wantInLog: []string{"gastown has 3 open gt:ready-to-land bead(s)"},
		},
		{
			name: "an unreadable queue skips", countErr: errExit,
			wantInLog: []string{
				"could not read gastown's landing queue",
				"NOT restarting the daemon",
				"gt daemon restart",
			},
		},
		{
			name: "a count that is not a number skips", count: "many\n",
			wantInLog: []string{"could not read gastown's landing queue"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			run := &restartRunner{count: tt.count, countErr: tt.countErr}
			c, log := restartCutover(false)
			c.Runner = run

			if err := c.RestartDaemonIfQuiet(); err != nil {
				t.Fatalf("RestartDaemonIfQuiet: %v", err)
			}

			if len(run.counts) != 1 || !slices.Equal(run.counts[0], queueArgv) {
				t.Fatalf("the landing queue reads = %q, want one call of %q", run.counts, queueArgv)
			}
			if tt.wantRestart {
				if len(run.runs) != 1 || !slices.Equal(run.runs[0], restartWanted) {
					t.Fatalf("restart calls = %q, want one call of %q", run.runs, restartWanted)
				}
			} else if len(run.runs) != 0 {
				t.Fatalf("the daemon was restarted over a landing queue: %q", run.runs)
			}
			joined := strings.Join(*log, "\n")
			for _, want := range tt.wantInLog {
				if !strings.Contains(joined, want) {
					t.Errorf("the log does not say %q:\n%s", want, joined)
				}
			}
		})
	}
}

// A dry run prints the restart it would make and runs nothing; the queue is still
// read, so the line says whether the queue is quiet.
func TestRestartDryRunRunsNothing(t *testing.T) {
	t.Parallel()
	run := &restartRunner{count: "0\n"}
	c, log := restartCutover(true)
	c.Runner = run

	if err := c.RestartDaemonIfQuiet(); err != nil {
		t.Fatalf("RestartDaemonIfQuiet: %v", err)
	}

	if len(run.counts) != 1 || !slices.Equal(run.counts[0], queueArgv) {
		t.Fatalf("the landing queue reads = %q, want one call of %q", run.counts, queueArgv)
	}
	if len(run.runs) != 0 {
		t.Fatalf("a dry run restarted the daemon: %q", run.runs)
	}
	joined := strings.Join(*log, "\n")
	for _, want := range []string{
		"+ env GT_TOWN_ROOT=/town GT_ROOT=/town gt daemon restart",
		"dry run: the daemon was not restarted",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("the log does not say %q:\n%s", want, joined)
		}
	}
}

// A cutover with no exec seam refuses the step: skipping would read as a daemon
// that already reads the cut-over block.
func TestRestartUnwiredExecSeamRefuses(t *testing.T) {
	t.Parallel()
	c, _ := restartCutover(false)

	if err := c.RestartDaemonIfQuiet(); !errors.Is(err, errRestartRunnerUnwired) {
		t.Fatalf("RestartDaemonIfQuiet without a Runner = %v, want %v", err, errRestartRunnerUnwired)
	}
}

// A restart that fails is an error that names the step and carries the child's
// failure.
func TestRestartFailureIsAnError(t *testing.T) {
	t.Parallel()
	run := &restartRunner{count: "0\n", runErr: errExit}
	c, _ := restartCutover(false)
	c.Runner = run

	err := c.RestartDaemonIfQuiet()
	if !errors.Is(err, errExit) {
		t.Fatalf("RestartDaemonIfQuiet error = %v, want one wrapping %v", err, errExit)
	}
	if !strings.Contains(err.Error(), "the daemon restart of gastown failed") {
		t.Fatalf("the error does not name the step: %v", err)
	}
}

// The operator steps are the script's final block, line for line: the promote
// branch, whose deploy key an earlier step printed. A changed line turns this red.
func TestOperatorStepsText(t *testing.T) {
	t.Parallel()
	c, _ := restartCutover(false)

	got := c.OperatorSteps()
	want := []string{
		"Finish the cutover by hand:",
		"  - add the promote deploy key above, then confirm the rig's next green main verdict advances GitHub main",
		"  - disable GitHub Actions on acme/rig:",
		"      gh api --method PUT repos/acme/rig/actions/permissions -F enabled=false",
		"  - stop (not remove) any self-hosted GitHub runner for acme/rig",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("OperatorSteps() = %q, want %q", got, want)
	}
}

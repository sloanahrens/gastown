package cmd

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/taskmap"
)

// seatShipRun is what a stubbed runSeatShip saw.
type seatShipRun struct {
	calls              int
	deps               seatShipDeps
	rig, polecat, bead string
	code               int
}

// run is the stub runSeatShip: it records its arguments and answers code.
func (r *seatShipRun) run(_ context.Context, d seatShipDeps, rig, polecat, bead string) int {
	r.calls++
	r.deps, r.rig, r.polecat, r.bead = d, rig, polecat, bead
	return r.code
}

func seatShipExec(t *testing.T, open func() (beads.Client, error), r *seatShipRun, args ...string) (string, error) {
	t.Helper()
	c := newSeatShipCmd(open, r.run)
	var out bytes.Buffer
	c.SetOut(&out)
	c.SetErr(&out)
	c.SetArgs(args)
	err := c.Execute()
	return out.String(), err
}

// TestSeatShipCmdIsUnderSeatWithItsFlags: gt seat ship is registered under gt
// seat and takes --rig, --polecat and --bead, each required.
func TestSeatShipCmdIsUnderSeatWithItsFlags(t *testing.T) {
	t.Parallel()
	got, _, err := rootCmd.Find([]string{"seat", "ship"})
	if err != nil || got.Name() != "ship" || got.Parent() != seatCmd {
		t.Fatalf("rootCmd.Find(seat ship) = %v, %v, want ship under seat", got, err)
	}
	for _, name := range []string{"rig", "polecat", "bead"} {
		f := got.Flags().Lookup(name)
		if f == nil {
			t.Errorf("gt seat ship has no --%s flag", name)
			continue
		}
		if req := f.Annotations["cobra_annotation_bash_completion_one_required_flag"]; len(req) != 1 || req[0] != "true" {
			t.Errorf("--%s is not required (annotations %v)", name, f.Annotations)
		}
	}
}

// TestSeatShipCmdPassesTheFlagsAndTheSeatDirectories: the flag values reach
// runSeatShip, with the client the opener gave, the sandbox's own projects and
// outbox directories and the 60 second deadline.
func TestSeatShipCmdPassesTheFlagsAndTheSeatDirectories(t *testing.T) {
	t.Parallel()
	db := beadsfake.New()
	r := &seatShipRun{}
	out, err := seatShipExec(t, func() (beads.Client, error) { return db, nil }, r,
		"--rig", "gastown", "--polecat", "nux", "--bead", "gt-seat")
	if err != nil {
		t.Fatalf("Execute() = %v (%s), want nil", err, out)
	}
	if r.calls != 1 || r.rig != "gastown" || r.polecat != "nux" || r.bead != "gt-seat" {
		t.Errorf("runSeatShip saw %d call(s) with rig %q polecat %q bead %q, want one with gastown nux gt-seat", r.calls, r.rig, r.polecat, r.bead)
	}
	if r.deps.Beads != beads.Client(db) {
		t.Error("runSeatShip was not given the client the opener returned")
	}
	if r.deps.Projects != session.SandboxProjectsDir || r.deps.Outbox != session.SandboxOutboxDir {
		t.Errorf("directories = %q and %q, want the sandbox's %q and %q", r.deps.Projects, r.deps.Outbox, session.SandboxProjectsDir, session.SandboxOutboxDir)
	}
	if r.deps.Deadline != seatShipDeadline {
		t.Errorf("deadline = %v, want %v", r.deps.Deadline, seatShipDeadline)
	}
	if r.deps.Stderr == nil {
		t.Error("runSeatShip was given no stderr writer")
	}
}

// TestSeatShipCmdExitCode: a zero exit from runSeatShip is success, any other
// is returned as a silent exit with that code (runSeatShip has already written
// its one stderr line), and an opener that fails stops before runSeatShip.
func TestSeatShipCmdExitCode(t *testing.T) {
	t.Parallel()
	open := func() (beads.Client, error) { return beadsfake.New(), nil }
	args := []string{"--rig", "r", "--polecat", "p", "--bead", "b"}

	t.Run("success", func(t *testing.T) {
		t.Parallel()
		if _, err := seatShipExec(t, open, &seatShipRun{code: 0}, args...); err != nil {
			t.Errorf("exit 0: Execute() = %v, want nil", err)
		}
	})
	t.Run("failure", func(t *testing.T) {
		t.Parallel()
		_, err := seatShipExec(t, open, &seatShipRun{code: 1}, args...)
		if code, ok := IsSilentExit(err); !ok || code != 1 {
			t.Errorf("exit 1: Execute() = %v, want a silent exit with code 1", err)
		}
	})
	t.Run("the client cannot be opened", func(t *testing.T) {
		t.Parallel()
		r := &seatShipRun{}
		_, err := seatShipExec(t, func() (beads.Client, error) { return nil, errors.New("no beads here") }, r, args...)
		if err == nil || !strings.Contains(err.Error(), "no beads here") {
			t.Errorf("Execute() = %v, want the opener's error", err)
		}
		if r.calls != 0 {
			t.Errorf("runSeatShip ran %d time(s) after the opener failed, want none", r.calls)
		}
	})
	t.Run("a required flag is missing", func(t *testing.T) {
		t.Parallel()
		r := &seatShipRun{}
		_, err := seatShipExec(t, open, r, "--rig", "r", "--polecat", "p")
		if err == nil || !strings.Contains(err.Error(), "bead") {
			t.Errorf("Execute() without --bead = %v, want an error naming it", err)
		}
		if r.calls != 0 {
			t.Errorf("runSeatShip ran %d time(s) without --bead, want none", r.calls)
		}
	})
}

// TestSeatShipCmdFlagsReachTheDeps: each of --projects, --outbox and --deadline
// given a value reaches the deps runSeatShip is called with.
func TestSeatShipCmdFlagsReachTheDeps(t *testing.T) {
	t.Parallel()
	required := []string{"--rig", "r", "--polecat", "p", "--bead", "b"}
	open := func() (beads.Client, error) { return beadsfake.New(), nil }
	rows := []struct {
		name string
		flag []string
		want func(d seatShipDeps) bool
	}{
		{"projects reaches the deps", []string{"--projects", "/seat/projects"}, func(d seatShipDeps) bool { return d.Projects == "/seat/projects" }},
		{"outbox reaches the deps", []string{"--outbox", "/seat/outbox"}, func(d seatShipDeps) bool { return d.Outbox == "/seat/outbox" }},
		{"deadline reaches the deps", []string{"--deadline", "5s"}, func(d seatShipDeps) bool { return d.Deadline == 5*time.Second }},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			r := &seatShipRun{}
			out, err := seatShipExec(t, open, r, append(append([]string{}, required...), row.flag...)...)
			if err != nil {
				t.Fatalf("Execute() = %v (%s), want nil", err, out)
			}
			if r.calls != 1 || !row.want(r.deps) {
				t.Errorf("runSeatShip got deps %+v over %d call(s), want the flag's value in it", r.deps, r.calls)
			}
		})
	}
}

// TestSeatShipCmdRefusesANonPositiveDeadline: a deadline of zero or less is a
// usage error, named by the flag, and runSeatShip never runs.
func TestSeatShipCmdRefusesANonPositiveDeadline(t *testing.T) {
	t.Parallel()
	for _, deadline := range []string{"0s", "-1s"} {
		t.Run(deadline, func(t *testing.T) {
			t.Parallel()
			r := &seatShipRun{}
			open := func() (beads.Client, error) { return beadsfake.New(), nil }
			_, err := seatShipExec(t, open, r, "--rig", "r", "--polecat", "p", "--bead", "b", "--deadline="+deadline)
			if err == nil || !strings.Contains(err.Error(), "--deadline") {
				t.Errorf("--deadline=%s: Execute() = %v, want a refusal naming --deadline", deadline, err)
			}
			if r.calls != 0 {
				t.Errorf("--deadline=%s: runSeatShip ran %d time(s), want none", deadline, r.calls)
			}
		})
	}
}

// TestSeatShipCmdHasItsTaskPlacement: the task map places gt seat ship in the
// seats group, so the command is found by the job it does.
func TestSeatShipCmdHasItsTaskPlacement(t *testing.T) {
	t.Parallel()
	entry, ok := taskmap.Default().Lookup("seat ship")
	if !ok {
		t.Fatal("taskmap has no entry for seat ship")
	}
	if entry.Group != "seats" {
		t.Errorf("seat ship placed in group %q, want seats", entry.Group)
	}
}

// TestSeatShipCmdOpensTheClientForTheCallerDirectory: a directory source that
// fails is the opener's error, naming the cause, and one that answers gives a
// client.
func TestSeatShipCmdOpensTheClientForTheCallerDirectory(t *testing.T) {
	t.Parallel()
	t.Run("the directory cannot be read", func(t *testing.T) {
		t.Parallel()
		client, err := openSeatShipClientFrom(func() (string, error) { return "", errors.New("no such directory") })
		if err == nil || !strings.Contains(err.Error(), "no such directory") || client != nil {
			t.Errorf("openSeatShipClientFrom(failing) = %v, %v, want no client and the cause", client, err)
		}
	})
	t.Run("the directory is read", func(t *testing.T) {
		t.Parallel()
		client, err := openSeatShipClientFrom(func() (string, error) { return t.TempDir(), nil })
		if err != nil || client == nil {
			t.Errorf("openSeatShipClientFrom(dir) = %v, %v, want a client", client, err)
		}
	})
}

package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/dispatch"
	"github.com/steveyegge/gastown/internal/estop"
	"github.com/steveyegge/gastown/internal/holdclaim"
	"github.com/steveyegge/gastown/internal/specdispatch"
)

// The Mac-side writer mirrors a hold or ESTOP into the daemon pod's claim, and
// the spec dispatcher's tick reads the claim as its town root. These tests run
// the writer into a claim directory on disk, then run one tick over that
// directory, so a hold the writer sets must stop the tick on the next tick.

// crossPodMount is the path the daemon pod mounts its town-state claim at. It
// mirrors holdclaim's podMount, which this package cannot import. A drift makes
// every pod call here fail, not pass.
const crossPodMount = "/Users/sloan/gt"

// crossOther is the second rig. A rig ESTOP on gastown leaves it open.
const crossOther = "otherrig"

// crossClaim is the daemon pod's claim on disk. Its Exec runs the commands the
// Mac-side writer sends against dir, so a file the writer leaves there is the
// file the tick reads.
type crossClaim struct {
	dir string
}

func (c *crossClaim) Exec(_ context.Context, argv []string, stdin []byte) ([]byte, error) {
	sep := slices.Index(argv, "--")
	if sep < 0 || sep+1 >= len(argv) {
		return nil, errors.New("crossClaim: no command after --")
	}
	command, args := argv[sep+1], argv[sep+2:]
	switch {
	case command == "test" && len(args) == 2 && args[0] == "-d":
		_, err := os.Stat(c.local(args[1]))
		return nil, err
	case command == "tee" && len(args) == 1:
		return nil, os.WriteFile(c.local(args[0]), stdin, 0o644)
	case command == "cat" && len(args) == 1:
		return os.ReadFile(c.local(args[0]))
	case command == "rm" && len(args) >= 1:
		if err := os.Remove(c.local(args[len(args)-1])); err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		return nil, nil
	case command == "find" && len(args) == 5 && args[1] == "-maxdepth" && args[3] == "-name":
		// find exits 0 and prints the path only when the entry is there.
		full := path.Join(args[0], args[4])
		if _, err := os.Lstat(c.local(full)); err == nil {
			return []byte(full + "\n"), nil
		}
		return nil, nil
	}
	return nil, fmt.Errorf("crossClaim: unexpected pod command %q", argv[sep+1:])
}

// local maps a path inside the pod to the claim directory on disk.
func (c *crossClaim) local(podPath string) string {
	return filepath.Join(c.dir, strings.TrimPrefix(podPath, crossPodMount))
}

// crossWriter returns a Mac town, a claim directory, and the Mac-side writer
// that mirrors the Mac town's sentinels into that claim.
func crossWriter(t *testing.T) (string, string, *holdclaim.Writer) {
	t.Helper()
	mac, claim := t.TempDir(), t.TempDir()
	target := holdclaim.Target{Context: "daemon-ctx", Namespace: "gastown"}
	w, err := holdclaim.New(mac, target, (&crossClaim{dir: claim}).Exec)
	if err != nil {
		t.Fatalf("holdclaim.New: %v", err)
	}
	return mac, claim, w
}

// crossSentinel is one hold or ESTOP the writer sets and lifts, and the rigs it
// stops: a town-wide sentinel stops every rig, a rig ESTOP only gastown.
type crossSentinel struct {
	name     string
	what     string
	local    func(root string) string
	content  []byte
	townWide bool
}

var crossSentinels = []crossSentinel{
	{
		name:     "operator hold",
		what:     "hold",
		local:    dispatch.HoldFilePath,
		townWide: true,
	},
	{
		name:     "town ESTOP",
		what:     "E-stop",
		local:    estop.FilePath,
		content:  estop.Render(estop.TriggerManual, "spec dispatch cross-boundary test", time.Now()),
		townWide: true,
	},
	{
		name:    "rig ESTOP",
		what:    "E-stop",
		local:   func(root string) string { return estop.RigFilePath(root, "gastown") },
		content: estop.Render(estop.TriggerManual, "spec dispatch cross-boundary test", time.Now()),
	},
}

// crossBoard is the board one tick reads: gt-a in gastown and gt-b in the other
// rig, so a rig ESTOP on gastown stops gt-a and leaves gt-b to dispatch.
func crossBoard() *fakeSpecTown {
	f := newFakeSpecTown(
		cleanSpec("gt-a", 1, "2026-09-29T10:00:00Z"),
		cleanSpec("gt-b", 2, "2026-09-29T10:01:00Z"),
	)
	f.rigOf = map[string]string{"gt-b": crossOther}
	return f
}

// crossTick runs one spec dispatch tick over claim as its town root. The holds
// are read from the claim directory the way the daemon reads them; the board is f's.
func crossTick(f *fakeSpecTown, claim string) specDispatchReport {
	env := f.env()
	// Both fixture beads may dispatch in one tick, so the per-tick limit cannot
	// hide a dispatch the hold let through.
	env.PerTick = 2
	env.Hold = func() string { return dispatch.OperatorHold(claim) }
	env.RigHold = func(rig string) string { return dispatch.RigHold(claim, rig) }
	return runSpecDispatchCycle(env)
}

// crossSlung is the beads the tick slung, sorted.
func crossSlung(f *fakeSpecTown) []string {
	got := slices.Clone(f.slung)
	slices.Sort(got)
	return got
}

// crossSkipLine is the skip line the tick wrote for bead id, or "" when it wrote none.
func crossSkipLine(r specDispatchReport, id string) string {
	for _, e := range r.Skipped {
		if e.Bead == id {
			return e.Line
		}
	}
	return ""
}

// TestSpecDispatchHoldSetThroughTheWriterStopsTheTickUntilLifted drives each
// sentinel through the Mac-side writer into the claim, then runs one tick over
// that claim before the set, after it, and after the lift.
func TestSpecDispatchHoldSetThroughTheWriterStopsTheTickUntilLifted(t *testing.T) {
	t.Parallel()
	for _, s := range crossSentinels {
		t.Run(s.name, func(t *testing.T) {
			t.Parallel()
			mac, claim, w := crossWriter(t)
			ctx := context.Background()
			local := s.local(mac)
			both := []string{"gt-a", "gt-b"}

			before := crossBoard()
			if r := crossTick(before, claim); r.Hold != "" || !slices.Equal(crossSlung(before), both) {
				t.Fatalf("before the set: hold %q, slung %v; want no hold and %v dispatched", r.Hold, crossSlung(before), both)
			}

			if err := holdclaim.Activate(ctx, w, s.what, local, s.content); err != nil {
				t.Fatalf("Activate %s: %v", s.name, err)
			}

			held := crossBoard()
			r := crossTick(held, claim)
			if s.townWide {
				if r.Hold == "" {
					t.Fatalf("tick after the %s set reports no hold, want it named", s.name)
				}
				if !strings.Contains(r.Hold, s.local(claim)) {
					t.Fatalf("tick hold %q does not name the claim's %s at %s", r.Hold, s.name, s.local(claim))
				}
				if len(held.slung) != 0 {
					t.Fatalf("tick after the %s set slung %v, want none", s.name, crossSlung(held))
				}
			} else {
				if r.Hold != "" {
					t.Fatalf("tick after the rig ESTOP reports town hold %q, want the rig only", r.Hold)
				}
				if got := crossSlung(held); !slices.Equal(got, []string{"gt-b"}) {
					t.Fatalf("tick after the rig ESTOP slung %v, want only gt-b in %s", got, crossOther)
				}
				line := crossSkipLine(r, "gt-a")
				if !strings.Contains(line, specdispatch.SkipRigHold) || !strings.Contains(line, s.local(claim)) {
					t.Fatalf("gt-a skip line %q does not report the rig hold naming %s", line, s.local(claim))
				}
			}

			if err := holdclaim.Lift(ctx, w, s.what, local); err != nil {
				t.Fatalf("Lift %s: %v", s.name, err)
			}

			lifted := crossBoard()
			if r := crossTick(lifted, claim); r.Hold != "" || !slices.Equal(crossSlung(lifted), both) {
				t.Fatalf("after the lift: hold %q, slung %v; want no hold and %v dispatched again", r.Hold, crossSlung(lifted), both)
			}
		})
	}
}

// TestSpecDispatchHoldUnreadableHoldPathFailsClosed sets the operator hold
// through the writer, then makes the claim's copy unreadable with a symbolic
// link to itself, which fails the stat with ELOOP. The tick must dispatch
// nothing and name the hold, not read the unreadable copy as absent.
func TestSpecDispatchHoldUnreadableHoldPathFailsClosed(t *testing.T) {
	t.Parallel()
	mac, claim, w := crossWriter(t)
	ctx := context.Background()
	if err := holdclaim.Activate(ctx, w, "hold", dispatch.HoldFilePath(mac), nil); err != nil {
		t.Fatalf("Activate hold: %v", err)
	}
	claimHold := dispatch.HoldFilePath(claim)
	if err := os.Remove(claimHold); err != nil {
		t.Fatalf("removing the claim's hold copy: %v", err)
	}
	if err := os.Symlink(claimHold, claimHold); err != nil {
		t.Fatalf("symlinking the claim's hold copy to itself: %v", err)
	}

	f := crossBoard()
	r := crossTick(f, claim)
	if len(f.slung) != 0 {
		t.Fatalf("tick through an unreadable hold slung %v, want none", crossSlung(f))
	}
	if !strings.Contains(r.Hold, "treating as held") || !strings.Contains(r.Hold, claimHold) {
		t.Fatalf("tick hold = %q, want the unreadable hold at %s named and treated as held", r.Hold, claimHold)
	}
}

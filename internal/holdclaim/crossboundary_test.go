package holdclaim

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
)

// The Mac town and the pod's claim are two directories here: the writer maps
// each sentinel under the Mac's town root to the same file under podMount, and
// diskPod runs those pod calls on the claim directory. The dispatcher then reads
// the claim as its town root, as the daemon pod does.
const (
	crossRig   = "gastown"
	crossOther = "otherrig"
)

// diskPod is the daemon pod's claim on disk. Its Exec runs the commands the
// writer sends against the claim directory, so a file the writer leaves there is
// the file the dispatcher reads.
type diskPod struct {
	claim string
}

func (p *diskPod) Exec(_ context.Context, argv []string, stdin []byte) ([]byte, error) {
	sep := slices.Index(argv, "--")
	if sep < 0 || sep+1 >= len(argv) {
		return nil, errors.New("diskPod: no command after --")
	}
	command, args := argv[sep+1], argv[sep+2:]
	switch command {
	case "test":
		if len(args) != 2 || args[0] != "-d" {
			return nil, fmt.Errorf("diskPod: unexpected test call %q", args)
		}
		_, err := os.Stat(p.local(args[1]))
		return nil, err
	case "tee":
		return nil, os.WriteFile(p.local(args[0]), stdin, 0o644)
	case "cat":
		return os.ReadFile(p.local(args[0]))
	case "rm":
		err := os.Remove(p.local(args[len(args)-1]))
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		return nil, nil
	case "find":
		// find DIR -maxdepth 1 -name NAME exits 0 and prints the path only when
		// the entry is there.
		if len(args) != 5 || args[1] != "-maxdepth" || args[3] != "-name" {
			return nil, fmt.Errorf("diskPod: unexpected find call %q", args)
		}
		full := path.Join(args[0], args[4])
		if _, err := os.Lstat(p.local(full)); err == nil {
			return []byte(full + "\n"), nil
		}
		return nil, nil
	}
	return nil, fmt.Errorf("diskPod: unexpected pod command %q", command)
}

// local maps a path inside the pod to the claim directory on disk.
func (p *diskPod) local(podPath string) string {
	return filepath.Join(p.claim, strings.TrimPrefix(podPath, podMount))
}

// tickHold is the reason the spec dispatcher's tick stops before it plans a
// sling for rig, read over the claim as the tick's town root. RigHold checks
// the town-wide holds first and then the rig's own, the two checks the tick
// makes (internal/cmd/spec.go, the env.Hold and env.RigHold seams). "" means
// the tick goes on to plan.
func tickHold(claim, rig string) string {
	return dispatch.RigHold(claim, rig)
}

// crossSentinel is one hold or ESTOP the writer sets and lifts, and the rigs it
// stops: a town-wide sentinel stops every rig, a rig ESTOP only its own.
type crossSentinel struct {
	name     string
	what     string
	local    func(macRoot string) string
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
		content:  estop.Render(estop.TriggerManual, "cross-boundary test", time.Now()),
		townWide: true,
	},
	{
		name:    "rig ESTOP",
		what:    "E-stop",
		local:   func(macRoot string) string { return estop.RigFilePath(macRoot, crossRig) },
		content: estop.Render(estop.TriggerManual, "cross-boundary test", time.Now()),
	},
}

// TestCrossBoundarySetThroughTheWriterStopsTheTickUntilLifted drives each
// sentinel through the Mac-side writer into the claim, then checks the tick over
// that claim: it plans before the set, stops after it, and plans again after the
// lift. A rig ESTOP must leave the other rig open.
func TestCrossBoundarySetThroughTheWriterStopsTheTickUntilLifted(t *testing.T) {
	t.Parallel()
	for _, s := range crossSentinels {
		t.Run(s.name, func(t *testing.T) {
			t.Parallel()
			mac := t.TempDir()
			claim := t.TempDir()
			pod := &diskPod{claim: claim}
			w, err := New(mac, testTarget, pod.Exec)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			local := s.local(mac)
			ctx := context.Background()

			if got := tickHold(claim, crossRig); got != "" {
				t.Fatalf("before the set the tick holds %q, want it to plan", got)
			}

			if err := Activate(ctx, w, s.what, local, s.content); err != nil {
				t.Fatalf("Activate %s: %v", s.name, err)
			}
			if got := tickHold(claim, crossRig); got == "" {
				t.Fatalf("after the writer set the %s the tick plans for %s, want a hold", s.name, crossRig)
			}
			if !s.townWide {
				if got := tickHold(claim, crossOther); got != "" {
					t.Fatalf("after a rig ESTOP for %s the tick holds %s with %q, want it to plan", crossRig, crossOther, got)
				}
			} else if got := tickHold(claim, crossOther); got == "" {
				t.Fatalf("after the town %s the tick plans for %s, want a hold", s.name, crossOther)
			}

			if err := Lift(ctx, w, s.what, local); err != nil {
				t.Fatalf("Lift %s: %v", s.name, err)
			}
			if got := tickHold(claim, crossRig); got != "" {
				t.Fatalf("after the lift the tick still holds %s with %q, want it to plan", crossRig, got)
			}
		})
	}
}

// TestCrossBoundaryUnreadableHoldPathFailsClosed sets the operator hold through
// the writer, then makes the claim's copy unreadable: a symbolic link to itself
// fails the stat with ELOOP. The tick must hold, not read the unreadable copy as
// absent.
func TestCrossBoundaryUnreadableHoldPathFailsClosed(t *testing.T) {
	t.Parallel()
	mac := t.TempDir()
	claim := t.TempDir()
	pod := &diskPod{claim: claim}
	w, err := New(mac, testTarget, pod.Exec)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()
	local := dispatch.HoldFilePath(mac)
	if err := Activate(ctx, w, "hold", local, nil); err != nil {
		t.Fatalf("Activate hold: %v", err)
	}
	claimHold := filepath.Join(claim, filepath.Base(local))
	if err := os.Remove(claimHold); err != nil {
		t.Fatalf("removing the claim's hold copy: %v", err)
	}
	if err := os.Symlink(claimHold, claimHold); err != nil {
		t.Fatal(err)
	}
	got := tickHold(claim, crossRig)
	if !strings.Contains(got, "treating as held") {
		t.Fatalf("tick over an unreadable hold path = %q, want a fail-closed hold", got)
	}
}

package power

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/session"
)

// sandboxBackend is a fake openshell backend. It records the sessions it is
// told to stop. Its other methods come from the nil embedded interface, so
// any call to them panics and fails the test: a stop-only path must not start
// or delete anything.
type sandboxBackend struct {
	session.Backend
	stopped []string
	stopErr error
}

func (b *sandboxBackend) Stop(name string) error {
	b.stopped = append(b.stopped, name)
	return b.stopErr
}

func (b *sandboxBackend) open(_ string, _ session.Profile) (session.Backend, error) {
	return b, nil
}

// withSandboxes gives the fake town two openshell polecats with registry
// records and one tmux polecat with none, all idle so a drain does not wait.
func (f *fake) withSandboxes(t *testing.T) {
	t.Helper()
	for _, polecat := range []string{"ace", "nux"} {
		rec := session.SandboxRecord{
			Session:   "gastown-" + polecat,
			Rig:       "gastown",
			Polecat:   polecat,
			Profile:   "polecat-default",
			Image:     "gt/polecat:test",
			CreatedAt: f.now,
		}
		if err := session.WriteSandboxRecord(f.town, rec); err != nil {
			t.Fatal(err)
		}
	}
	f.polecats = []polecatRow{
		{Rig: "gastown", Name: "furiosa", State: "idle"},
		{Rig: "gastown", Name: "ace", State: "idle"},
		{Rig: "gastown", Name: "nux", State: "idle"},
	}
}

func TestSandboxStopOnStandbyAndOffStopsOnlyOpenShell(t *testing.T) {
	t.Parallel()
	states := map[string]func(context.Context, Options) error{
		"standby": Standby,
		"off":     Off,
	}
	for name, state := range states {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFake(t)
			f.setHealthy()
			f.setPending(0)
			f.withSandboxes(t)
			b := &sandboxBackend{}
			o, out, _ := f.optsWithOut()
			o.Yes = true
			o.NewSandboxBackend = b.open
			if err := state(context.Background(), o); err != nil {
				t.Fatalf("%s: %v (output:\n%s)", name, err, out)
			}
			want := []string{"gastown-ace", "gastown-nux"}
			if !reflect.DeepEqual(b.stopped, want) {
				t.Errorf("stopped = %v, want %v (the tmux polecat must not be stopped here)", b.stopped, want)
			}
		})
	}
}

func TestSandboxFullNeverStartsSandboxes(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	f.setHealthy()
	f.withSandboxes(t)
	b := &sandboxBackend{}
	o, out, _ := f.optsWithOut()
	o.NewSandboxBackend = b.open
	if err := Full(context.Background(), o); err != nil {
		t.Fatalf("Full: %v (output:\n%s)", err, out)
	}
	if len(b.stopped) != 0 {
		t.Errorf("full stopped %v, want no sandbox touched", b.stopped)
	}
}

func TestSandboxStopErrorIsLoggedNotFatal(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	f.setHealthy()
	f.setPending(0)
	f.withSandboxes(t)
	b := &sandboxBackend{stopErr: errors.New("gateway unreachable")}
	o, out, errOut := f.optsWithOut()
	o.NewSandboxBackend = b.open
	if err := Standby(context.Background(), o); err != nil {
		t.Fatalf("Standby failed on a sandbox stop error: %v (output:\n%s)", err, out)
	}
	if len(b.stopped) != 2 {
		t.Errorf("stop attempts = %v, want both sandboxes tried", b.stopped)
	}
	if !strings.Contains(errOut.String(), "sandbox gastown-ace: stop failed") {
		t.Errorf("stop error not logged; stderr:\n%s", errOut)
	}
	data, err := os.ReadFile(StatePath(f.town))
	if err != nil || !strings.HasPrefix(string(data), "standby ") {
		t.Errorf("state file = %q, %v; want the standby record despite the stop error", data, err)
	}
}

func TestSandboxDryRunStopsNothing(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	f.setHealthy()
	f.setPending(0)
	f.withSandboxes(t)
	b := &sandboxBackend{}
	o, out, _ := f.optsWithOut()
	o.DryRun = true
	o.NewSandboxBackend = b.open
	if err := Off(context.Background(), o); err != nil {
		t.Fatalf("Off --dry-run: %v (output:\n%s)", err, out)
	}
	if len(b.stopped) != 0 {
		t.Errorf("dry-run stopped %v, want nothing", b.stopped)
	}
	if !strings.Contains(out.String(), "[dry-run] stop sandbox gastown-ace") {
		t.Errorf("dry-run did not preview the sandbox stop:\n%s", out)
	}
}

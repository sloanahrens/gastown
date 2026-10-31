package cmd

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/power"
	"github.com/steveyegge/gastown/internal/ring"
	"github.com/steveyegge/gastown/internal/townhealth"
)

// powerRingHost answers the probes a healthy town gives and the commands the
// states run, so standby, full and off run their real steps. A command it was
// not written for fails the test: no subprocess runs and nothing outside the
// town is touched.
func powerRingHost(t *testing.T) func(ctx context.Context, dir, name string, args ...string) (string, error) {
	t.Helper()
	return func(_ context.Context, _, name string, args ...string) (string, error) {
		line := strings.Join(append([]string{name}, args...), " ")
		switch {
		case strings.HasPrefix(line, "docker info"):
			return "Server Version: 24\n", nil
		case strings.HasPrefix(line, "docker compose ps"):
			return strings.Repeat("container-id\n", 5), nil
		case strings.HasPrefix(line, "docker compose up"), strings.HasPrefix(line, "docker compose stop"):
			return "", nil
		case strings.HasPrefix(line, "gt dolt status"):
			return "● Dolt server is running (PID 1)\n", nil
		case strings.HasPrefix(line, "gt dolt start"), strings.HasPrefix(line, "gt dolt stop"):
			return "", nil
		case strings.HasPrefix(line, "gt estop"), strings.HasPrefix(line, "gt thaw"), strings.HasPrefix(line, "gt status"):
			return "", nil
		case strings.HasPrefix(line, "gt polecat list"):
			return "[]", nil
		case strings.HasPrefix(line, "launchctl print"), strings.HasPrefix(line, "launchctl bootout"):
			return "", nil
		}
		t.Errorf("unexpected command: %s", line)
		return "", errors.New("unexpected command")
	}
}

// powerRingOptions is the options gt power builds for root, with the host
// swapped for powerRingHost and the clock fixed at ringTestNow. The key is
// whatever RealSeams resolves, so the test runs the production wiring.
func powerRingOptions(t *testing.T, root string) power.Options {
	t.Helper()
	o, err := powerOptionsFor(root)
	if err != nil {
		t.Fatalf("powerOptionsFor: %v", err)
	}
	o.GTCommand = "gt"
	o.Run = powerRingHost(t)
	o.HTTPStatus = func(string) (int, error) { return 200, nil }
	o.Glob = func(string) ([]string, error) { return nil, nil }
	o.LookPath = func(string) (string, error) { return "", exec.ErrNotFound }
	o.Sleep = func(context.Context, time.Duration) error { return nil }
	o.Now = func() time.Time { return ringTestNow }
	o.Actor = "nux"
	o.Timeout = time.Minute
	o.Yes = true
	return o
}

// writePowerRingHealth records a landing queue of zero at ringTestNow, so a
// drain can read the town as drained.
func writePowerRingHealth(t *testing.T, root string) {
	t.Helper()
	err := townhealth.Write(root, townhealth.Report{
		At:      ringTestNow,
		Verdict: townhealth.Green,
		Fields: []townhealth.Field{{
			Name:    townhealth.FieldLanding,
			Rig:     "gastown",
			Tag:     townhealth.Recorded,
			Verdict: townhealth.Green,
			Value:   "0 pending",
		}},
	})
	if err != nil {
		t.Fatalf("writing the landing report: %v", err)
	}
}

// TestPowerRingWiresTheKeyToTownKey: the production options read the Ring key
// through ring.TownKey, so a town that names no ring.key is refused with the
// resolver's own error, whatever the host environment holds.
func TestPowerRingWiresTheKeyToTownKey(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	ringTestTownFile(t, root)
	o, err := powerOptionsFor(root)
	if err != nil {
		t.Fatalf("powerOptionsFor: %v", err)
	}
	if _, err := o.RingKey(root); !errors.Is(err, ring.ErrKeyNotConfigured) {
		t.Fatalf("RingKey with no ring.key = %v, want ring.ErrKeyNotConfigured", err)
	}
}

// TestPowerRingResolvesTheKeyThroughTownKey runs standby, full and off in turn
// against a town whose ring.key is a reference the environment resolves. Each
// verb reads the Ring under the key ring.TownKey returns, leaves the Ring the
// state calls for, and prints nothing of the key. The environment is injected
// into ring.TownKey, as the ring verbs' tests inject theirs.
func TestPowerRingResolvesTheKeyThroughTownKey(t *testing.T) {
	t.Parallel()
	root := ringTestTown(t)
	writePowerRingHealth(t, root)
	o := powerRingOptions(t, root)
	o.RingKey = ring.TownKey(ringTestEnv)

	key, err := o.RingKey(root)
	if err != nil {
		t.Fatalf("the key did not resolve through ring.TownKey: %v", err)
	}
	if string(key) != ringTestKey {
		t.Fatal("ring.TownKey resolved a key other than the environment's")
	}
	rec, err := ring.Sign(ring.Record{ForgedAt: ringTestNow, ExpiresAt: ringTestNow.Add(ring.Lease), ForgedBy: "nux"}, key)
	if err != nil {
		t.Fatalf("signing the Ring: %v", err)
	}
	if err := ring.Write(root, rec); err != nil {
		t.Fatalf("writing the Ring: %v", err)
	}

	steps := []struct {
		name string
		run  func(context.Context, power.Options) error
		want string
		kind ring.Kind
	}{
		{"standby", power.Standby, "Ring waned", ring.Waned},
		{"full", power.Full, "Ring renewed", ring.Valid},
		{"off", power.Off, "Ring removed", ring.Absent},
	}
	for _, step := range steps {
		var out, errOut bytes.Buffer
		o.Out, o.ErrOut = &out, &errOut
		if err := step.run(context.Background(), o); err != nil {
			t.Fatalf("%s: %v (output:\n%s%s)", step.name, err, out.String(), errOut.String())
		}
		if !strings.Contains(out.String(), step.want) {
			t.Errorf("%s did not print %q:\n%s", step.name, step.want, out.String())
		}
		if strings.Contains(out.String()+errOut.String(), ringTestKey) {
			t.Errorf("%s printed the signing key", step.name)
		}
		st, err := ring.Read(root, key, ringTestNow)
		if err != nil {
			t.Fatalf("%s: reading the Ring: %v", step.name, err)
		}
		if st.Kind != step.kind {
			t.Errorf("after %s the Ring is %s, want %s", step.name, st, step.kind)
		}
	}
}

// TestPowerRingWithoutAKeyRefusesFullAndWritesNothing: a ring.key the environment
// does not resolve leaves full unable to renew a Ring it finds, so full refuses
// before it runs any command, and the Ring keeps its record.
func TestPowerRingWithoutAKeyRefusesFullAndWritesNothing(t *testing.T) {
	t.Parallel()
	root := ringTestTown(t)
	rec, err := ring.Sign(ring.Record{ForgedAt: ringTestNow, ExpiresAt: ringTestNow.Add(ring.Lease), ForgedBy: "nux"}, []byte(ringTestKey))
	if err != nil {
		t.Fatalf("signing the Ring: %v", err)
	}
	if err := ring.Write(root, rec); err != nil {
		t.Fatalf("writing the Ring: %v", err)
	}
	o := powerRingOptions(t, root)
	o.RingKey = ring.TownKey(func(string) string { return "" })
	before, err := ring.Read(root, []byte(ringTestKey), ringTestNow)
	if err != nil || before.Kind != ring.Valid {
		t.Fatalf("precondition: the Ring is %s, %v; want valid", before, err)
	}
	var out, errOut bytes.Buffer
	o.Out, o.ErrOut = &out, &errOut
	o.Run = func(_ context.Context, _, name string, args ...string) (string, error) {
		t.Errorf("full ran %s %s before refusing", name, strings.Join(args, " "))
		return "", errors.New("unexpected command")
	}
	err = power.Full(context.Background(), o)
	if err == nil || !strings.Contains(err.Error(), "nothing was changed") {
		t.Fatalf("full without a key = %v, want a refusal that changed nothing", err)
	}
	if strings.Contains(err.Error(), ringTestKey) {
		t.Errorf("the refusal printed the signing key: %v", err)
	}
	after, err := ring.Read(root, []byte(ringTestKey), ringTestNow)
	if err != nil || after.Kind != ring.Valid {
		t.Errorf("the Ring after the refusal is %s, %v; want it untouched and valid", after, err)
	}
}

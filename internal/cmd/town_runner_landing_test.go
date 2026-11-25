package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/townhealth"
)

// landingReport is the daemon's health file; withPasses false predates the key.
func landingReport(t *testing.T, pause string, passes int, withPasses bool) []byte {
	t.Helper()
	r := map[string]any{"at": time.Date(2026, 11, 25, 13, 0, 0, 0, time.UTC), "verdict": "green", "fields": []any{}, "landing_paused": pause}
	if withPasses {
		r["landing_passes"] = passes
	}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("encoding the report: %v", err)
	}
	return b
}

// TestRunnerRollLandingRead: the read fetches the pod's health file through the
// pod runner and returns the pause, the passes in flight and known.
func TestRunnerRollLandingRead(t *testing.T) {
	t.Parallel()
	var argv []string
	run := func(_ context.Context, got []string, _ []byte) ([]byte, error) {
		argv = got
		return landingReport(t, "runner roll", 2, true), nil
	}
	pause, passes, known, err := podLandingRead("/town", run)(t.Context())
	if err != nil || pause != "runner roll" || passes != 2 || !known {
		t.Errorf("landing read = (%q, %d, %t, %v), want (\"runner roll\", 2, true, nil)", pause, passes, known, err)
	}
	if want := []string{"cat", "--", townhealth.Path("/town")}; !slices.Equal(argv, want) {
		t.Errorf("pod argv = %q, want %q", argv, want)
	}
}

// TestRunnerRollLandingReadRefuses: a failed read, an unparsable report and a
// report with no landing_passes are each an error with known false.
func TestRunnerRollLandingReadRefuses(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		reply []byte
		err   error
	}{
		{"read fails", nil, errors.New("running cat in the daemon pod: kubectl exited 1")},
		{"report does not parse", []byte("{"), nil},
		{"report has no landing_passes", landingReport(t, "runner roll", 0, false), nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			run := func(context.Context, []string, []byte) ([]byte, error) { return tc.reply, tc.err }
			_, _, known, err := podLandingRead("/town", run)(t.Context())
			if err == nil || known {
				t.Errorf("landing read: err = %v, known = %t, want an error and known false", err, known)
			}
		})
	}
}

// TestRunnerRollLandingReadIsWiredInDeps: the deps' read uses runnerRollDepsFor's exec.
func TestRunnerRollLandingReadIsWiredInDeps(t *testing.T) {
	t.Parallel()
	fake := &fakePodExec{stdout: landingReport(t, "runner roll", 2, true)}
	deps := runnerRollDepsFor("/town", fake.exec, fixedRunsSeam(), &bytes.Buffer{}, time.Minute, time.Minute, time.Minute, false)
	if deps.landing == nil {
		t.Fatal("the production deps have no landing read")
	}
	_, passes, known, err := deps.landing(t.Context())
	if err != nil || passes != 2 || !known {
		t.Errorf("landing read = (%d, %t, %v), want (2, true, nil)", passes, known, err)
	}
	if !slices.Contains(fake.argv, townhealth.Path("/town")) {
		t.Errorf("pod argv %q does not name the health file under the town root", fake.argv)
	}
}

package sweeprunner

import (
	"strings"
	"testing"
)

// TestGoCacheProgRunnerEnv holds the runner's GOCACHEPROG to the Spec's cap: a
// positive cap renders the cache program with its store on the sweep cache
// claim, and zero renders no variable, so the Job is as before (gt-7gezj1).
func TestGoCacheProgRunnerEnv(t *testing.T) {
	t.Parallel()
	const want = "/usr/local/bin/gt-gocacheprog -dir /var/cache/gt-sweep/gtprog -max-bytes 1073741824"
	for _, tc := range []struct {
		name     string
		maxBytes int
		want     string
		wantSet  bool
	}{
		{name: "zero renders no variable"},
		{name: "a cap renders the program", maxBytes: 1073741824, want: want, wantSet: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			spec := goldenSpec()
			spec.CPUs, spec.MemoryGB = 8, 24
			spec.GoCacheProgMaxBytes = tc.maxBytes
			plan, err := Kubernetes{}.Render(spec)
			if err != nil {
				t.Fatalf("rendering goCacheProgMaxBytes %d: %v", tc.maxBytes, err)
			}
			runner, ok := containerNamed(plan.Job, runnerContainerName)
			if !ok {
				t.Fatalf("the Job renders no %q container", runnerContainerName)
			}
			got, set := "", false
			for _, e := range runner.Env {
				if e.Name == "GOCACHEPROG" {
					got, set = e.Value, true
				}
			}
			if set != tc.wantSet {
				t.Fatalf("GOCACHEPROG set = %v, want %v", set, tc.wantSet)
			}
			if got != tc.want {
				t.Errorf("GOCACHEPROG = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestGoCacheProgSpecRefusesNegative holds the Spec to a cap of zero or more:
// a negative cap is refused by Render, naming the field, before any Job is
// built (gt-7gezj1).
func TestGoCacheProgSpecRefusesNegative(t *testing.T) {
	t.Parallel()
	spec := goldenSpec()
	spec.CPUs, spec.MemoryGB = 8, 24
	spec.GoCacheProgMaxBytes = -1
	_, err := Kubernetes{}.Render(spec)
	if err == nil || !strings.Contains(err.Error(), "goCacheProgMaxBytes") {
		t.Fatalf("error = %v, want a refusal naming goCacheProgMaxBytes", err)
	}
}

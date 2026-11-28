package sweeprunner

import "testing"

// TestK8sRunnerCPUMillisRequestsTheRunner holds the runner container to the
// Spec's millicore CPU request while its limit stays the whole share, so a
// smaller request places the run on less CPU without widening its share
// (gt-vv43zw).
func TestK8sRunnerCPUMillisRequestsTheRunner(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name              string
		requestMillis     int
		wantRunnerRequest string
		wantDindRequest   string
	}{
		{name: "zero asks for the whole shares", wantRunnerRequest: "6", wantDindRequest: "3"},
		{name: "500m requests below the shares", requestMillis: 500, wantRunnerRequest: "500m", wantDindRequest: "500m"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			spec := goldenSpec()
			spec.CPUs, spec.MemoryGB, spec.Dind, spec.DindCPUs = 9, 24, true, 3
			spec.RunnerCPURequestMillis = tc.requestMillis
			spec.DindCPURequestMillis = tc.requestMillis
			plan, err := Kubernetes{}.Render(spec)
			if err != nil {
				t.Fatalf("rendering runnerCPURequestMillis %d: %v", tc.requestMillis, err)
			}
			runner, ok := containerNamed(plan.Job, runnerContainerName)
			if !ok {
				t.Fatalf("the Job renders no %q container", runnerContainerName)
			}
			if got := runner.Resources.Requests.CPU; got != tc.wantRunnerRequest {
				t.Errorf("the runner asks for %s CPUs, want %s", got, tc.wantRunnerRequest)
			}
			if got := runner.Resources.Limits.CPU; got != "6" {
				t.Errorf("the runner's CPU limit is %s, want its whole share 6 unchanged", got)
			}
			dind, ok := containerNamed(plan.Job, dindContainerName)
			if !ok {
				t.Fatalf("the Job renders no %q sidecar", dindContainerName)
			}
			if got := dind.Resources.Requests.CPU; got != tc.wantDindRequest {
				t.Errorf("the sidecar asks for %s CPUs, want %s", got, tc.wantDindRequest)
			}
			if got := dind.Resources.Limits.CPU; got != "3" {
				t.Errorf("the sidecar's CPU limit is %s, want its share 3 unchanged", got)
			}
		})
	}
}

// TestK8sRunnerCPUMillisCapacityPreflight proves the millicore requests through
// the entry the daemon uses: the runner block's config, read by
// CapacityPreflight, then judged against a node with room. Setting both
// requests to 500m lowers the Job's CPU by the runner's and the sidecar's
// requests fall (7500m + 2500m), so the preflight asks 8000m less (gt-vv43zw).
func TestK8sRunnerCPUMillisCapacityPreflight(t *testing.T) {
	t.Parallel()
	cfg := capacityConfig()
	cfg.CPUs, cfg.Dind, cfg.DindCPUs = 9, true, 3

	zero, err := CapacityPreflight(cfg, "/town")
	if err != nil {
		t.Fatalf("CapacityPreflight with zero millicore requests: %v", err)
	}
	zeroReport, err := zero.Read([]byte(runnerRequestListing))
	if err != nil {
		t.Fatalf("the preflight declined a node with room at zero requests: %v", err)
	}

	cfg.RunnerCPURequestMillis, cfg.DindCPURequestMillis = 500, 500
	asked, err := CapacityPreflight(cfg, "/town")
	if err != nil {
		t.Fatalf("CapacityPreflight with 500m requests: %v", err)
	}
	askedReport, err := asked.Read([]byte(runnerRequestListing))
	if err != nil {
		t.Fatalf("the preflight declined a node with room at 500m requests: %v", err)
	}

	if drop := zeroReport.Job.CPU - askedReport.Job.CPU; drop != 8000 {
		t.Errorf("the preflight's job CPU fell by %dm with 500m requests, want 8000m (zero asks %s, 500m asks %s)",
			drop, zeroReport.Job, askedReport.Job)
	}
}

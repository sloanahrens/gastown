package sweeprunner

import "testing"

// runnerRequestListing is a cluster with one node holding room for any sweep.
const runnerRequestListing = `{"items": [{"kind": "Node", "metadata": {"name": "gt-control"}, "status": {"conditions": [{"type": "Ready", "status": "True"}], "allocatable": {"cpu": "64", "memory": "256Gi"}}}]}`

// TestRunnerRendersTheCPURequest holds the runner container to the Spec's CPU
// request (gt-13dre6.13): it asks for the configured request, its CPU limit
// stays the whole share, and the preflight's figure falls by the difference.
func TestRunnerRendersTheCPURequest(t *testing.T) {
	t.Parallel()
	plain, err := Kubernetes{}.Render(goldenSpec())
	if err != nil {
		t.Fatalf("rendering the golden spec: %v", err)
	}
	plainReport, err := plain.Preflight.Read([]byte(runnerRequestListing))
	if err != nil {
		t.Fatalf("the preflight declined a node with room: %v", err)
	}
	for _, tc := range []struct {
		name         string
		request      int
		wantRequests string
		wantJobDrop  int64
	}{
		{name: "no request asks for the whole share", wantRequests: "6"},
		{name: "a request below the share", request: 3, wantRequests: "3", wantJobDrop: 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := goldenSpec()
			spec.RunnerCPURequest = tc.request
			plan, err := Kubernetes{}.Render(spec)
			if err != nil {
				t.Fatalf("rendering runnerCPURequest %d: %v", tc.request, err)
			}
			runner, ok := containerNamed(plan.Job, runnerContainerName)
			if !ok {
				t.Fatalf("the Job renders no %q container", runnerContainerName)
			}
			requests, limits := runner.Resources.Requests, runner.Resources.Limits
			if requests.CPU != tc.wantRequests {
				t.Errorf("the runner requests %s CPUs, want %s", requests.CPU, tc.wantRequests)
			}
			if limits.CPU != "6" {
				t.Errorf("the runner's CPU limit is %s, want its whole share 6 unchanged", limits.CPU)
			}
			if requests.Memory != limits.Memory {
				t.Errorf("the runner's memory request %s differs from its limit %s; only the CPU request changes", requests.Memory, limits.Memory)
			}
			report, err := plan.Preflight.Read([]byte(runnerRequestListing))
			if err != nil {
				t.Fatalf("the preflight declined a node with room: %v", err)
			}
			wantJob := plainReport.Job.Sub(Amount{CPU: tc.wantJobDrop * cpuMilli})
			if report.Job != wantJob {
				t.Errorf("the preflight asks for %s with runnerCPURequest %d, want %s", report.Job, tc.request, wantJob)
			}
		})
	}
}

package sweeprunner

import "testing"

// TestK8sDindCPUMillisRequestsTheSidecar holds the dind sidecar's CPU request
// to the Spec's DindCPURequestMillis while its limit stays the whole share, and
// keeps the runner's share at the pod total less the sidecar's limit: a smaller
// sidecar request must not widen the runner's share (gt-w38cb5).
func TestK8sDindCPUMillisRequestsTheSidecar(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name          string
		requestMillis int
		wantRequest   string
	}{
		{name: "zero asks for the whole share", wantRequest: "3"},
		{name: "a millicore request below the share", requestMillis: 500, wantRequest: "500m"},
		{name: "a whole-core request", requestMillis: 2000, wantRequest: "2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			spec := goldenSpec()
			spec.CPUs, spec.MemoryGB, spec.Dind, spec.DindCPUs = 9, 24, true, 3
			spec.DindCPURequestMillis = tc.requestMillis
			plan, err := Kubernetes{}.Render(spec)
			if err != nil {
				t.Fatalf("rendering dindCPURequestMillis %d: %v", tc.requestMillis, err)
			}
			dind, ok := containerNamed(plan.Job, dindContainerName)
			if !ok {
				t.Fatalf("the Job renders no %q sidecar", dindContainerName)
			}
			if got := dind.Resources.Requests.CPU; got != tc.wantRequest {
				t.Errorf("the sidecar asks for %s CPUs, want %s", got, tc.wantRequest)
			}
			if got := dind.Resources.Limits.CPU; got != "3" {
				t.Errorf("the sidecar's CPU limit is %s, want its share 3 unchanged by the request", got)
			}
			if dind.Resources.Requests.Memory != dind.Resources.Limits.Memory {
				t.Errorf("the sidecar's memory request %s differs from its limit %s; only the CPU request changes",
					dind.Resources.Requests.Memory, dind.Resources.Limits.Memory)
			}
			runner, ok := containerNamed(plan.Job, runnerContainerName)
			if !ok {
				t.Fatalf("the Job renders no %q container", runnerContainerName)
			}
			if got := runner.Resources.Limits.CPU; got != "6" {
				t.Errorf("the runner's CPU limit is %s, want 6: the sidecar's request must not move the runner's share", got)
			}
			if got := runner.Resources.Requests.CPU; got != "6" {
				t.Errorf("the runner asks for %s CPUs, want its whole share 6", got)
			}
		})
	}
}

package daemon

import (
	"strings"
	"testing"
)

// TestDaemonCPURequest holds Values.CPURequest to the daemon container: a request
// under Values.CPU renders as requests.cpu with limits.cpu unchanged, so the
// container is Burstable; empty keeps requests equal limits, memory equal too.
func TestDaemonCPURequest(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, cpu, request, want string }{
		{"a request under the limit", "3", "1", "1"},
		{"empty asks the limit", "3", "", "3"},
		{"millicores", "3", "500m", "500m"},
		{"millicores equal to the limit", "1", "1000m", "1000m"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			v := goldenValues(t)
			v.CPU, v.CPURequest = tc.cpu, tc.request
			res := mustBuild(t, v).Deployment.Spec.Template.Spec.Containers[0].Resources
			if got := res.Requests["cpu"]; got != tc.want {
				t.Errorf("requests.cpu = %q, want %q", got, tc.want)
			}
			if got := res.Limits["cpu"]; got != tc.cpu {
				t.Errorf("limits.cpu = %q, want %q", got, tc.cpu)
			}
			if res.Requests["memory"] != res.Limits["memory"] {
				t.Errorf("memory request %q and limit %q differ", res.Requests["memory"], res.Limits["memory"])
			}
		})
	}
}

// TestDaemonCPURequestRefused holds the request to cpuRE and to Values.CPU: a
// value the grammar does not read, or one above the limit counted in millicores,
// is refused naming Values.CPURequest.
func TestDaemonCPURequestRefused(t *testing.T) {
	t.Parallel()
	for _, value := range []string{"4", "3500m", "0", "abc"} {
		t.Run(value, func(t *testing.T) {
			t.Parallel()
			v := goldenValues(t)
			v.CPU, v.CPURequest = "3", value
			if _, err := Build(v); err == nil || !strings.Contains(err.Error(), "Values.CPURequest") {
				t.Errorf("Build(CPURequest %q) error = %v, want a refusal naming Values.CPURequest", value, err)
			}
		})
	}
}

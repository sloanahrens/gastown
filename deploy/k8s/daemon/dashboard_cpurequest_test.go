package daemon

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// TestDashboardCPURequest holds Values.DashboardCPURequest to the dashboard container:
// a request renders as requests.cpu alone, so the container carries no limit and is
// Burstable.
func TestDashboardCPURequest(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, request, want string }{
		{"millicores", "100m", "100m"},
		{"whole cores", "2", "2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			v := dashboardValues(t)
			v.DashboardCPURequest = tc.request
			res := mustBuild(t, v).DashboardDeployment.Spec.Template.Spec.Containers[0].Resources
			if res == nil {
				t.Fatalf("the dashboard renders no resources, want requests.cpu %q", tc.want)
			}
			if got := res.Requests["cpu"]; got != tc.want {
				t.Errorf("requests.cpu = %q, want %q", got, tc.want)
			}
			if len(res.Limits) != 0 {
				t.Errorf("limits = %v, want none: the request sets requests alone", res.Limits)
			}
		})
	}
}

// TestDashboardCPURequestEmptyRendersNoResources holds the empty request to the
// dashboard as it rendered before the field existed: no resources block at all.
func TestDashboardCPURequestEmptyRendersNoResources(t *testing.T) {
	t.Parallel()
	v := dashboardValues(t)
	v.DashboardCPURequest = ""
	if res := mustBuild(t, v).DashboardDeployment.Spec.Template.Spec.Containers[0].Resources; res != nil {
		t.Errorf("the dashboard renders resources %+v, want none for an empty request", res)
	}
}

// TestDashboardCPURequestLeavesTheDaemonAlone holds the request to the dashboard
// container only: the daemon container renders the same with the request set as
// without it.
func TestDashboardCPURequestLeavesTheDaemonAlone(t *testing.T) {
	t.Parallel()
	with := dashboardValues(t)
	with.DashboardCPURequest = "100m"
	without := dashboardValues(t)
	got := mustBuild(t, with).Deployment.Spec.Template.Spec.Containers[0].Resources
	want := mustBuild(t, without).Deployment.Spec.Template.Spec.Containers[0].Resources
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the daemon container's resources changed with the dashboard request: %+v, want %+v", got, want)
	}
}

// TestDashboardCPURequestRefused holds the request to cpuRE, with or without the
// dashboard rendered: a value the grammar does not read is refused naming the field.
func TestDashboardCPURequestRefused(t *testing.T) {
	t.Parallel()
	for _, value := range []string{"abc", "0", "4.5", "500M"} {
		for _, dashboard := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s dashboard=%t", value, dashboard), func(t *testing.T) {
				t.Parallel()
				v := goldenValues(t)
				if dashboard {
					v = dashboardValues(t)
				}
				v.DashboardCPURequest = value
				if _, err := Build(v); err == nil || !strings.Contains(err.Error(), "Values.DashboardCPURequest") {
					t.Errorf("Build(DashboardCPURequest %q, Dashboard %t) error = %v, want a refusal naming Values.DashboardCPURequest", value, dashboard, err)
				}
			})
		}
	}
}

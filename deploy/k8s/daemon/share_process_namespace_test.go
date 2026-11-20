package daemon

import "testing"

// TestTickEffDaemonSharesProcessNamespace holds the daemon pod to the shared
// process namespace and every other rendered workload out of it: the daemon pod's
// pause container is then PID 1 and reaps the daemon's orphaned children, while no
// other pod in the render needs the daemon's process table (gt-cw9072).
func TestTickEffDaemonSharesProcessNamespace(t *testing.T) {
	t.Parallel()
	v := trackerBackupValues(t)
	v.Dashboard = true
	v.BdGatewayImage = testBdGatewayImage
	v.BdGatewayTokenSecret = testBdGatewayTokenSecret
	m := mustBuild(t, v)

	daemon := m.Deployment.Spec.Template.Spec
	if daemon.ShareProcessNamespace == nil || !*daemon.ShareProcessNamespace {
		t.Errorf("the daemon pod sets shareProcessNamespace to %v, want true", daemon.ShareProcessNamespace)
	}

	for _, tc := range []struct {
		what string
		pod  PodSpec
	}{
		{"the dashboard pod", m.DashboardDeployment.Spec.Template.Spec},
		{"the tracker pod", m.TrackerDeployment.Spec.Template.Spec},
		{"the tracker backup pod", m.TrackerBackupCronJob.Spec.JobTemplate.Spec.Template.Spec},
		{"the bd gateway pod", m.BdGatewayDeployment.Spec.Template.Spec},
	} {
		if tc.pod.ShareProcessNamespace != nil {
			t.Errorf("%s sets shareProcessNamespace to %v, want it unset", tc.what, *tc.pod.ShareProcessNamespace)
		}
	}
}

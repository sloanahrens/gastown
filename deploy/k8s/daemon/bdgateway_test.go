package daemon

import (
	"path/filepath"
	"strings"
	"testing"
)

// testBdGatewayURL is the gateway address the bd gateway goldens record.
const testBdGatewayURL = "http://host.docker.internal:7440"

// bdGatewayDashboardValues is dashboardValues with the gateway URL set.
func bdGatewayDashboardValues(t *testing.T) Values {
	t.Helper()
	v := dashboardValues(t)
	v.BdGatewayURL = testBdGatewayURL
	return v
}

// dashboardGatewayEnv returns the value of the named variable in the dashboard
// container, and whether the container sets it.
func dashboardGatewayEnv(t *testing.T, m Manifests, name string) (string, bool) {
	t.Helper()
	for _, e := range m.DashboardDeployment.Spec.Template.Spec.Containers[0].Env {
		if e.Name == name {
			return e.Value, true
		}
	}
	return "", false
}

// TestDashboardBdGateway holds the dashboard Deployment with the gateway set: the
// two environment names, and the token Secret mounted read-only as the directory
// the token file sits in.
func TestDashboardBdGateway(t *testing.T) {
	t.Parallel()
	m := mustBuild(t, bdGatewayDashboardValues(t))
	compareGolden(t, filepath.Join(packageDir(t), "testdata"), "deployment-dashboard-bdgateway.yaml", document(t, m.DashboardDeployment))

	if got, ok := dashboardGatewayEnv(t, m, BdGatewayURLEnv); !ok || got != testBdGatewayURL {
		t.Errorf("%s = %q (set %v), want %q", BdGatewayURLEnv, got, ok, testBdGatewayURL)
	}
	if got, ok := dashboardGatewayEnv(t, m, BdGatewayTokenFileEnv); !ok || got != BdGatewayTokenPath {
		t.Errorf("%s = %q (set %v), want %q", BdGatewayTokenFileEnv, got, ok, BdGatewayTokenPath)
	}
	var mounted bool
	for _, vm := range m.DashboardDeployment.Spec.Template.Spec.Containers[0].VolumeMounts {
		if vm.MountPath == BdGatewayTokenDir {
			mounted = vm.ReadOnly
		}
	}
	if !mounted {
		t.Errorf("the dashboard does not mount %s read-only", BdGatewayTokenDir)
	}
}

// TestDashboardBdGatewayOff holds the flag-absent render to the dashboard golden it
// had before the gateway: with BdGatewayURL unset the Deployment is byte-identical.
func TestDashboardBdGatewayOff(t *testing.T) {
	t.Parallel()
	v := dashboardValues(t)
	v.BdGatewayURL = ""
	m := mustBuild(t, v)
	compareGolden(t, filepath.Join(packageDir(t), "testdata"), "deployment-dashboard.yaml", document(t, m.DashboardDeployment))
	for _, name := range []string{BdGatewayURLEnv, BdGatewayTokenFileEnv} {
		if _, ok := dashboardGatewayEnv(t, m, name); ok {
			t.Errorf("the dashboard sets %s with the gateway unset", name)
		}
	}
}

// TestDashboardSecretIsItsOwn holds the Secret names apart: the dashboard mounts
// the read-class Secret, the daemon's Secret is never mounted into the dashboard,
// and the render creates no gateway Secret of its own.
func TestDashboardSecretIsItsOwn(t *testing.T) {
	t.Parallel()
	if BdGatewayDashboardSecretName == BdGatewayDaemonSecretName {
		t.Fatalf("the dashboard and the daemon name the same gateway Secret %q", BdGatewayDashboardSecretName)
	}
	m := mustBuild(t, bdGatewayDashboardValues(t))

	var dashboardSecrets []string
	for _, vol := range m.DashboardDeployment.Spec.Template.Spec.Volumes {
		if vol.Secret != nil {
			dashboardSecrets = append(dashboardSecrets, vol.Secret.SecretName)
		}
	}
	if !containsString(dashboardSecrets, SecretNamePrefix+ViewerTokenFile) {
		t.Fatalf("the dashboard's Secrets = %q: the viewer token is missing", dashboardSecrets)
	}
	if !containsString(dashboardSecrets, BdGatewayDashboardSecretName) {
		t.Errorf("the dashboard's Secrets = %q: the gateway's read-class Secret %q is missing", dashboardSecrets, BdGatewayDashboardSecretName)
	}
	if containsString(dashboardSecrets, BdGatewayDaemonSecretName) {
		t.Errorf("the dashboard mounts the daemon's Secret %q", BdGatewayDaemonSecretName)
	}
	for _, s := range m.Secrets {
		if s.Metadata.Name == BdGatewayDashboardSecretName || s.Metadata.Name == BdGatewayDaemonSecretName {
			t.Errorf("the render creates the gateway Secret %q, which the operator owns", s.Metadata.Name)
		}
	}
}

// TestBdGatewayRefusals drives each refusal of the gateway values: the URL with no
// dashboard to read it, and each malformed URL. A refused render builds nothing.
func TestBdGatewayRefusals(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		dashboard bool
		url       string
		// names is the field the refusal must name.
		names string
	}{
		{"the URL without the dashboard", false, testBdGatewayURL, "Values.Dashboard"},
		{"a scheme that is not http", true, "ftp://host.docker.internal:7440", "Values.BdGatewayURL"},
		{"no scheme", true, "host.docker.internal:7440", "Values.BdGatewayURL"},
		{"no host", true, "http:///v1", "Values.BdGatewayURL"},
		{"user information", true, "http://gateway:hunter2@host.docker.internal:7440", "Values.BdGatewayURL"},
		{"an unparsable URL", true, "http://[::1", "Values.BdGatewayURL"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			v := dashboardValues(t)
			v.Dashboard = tc.dashboard
			v.BdGatewayURL = tc.url
			m, err := Build(v)
			if err == nil {
				t.Fatalf("Build with %s succeeded, want a refusal", tc.name)
			}
			if !strings.Contains(err.Error(), tc.names) {
				t.Errorf("the refusal = %q, want it to name %s", err, tc.names)
			}
			if m.DashboardDeployment.Kind != "" {
				t.Errorf("a refused Build returned a dashboard Deployment")
			}
			if strings.Contains(err.Error(), "hunter2") {
				t.Errorf("the refusal echoes the credential: %v", err)
			}
		})
	}
}

// containsString reports whether want is one of got.
func containsString(got []string, want string) bool {
	for _, g := range got {
		if g == want {
			return true
		}
	}
	return false
}

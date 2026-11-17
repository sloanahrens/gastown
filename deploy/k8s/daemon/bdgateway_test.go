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

// TestDashboardBdGatewayEnvNoDuplicates holds the dashboard to one gateway pair
// however the two gateway flags combine. With both set, as the bd container
// cutover sets them, the container lists each name once and the pair holds the
// dashboard's own URL; with only the daemon's value set it names neither, since
// the daemon's token Secret is never mounted into the dashboard.
func TestDashboardBdGatewayEnvNoDuplicates(t *testing.T) {
	t.Parallel()
	const daemonURL = "http://host.docker.internal:7441"

	t.Run("both gateway values", func(t *testing.T) {
		t.Parallel()
		v := bdGatewayDashboardValues(t)
		v.DaemonBdGatewayURL = daemonURL
		c := mustBuild(t, v).DashboardDeployment.Spec.Template.Spec.Containers[0]
		for _, tc := range []struct{ name, want string }{
			{BdGatewayURLEnv, testBdGatewayURL},
			{BdGatewayTokenFileEnv, BdGatewayTokenPath},
		} {
			if got := envValuesNamed(c, tc.name); len(got) != 1 || got[0] != tc.want {
				t.Errorf("the dashboard's %s = %q, want exactly [%q]: the daemon's pair belongs to the daemon container", tc.name, got, tc.want)
			}
		}
	})

	t.Run("only the daemon's gateway", func(t *testing.T) {
		t.Parallel()
		v := dashboardValues(t)
		v.DaemonBdGatewayURL = daemonURL
		c := mustBuild(t, v).DashboardDeployment.Spec.Template.Spec.Containers[0]
		for _, name := range []string{BdGatewayURLEnv, BdGatewayTokenFileEnv} {
			if got := envValuesNamed(c, name); len(got) != 0 {
				t.Errorf("the dashboard names %s = %q with only the daemon's gateway set, want none", name, got)
			}
		}
	})
}

// renderedPods is every pod the render writes, in the order Manifests holds them:
// the daemon's Deployment, the dashboard's, the tracker's and the tracker backup's.
func renderedPods(m Manifests) []PodSpec {
	return []PodSpec{
		m.Deployment.Spec.Template.Spec,
		m.DashboardDeployment.Spec.Template.Spec,
		m.TrackerDeployment.Spec.Template.Spec,
		m.TrackerBackupCronJob.Spec.JobTemplate.Spec.Template.Spec,
	}
}

// envNamesNamedTwice returns one "container: name" line per environment variable
// a rendered container lists twice, so an empty result means no container names
// one twice. kubectl refuses such a pod: a patch fails its $setElementOrder check
// and an apply rejects the duplicate name, so a render that wrote one could not
// be applied.
func envNamesNamedTwice(m Manifests) []string {
	var out []string
	for _, pod := range renderedPods(m) {
		for _, containers := range [][]Container{pod.InitContainers, pod.Containers} {
			for _, c := range containers {
				seen := make(map[string]struct{}, len(c.Env))
				for _, e := range c.Env {
					if _, ok := seen[e.Name]; ok {
						out = append(out, c.Name+": "+e.Name)
					}
					seen[e.Name] = struct{}{}
				}
			}
		}
	}
	return out
}

// TestRenderedContainersHaveUniqueEnvNames renders every combination of the two
// gateway values and holds every rendered container's environment to unique
// names. The last case feeds a name appended twice to the same check, so the
// check cannot pass a duplicate unnoticed.
func TestRenderedContainersHaveUniqueEnvNames(t *testing.T) {
	t.Parallel()
	const daemonURL = "http://host.docker.internal:7441"

	for _, tc := range []struct {
		name         string
		dashboardURL string
		daemonURL    string
	}{
		{name: "neither gateway"},
		{name: "the dashboard's gateway only", dashboardURL: testBdGatewayURL},
		{name: "the daemon's gateway only", daemonURL: daemonURL},
		{name: "both gateways", dashboardURL: testBdGatewayURL, daemonURL: daemonURL},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			v := dashboardValues(t)
			v.BdGatewayURL = tc.dashboardURL
			v.DaemonBdGatewayURL = tc.daemonURL
			m := mustBuild(t, v)
			if dup := envNamesNamedTwice(m); len(dup) != 0 {
				t.Errorf("the render names environment variables twice: %q", dup)
			}
		})
	}

	// The shape the check exists to catch, fed to it directly: no Values renders a
	// duplicate, so the negative case cannot come through Build.
	dup := Manifests{}
	dup.DashboardDeployment.Spec.Template.Spec.Containers = []Container{{
		Name: DashboardName,
		Env: []EnvVar{
			{Name: BdGatewayURLEnv, Value: testBdGatewayURL},
			{Name: BdGatewayURLEnv, Value: testBdGatewayURL},
		},
	}}
	got := envNamesNamedTwice(dup)
	if len(got) != 1 || !strings.Contains(got[0], DashboardName) || !strings.Contains(got[0], BdGatewayURLEnv) {
		t.Fatalf("the check saw %q, want one line naming container %q and %s", got, DashboardName, BdGatewayURLEnv)
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

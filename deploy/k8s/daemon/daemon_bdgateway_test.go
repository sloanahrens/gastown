package daemon

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

// daemonBdGatewayValues is goldenValues with the daemon's gateway URL set.
func daemonBdGatewayValues(t *testing.T) Values {
	t.Helper()
	v := goldenValues(t)
	v.DaemonBdGatewayURL = testBdGatewayURL
	return v
}

// daemonGatewayEnv returns the value of the named variable in the daemon container,
// and whether the container sets it.
func daemonGatewayEnv(t *testing.T, m Manifests, name string) (string, bool) {
	t.Helper()
	for _, e := range m.Deployment.Spec.Template.Spec.Containers[0].Env {
		if e.Name == name {
			return e.Value, true
		}
	}
	return "", false
}

// TestDaemonBdGateway holds the daemon Deployment with the gateway set to its golden:
// the forwarder's two names, and the daemon-class token Secret mounted read-only as
// the directory its token file sits in. The render writes no Secret of that name.
func TestDaemonBdGateway(t *testing.T) {
	t.Parallel()
	m := mustBuild(t, daemonBdGatewayValues(t))
	compareGolden(t, filepath.Join(packageDir(t), "testdata"), "deployment-daemon-bdgateway.yaml", document(t, m.Deployment))

	if got, ok := daemonGatewayEnv(t, m, BdGatewayURLEnv); !ok || got != testBdGatewayURL {
		t.Errorf("%s = %q (set %v), want %q", BdGatewayURLEnv, got, ok, testBdGatewayURL)
	}
	if got, ok := daemonGatewayEnv(t, m, BdGatewayTokenFileEnv); !ok || got != BdGatewayTokenPath {
		t.Errorf("%s = %q (set %v), want %q", BdGatewayTokenFileEnv, got, ok, BdGatewayTokenPath)
	}

	pod := m.Deployment.Spec.Template.Spec
	var volume *Volume
	for i := range pod.Volumes {
		if pod.Volumes[i].Name == bdGatewayVolumeName {
			volume = &pod.Volumes[i]
		}
	}
	switch {
	case volume == nil || volume.Secret == nil:
		t.Fatalf("the daemon pod has no Secret volume %q: %+v", bdGatewayVolumeName, pod.Volumes)
	case volume.Secret.SecretName != BdGatewayDaemonSecretName:
		t.Errorf("the volume names Secret %q, want the daemon's %q", volume.Secret.SecretName, BdGatewayDaemonSecretName)
	case volume.Secret.DefaultMode == nil || *volume.Secret.DefaultMode != 0o400:
		t.Errorf("the volume's default mode is %v, want 0400: the token is readable by its owner only", volume.Secret.DefaultMode)
	}

	var mounted, readOnly bool
	for _, vm := range pod.Containers[0].VolumeMounts {
		if vm.MountPath == BdGatewayTokenDir {
			mounted = vm.Name == bdGatewayVolumeName
			readOnly = vm.ReadOnly
		}
	}
	if !mounted || !readOnly {
		t.Errorf("the daemon does not mount %s read-only from %s", BdGatewayTokenDir, bdGatewayVolumeName)
	}

	for _, s := range m.Secrets {
		if s.Metadata.Name == BdGatewayDaemonSecretName {
			t.Errorf("the render writes Secret %q; the operator creates it, so no manifest carries it", BdGatewayDaemonSecretName)
		}
	}
}

// TestDaemonBdGatewayIsNotTheDashboards holds the two gateway Secrets apart: the
// daemon mounts its own class's Secret and never the dashboard's read-class one.
func TestDaemonBdGatewayIsNotTheDashboards(t *testing.T) {
	t.Parallel()
	m := mustBuild(t, daemonBdGatewayValues(t))
	for _, vol := range m.Deployment.Spec.Template.Spec.Volumes {
		if vol.Secret != nil && vol.Secret.SecretName == BdGatewayDashboardSecretName {
			t.Errorf("the daemon mounts the dashboard's read-class Secret %q", BdGatewayDashboardSecretName)
		}
	}
	if BdGatewayDaemonSecretName == BdGatewayDashboardSecretName {
		t.Fatalf("the daemon and the dashboard name the same gateway Secret %q", BdGatewayDaemonSecretName)
	}
}

// TestDaemonBdGatewayOff holds the render with the flag absent to the daemon golden it
// has always had: no gateway variable, no token volume and no token mount.
func TestDaemonBdGatewayOff(t *testing.T) {
	t.Parallel()
	m := mustBuild(t, goldenValues(t))
	body := document(t, m.Deployment)
	compareGolden(t, filepath.Join(packageDir(t), "testdata"), "deployment.yaml", body)
	for _, name := range []string{BdGatewayURLEnv, BdGatewayTokenFileEnv, BdGatewayDaemonSecretName, bdGatewayVolumeName} {
		if bytes.Contains(body, []byte(name)) {
			t.Errorf("the flag-free daemon Deployment names %q", name)
		}
	}
}

// TestDaemonBdGatewayRefusesABadURL holds each refusal to a row that names the field
// it is about, and no refusal echoes a URL, since one that carries credentials would
// put them in the error. The good URLs are the rows that pass.
func TestDaemonBdGatewayRefusesABadURL(t *testing.T) {
	t.Parallel()
	const secret = "gateway-password-sentinel"
	tests := []struct {
		name    string
		mutate  func(*Values)
		refused bool
		names   string
	}{
		{name: "an http URL with a host", mutate: func(v *Values) { v.DaemonBdGatewayURL = testBdGatewayURL }},
		{name: "an https URL with a path", mutate: func(v *Values) { v.DaemonBdGatewayURL = "https://gateway.example:443/base" }},
		{name: "the highest port", mutate: func(v *Values) { v.DaemonBdGatewayURL = "http://gateway.example:65535" }},
		{name: "a port above the range", refused: true, names: "Values.DaemonBdGatewayURL", mutate: func(v *Values) {
			v.DaemonBdGatewayURL = "http://gateway.example:65536"
		}},
		{name: "port zero", refused: true, names: "Values.DaemonBdGatewayURL", mutate: func(v *Values) {
			v.DaemonBdGatewayURL = "http://gateway.example:0"
		}},
		{name: "a non-http scheme", refused: true, names: "Values.DaemonBdGatewayURL", mutate: func(v *Values) { v.DaemonBdGatewayURL = "ftp://host:7440" }},
		{name: "no scheme", refused: true, names: "Values.DaemonBdGatewayURL", mutate: func(v *Values) { v.DaemonBdGatewayURL = "host.docker.internal:7440" }},
		{name: "no host", refused: true, names: "Values.DaemonBdGatewayURL", mutate: func(v *Values) { v.DaemonBdGatewayURL = "http://:7440" }},
		{name: "credentials", refused: true, names: "Values.DaemonBdGatewayURL", mutate: func(v *Values) {
			v.DaemonBdGatewayURL = "http://gt:" + secret + "@host:7440"
		}},
		{name: "a query", refused: true, names: "Values.DaemonBdGatewayURL", mutate: func(v *Values) { v.DaemonBdGatewayURL = "http://host:7440/?x=1" }},
		{name: "a fragment", refused: true, names: "Values.DaemonBdGatewayURL", mutate: func(v *Values) { v.DaemonBdGatewayURL = "http://host:7440/#f" }},
		{name: "a rehearsal", refused: true, names: "Values.DaemonBdGatewayURL", mutate: func(v *Values) {
			*v = rehearsalValues(t)
			v.DaemonBdGatewayURL = testBdGatewayURL
		}},
		{name: "the pod's own URL variable in Env", refused: true, names: BdGatewayURLEnv, mutate: func(v *Values) {
			v.DaemonBdGatewayURL = testBdGatewayURL
			v.Env[BdGatewayURLEnv] = "http://other:7440"
		}},
		{name: "the token file's variable in Env", refused: true, names: BdGatewayTokenFileEnv, mutate: func(v *Values) {
			v.DaemonBdGatewayURL = testBdGatewayURL
			v.Env[BdGatewayTokenFileEnv] = "/run/other-token"
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			v := goldenValues(t)
			v.Env = map[string]string{awsRegionEnv: "us-east-1"}
			tc.mutate(&v)
			m, err := Build(v)
			if !tc.refused {
				if err != nil {
					t.Fatalf("Build refused a good gateway: %v", err)
				}
				if m.Deployment.Metadata.Name == "" {
					t.Fatal("Build returned no Deployment for a good gateway")
				}
				return
			}
			if err == nil {
				t.Fatal("Build accepted a bad gateway, want a refusal")
			}
			if !strings.Contains(err.Error(), tc.names) {
				t.Errorf("the refusal %q does not name %q", err, tc.names)
			}
			if strings.Contains(err.Error(), secret) {
				t.Errorf("the refusal %q echoes the credentials", err)
			}
		})
	}
}

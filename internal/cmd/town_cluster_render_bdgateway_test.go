package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	k8sdaemon "github.com/steveyegge/gastown/deploy/k8s/daemon"
)

// renderedDeployment decodes the rendered Deployment whose name ends with suffix.
func renderedDeployment(t *testing.T, out, suffix string) k8sdaemon.Deployment {
	t.Helper()
	var d k8sdaemon.Deployment
	if err := yaml.Unmarshal([]byte(renderedFile(t, out, suffix)), &d); err != nil {
		t.Fatalf("decoding the Deployment ending %q: %v", suffix, err)
	}
	return d
}

// renderedFile returns the bytes of the rendered file whose name ends with
// suffix, and fails the test when no file does.
func renderedFile(t *testing.T, out, suffix string) string {
	t.Helper()
	for _, name := range dirNames(t, out) {
		if !strings.HasSuffix(name, suffix) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(out, name))
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		return string(data)
	}
	t.Fatalf("no rendered file ends with %q in %s", suffix, out)
	return ""
}

// TestTownClusterRenderBdGatewayFlag holds --bd-gateway-url: with --dashboard the
// dashboard Deployment carries the gateway's URL and the token Secret the render
// names, and the daemon's manifests carry neither. Without the flag nothing names
// the gateway, and a flag that would be dropped, or a URL that is not one, is
// refused before a write.
func TestTownClusterRenderBdGatewayFlag(t *testing.T) {
	t.Parallel()

	t.Run("the dashboard carries the gateway and the daemon does not", func(t *testing.T) {
		f := clusterRenderFixture(t)
		f.dashboard = true
		f.bdGatewayURL = " http://host.docker.internal:7440 "
		var out bytes.Buffer
		if err := runClusterRender(clusterRenderTestDeps(t, &out), f); err != nil {
			t.Fatalf("render: %v", err)
		}
		pod := renderedDeployment(t, f.out, "-deployment-dashboard.yaml").Spec.Template.Spec
		env := map[string]string{}
		for _, e := range pod.Containers[0].Env {
			env[e.Name] = e.Value
		}
		if got := env["BD_GATEWAY_URL"]; got != "http://host.docker.internal:7440" {
			t.Errorf("BD_GATEWAY_URL = %q, want the trimmed flag value", got)
		}
		if got := env["BD_GATEWAY_TOKEN_FILE"]; got != k8sdaemon.BdGatewayTokenPath {
			t.Errorf("BD_GATEWAY_TOKEN_FILE = %q, want %q", got, k8sdaemon.BdGatewayTokenPath)
		}
		var secrets []string
		for _, vol := range pod.Volumes {
			if vol.Secret != nil {
				secrets = append(secrets, vol.Secret.SecretName)
			}
		}
		if !slices.Contains(secrets, k8sdaemon.BdGatewayDashboardSecretName) {
			t.Errorf("the dashboard's Secret volumes = %q, want %q among them", secrets, k8sdaemon.BdGatewayDashboardSecretName)
		}
		daemon := renderedFile(t, f.out, "-deployment-daemon.yaml")
		if strings.Contains(daemon, "bd-gateway") || strings.Contains(daemon, "BD_GATEWAY") {
			t.Errorf("the daemon Deployment names the gateway, which this flag does not render for it")
		}
	})

	t.Run("without the flag the dashboard names no gateway", func(t *testing.T) {
		f := clusterRenderFixture(t)
		f.dashboard = true
		var out bytes.Buffer
		if err := runClusterRender(clusterRenderTestDeps(t, &out), f); err != nil {
			t.Fatalf("render: %v", err)
		}
		if dashboard := renderedFile(t, f.out, "-deployment-dashboard.yaml"); strings.Contains(dashboard, "BD_GATEWAY") || strings.Contains(dashboard, "bd-gateway") {
			t.Errorf("the dashboard names the gateway with the flag absent")
		}
	})

	t.Run("the URL without the dashboard refuses before a write", func(t *testing.T) {
		f := clusterRenderFixture(t)
		f.bdGatewayURL = "http://host.docker.internal:7440"
		var out bytes.Buffer
		err := runClusterRender(clusterRenderTestDeps(t, &out), f)
		requireErrorNames(t, err, "--bd-gateway-url", "--dashboard")
		if got := outState(f.out); got != "absent" {
			t.Errorf("the refused render left %s: %s", f.out, got)
		}
	})

	t.Run("a whitespace-only URL refuses before a write", func(t *testing.T) {
		f := clusterRenderFixture(t)
		f.dashboard = true
		f.bdGatewayURL = "   "
		var out bytes.Buffer
		err := runClusterRender(clusterRenderTestDeps(t, &out), f)
		requireErrorNames(t, err, "--bd-gateway-url", "empty")
		if got := outState(f.out); got != "absent" {
			t.Errorf("the refused render left %s: %s", f.out, got)
		}
	})

	t.Run("a URL that is not http or https refuses before a write", func(t *testing.T) {
		f := clusterRenderFixture(t)
		f.dashboard = true
		f.bdGatewayURL = "ftp://host.docker.internal:7440"
		var out bytes.Buffer
		err := runClusterRender(clusterRenderTestDeps(t, &out), f)
		requireErrorNames(t, err, "BdGatewayURL")
		if got := outState(f.out); got != "absent" {
			t.Errorf("the refused render left %s: %s", f.out, got)
		}
	})
}

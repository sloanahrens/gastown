package cmd

import (
	"bytes"
	"slices"
	"strings"
	"testing"

	k8sdaemon "github.com/steveyegge/gastown/deploy/k8s/daemon"
	"github.com/steveyegge/gastown/internal/imagevalues"
)

// clusterRenderBdDigest is a bd gateway image by digest, as a values file names it
// once gt update images --bd has written it.
const clusterRenderBdDigest = "registry.example.test/gastown/gastown-bd@sha256:" +
	"0000000000000000000000000000000000000000000000000000000000000002"

// clusterRenderInClusterURL is the gateway's Service URL in the default namespace.
const clusterRenderInClusterURL = "http://bd-gateway.gt.svc:7440"

// clusterRenderInClusterPushURL is the push Service's URL in the default namespace.
const clusterRenderInClusterPushURL = "http://bd-gateway-push.gt.svc:7441"

// withBdImage rewrites the fixture's values file to name a bd image beside the
// daemon's, and sets the host address the gateway's Dolt host resolves through.
func withBdImage(t *testing.T, f clusterRenderFlags) clusterRenderFlags {
	t.Helper()
	if err := imagevalues.Write(imagevalues.PathIn(f.dir), imagevalues.Values{
		Daemon:  clusterRenderDaemonDigest,
		Polecat: clusterRenderDaemonDigest,
		Bd:      clusterRenderBdDigest,
		Commit:  strings.Repeat("a", 40),
	}); err != nil {
		t.Fatalf("writing the values file with a bd image: %v", err)
	}
	f.hostGateway = "192.168.64.1"
	// The gateway's tokens-0600 init container runs the proxy image, and a render
	// with a bd image and no proxy image is refused.
	f.proxyImage = clusterRenderDaemonDigest
	// The push URL the bd image renders sends the bearer over plain http to a
	// Service name the admission policy bounds; the operator's attestation is what
	// a live render passes once the policy is applied.
	f.bdGatewayServicePolicyApplied = true
	return f
}

// deploymentEnv is the environment of every container in the rendered Deployment
// whose name ends with suffix.
func deploymentEnv(t *testing.T, out, suffix string) map[string]string {
	t.Helper()
	env := map[string]string{}
	for _, c := range renderedDeployment(t, out, suffix).Spec.Template.Spec.Containers {
		for _, e := range c.Env {
			env[e.Name] = e.Value
		}
	}
	return env
}

// renderedBdGatewayObjects names the rendered files that carry the bd gateway:
// its Deployment, Service and workspace claim.
func renderedBdGatewayObjects(t *testing.T, out string) []string {
	t.Helper()
	var names []string
	for _, name := range dirNames(t, out) {
		if strings.Contains(name, "bd-gateway") {
			names = append(names, name)
		}
	}
	return names
}

// TestClusterRenderBdGatewayWorkload holds the render of a values file with a bd
// image: the gateway's Deployment, Service and claim are written, and the daemon,
// the dashboard and the update Job are given the in-cluster URL. The update Job is
// not rendered here; launchUpdateJob takes its URL from the daemon's BD_GATEWAY_URL,
// so the daemon's environment is what is checked for it.
func TestClusterRenderBdGatewayWorkload(t *testing.T) {
	t.Parallel()
	f := withBdImage(t, clusterRenderFixture(t))
	f.dashboard = true
	var out bytes.Buffer
	if err := runClusterRender(clusterRenderTestDeps(t, &out), f); err != nil {
		t.Fatalf("render: %v", err)
	}

	for _, suffix := range []string{
		"-deployment-bd-gateway.yaml",
		"-service-bd-gateway.yaml",
		"-persistentvolumeclaim-bd-gateway-workspace.yaml",
	} {
		renderedFile(t, f.out, suffix)
	}
	gateway := renderedDeployment(t, f.out, "-deployment-bd-gateway.yaml").Spec.Template.Spec
	if got := gateway.Containers[0].Image; got != clusterRenderBdDigest {
		t.Errorf("the gateway's image = %q, want the values file's bd digest %q", got, clusterRenderBdDigest)
	}
	var tokens []string
	for _, vol := range gateway.Volumes {
		if vol.Secret != nil {
			tokens = append(tokens, vol.Secret.SecretName)
		}
	}
	if !slices.Contains(tokens, defaultBdGatewayTokenSecret) {
		t.Errorf("the gateway's Secret volumes = %q, want %q among them", tokens, defaultBdGatewayTokenSecret)
	}

	daemon := deploymentEnv(t, f.out, "-deployment-daemon.yaml")
	if got := daemon[k8sdaemon.BdGatewayURLEnv]; got != clusterRenderInClusterURL {
		t.Errorf("the daemon's %s = %q, want the in-cluster %q", k8sdaemon.BdGatewayURLEnv, got, clusterRenderInClusterURL)
	}
	if got := daemon[k8sdaemon.BdGatewayTokenFileEnv]; got != k8sdaemon.BdGatewayTokenPath {
		t.Errorf("the daemon's %s = %q, want %q", k8sdaemon.BdGatewayTokenFileEnv, got, k8sdaemon.BdGatewayTokenPath)
	}
	dashboard := deploymentEnv(t, f.out, "-deployment-dashboard.yaml")
	if got := dashboard[k8sdaemon.BdGatewayURLEnv]; got != clusterRenderInClusterURL {
		t.Errorf("the dashboard's %s = %q, want the in-cluster %q", k8sdaemon.BdGatewayURLEnv, got, clusterRenderInClusterURL)
	}
	if !strings.Contains(out.String(), "Secret "+defaultBdGatewayTokenSecret) {
		t.Errorf("the output names no note for the gateway's token Secret:\n%s", out.String())
	}
}

// TestClusterRenderBdGatewayPushURL holds the render's push Service default: a
// values file with a bd image writes the push Service and points the daemon and,
// with --dashboard, the dashboard at it, and a render with no bd image does neither.
func TestClusterRenderBdGatewayPushURL(t *testing.T) {
	t.Parallel()
	t.Run("a bd image writes the push Service and names it", func(t *testing.T) {
		t.Parallel()
		f := withBdImage(t, clusterRenderFixture(t))
		f.dashboard = true
		var out bytes.Buffer
		if err := runClusterRender(clusterRenderTestDeps(t, &out), f); err != nil {
			t.Fatalf("render: %v", err)
		}
		renderedFile(t, f.out, "-service-bd-gateway-push.yaml")
		for _, tc := range []struct{ name, suffix string }{
			{"the daemon", "-deployment-daemon.yaml"},
			{"the dashboard", "-deployment-dashboard.yaml"},
		} {
			if got := deploymentEnv(t, f.out, tc.suffix)[k8sdaemon.BdGatewayPushURLEnv]; got != clusterRenderInClusterPushURL {
				t.Errorf("%s names %s = %q, want the in-cluster %q", tc.name, k8sdaemon.BdGatewayPushURLEnv, got, clusterRenderInClusterPushURL)
			}
		}
	})

	t.Run("a bd image without the attestation is refused", func(t *testing.T) {
		t.Parallel()
		f := withBdImage(t, clusterRenderFixture(t))
		f.dashboard = true
		// Clearing the flag is what a live render without
		// --bd-gateway-service-policy-applied carries: the push URL it would set
		// is refused, so the command's wiring is what admits it.
		f.bdGatewayServicePolicyApplied = false
		var out bytes.Buffer
		err := runClusterRender(clusterRenderTestDeps(t, &out), f)
		if err == nil {
			t.Fatal("the render accepted a bd image without the Service policy attestation, want a refusal")
		}
		if !strings.Contains(err.Error(), "Values.BdGatewayServicePolicyApplied") {
			t.Errorf("the refusal %q does not name Values.BdGatewayServicePolicyApplied", err)
		}
	})

	t.Run("no bd image names no push Service", func(t *testing.T) {
		t.Parallel()
		f := clusterRenderFixture(t)
		var out bytes.Buffer
		if err := runClusterRender(clusterRenderTestDeps(t, &out), f); err != nil {
			t.Fatalf("render: %v", err)
		}
		if objects := renderedBdGatewayObjects(t, f.out); len(objects) != 0 {
			t.Errorf("the render wrote gateway objects %q, want none", objects)
		}
		if _, ok := deploymentEnv(t, f.out, "-deployment-daemon.yaml")[k8sdaemon.BdGatewayPushURLEnv]; ok {
			t.Errorf("the daemon names %s with no bd image", k8sdaemon.BdGatewayPushURLEnv)
		}
	})
}

// TestClusterRenderBdGatewayAbsent holds the renders that carry no gateway: a values
// file with no bd image renders what it did before, and a rehearsal renders no
// gateway even with a bd image, since it mounts no token.
func TestClusterRenderBdGatewayAbsent(t *testing.T) {
	t.Parallel()
	const host = "http://host.docker.internal:7440"
	tests := []struct {
		name  string
		build func(t *testing.T) clusterRenderFlags
		check func(t *testing.T, out string)
	}{
		{
			name: "no bd image with the host URL flags keeps the host URLs",
			build: func(t *testing.T) clusterRenderFlags {
				f := clusterRenderFixture(t)
				f.dashboard = true
				f.bdGatewayURL = host
				f.daemonBdGatewayURL = host
				f.daemonBdGatewayURLSet = true
				return f
			},
			check: func(t *testing.T, out string) {
				if got := deploymentEnv(t, out, "-deployment-daemon.yaml")[k8sdaemon.BdGatewayURLEnv]; got != host {
					t.Errorf("the daemon's %s = %q, want the host URL %q", k8sdaemon.BdGatewayURLEnv, got, host)
				}
				if got := deploymentEnv(t, out, "-deployment-dashboard.yaml")[k8sdaemon.BdGatewayURLEnv]; got != host {
					t.Errorf("the dashboard's %s = %q, want the host URL %q", k8sdaemon.BdGatewayURLEnv, got, host)
				}
			},
		},
		{
			name: "no bd image and no URL flags names no gateway",
			build: func(t *testing.T) clusterRenderFlags {
				return clusterRenderFixture(t)
			},
			check: func(t *testing.T, out string) {
				if _, ok := deploymentEnv(t, out, "-deployment-daemon.yaml")[k8sdaemon.BdGatewayURLEnv]; ok {
					t.Errorf("the daemon names %s with no bd image and no flag", k8sdaemon.BdGatewayURLEnv)
				}
			},
		},
		{
			name: "a bd image in a rehearsal renders no gateway",
			build: func(t *testing.T) clusterRenderFlags {
				f := withBdImage(t, clusterRenderFixture(t))
				f.rehearsal = true
				f.namespace = "gt-rehearsal"
				f.forgeHost = ""
				return f
			},
			check: func(t *testing.T, out string) {
				if _, ok := deploymentEnv(t, out, "-deployment-daemon.yaml")[k8sdaemon.BdGatewayURLEnv]; ok {
					t.Errorf("the rehearsal daemon names %s, which a rehearsal does not render", k8sdaemon.BdGatewayURLEnv)
				}
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := tc.build(t)
			var out bytes.Buffer
			if err := runClusterRender(clusterRenderTestDeps(t, &out), f); err != nil {
				t.Fatalf("render: %v", err)
			}
			if objects := renderedBdGatewayObjects(t, f.out); len(objects) != 0 {
				t.Errorf("the render wrote gateway objects %q, want none", objects)
			}
			if strings.Contains(out.String(), "bd-gateway-tokens") {
				t.Errorf("the output names the gateway's token Secret, which this render does not use:\n%s", out.String())
			}
			tc.check(t, f.out)
		})
	}
}

// TestClusterRenderBdGatewayURLFlagWins holds that an explicit URL flag beats the
// in-cluster default for the URL it names, and the other URL keeps the default.
func TestClusterRenderBdGatewayURLFlagWins(t *testing.T) {
	t.Parallel()
	const daemonFlag = "http://daemon-gateway.example.test:7440"
	const dashboardFlag = "http://dashboard-gateway.example.test:7440"
	tests := []struct {
		name          string
		daemonURL     string
		dashboardURL  string
		wantDaemon    string
		wantDashboard string
	}{
		{name: "no URL flag takes the in-cluster default", wantDaemon: clusterRenderInClusterURL, wantDashboard: clusterRenderInClusterURL},
		{name: "the daemon flag wins for the daemon only", daemonURL: daemonFlag, wantDaemon: daemonFlag, wantDashboard: clusterRenderInClusterURL},
		{name: "the dashboard flag wins for the dashboard only", dashboardURL: dashboardFlag, wantDaemon: clusterRenderInClusterURL, wantDashboard: dashboardFlag},
		{name: "both flags win for their own pod", daemonURL: daemonFlag, dashboardURL: dashboardFlag, wantDaemon: daemonFlag, wantDashboard: dashboardFlag},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := withBdImage(t, clusterRenderFixture(t))
			f.dashboard = true
			f.bdGatewayURL = tc.dashboardURL
			f.daemonBdGatewayURL = tc.daemonURL
			f.daemonBdGatewayURLSet = tc.daemonURL != ""
			var out bytes.Buffer
			if err := runClusterRender(clusterRenderTestDeps(t, &out), f); err != nil {
				t.Fatalf("render: %v", err)
			}
			if got := deploymentEnv(t, f.out, "-deployment-daemon.yaml")[k8sdaemon.BdGatewayURLEnv]; got != tc.wantDaemon {
				t.Errorf("the daemon's %s = %q, want %q", k8sdaemon.BdGatewayURLEnv, got, tc.wantDaemon)
			}
			if got := deploymentEnv(t, f.out, "-deployment-dashboard.yaml")[k8sdaemon.BdGatewayURLEnv]; got != tc.wantDashboard {
				t.Errorf("the dashboard's %s = %q, want %q", k8sdaemon.BdGatewayURLEnv, got, tc.wantDashboard)
			}
		})
	}
}

// TestClusterRenderBdGatewayTokenSecret holds the gateway's token Secret name: the
// default is bd-gateway-tokens, a flag names another, and the gateway mounts the
// Secret the render was given.
func TestClusterRenderBdGatewayTokenSecret(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		secret string
		want   string
	}{
		{name: "the default names bd-gateway-tokens", want: "bd-gateway-tokens"},
		{name: "the flag names the Secret the gateway mounts", secret: " my-bd-tokens ", want: "my-bd-tokens"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := withBdImage(t, clusterRenderFixture(t))
			f.bdGatewayTokenSecret = tc.secret
			var out bytes.Buffer
			if err := runClusterRender(clusterRenderTestDeps(t, &out), f); err != nil {
				t.Fatalf("render: %v", err)
			}
			var tokens []string
			for _, vol := range renderedDeployment(t, f.out, "-deployment-bd-gateway.yaml").Spec.Template.Spec.Volumes {
				if vol.Secret != nil {
					tokens = append(tokens, vol.Secret.SecretName)
				}
			}
			if !slices.Contains(tokens, tc.want) {
				t.Errorf("the gateway's Secret volumes = %q, want %q among them", tokens, tc.want)
			}
			if !strings.Contains(out.String(), "Secret "+tc.want) {
				t.Errorf("the output names no note for Secret %s:\n%s", tc.want, out.String())
			}
		})
	}
}

// TestClusterRenderBdGatewayRefusals holds the gateway's refusals: a bd image with
// no host address, and a token Secret name that is not one. Each refusal writes
// nothing.
func TestClusterRenderBdGatewayRefusals(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		noHostGateway bool
		tokenSecret   string
		names         string
	}{
		{name: "no host gateway address", noHostGateway: true, names: "Values.HostGatewayIP"},
		{name: "a token Secret name that is not one", tokenSecret: "Bad_Name", names: "BdGatewayTokenSecret"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := withBdImage(t, clusterRenderFixture(t))
			if tc.noHostGateway {
				f.hostGateway = ""
			}
			f.bdGatewayTokenSecret = tc.tokenSecret
			var out bytes.Buffer
			err := runClusterRender(clusterRenderTestDeps(t, &out), f)
			requireErrorNames(t, err, tc.names)
			if got := outState(f.out); got != "absent" {
				t.Errorf("the refused render left %s: %s", f.out, got)
			}
		})
	}
}

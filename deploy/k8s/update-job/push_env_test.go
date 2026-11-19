package updatejob

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	k8sdaemon "github.com/steveyegge/gastown/deploy/k8s/daemon"
	"github.com/steveyegge/gastown/internal/templates"
)

// The fixture's images, by digest as deploy/values/images.yaml writes them.
const (
	pushTestDaemonImage  = "registry.example.com/gt/gastown-daemon@sha256:" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	pushTestProxyImage   = "registry.example.com/gt/socat@sha256:" + "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
	pushTestGatewayImage = "registry.example.com/gt/bdgw@sha256:" + "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
)

// pushTestEnvNames is the environment deploy/k8s/update-admission.yaml pins on every
// update Job container: the gateway-on six. The Job must not carry the push URL, so
// a seventh name is refused at apply.
var pushTestEnvNames = []string{
	"XDG_CONFIG_HOME",
	"GIT_CONFIG_COUNT",
	"GIT_CONFIG_KEY_0",
	"GIT_CONFIG_VALUE_0",
	"BD_GATEWAY_URL",
	"BD_GATEWAY_TOKEN_FILE",
}

// TestBdGatewayPushServiceUpdateJobEnv builds one fixture through the production
// Values and holds both of its renders: the daemon and dashboard Deployments, which
// must carry the push URL the fixture names (the first assertion, so the Job check
// cannot pass by the URL never being set), and the update Job, whose containers must
// carry exactly the six environment names the admission policy pins and no push URL.
func TestBdGatewayPushServiceUpdateJobEnv(t *testing.T) {
	t.Parallel()

	const (
		gatewayURL = "http://127.0.0.1:7440"
		pushURL    = "http://127.0.0.1:7441"
		tokenName  = "bd-gateway-tokens"
	)
	data, err := os.ReadFile(filepath.Join("..", "..", "daemon", "secrets.yaml"))
	if err != nil {
		t.Fatalf("reading the secrets manifest: %v", err)
	}
	manifest, err := templates.ParseSecretsManifest(data)
	if err != nil {
		t.Fatalf("parsing the secrets manifest: %v", err)
	}

	// The fixture: one Values with the gateway workload and both push URLs set, so
	// the daemon and the dashboard are the production render's, not an env slice
	// built here.
	m, err := k8sdaemon.Build(k8sdaemon.Values{
		Secrets:                 manifest.Files,
		Image:                   pushTestDaemonImage,
		ProxyImage:              pushTestProxyImage,
		CPU:                     "2",
		Memory:                  "4Gi",
		ConfirmContainerStopped: true,
		HostGatewayIP:           "192.168.5.2",
		Dashboard:               true,
		BdGatewayURL:            gatewayURL,
		BdGatewayPushURL:        pushURL,
		DaemonBdGatewayURL:      gatewayURL,
		DaemonBdGatewayPushURL:  pushURL,
		BdGatewayImage:          pushTestGatewayImage,
		BdGatewayTokenSecret:    tokenName,
		// The push URL renders only with the policy rendered or attested; this
		// fixture exercises the push render, not the refusal.
		BdGatewayServicePolicyApplied: true,
	})
	if err != nil {
		t.Fatalf("building the daemon manifests: %v", err)
	}

	// (a) The push URL really was set: both rendered Deployments carry it.
	for _, tc := range []struct {
		name      string
		container k8sdaemon.Container
	}{
		{"the daemon", m.Deployment.Spec.Template.Spec.Containers[0]},
		{"the dashboard", m.DashboardDeployment.Spec.Template.Spec.Containers[0]},
	} {
		var got []string
		for _, e := range tc.container.Env {
			if e.Name == k8sdaemon.BdGatewayPushURLEnv {
				got = append(got, e.Value)
			}
		}
		if !slices.Equal(got, []string{pushURL}) {
			t.Fatalf("%s names %s = %q, want exactly %q", tc.name, k8sdaemon.BdGatewayPushURLEnv, got, pushURL)
		}
	}
	if m.BdGatewayPushService.Metadata.Name != k8sdaemon.BdGatewayPushName {
		t.Fatalf("the render carries no %s Service: %q", k8sdaemon.BdGatewayPushName, m.BdGatewayPushService.Metadata.Name)
	}

	// (b) The same fixture's Job values: the daemon's gateway URL, and nothing that
	// names the push Service, which the admission policy would refuse.
	job, err := Build(Values{
		Image:        pushTestDaemonImage,
		GreenCommit:  strings.Repeat("ab", 20),
		MaxRuntime:   2 * time.Hour,
		BdGatewayURL: gatewayURL,
	})
	if err != nil {
		t.Fatalf("building the update Job: %v", err)
	}
	pod := job.Job.Spec.Template.Spec
	containers := append(slices.Clone(pod.InitContainers), pod.Containers...)
	if len(containers) != 2 {
		t.Fatalf("the Job carries %d containers, want the init container and the update container", len(containers))
	}
	for _, c := range containers {
		var names []string
		for _, e := range c.Env {
			names = append(names, e.Name)
			if e.Name == k8sdaemon.BdGatewayPushURLEnv {
				t.Errorf("the Job's %s container names %s = %q: the admission policy pins six names and refuses a seventh", c.Name, e.Name, e.Value)
			}
		}
		if got, want := sortedCopy(names), sortedCopy(pushTestEnvNames); !slices.Equal(got, want) {
			t.Errorf("the Job's %s container names %q, want exactly %q", c.Name, got, want)
		}
	}
}

// TestUpdateJobEnvPinNamesThePolicy holds the six names above to
// deploy/k8s/update-admission.yaml: each one is pinned there, so a policy edit that
// adds or drops a name fails here rather than silently disagreeing with the Job.
func TestUpdateJobEnvPinNamesThePolicy(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile(filepath.Join("..", "update-admission.yaml"))
	if err != nil {
		t.Fatalf("reading the update admission policy: %v", err)
	}
	policy := string(data)
	for _, name := range pushTestEnvNames {
		if !strings.Contains(policy, "'"+name+"'") {
			t.Errorf("the admission policy does not pin the name %q the Job carries", name)
		}
	}
	if !strings.Contains(policy, "size(c.env) == 6") {
		t.Errorf("the admission policy does not pin the Job's environment at six names")
	}
}

// sortedCopy returns s sorted, without changing it.
func sortedCopy(s []string) []string {
	out := slices.Clone(s)
	slices.Sort(out)
	return out
}

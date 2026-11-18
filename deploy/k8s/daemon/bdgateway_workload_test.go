package daemon

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

// testBdGatewayImage is the gateway image the workload goldens record, by digest
// as deploy/values/images.yaml writes one.
const testBdGatewayImage = "registry.example.com/gt/bdgw@sha256:" +
	"abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"

// testBdGatewayTokenSecret is the token Secret name the workload goldens record.
const testBdGatewayTokenSecret = "bd-gateway-tokens"

// bdGatewayWorkloadValues is goldenValues with the gateway image and token Secret set.
func bdGatewayWorkloadValues(t *testing.T) Values {
	t.Helper()
	v := goldenValues(t)
	v.BdGatewayImage = testBdGatewayImage
	v.BdGatewayTokenSecret = testBdGatewayTokenSecret
	return v
}

// TestBdGatewayWorkloadGolden holds the gateway's Deployment, Service and workspace
// claim to their goldens.
func TestBdGatewayWorkloadGolden(t *testing.T) {
	t.Parallel()
	m := mustBuild(t, bdGatewayWorkloadValues(t))
	dir := filepath.Join(packageDir(t), "testdata")
	compareGolden(t, dir, "deployment-bd-gateway.yaml", document(t, m.BdGatewayDeployment))
	compareGolden(t, dir, "service-bd-gateway.yaml", document(t, m.BdGatewayService))
	compareGolden(t, dir, "bd-gateway-workspace-claim.yaml", document(t, m.BdGatewayClaim))
}

// TestBdGatewayWorkloadRunsAsItsIdentity holds the gateway's identity, its
// privilege setting, its command and its token mount to the values the spec names:
// a pod that runs as root, or a command with a flag missing, is not the gateway.
func TestBdGatewayWorkloadRunsAsItsIdentity(t *testing.T) {
	t.Parallel()
	m := mustBuild(t, bdGatewayWorkloadValues(t))
	d := m.BdGatewayDeployment
	pod := d.Spec.Template.Spec
	c := pod.Containers[0]

	if pod.SecurityContext == nil || pod.SecurityContext.RunAsUser == nil || *pod.SecurityContext.RunAsUser != 65532 ||
		pod.SecurityContext.RunAsGroup == nil || *pod.SecurityContext.RunAsGroup != 65532 {
		t.Errorf("the gateway pod does not run as user and group 65532")
	}
	if c.SecurityContext == nil || c.SecurityContext.AllowPrivilegeEscalation == nil || *c.SecurityContext.AllowPrivilegeEscalation {
		t.Errorf("the gateway container does not set allowPrivilegeEscalation to false")
	}
	if d.Spec.Strategy.Type != "Recreate" {
		t.Errorf("the gateway strategy is %q, want Recreate", d.Spec.Strategy.Type)
	}
	if c.Image != testBdGatewayImage {
		t.Errorf("the gateway image is %q, want the digest %q", c.Image, testBdGatewayImage)
	}

	const wantCommand = "bdgw serve --tokens-dir /run/bdgw/tokens --verbs /etc/bdgw/verbs.json --log-dir /var/log/bdgw --workspace /var/lib/bdgw/ws --dolt-host host.docker.internal --dolt-port 3307"
	if got := strings.Join(c.Command, " "); got != wantCommand {
		t.Errorf("the gateway command is\n%s\nwant\n%s", got, wantCommand)
	}
	if c.ReadinessProbe == nil || c.ReadinessProbe.Exec == nil ||
		strings.Join(c.ReadinessProbe.Exec.Command, " ") != "bdgw ready --workspace /var/lib/bdgw/ws" {
		t.Errorf("the gateway readiness probe is %+v, want bdgw ready on the workspace", c.ReadinessProbe)
	}

	var tokens *Volume
	for i := range pod.Volumes {
		if pod.Volumes[i].Name == bdGatewayTokensVolume {
			tokens = &pod.Volumes[i]
		}
	}
	switch {
	case tokens == nil || tokens.Secret == nil:
		t.Fatalf("the gateway pod has no token Secret volume: %+v", pod.Volumes)
	case tokens.Secret.SecretName != testBdGatewayTokenSecret:
		t.Errorf("the token volume names Secret %q, want %q", tokens.Secret.SecretName, testBdGatewayTokenSecret)
	case tokens.Secret.DefaultMode == nil || *tokens.Secret.DefaultMode != 0o440:
		t.Errorf("the token volume's mode is not 0440, so the fsGroup cannot read it")
	}

	var mountedReadOnly bool
	for _, vm := range c.VolumeMounts {
		if vm.MountPath == bdGatewayTokensDir {
			mountedReadOnly = vm.Name == bdGatewayTokensVolume && vm.ReadOnly
		}
	}
	if !mountedReadOnly {
		t.Errorf("the gateway does not mount %s read-only from %s", bdGatewayTokensDir, bdGatewayTokensVolume)
	}
	if pod.SecurityContext == nil || pod.SecurityContext.FSGroup == nil || *pod.SecurityContext.FSGroup != 65532 {
		t.Errorf("the gateway pod has no fsGroup 65532, so its non-root process cannot read the token Secret")
	}
}

// TestBdGatewayWorkloadRefusals drives each refusal of the gateway workload values.
// Each refusal names the field it is about, and a refused Build returns no gateway.
func TestBdGatewayWorkloadRefusals(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(*Values)
		names  string
	}{
		{"a tag instead of a digest", func(v *Values) { v.BdGatewayImage = "registry.example.com/gt/bdgw:latest" }, "Values.BdGatewayImage"},
		{"a digest that is not 64 hex", func(v *Values) { v.BdGatewayImage = "registry.example.com/gt/bdgw@sha256:abc" }, "Values.BdGatewayImage"},
		{"an empty token Secret name with an image", func(v *Values) { v.BdGatewayTokenSecret = "" }, "Values.BdGatewayTokenSecret"},
		{"a token Secret name that is not a DNS-1123 subdomain", func(v *Values) { v.BdGatewayTokenSecret = "Bd_Gateway_Tokens" }, "Values.BdGatewayTokenSecret"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			v := bdGatewayWorkloadValues(t)
			tc.mutate(&v)
			m, err := Build(v)
			if err == nil {
				t.Fatalf("Build succeeded, want a refusal naming %s", tc.names)
			}
			if !strings.Contains(err.Error(), tc.names) {
				t.Errorf("the refusal = %q, want it to name %s", err, tc.names)
			}
			if m.BdGatewayDeployment.Kind != "" {
				t.Errorf("a refused Build returned a gateway Deployment")
			}
		})
	}
}

// TestBdGatewayWorkloadOff holds a Values with no gateway image to the render it
// had before the gateway: no gateway object, and no gateway name in the stream. A
// token Secret named without an image is not read, so it renders nothing either.
func TestBdGatewayWorkloadOff(t *testing.T) {
	t.Parallel()
	v := goldenValues(t)
	v.BdGatewayTokenSecret = testBdGatewayTokenSecret
	m := mustBuild(t, v)
	if m.BdGatewayDeployment.Kind != "" || m.BdGatewayService.Kind != "" || m.BdGatewayClaim.Kind != "" {
		t.Errorf("a Values with no gateway image rendered a gateway object")
	}
	var buf bytes.Buffer
	if err := Render(&buf, m); err != nil {
		t.Fatalf("Render: %v", err)
	}
	for _, name := range []string{BdGatewayName, BdGatewayWorkspaceClaim, bdGatewayTokensVolume} {
		if bytes.Contains(buf.Bytes(), []byte(name)) {
			t.Errorf("the render with no gateway image names %q", name)
		}
	}
}

// TestBdGatewayWorkloadRendered holds the stream to the gateway's objects: a
// Values with the image renders the Deployment, Service and claim exactly as their
// documents encode, so Render writes what Build built.
func TestBdGatewayWorkloadRendered(t *testing.T) {
	t.Parallel()
	m := mustBuild(t, bdGatewayWorkloadValues(t))
	var buf bytes.Buffer
	if err := Render(&buf, m); err != nil {
		t.Fatalf("Render: %v", err)
	}
	for name, doc := range map[string][]byte{
		"the Deployment": document(t, m.BdGatewayDeployment),
		"the Service":    document(t, m.BdGatewayService),
		"the claim":      document(t, m.BdGatewayClaim),
	} {
		if !bytes.Contains(buf.Bytes(), doc) {
			t.Errorf("Render does not write %s as Build built it", name)
		}
	}
}

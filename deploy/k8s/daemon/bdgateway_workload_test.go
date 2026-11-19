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
	if c.ReadinessProbe == nil || c.ReadinessProbe.Exec == nil {
		t.Errorf("the gateway has no readiness probe")
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

// TestBdGatewayWorkloadHostAlias holds the gateway's hostAliases to the daemon's:
// the gateway resolves its Dolt host, host.docker.internal, through the same
// address the live daemon's pod does, so a gateway with no entry cannot reach Dolt.
func TestBdGatewayWorkloadHostAlias(t *testing.T) {
	t.Parallel()
	v := bdGatewayWorkloadValues(t)
	m := mustBuild(t, v)
	got := m.BdGatewayDeployment.Spec.Template.Spec.HostAliases
	want := m.Deployment.Spec.Template.Spec.HostAliases
	if len(got) != 1 || got[0].IP != v.HostGatewayIP || len(got[0].Hostnames) != 1 || got[0].Hostnames[0] != DoltHostName {
		t.Fatalf("the gateway hostAliases = %+v, want one entry for %s at %s", got, DoltHostName, v.HostGatewayIP)
	}
	if len(want) != 1 || want[0].IP != got[0].IP || want[0].Hostnames[0] != got[0].Hostnames[0] {
		t.Errorf("the gateway hostAliases %+v differ from the daemon's %+v", got, want)
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
		{"an empty host gateway address with an image", func(v *Values) { v.HostGatewayIP = ""; v.ProxyImage = "" }, "Values.HostGatewayIP"},
		{"a rehearsal with an image", func(v *Values) {
			v.Rehearsal = true
			v.Namespace = "gt-rehearsal"
			v.ConfirmContainerStopped = false
			v.OMToken = false
		}, "Values.Rehearsal"},
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

// renderVolume returns the named volume of a pod, or nil when it has none.
func renderVolume(pod PodSpec, name string) *Volume {
	for i := range pod.Volumes {
		if pod.Volumes[i].Name == name {
			return &pod.Volumes[i]
		}
	}
	return nil
}

// flagValue returns the argument after flag in args, or "" when flag is absent.
func flagValue(args []string, flag string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag {
			return args[i+1]
		}
	}
	return ""
}

// TestBdGatewayRenderTokenCopy holds the token copy's command and mounts: the init
// container reads the Secret read-only, writes the emptyDir, and chmods the copies
// 0600 before the gateway starts.
func TestBdGatewayRenderTokenCopy(t *testing.T) {
	t.Parallel()
	m := mustBuild(t, bdGatewayWorkloadValues(t))
	pod := m.BdGatewayDeployment.Spec.Template.Spec
	if len(pod.InitContainers) != 1 || pod.InitContainers[0].Name != "tokens-0600" {
		t.Fatalf("the gateway init containers are %+v, want one named tokens-0600", pod.InitContainers)
	}
	init := pod.InitContainers[0]

	const wantCommand = "sh -c cp /secrets/* /tokens/ && chmod 0600 /tokens/*"
	if got := strings.Join(init.Command, " "); got != wantCommand {
		t.Errorf("the init command is\n%s\nwant\n%s", got, wantCommand)
	}

	var secretMount, copyMount *VolumeMount
	for i := range init.VolumeMounts {
		switch init.VolumeMounts[i].MountPath {
		case "/secrets":
			secretMount = &init.VolumeMounts[i]
		case "/tokens":
			copyMount = &init.VolumeMounts[i]
		}
	}
	if secretMount == nil || secretMount.Name != bdGatewayTokenSecretVolume || !secretMount.ReadOnly {
		t.Errorf("the init container does not mount the token Secret read-only at /secrets: %+v", init.VolumeMounts)
	}
	if copyMount == nil || copyMount.Name != bdGatewayTokensVolume || copyMount.ReadOnly {
		t.Errorf("the init container does not mount the emptyDir writable at /tokens: %+v", init.VolumeMounts)
	}
}

// TestBdGatewayRenderTokenVolumes holds the volumes the token copy reads and writes:
// the Secret at the group-readable mode, the emptyDir the copies land in, and the
// gateway mounting only that emptyDir, read-only, at its token directory.
func TestBdGatewayRenderTokenVolumes(t *testing.T) {
	t.Parallel()
	m := mustBuild(t, bdGatewayWorkloadValues(t))
	pod := m.BdGatewayDeployment.Spec.Template.Spec

	secret := renderVolume(pod, bdGatewayTokenSecretVolume)
	switch {
	case secret == nil || secret.Secret == nil:
		t.Fatalf("the gateway pod has no token Secret volume: %+v", pod.Volumes)
	case secret.Secret.SecretName != testBdGatewayTokenSecret:
		t.Errorf("the token Secret volume names %q, want %q", secret.Secret.SecretName, testBdGatewayTokenSecret)
	case secret.Secret.DefaultMode == nil || *secret.Secret.DefaultMode != 0o440:
		t.Errorf("the token Secret volume's mode is not 0440, so the init container's fsGroup cannot read it")
	}

	if tokens := renderVolume(pod, bdGatewayTokensVolume); tokens == nil || tokens.EmptyDir == nil {
		t.Errorf("the gateway pod has no emptyDir for the 0600 token copies: %+v", pod.Volumes)
	}

	gateway := pod.Containers[0]
	var mounts []VolumeMount
	for _, vm := range gateway.VolumeMounts {
		if vm.MountPath == bdGatewayTokensDir {
			mounts = append(mounts, vm)
		}
		if vm.Name == bdGatewayTokenSecretVolume {
			t.Errorf("the gateway container mounts the token Secret at %s; it must read only the copies", vm.MountPath)
		}
	}
	if len(mounts) != 1 || mounts[0].Name != bdGatewayTokensVolume || !mounts[0].ReadOnly {
		t.Errorf("the gateway mounts at %s are %+v, want the token emptyDir read-only", bdGatewayTokensDir, mounts)
	}
}

// TestBdGatewayRenderTokenInitImage holds the token copy's image to Values.ProxyImage,
// the image the daemon's relays run, and to no literal.
func TestBdGatewayRenderTokenInitImage(t *testing.T) {
	t.Parallel()
	v := bdGatewayWorkloadValues(t)
	m := mustBuild(t, v)
	init := m.BdGatewayDeployment.Spec.Template.Spec.InitContainers[0]
	if init.Image != v.ProxyImage {
		t.Errorf("the init container image is %q, want Values.ProxyImage %q", init.Image, v.ProxyImage)
	}
}

// TestBdGatewayRenderProxyImageRefusal holds a gateway render with no proxy image: the
// init container has no image to run, so Build refuses and returns no gateway.
func TestBdGatewayRenderProxyImageRefusal(t *testing.T) {
	t.Parallel()
	v := bdGatewayWorkloadValues(t)
	v.ProxyImage = ""
	m, err := Build(v)
	if err == nil || !strings.Contains(err.Error(), "Values.ProxyImage") {
		t.Fatalf("Build error = %v, want a refusal naming Values.ProxyImage", err)
	}
	if m.BdGatewayDeployment.Kind != "" {
		t.Errorf("a refused Build returned a gateway Deployment")
	}
}

// TestBdGatewayRenderMaxConcurrent holds --max-concurrent to the value: a positive cap
// is passed to bdgw serve, and zero leaves the flag off so the gateway's default
// applies.
func TestBdGatewayRenderMaxConcurrent(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		cap  int
		want string
	}{
		{"zero leaves the flag off", 0, ""},
		{"a positive cap is passed", 32, "32"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			v := bdGatewayWorkloadValues(t)
			v.BdGatewayMaxConcurrent = tc.cap
			m := mustBuild(t, v)
			command := m.BdGatewayDeployment.Spec.Template.Spec.Containers[0].Command
			if got := flagValue(command, "--max-concurrent"); got != tc.want {
				t.Errorf("--max-concurrent is %q, want %q; command %v", got, tc.want, command)
			}
			if tc.want == "" && strings.Contains(strings.Join(command, " "), "--max-concurrent") {
				t.Errorf("the flag is in the command with a cap of 0: %v", command)
			}
		})
	}
}

// TestBdGatewayRenderMaxConcurrentRefusal holds a negative cap to an error naming the
// field, with or without the gateway image: the value is refused, not ignored.
func TestBdGatewayRenderMaxConcurrentRefusal(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		image string
	}{
		{"with the gateway image", testBdGatewayImage},
		{"with no gateway image", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			v := bdGatewayWorkloadValues(t)
			v.BdGatewayImage = tc.image
			v.BdGatewayMaxConcurrent = -1
			_, err := Build(v)
			if err == nil || !strings.Contains(err.Error(), "Values.BdGatewayMaxConcurrent") {
				t.Errorf("Build error = %v, want a refusal naming Values.BdGatewayMaxConcurrent", err)
			}
		})
	}
}

// bdgwReadyRequires is the flag set cmd/bdgw/ready.go's require call names: bdgw
// ready exits 2 without any of them, so the probe must pass each one.
var bdgwReadyRequires = []string{"--tokens-dir", "--verbs", "--workspace"}

// TestBdGatewayRenderReadiness holds the readiness probe to what bdgw ready requires:
// --tokens-dir, --verbs and --workspace, each carrying the path the serve command
// names, so the probe checks the same tokens, verbs and workspace the gateway runs on.
func TestBdGatewayRenderReadiness(t *testing.T) {
	t.Parallel()
	m := mustBuild(t, bdGatewayWorkloadValues(t))
	c := m.BdGatewayDeployment.Spec.Template.Spec.Containers[0]
	if c.ReadinessProbe == nil || c.ReadinessProbe.Exec == nil {
		t.Fatalf("the gateway has no exec readiness probe")
	}
	probe := c.ReadinessProbe.Exec.Command

	const wantProbe = "bdgw ready --tokens-dir /run/bdgw/tokens --verbs /etc/bdgw/verbs.json --workspace /var/lib/bdgw/ws"
	if got := strings.Join(probe, " "); got != wantProbe {
		t.Errorf("the readiness probe is\n%s\nwant\n%s", got, wantProbe)
	}
	for _, flag := range bdgwReadyRequires {
		if got := flagValue(probe, flag); got == "" {
			t.Errorf("the readiness probe has no %s, which bdgw ready requires", flag)
		}
		if got, want := flagValue(probe, flag), flagValue(c.Command, flag); got != want {
			t.Errorf("the probe's %s is %q, the serve command's is %q", flag, got, want)
		}
	}
}

// TestBdGatewayProbeTimingGolden holds the gateway container's probes to the timing a
// roll needs: a readiness probe that starts at 2 s and polls every 3 s, and a startup
// probe that GETs /healthz on the gateway port, which holds the readiness probe back
// until the server answers. The fields are asserted by name, so a golden regenerated
// with -update cannot move them unnoticed.
func TestBdGatewayProbeTimingGolden(t *testing.T) {
	t.Parallel()
	m := mustBuild(t, bdGatewayWorkloadValues(t))
	dir := filepath.Join(packageDir(t), "testdata")
	compareGolden(t, dir, "deployment-bd-gateway.yaml", document(t, m.BdGatewayDeployment))

	c := m.BdGatewayDeployment.Spec.Template.Spec.Containers[0]
	if r := c.ReadinessProbe; r == nil || r.InitialDelaySeconds != 2 || r.PeriodSeconds != 3 || r.TimeoutSeconds != 5 || r.FailureThreshold != 3 {
		t.Errorf("the readiness probe is %+v, want initialDelaySeconds 2, periodSeconds 3, timeoutSeconds 5, failureThreshold 3", r)
	}
	s := c.StartupProbe
	if s == nil || s.HTTPGet == nil {
		t.Fatalf("the gateway has no startup HTTP GET probe")
	}
	if s.HTTPGet.Path != "/healthz" || s.HTTPGet.Port != BdGatewayPort || s.PeriodSeconds != 2 || s.FailureThreshold != 60 {
		t.Errorf("the startup probe is GET %s on %d every %ds with failureThreshold %d, want GET /healthz on %d every 2s with failureThreshold 60",
			s.HTTPGet.Path, s.HTTPGet.Port, s.PeriodSeconds, s.FailureThreshold, BdGatewayPort)
	}
}

// TestBdGatewayProbeTimingStrategy holds the gateway to one replica under Recreate: a
// second replica or a surge would put two gateways on the workspace claim at once.
func TestBdGatewayProbeTimingStrategy(t *testing.T) {
	t.Parallel()
	m := mustBuild(t, bdGatewayWorkloadValues(t))
	d := m.BdGatewayDeployment
	if d.Spec.Replicas != 1 {
		t.Errorf("the gateway has %d replicas, want 1", d.Spec.Replicas)
	}
	if d.Spec.Strategy.Type != "Recreate" {
		t.Errorf("the gateway strategy is %q, want Recreate", d.Spec.Strategy.Type)
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

package daemon

import (
	"bytes"
	"path/filepath"
	"slices"
	"strconv"
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
	if d.Spec.Strategy.Type != "RollingUpdate" {
		t.Errorf("the gateway strategy is %q, want RollingUpdate", d.Spec.Strategy.Type)
	}
	if c.Image != testBdGatewayImage {
		t.Errorf("the gateway image is %q, want the digest %q", c.Image, testBdGatewayImage)
	}

	const wantCommand = "bdgw serve --tokens-dir /run/bdgw/tokens --verbs /etc/bdgw/verbs.json --log-dir /var/log/bdgw --log-per-pod --workspace /var/lib/bdgw/ws --dolt-host host.docker.internal --dolt-port 3307 --push-addr :7441"
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
	if m.BdGatewayDeployment.Kind != "" || m.BdGatewayService.Kind != "" || m.BdGatewayPushService.Kind != "" || m.BdGatewayClaim.Kind != "" {
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

// TestBdGatewayReplicasRender holds Values.BdGatewayReplicas to the Deployment's
// replica count: two renders two pods, and zero and one render one, so the count a
// render keeps is one until an operator sets two.
func TestBdGatewayReplicasRender(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		replicas int
		want     int32
	}{
		{"zero renders one", 0, 1},
		{"one renders one", 1, 1},
		{"two renders two", 2, 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			v := bdGatewayWorkloadValues(t)
			v.BdGatewayReplicas = tc.replicas
			m := mustBuild(t, v)
			if got := m.BdGatewayDeployment.Spec.Replicas; got != tc.want {
				t.Errorf("the gateway has %d replicas, want %d", got, tc.want)
			}
		})
	}
}

// TestBdGatewayReplicasRefusal holds a count below zero or above two to an error
// naming the field, with or without the gateway image: the value is refused, not
// rendered as one pod.
func TestBdGatewayReplicasRefusal(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		image    string
		replicas int
	}{
		{"three with the gateway image", testBdGatewayImage, 3},
		{"three with no gateway image", "", 3},
		{"negative with the gateway image", testBdGatewayImage, -1},
		{"negative with no gateway image", "", -1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			v := bdGatewayWorkloadValues(t)
			v.BdGatewayImage = tc.image
			v.BdGatewayReplicas = tc.replicas
			_, err := Build(v)
			if err == nil || !strings.Contains(err.Error(), "Values.BdGatewayReplicas") {
				t.Errorf("Build error = %v, want a refusal naming Values.BdGatewayReplicas", err)
			}
		})
	}
}

// TestBdGatewayReplicasTwoGolden holds the Deployment of a two-replica render to its
// golden, so the replica count and the rest of the gateway's pod are reviewed together.
func TestBdGatewayReplicasTwoGolden(t *testing.T) {
	t.Parallel()
	v := bdGatewayWorkloadValues(t)
	v.BdGatewayReplicas = 2
	m := mustBuild(t, v)
	compareGolden(t, filepath.Join(packageDir(t), "testdata"), "deployment-bd-gateway-two-replicas.yaml", document(t, m.BdGatewayDeployment))
}

// bdgwReadyRequires is the flag set cmd/bdgw/ready.go's require call names: bdgw
// ready exits 2 without any of them, so the probe must pass each one.
var bdgwReadyRequires = []string{"--tokens-dir", "--verbs", "--workspace"}

// bdgwProbeEndpointFlags are the Dolt endpoint flags the readiness probe must carry.
// bdgw ready does not require them, but without them it probes dolt:3306 while the
// serve command dials the endpoint named here, so a gateway that cannot reach Dolt
// reads as ready.
var bdgwProbeEndpointFlags = []string{"--dolt-host", "--dolt-port"}

// TestBdGatewayRenderReadiness holds the readiness probe to what bdgw ready requires:
// --tokens-dir, --verbs and --workspace, each carrying the path the serve command
// names, and the Dolt endpoint flags, each carrying the serve command's value, so the
// probe checks the same tokens, verbs, workspace and Dolt the gateway runs on.
func TestBdGatewayRenderReadiness(t *testing.T) {
	t.Parallel()
	m := mustBuild(t, bdGatewayWorkloadValues(t))
	c := m.BdGatewayDeployment.Spec.Template.Spec.Containers[0]
	if c.ReadinessProbe == nil || c.ReadinessProbe.Exec == nil {
		t.Fatalf("the gateway has no exec readiness probe")
	}
	probe := c.ReadinessProbe.Exec.Command

	const wantProbe = "bdgw ready --tokens-dir /run/bdgw/tokens --verbs /etc/bdgw/verbs.json --workspace /var/lib/bdgw/ws --dolt-host host.docker.internal --dolt-port 3307"
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
	for _, flag := range bdgwProbeEndpointFlags {
		if got := flagValue(probe, flag); got == "" {
			t.Errorf("the readiness probe has no %s, so it checks the default Dolt endpoint", flag)
		}
		if got, want := flagValue(probe, flag), flagValue(c.Command, flag); got != want {
			t.Errorf("the probe's %s is %q, the serve command's is %q", flag, got, want)
		}
	}
}

// TestBdGatewayDoltEndpointSharedByProbeAndServe holds the probe and the serve command
// to one Dolt endpoint: the host and port bdGatewayContainer is given are the values
// both the serve command and the readiness probe carry, so a change to either moves
// both together. The values differ from the production endpoint, so a probe that
// hard-codes its flags, or a serve command that ignores its parameters, fails.
func TestBdGatewayDoltEndpointSharedByProbeAndServe(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		host string
		port int
	}{
		{"the production endpoint", DoltHostName, bdGatewayDoltPort},
		{"another host", "dolt.gateway.internal", bdGatewayDoltPort},
		{"another port", DoltHostName, 4406},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := bdGatewayContainer(bdGatewayWorkloadValues(t), tc.host, tc.port)
			probe := c.ReadinessProbe.Exec.Command
			for _, cmd := range [][]string{c.Command, probe} {
				if got := flagValue(cmd, "--dolt-host"); got != tc.host {
					t.Errorf("--dolt-host is %q in %v, want %q", got, cmd, tc.host)
				}
				if got, want := flagValue(cmd, "--dolt-port"), strconv.Itoa(tc.port); got != want {
					t.Errorf("--dolt-port is %q in %v, want %q", got, cmd, want)
				}
			}
		})
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

// TestBdGatewayProbeTimingStrategy holds the gateway to one replica under RollingUpdate:
// a second replica would put two gateways on the workspace claim outside a roll.
func TestBdGatewayProbeTimingStrategy(t *testing.T) {
	t.Parallel()
	m := mustBuild(t, bdGatewayWorkloadValues(t))
	d := m.BdGatewayDeployment
	if d.Spec.Replicas != 1 {
		t.Errorf("the gateway has %d replicas, want 1", d.Spec.Replicas)
	}
	if d.Spec.Strategy.Type != "RollingUpdate" {
		t.Errorf("the gateway strategy is %q, want RollingUpdate", d.Spec.Strategy.Type)
	}
}

// TestBdGatewayRollingStrategy pins the gateway's rolling update against the golden:
// one replica, maxUnavailable 0 and maxSurge 1, so the new gateway is up before the
// old one stops, and the per-pod call log flag in the command. The fields are asserted
// by name, so a golden regenerated with -update cannot move them unnoticed.
func TestBdGatewayRollingStrategy(t *testing.T) {
	t.Parallel()
	m := mustBuild(t, bdGatewayWorkloadValues(t))
	d := m.BdGatewayDeployment
	compareGolden(t, filepath.Join(packageDir(t), "testdata"), "deployment-bd-gateway.yaml", document(t, d))

	if d.Spec.Replicas != 1 {
		t.Errorf("the gateway has %d replicas, want 1", d.Spec.Replicas)
	}
	ru := d.Spec.Strategy.RollingUpdate
	if d.Spec.Strategy.Type != "RollingUpdate" || ru == nil || ru.MaxUnavailable == nil || ru.MaxSurge == nil {
		t.Fatalf("the gateway strategy is %+v, want RollingUpdate with maxUnavailable and maxSurge set", d.Spec.Strategy)
	}
	if *ru.MaxUnavailable != 0 || *ru.MaxSurge != 1 {
		t.Errorf("the gateway rolls with maxUnavailable %d and maxSurge %d, want 0 and 1", *ru.MaxUnavailable, *ru.MaxSurge)
	}
	containers := d.Spec.Template.Spec.Containers
	if len(containers) != 1 {
		t.Fatalf("the gateway has %d containers, want 1", len(containers))
	}
	if c := containers[0]; !slices.Contains(c.Command, "--log-per-pod") {
		t.Errorf("the gateway command %v does not carry --log-per-pod", c.Command)
	}
}

// TestBdGatewayRollingOthersRecreate holds the daemon and the dashboard to Recreate. The
// rolling update is the gateway's rule alone: the daemon and the dashboard share the
// town's volume, and a second daemon must never start beside the first.
func TestBdGatewayRollingOthersRecreate(t *testing.T) {
	t.Parallel()
	m := mustBuild(t, bdGatewayDashboardValues(t))
	for name, d := range map[string]Deployment{"the daemon": m.Deployment, "the dashboard": m.DashboardDeployment} {
		if d.Spec.Strategy.Type != "Recreate" || d.Spec.Strategy.RollingUpdate != nil {
			t.Errorf("%s strategy is %+v, want Recreate with no rollingUpdate", name, d.Spec.Strategy)
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
		"the Deployment":   document(t, m.BdGatewayDeployment),
		"the Service":      document(t, m.BdGatewayService),
		"the push Service": document(t, m.BdGatewayPushService),
		"the claim":        document(t, m.BdGatewayClaim),
	} {
		if !bytes.Contains(buf.Bytes(), doc) {
			t.Errorf("Render does not write %s as Build built it", name)
		}
	}
}

// TestBdGatewayLogClaimRender holds the call-log claim to the render: a Values with
// the gateway image writes bd-gateway-log, 512Mi, ReadWriteOnce, in the render's
// namespace and storage class, and a Values with no image writes no claim and
// names none.
func TestBdGatewayLogClaimRender(t *testing.T) {
	t.Parallel()
	m := mustBuild(t, bdGatewayWorkloadValues(t))
	c := m.BdGatewayLogClaim
	if c.Kind != "PersistentVolumeClaim" || c.Metadata.Name != BdGatewayLogClaim {
		t.Fatalf("the gateway log claim is a %q named %q, want a PersistentVolumeClaim named %q", c.Kind, c.Metadata.Name, BdGatewayLogClaim)
	}
	if c.Metadata.Namespace != m.Namespace.Metadata.Name {
		t.Errorf("the log claim's namespace = %q, want the render's %q", c.Metadata.Namespace, m.Namespace.Metadata.Name)
	}
	if got := c.Spec.Resources.Requests["storage"]; got != "512Mi" {
		t.Errorf("the log claim requests %q, want 512Mi", got)
	}
	if len(c.Spec.AccessModes) != 1 || c.Spec.AccessModes[0] != "ReadWriteOnce" {
		t.Errorf("the log claim's access modes = %q, want ReadWriteOnce", c.Spec.AccessModes)
	}
	if got := c.Spec.StorageClassName; got != m.TownStateClaim.Spec.StorageClassName {
		t.Errorf("the log claim's class = %q, want the render's %q", got, m.TownStateClaim.Spec.StorageClassName)
	}
	var buf bytes.Buffer
	if err := Render(&buf, m); err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !bytes.Contains(buf.Bytes(), document(t, c)) {
		t.Errorf("Render does not write the log claim as Build built it")
	}

	off := goldenValues(t)
	mOff := mustBuild(t, off)
	if mOff.BdGatewayLogClaim.Kind != "" {
		t.Errorf("a Values with no gateway image rendered the log claim")
	}
	buf.Reset()
	if err := Render(&buf, mOff); err != nil {
		t.Fatalf("Render: %v", err)
	}
	if bytes.Contains(buf.Bytes(), []byte(BdGatewayLogClaim)) {
		t.Errorf("the render with no gateway image names %q", BdGatewayLogClaim)
	}
}

// TestBdGatewayLogClaimGolden holds the workspace claim and the log claim, as the
// render writes them, to their golden stream: the log claim comes directly after
// the workspace claim, with the stream's separator between them.
func TestBdGatewayLogClaimGolden(t *testing.T) {
	t.Parallel()
	m := mustBuild(t, bdGatewayWorkloadValues(t))
	var buf bytes.Buffer
	if err := Render(&buf, m); err != nil {
		t.Fatalf("Render: %v", err)
	}
	workspace := document(t, m.BdGatewayClaim)
	logClaim := document(t, m.BdGatewayLogClaim)
	claims := append(append(append([]byte{}, workspace...), "---\n"...), logClaim...)
	if !bytes.Contains(buf.Bytes(), claims) {
		t.Fatalf("the render does not write the log claim directly after the workspace claim:\n%s", buf.Bytes())
	}
	compareGolden(t, filepath.Join(packageDir(t), "testdata"), "bd-gateway-claims.yaml", claims)
}

// TestBdGatewayLogVolumePinned holds the call log to the claim, so a pod
// replacement keeps it: the pod's bd-gateway-log volume is a
// PersistentVolumeClaim naming BdGatewayLogClaim, mounted at /var/log/bdgw, and no
// volume of that name is an emptyDir.
func TestBdGatewayLogVolumePinned(t *testing.T) {
	t.Parallel()
	pod := mustBuild(t, bdGatewayWorkloadValues(t)).BdGatewayDeployment.Spec.Template.Spec

	for i := range pod.Volumes {
		v := &pod.Volumes[i]
		if v.Name == bdGatewayLogVolume && v.EmptyDir != nil {
			t.Errorf("the %q volume is an emptyDir, want the %q claim", bdGatewayLogVolume, BdGatewayLogClaim)
		}
	}
	vol := renderVolume(pod, bdGatewayLogVolume)
	switch {
	case vol == nil:
		t.Fatalf("the gateway pod has no %q volume: %+v", bdGatewayLogVolume, pod.Volumes)
	case vol.PersistentVolumeClaim == nil:
		t.Fatalf("the %q volume is not a PersistentVolumeClaim: %+v", bdGatewayLogVolume, *vol)
	case vol.PersistentVolumeClaim.ClaimName != BdGatewayLogClaim:
		t.Errorf("the %q volume names claim %q, want %q", bdGatewayLogVolume, vol.PersistentVolumeClaim.ClaimName, BdGatewayLogClaim)
	}

	var mounted bool
	for _, m := range pod.Containers[0].VolumeMounts {
		if m.Name == bdGatewayLogVolume && m.MountPath == "/var/log/bdgw" {
			mounted = true
		}
	}
	if !mounted {
		t.Errorf("the gateway container does not mount %q at /var/log/bdgw: %+v", bdGatewayLogVolume, pod.Containers[0].VolumeMounts)
	}
}

// TestBdGatewayLogVolumeRest holds what the claim switch leaves alone: one replica
// under RollingUpdate, and the token copies still on an emptyDir.
func TestBdGatewayLogVolumeRest(t *testing.T) {
	t.Parallel()
	d := mustBuild(t, bdGatewayWorkloadValues(t)).BdGatewayDeployment
	if d.Spec.Replicas != 1 {
		t.Errorf("the gateway has %d replicas, want 1", d.Spec.Replicas)
	}
	if d.Spec.Strategy.Type != "RollingUpdate" {
		t.Errorf("the gateway strategy is %q, want RollingUpdate", d.Spec.Strategy.Type)
	}
	if vol := renderVolume(d.Spec.Template.Spec, bdGatewayTokensVolume); vol == nil || vol.EmptyDir == nil {
		t.Errorf("the token copies are not on an emptyDir: %+v", d.Spec.Template.Spec.Volumes)
	}
}

package daemon

import (
	"bytes"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// testBdGatewayDoltCredentialSecret is the Dolt credential Secret name the golden
// records.
const testBdGatewayDoltCredentialSecret = "bd-gateway-dolt-credentials"

// bdGatewayDoltCredentialValues is bdGatewayWorkloadValues with the Dolt credential
// Secret set.
func bdGatewayDoltCredentialValues(t *testing.T) Values {
	t.Helper()
	v := bdGatewayWorkloadValues(t)
	v.BdGatewayDoltCredentialSecret = testBdGatewayDoltCredentialSecret
	return v
}

// readinessCommand is the gateway's readiness exec command. It fails the test when
// the container has no exec probe, so a probe assertion cannot pass over an absent
// probe.
func readinessCommand(t *testing.T, pod PodSpec) []string {
	t.Helper()
	probe := pod.Containers[0].ReadinessProbe
	if probe == nil || probe.Exec == nil {
		t.Fatalf("the gateway has no readiness exec probe")
	}
	return probe.Exec.Command
}

// TestBdGatewayDoltCredentialOff holds an unset value to the golden the gateway had
// before the Dolt credential: a render with no credential Secret adds no volume and
// no flag.
func TestBdGatewayDoltCredentialOff(t *testing.T) {
	t.Parallel()
	m := mustBuild(t, bdGatewayWorkloadValues(t))
	compareGolden(t, filepath.Join(packageDir(t), "testdata"), "deployment-bd-gateway.yaml", document(t, m.BdGatewayDeployment))
	pod := m.BdGatewayDeployment.Spec.Template.Spec
	if vol := renderVolume(pod, bdGatewayDoltCredentialSecretVolume); vol != nil {
		t.Errorf("a render with no Dolt credential Secret carries the %q volume", bdGatewayDoltCredentialSecretVolume)
	}
	if cmd := pod.Containers[0].Command; slices.ContainsFunc(cmd, func(arg string) bool { return strings.HasPrefix(arg, "--dolt-credential-file") }) {
		t.Errorf("a render with no Dolt credential Secret passes %v", cmd)
	}
	if cmd := readinessCommand(t, pod); slices.ContainsFunc(cmd, func(arg string) bool { return strings.HasPrefix(arg, "--dolt-credential-file") }) {
		t.Errorf("the readiness probe of a render with no Dolt credential Secret passes %v", cmd)
	}
}

// TestBdGatewayDoltCredentialGolden holds the Deployment of a render with the
// credential Secret to its golden, so the Secret volume, the copy, the mount and
// the flag are reviewed together.
func TestBdGatewayDoltCredentialGolden(t *testing.T) {
	t.Parallel()
	m := mustBuild(t, bdGatewayDoltCredentialValues(t))
	compareGolden(t, filepath.Join(packageDir(t), "testdata"), "deployment-bd-gateway-dolt-credential.yaml", document(t, m.BdGatewayDeployment))
}

// TestBdGatewayDoltCredentialRender holds a set value to what the golden cannot
// state by name: the Secret volume carries only the credential key at the
// group-readable mode, the tokens-0600 init container copies it at 0600, the
// gateway mounts only the copy read-only at its credential directory, and the serve
// command and the readiness probe both name the copy's file.
func TestBdGatewayDoltCredentialRender(t *testing.T) {
	t.Parallel()
	m := mustBuild(t, bdGatewayDoltCredentialValues(t))
	pod := m.BdGatewayDeployment.Spec.Template.Spec

	secret := renderVolume(pod, bdGatewayDoltCredentialSecretVolume)
	switch {
	case secret == nil || secret.Secret == nil:
		t.Fatalf("the gateway pod has no Dolt credential Secret volume: %+v", pod.Volumes)
	case secret.Secret.SecretName != testBdGatewayDoltCredentialSecret:
		t.Errorf("the Dolt credential Secret volume names %q, want %q", secret.Secret.SecretName, testBdGatewayDoltCredentialSecret)
	case secret.Secret.DefaultMode == nil || *secret.Secret.DefaultMode != 0o440:
		t.Errorf("the Dolt credential Secret volume's mode is not 0440, so the init container's fsGroup cannot read it")
	case len(secret.Secret.Items) != 1 || secret.Secret.Items[0].Key != bdGatewayDoltCredentialKey || secret.Secret.Items[0].Path != bdGatewayDoltCredentialKey:
		t.Errorf("the Dolt credential Secret mounts %+v, want only the %q key at %q", secret.Secret.Items, bdGatewayDoltCredentialKey, bdGatewayDoltCredentialKey)
	}

	copied := renderVolume(pod, bdGatewayDoltCredentialVolume)
	if copied == nil || copied.EmptyDir == nil {
		t.Fatalf("the gateway pod has no emptyDir for the 0600 Dolt credential copy: %+v", pod.Volumes)
	}

	if len(pod.InitContainers) != 1 || pod.InitContainers[0].Name != bdGatewayTokenInit {
		t.Fatalf("the gateway init containers are %+v, want one named %s", pod.InitContainers, bdGatewayTokenInit)
	}
	init := pod.InitContainers[0]
	if cmd := strings.Join(init.Command, " "); !strings.Contains(cmd, "cp /dolt-secrets/* /dolt/ && chmod 0600 /dolt/*") {
		t.Errorf("the init command is\n%s\nwant it to copy the Dolt credential at 0600", cmd)
	}
	var secretMount, copyMount *VolumeMount
	for i := range init.VolumeMounts {
		switch init.VolumeMounts[i].Name {
		case bdGatewayDoltCredentialSecretVolume:
			secretMount = &init.VolumeMounts[i]
		case bdGatewayDoltCredentialVolume:
			copyMount = &init.VolumeMounts[i]
		}
	}
	if secretMount == nil || !secretMount.ReadOnly {
		t.Errorf("the init container does not mount the Dolt credential Secret read-only: %+v", init.VolumeMounts)
	}
	if copyMount == nil || copyMount.ReadOnly {
		t.Errorf("the init container does not mount the Dolt credential copy writable: %+v", init.VolumeMounts)
	}

	gateway := pod.Containers[0]
	var mount *VolumeMount
	for i := range gateway.VolumeMounts {
		if gateway.VolumeMounts[i].Name == bdGatewayDoltCredentialSecretVolume {
			t.Errorf("the gateway container mounts the Dolt credential Secret at %s; it must read only the 0600 copy", gateway.VolumeMounts[i].MountPath)
		}
		if gateway.VolumeMounts[i].Name == bdGatewayDoltCredentialVolume {
			mount = &gateway.VolumeMounts[i]
		}
	}
	if mount == nil || mount.MountPath != bdGatewayDoltCredentialDir || !mount.ReadOnly {
		t.Errorf("the gateway does not mount the 0600 copy read-only at %s: %+v", bdGatewayDoltCredentialDir, gateway.VolumeMounts)
	}
	wantFlag := "--dolt-credential-file=" + bdGatewayDoltCredentialPath
	if !slices.Contains(gateway.Command, wantFlag) {
		t.Errorf("the gateway command %v does not carry %s", gateway.Command, wantFlag)
	}
	if probe := readinessCommand(t, pod); !slices.Contains(probe, wantFlag) {
		t.Errorf("the readiness probe command %v does not carry %s, so the probe names a different Dolt credential setting than serve", probe, wantFlag)
	}

	var buf bytes.Buffer
	if err := Render(&buf, m); err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !bytes.Contains(buf.Bytes(), []byte("secretName: "+testBdGatewayDoltCredentialSecret)) {
		t.Errorf("the render does not name the Dolt credential Secret %q", testBdGatewayDoltCredentialSecret)
	}
	for _, s := range m.Secrets {
		if s.Metadata.Name == testBdGatewayDoltCredentialSecret {
			t.Errorf("the render creates the Dolt credential Secret; the operator does")
		}
	}
}

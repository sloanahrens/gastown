package daemon

import (
	"bytes"
	"errors"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/steveyegge/gastown/internal/manifestdata"
)

// rehearsalValues is the rehearsal the rehearsal goldens record: the golden
// configuration in a namespace of its own, without the live container's
// acknowledgement, which a rehearsal does not need.
func rehearsalValues(t *testing.T) Values {
	t.Helper()
	v := goldenValues(t)
	v.Rehearsal = true
	v.Namespace = "gt-rehearsal"
	v.ConfirmContainerStopped = false
	return v
}

// TestRehearsalRefusesTheLiveNamespace holds the namespace refusal: a rehearsal
// names a namespace of its own, and renders without the live pod's
// acknowledgement.
func TestRehearsalRefusesTheLiveNamespace(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		namespace string
	}{
		{"an empty namespace", ""},
		{"the live namespace", DefaultNamespace},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v := rehearsalValues(t)
			v.Namespace = tc.namespace
			_, err := Build(v)
			if err == nil {
				t.Fatal("Build = a render, want a refusal naming the live namespace")
			}
			if !strings.Contains(err.Error(), "Values.Namespace") || !strings.Contains(err.Error(), `"gt"`) {
				t.Errorf("Build refused with %q, want Values.Namespace and \"gt\" named", err)
			}
		})
	}

	if _, err := Build(rehearsalValues(t)); err != nil {
		t.Fatalf("Build of a rehearsal without ConfirmContainerStopped: %v", err)
	}
}

// TestRehearsalNeedsItsForgeTokenSecret holds the forge token a rehearsal mounts:
// the read-only viewer's Secret. The landing bot's Secret alone is refused, since
// the rehearsal never renders it.
func TestRehearsalNeedsItsForgeTokenSecret(t *testing.T) {
	t.Parallel()
	v := rehearsalValues(t)
	v.Secrets = withoutSecret(v.Secrets, RehearsalForgeTokenFile)
	_, err := Build(v)
	if err == nil {
		t.Fatal("Build of a rehearsal with a forge host and no viewer Secret = a render, want a refusal")
	}
	if !strings.Contains(err.Error(), RehearsalForgeTokenFile) {
		t.Errorf("Build refused with %q, want %s named", err, RehearsalForgeTokenFile)
	}
}

// TestRehearsalMountsNoPromoteKeyAndBindsNoSweepRole holds what a rehearsal
// renders: the om token, its meta file and the viewer's token mounted at the
// forge path, and no promote key, jev.env, landing token or sweep binding.
func TestRehearsalMountsNoPromoteKeyAndBindsNoSweepRole(t *testing.T) {
	t.Parallel()
	m := mustBuild(t, rehearsalValues(t))

	var secrets []string
	for _, s := range m.Secrets {
		secrets = append(secrets, s.Metadata.Annotations[SourceFileAnnotation])
	}
	wantSecrets := []string{RehearsalForgeTokenFile, OMTokenFile, OMTokenMetaFile}
	if !slices.Equal(secrets, wantSecrets) {
		t.Errorf("the rehearsal renders Secrets %q, want %q", secrets, wantSecrets)
	}

	pod := m.Deployment.Spec.Template.Spec
	for _, vol := range pod.Volumes {
		if vol.Secret != nil && refusedInRehearsal(strings.TrimPrefix(vol.Secret.SecretName, SecretNamePrefix)) {
			t.Errorf("the rehearsal's volume %s mounts the Secret %s", vol.Name, vol.Secret.SecretName)
		}
	}
	mounts := pod.Containers[0].VolumeMounts
	for _, mount := range mounts {
		if refusedInRehearsal(mount.SubPath) {
			t.Errorf("the rehearsal mounts %s at %s", mount.SubPath, mount.MountPath)
		}
	}

	forge := volumeMountAt(mounts, manifestdata.LiveForgeTokenPath)
	if forge == nil {
		t.Fatalf("the rehearsal mounts nothing at the forge token path %s", manifestdata.LiveForgeTokenPath)
	}
	if forge.SubPath != RehearsalForgeTokenFile {
		t.Errorf("the forge token path is filled by %q, want %q", forge.SubPath, RehearsalForgeTokenFile)
	}
	if got := volumeSecret(pod.Volumes, forge.Name); got != SecretNamePrefix+RehearsalForgeTokenFile {
		t.Errorf("the forge token volume %s is the Secret %q, want %q", forge.Name, got, SecretNamePrefix+RehearsalForgeTokenFile)
	}

	var out bytes.Buffer
	if err := Render(&out, m); err != nil {
		t.Fatalf("Render: %v", err)
	}
	var kinds []string
	dec := yaml.NewDecoder(&out)
	for {
		var obj struct {
			Kind string `yaml:"kind"`
		}
		err := dec.Decode(&obj)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("decoding the stream: %v", err)
		}
		kinds = append(kinds, obj.Kind)
	}
	// No binding document of any kind, not even an empty one.
	want := []string{"Namespace", "ServiceAccount", "PersistentVolumeClaim", "PersistentVolumeClaim", "Service", "Secret", "Secret", "Secret", "Deployment"}
	if !slices.Equal(kinds, want) {
		t.Errorf("the rehearsal renders the kinds %q, want %q: no RoleBinding or ClusterRoleBinding", kinds, want)
	}
}

// TestRehearsalDeploymentIsNotTheTownOwner holds the Deployment a rehearsal
// renders: it owns nothing, and it keeps the pod's replica count and strategy.
func TestRehearsalDeploymentIsNotTheTownOwner(t *testing.T) {
	t.Parallel()
	m := mustBuild(t, rehearsalValues(t))
	d := m.Deployment
	if got := d.Metadata.Annotations[TownOwnerAnnotation]; got != TownOwnerRehearsal {
		t.Errorf("the rehearsal's %s annotation = %q, want %q", TownOwnerAnnotation, got, TownOwnerRehearsal)
	}
	if d.Spec.Replicas != 1 {
		t.Errorf("the rehearsal runs %d replicas, want 1", d.Spec.Replicas)
	}
	if d.Spec.Strategy.Type != "Recreate" {
		t.Errorf("the rehearsal strategy = %q, want Recreate", d.Spec.Strategy.Type)
	}
	compareGolden(t, filepath.Join(packageDir(t), "testdata", "rehearsal"), "deployment.yaml", document(t, d))
}

// TestRehearsalGolden holds the rehearsal's other objects to their goldens: the
// namespace, the ServiceAccount, the claims, the Service and each Secret.
func TestRehearsalGolden(t *testing.T) {
	t.Parallel()
	m := mustBuild(t, rehearsalValues(t))
	dir := filepath.Join(packageDir(t), "testdata", "rehearsal")
	for _, doc := range []struct {
		file  string
		value any
	}{
		{"namespace.yaml", m.Namespace},
		{"service-account.yaml", m.ServiceAccount},
		{"town-state-claim.yaml", m.TownStateClaim},
		{"landing-work-claim.yaml", m.LandingWorkClaim},
		{"forgejo-service.yaml", m.ForgejoService},
	} {
		compareGolden(t, dir, doc.file, document(t, doc.value))
	}
	for _, s := range m.Secrets {
		compareGolden(t, dir, "secret-"+s.Metadata.Annotations[SourceFileAnnotation]+".yaml", document(t, s))
	}
}

// refusedInRehearsal reports whether a rehearsal may never carry the entry
// named file: a promote key, jev.env or the landing bot's token.
func refusedInRehearsal(file string) bool {
	promote := strings.HasPrefix(file, "promote-") && strings.HasSuffix(file, ".key")
	return promote || file == "jev.env" || file == ForgeTokenFile
}

// volumeMountAt returns the mount at path, or nil.
func volumeMountAt(mounts []VolumeMount, path string) *VolumeMount {
	for i := range mounts {
		if mounts[i].MountPath == path {
			return &mounts[i]
		}
	}
	return nil
}

// volumeSecret returns the Secret the volume named name carries, or "".
func volumeSecret(volumes []Volume, name string) string {
	for _, vol := range volumes {
		if vol.Name == name && vol.Secret != nil {
			return vol.Secret.SecretName
		}
	}
	return ""
}

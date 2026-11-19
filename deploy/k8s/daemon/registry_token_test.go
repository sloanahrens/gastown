package daemon

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/manifestdata"
	"github.com/steveyegge/gastown/internal/templates"
)

// registryTokenPath is where the daemon pod mounts the registry bot's token:
// the XDG config directory every Secrets entry lands in (gt-h53ixs).
const registryTokenPath = manifestdata.LiveConfigDirPath + "/" + RegistryTokenFile

// TestRegistryTokenMountPresent holds the live pod's registry token mount when
// the secrets manifest names the entry: the volume carries
// daemon-forgejo-registry.env, the mount fills
// /run/gt/xdg/gt/forgejo-registry.env from the entry's own subPath, read-only,
// and the volume's defaultMode is 0400 rather than the entry's 0600 maximum
// (gt-h53ixs).
func TestRegistryTokenMountPresent(t *testing.T) {
	t.Parallel()
	v := goldenValues(t)
	if !filesHold(v.Secrets, RegistryTokenFile) {
		t.Fatalf("Values.Secrets does not hold %s; the shipped manifest no longer names it, so this test would pass without exercising the mount", RegistryTokenFile)
	}
	m := mustBuild(t, v)
	pod := m.Deployment.Spec.Template.Spec

	mount := volumeMountAt(pod.Containers[0].VolumeMounts, registryTokenPath)
	if mount == nil {
		t.Fatalf("the daemon mounts nothing at %s", registryTokenPath)
	}
	if mount.SubPath != RegistryTokenFile {
		t.Errorf("the mount at %s has subPath %q, want %q", registryTokenPath, mount.SubPath, RegistryTokenFile)
	}
	if !mount.ReadOnly {
		t.Errorf("the mount at %s is writable, want ReadOnly", registryTokenPath)
	}

	vol := secretVolumeNamed(pod.Volumes, mount.Name)
	if vol == nil {
		t.Fatalf("the mount names the volume %q, which the pod does not render", mount.Name)
	}
	if got, want := vol.Secret.SecretName, SecretNamePrefix+RegistryTokenFile; got != want {
		t.Errorf("the volume %s carries the Secret %q, want %q", vol.Name, got, want)
	}
	if vol.Secret.DefaultMode == nil {
		t.Fatal("the registry token volume has no defaultMode")
	}
	if got := *vol.Secret.DefaultMode; got != 0o400 {
		t.Errorf("the registry token volume's defaultMode = %04o, want 0400: the token can write packages, so the pod reads it owner-only", got)
	}

	if !slices.Contains(secretNames(m.Secrets), SecretNamePrefix+RegistryTokenFile) {
		t.Errorf("the render writes no Secret %q for the volume to mount", SecretNamePrefix+RegistryTokenFile)
	}
}

// TestRegistryTokenMountAbsent holds the two renders that must not carry the
// registry token: one whose Values.Secrets does not name the entry, whose
// output is the render before this mount existed, and a rehearsal, which never
// carries a write-capable token.
func TestRegistryTokenMountAbsent(t *testing.T) {
	t.Parallel()

	t.Run("a render without the entry", func(t *testing.T) {
		t.Parallel()
		v := goldenValues(t)
		v.Secrets = withoutSecret(v.Secrets, RegistryTokenFile)
		m := mustBuild(t, v)
		assertNoRegistryToken(t, m)
	})

	t.Run("a rehearsal", func(t *testing.T) {
		t.Parallel()
		v := rehearsalValues(t)
		if !filesHold(v.Secrets, RegistryTokenFile) {
			t.Fatalf("Values.Secrets does not hold %s, so the rehearsal test would pass without exercising the drop", RegistryTokenFile)
		}
		m := mustBuild(t, v)
		assertNoRegistryToken(t, m)
	})
}

// TestRegistryTokenMountManifest holds deploy/daemon/secrets.yaml to the entry
// the pod mounts: forgejo-registry.env is a files entry, optional, mode 0600,
// with its consumer, and it is no longer recorded as unclaimed (gt-h53ixs).
func TestRegistryTokenMountManifest(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile(filepath.Join(packageDir(t), "..", "..", "daemon", "secrets.yaml"))
	if err != nil {
		t.Fatalf("reading the secrets manifest: %v", err)
	}
	manifest, err := templates.ParseSecretsManifest(data)
	if err != nil {
		t.Fatalf("parsing the secrets manifest: %v", err)
	}

	var entry *templates.SecretFile
	for i := range manifest.Files {
		if manifest.Files[i].Name == RegistryTokenFile {
			entry = &manifest.Files[i]
		}
	}
	if entry == nil {
		t.Fatalf("the secrets manifest does not list %s as a file the daemon reads", RegistryTokenFile)
	}
	if entry.Required {
		t.Error("forgejo-registry.env is required, but the daemon starts without the registry token and only the tag prune needs it")
	}
	if entry.Mode != "0600" {
		t.Errorf("forgejo-registry.env mode = %q, want 0600", entry.Mode)
	}
	if strings.TrimSpace(entry.Consumer) == "" {
		t.Error("forgejo-registry.env has no consumer, so nothing names the code that reads it")
	}
	for _, u := range manifest.Unclaimed {
		if u.Name == RegistryTokenFile {
			t.Errorf("forgejo-registry.env is still recorded as unclaimed, though the daemon mounts it")
		}
	}
}

// assertNoRegistryToken fails when m mounts or writes the registry bot's token.
func assertNoRegistryToken(t *testing.T, m Manifests) {
	t.Helper()
	pod := m.Deployment.Spec.Template.Spec
	if got := volumeSecret(pod.Volumes, secretVolumeName(RegistryTokenFile)); got != "" {
		t.Errorf("the pod carries the volume %s for the Secret %q, want no registry token volume", secretVolumeName(RegistryTokenFile), got)
	}
	for _, mount := range pod.Containers[0].VolumeMounts {
		if mount.MountPath == registryTokenPath || mount.SubPath == RegistryTokenFile {
			t.Errorf("the daemon mounts the registry token at %s (subPath %q)", mount.MountPath, mount.SubPath)
		}
	}
	if slices.Contains(secretNames(m.Secrets), SecretNamePrefix+RegistryTokenFile) {
		t.Errorf("the render writes the Secret %q, want none", SecretNamePrefix+RegistryTokenFile)
	}
}

// secretVolumeNamed returns the pod volume named name, or nil.
func secretVolumeNamed(volumes []Volume, name string) *Volume {
	for i := range volumes {
		if volumes[i].Name == name {
			return &volumes[i]
		}
	}
	return nil
}

// filesHold reports whether files names the secret file name.
func filesHold(files []templates.SecretFile, name string) bool {
	return slices.ContainsFunc(files, func(f templates.SecretFile) bool { return f.Name == name })
}

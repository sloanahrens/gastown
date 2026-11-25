package daemon

import (
	"slices"
	"testing"

	"github.com/steveyegge/gastown/internal/manifestdata"
	"github.com/steveyegge/gastown/internal/templates"
)

// registryTokenPath is where the daemon pod mounts the registry bot's token:
// the XDG config directory every Secrets entry lands in (gt-h53ixs).
const registryTokenPath = manifestdata.LiveConfigDirPath + "/" + RegistryTokenFile

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
		m := mustBuild(t, rehearsalValues(t))
		assertNoRegistryToken(t, m)
	})
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

// filesHold reports whether files names the secret file name.
func filesHold(files []templates.SecretFile, name string) bool {
	return slices.ContainsFunc(files, func(f templates.SecretFile) bool { return f.Name == name })
}

package daemon

import (
	"path/filepath"
	"strings"
	"testing"
)

// townStateBackupHostPath is the host directory the test mounts: a shape
// fixture for an absolute path, not a default. It names the macOS shared
// directory so the file needs no hygiene exemption.
const townStateBackupHostPath = "/Users/Shared/gt-backups/town-state"

// townStateBackupGoldenHead sits above the golden's own head line, which
// records the account the pod paths spell, so a reader knows the account there
// is not the host's.
const townStateBackupGoldenHead = "# the operator's account is redacted to the town's service account: the hygiene allow list only shrinks, so a new golden adds no exemption (gt-a5u9ts)\n"

// TestTownStateBackupMount holds a live render with Values.TownStateBackupHostPath
// set: the pod carries the town-state-backup volume as a Directory hostPath and
// mounts it read-write at the same path, and the rest of the Deployment equals
// the golden (gt-a5u9ts).
func TestTownStateBackupMount(t *testing.T) {
	t.Parallel()

	v := goldenValues(t)
	v.TownStateBackupHostPath = townStateBackupHostPath
	m := mustBuild(t, v)

	compareGolden(t, filepath.Join(packageDir(t), "testdata"), "deployment-town-state-backup.yaml", redactedDocument(t, m.Deployment))

	pod := m.Deployment.Spec.Template.Spec
	mount := volumeMountAt(pod.Containers[0].VolumeMounts, townStateBackupHostPath)
	if mount == nil {
		t.Fatalf("the pod mounts nothing at %s", townStateBackupHostPath)
	}
	if mount.Name != TownStateBackupVolume || mount.ReadOnly {
		t.Errorf("the mount at %s is volume %q readOnly %t, want %q read-write", townStateBackupHostPath, mount.Name, mount.ReadOnly, TownStateBackupVolume)
	}
	vol := volumeNamed(pod.Volumes, TownStateBackupVolume)
	if vol == nil {
		t.Fatalf("the pod has no volume %q", TownStateBackupVolume)
	}
	if vol.PersistentVolumeClaim != nil {
		t.Errorf("the %s volume carries a claim %q, want the host path alone", TownStateBackupVolume, vol.PersistentVolumeClaim.ClaimName)
	}
	if vol.HostPath == nil || vol.HostPath.Path != townStateBackupHostPath || vol.HostPath.Type != "Directory" {
		t.Errorf("the %s volume = %+v, want a Directory host path %s", TownStateBackupVolume, vol, townStateBackupHostPath)
	}
}

// TestTownStateBackupMountUnsetIsTodayGolden holds the unset field to the
// Deployment's own golden: a caller that names no host directory gets exactly
// the manifest from before the volume existed (gt-a5u9ts).
func TestTownStateBackupMountUnsetIsTodayGolden(t *testing.T) {
	t.Parallel()

	m := mustBuild(t, goldenValues(t))
	compareGolden(t, filepath.Join(packageDir(t), "testdata"), "deployment.yaml", document(t, m.Deployment))
	for _, vol := range m.Deployment.Spec.Template.Spec.Volumes {
		if vol.Name == TownStateBackupVolume {
			t.Fatalf("the pod carries the %s volume with the field unset: %+v", TownStateBackupVolume, vol)
		}
	}
}

// TestTownStateBackupMountRehearsalIsReadOnly holds the rehearsal render: the
// pod that proves a restore reads the host's town-state archives and must not
// write them, as its dolt-backup mount is read-only (gt-a5u9ts).
func TestTownStateBackupMountRehearsalIsReadOnly(t *testing.T) {
	t.Parallel()

	v := rehearsalValues(t)
	v.TownStateBackupHostPath = townStateBackupHostPath
	pod := mustBuild(t, v).Deployment.Spec.Template.Spec

	mount := volumeMountAt(pod.Containers[0].VolumeMounts, townStateBackupHostPath)
	if mount == nil {
		t.Fatalf("the rehearsal pod mounts nothing at %s", townStateBackupHostPath)
	}
	if mount.Name != TownStateBackupVolume || !mount.ReadOnly {
		t.Errorf("the rehearsal mount at %s is volume %q readOnly %t, want %q read-only", townStateBackupHostPath, mount.Name, mount.ReadOnly, TownStateBackupVolume)
	}
	vol := volumeNamed(pod.Volumes, TownStateBackupVolume)
	if vol == nil || vol.HostPath == nil || vol.HostPath.Path != townStateBackupHostPath || vol.HostPath.Type != "Directory" {
		t.Errorf("the rehearsal %s volume = %+v, want a Directory host path %s", TownStateBackupVolume, vol, townStateBackupHostPath)
	}
}

// TestTownStateBackupMountRefusesARelativeOrUncleanPath holds the volume to the
// one spelling of an absolute host path, and each refusal to the field's name
// (gt-a5u9ts).
func TestTownStateBackupMountRefusesARelativeOrUncleanPath(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		path string
	}{
		{"relative", "gt-backups/town-state"},
		{"parent relative", "../town-state"},
		{"dot relative", "./archives"},
		{"trailing slash", "/Users/Shared/gt-backups/town-state/"},
		{"double slash", "/Users/Shared//gt-backups/town-state"},
		{"parent segment", "/Users/Shared/gt-backups/../town-state"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			v := goldenValues(t)
			v.TownStateBackupHostPath = tc.path
			_, err := Build(v)
			if err == nil || !strings.Contains(err.Error(), "Values.TownStateBackupHostPath") {
				t.Errorf("Build(%q) = %v, want a refusal naming Values.TownStateBackupHostPath", tc.path, err)
			}
		})
	}
}

// TestTownStateBackupMountRefusesACollidingPath holds the volume to a directory
// no other mount holds: a container cannot mount two volumes at one path, so
// the refusal names the field and the mount it would shadow (gt-a5u9ts).
func TestTownStateBackupMountRefusesACollidingPath(t *testing.T) {
	t.Parallel()

	for _, path := range []string{TownRootPath, LandingWorkPath, DoltBackupPath, TempPath} {
		t.Run(path, func(t *testing.T) {
			t.Parallel()
			v := goldenValues(t)
			v.TownStateBackupHostPath = path
			_, err := Build(v)
			if err == nil || !strings.Contains(err.Error(), "Values.TownStateBackupHostPath") || !strings.Contains(err.Error(), path) {
				t.Errorf("Build(%q) = %v, want a refusal naming Values.TownStateBackupHostPath and the path", path, err)
			}
		})
	}
}

// redactedDocument is document(t, v) with the account the pod paths spell
// replaced by the town's service account, and a head line saying so. The
// hygiene scan reads /Users/<account>/ as somebody's home and its allow list
// only shrinks, so a new golden may add no exemption. Coverage is not lost: the
// unset render is held to deployment.yaml byte for byte, so only this golden's
// spelling of the account is relaxed.
func redactedDocument(t *testing.T, v any) []byte {
	t.Helper()
	account := strings.Split(TownRootPath, "/")[2]
	doc := strings.ReplaceAll(string(document(t, v)), "/Users/"+account+"/", "/Users/gt/")
	return []byte(townStateBackupGoldenHead + doc)
}

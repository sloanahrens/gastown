package daemon

import (
	"path/filepath"
	"strings"
	"testing"
)

// Fixtures naming the macOS shared directories the reporters read (no hygiene exemption needed).
const (
	cloudReportsHostPath  = "/Users/Shared/gt-cloud/reports"
	cloudReleasesHostPath = "/Users/Shared/gt-cloud/releases"
)

// TestCloudInputMountRendersReadOnly holds both cloud directories as one
// Directory hostPath volume each, mounted read-only at the same path in a
// rehearsal as in the live pod; unset, the Deployment is byte for byte today's
// (gt-9gcfj3).
func TestCloudInputMountRendersReadOnly(t *testing.T) {
	t.Parallel()

	for _, v := range []Values{goldenValues(t), rehearsalValues(t)} {
		v.CloudReportsHostPath, v.CloudReleasesHostPath = cloudReportsHostPath, cloudReleasesHostPath
		pod := mustBuild(t, v).Deployment.Spec.Template.Spec
		for _, in := range []struct{ volume, path string }{
			{CloudReportsVolume, cloudReportsHostPath},
			{CloudReleasesVolume, cloudReleasesHostPath},
		} {
			mount := volumeMountAt(pod.Containers[0].VolumeMounts, in.path)
			if mount == nil || mount.Name != in.volume || !mount.ReadOnly {
				t.Errorf("rehearsal %t: the mount at %s = %+v, want %s read-only", v.Rehearsal, in.path, mount, in.volume)
			}
			vol := volumeNamed(pod.Volumes, in.volume)
			if vol == nil || vol.HostPath == nil || vol.HostPath.Path != in.path || vol.HostPath.Type != "Directory" {
				t.Errorf("rehearsal %t: the %s volume = %+v, want a Directory host path %s", v.Rehearsal, in.volume, vol, in.path)
			}
		}
	}

	compareGolden(t, filepath.Join(packageDir(t), "testdata"), "deployment.yaml", document(t, mustBuild(t, goldenValues(t)).Deployment))
}

// TestCloudInputMountRefusesBadPaths holds each cloud directory to an absolute, clean path no other mount holds, naming the field it refuses (gt-9gcfj3).
func TestCloudInputMountRefusesBadPaths(t *testing.T) {
	t.Parallel()

	for _, field := range []string{"Values.CloudReportsHostPath", "Values.CloudReleasesHostPath"} {
		for _, tc := range []struct{ name, path string }{
			{"relative", "gt-cloud/reports"},
			{"unclean", "/Users/Shared/gt-cloud/../reports"},
			{"a claim's path", TownRootPath},
			{"the emptyDir's path", TempPath},
		} {
			t.Run(field+"/"+tc.name, func(t *testing.T) {
				t.Parallel()
				v := goldenValues(t)
				v.CloudReportsHostPath, v.CloudReleasesHostPath = cloudReportsHostPath, cloudReleasesHostPath
				if field == "Values.CloudReportsHostPath" {
					v.CloudReportsHostPath = tc.path
				} else {
					v.CloudReleasesHostPath = tc.path
				}
				if _, err := Build(v); err == nil || !strings.Contains(err.Error(), field) {
					t.Errorf("Build with %s = %q = %v, want a refusal naming %s", field, tc.path, err, field)
				}
			})
		}
	}

	// The container cannot mount two volumes at one path.
	v := goldenValues(t)
	v.CloudReportsHostPath, v.CloudReleasesHostPath = cloudReportsHostPath, cloudReportsHostPath
	if _, err := Build(v); err == nil || !strings.Contains(err.Error(), "Values.Cloud") {
		t.Errorf("Build with both cloud directories on one path = %v, want a refusal naming a cloud field", err)
	}
}

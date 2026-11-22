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

// TestCloudInputMountSetsTheEnv names each mounted cloud directory in the daemon
// container's environment at the path the pod mounts it, and names neither when
// no Values path mounts one; the dashboard names neither either (gt-9936jc).
func TestCloudInputMountSetsTheEnv(t *testing.T) {
	t.Parallel()

	for _, m := range []struct {
		name string
		v    Values
	}{{"golden", goldenValues(t)}, {"rehearsal", rehearsalValues(t)}} {
		unset := podEnvMap(mustBuild(t, m.v).Deployment.Spec.Template.Spec.Containers[0])
		set := m.v
		set.CloudReportsHostPath, set.CloudReleasesHostPath = cloudReportsHostPath, cloudReleasesHostPath
		setEnv := podEnvMap(mustBuild(t, set).Deployment.Spec.Template.Spec.Containers[0])
		for name, want := range map[string]string{
			cloudReportsEnv:  cloudReportsHostPath,
			cloudReleasesEnv: cloudReleasesHostPath,
		} {
			if got, ok := setEnv[name]; !ok || got != want {
				t.Errorf("%s: the daemon's %s = %q (present %t), want %q", m.name, name, got, ok, want)
			}
			if got, ok := unset[name]; ok {
				t.Errorf("%s: the daemon carries %s = %q with no Values path, want no such name", m.name, name, got)
			}
		}
	}

	// The dashboard mounts no cloud directory, so it names none of them.
	dv := dashboardValues(t)
	dv.CloudReportsHostPath, dv.CloudReleasesHostPath = cloudReportsHostPath, cloudReleasesHostPath
	dashboard := podEnvMap(mustBuild(t, dv).DashboardDeployment.Spec.Template.Spec.Containers[0])
	for _, name := range []string{cloudReportsEnv, cloudReleasesEnv} {
		if got, ok := dashboard[name]; ok {
			t.Errorf("the dashboard carries %s = %q, but it mounts no such directory", name, got)
		}
	}
}

// TestPodEnvCloudDropReasonsNameTheMount holds each cloud drop reason to its
// condition, naming the Values field whose mount makes the pod set the name (gt-9936jc).
func TestPodEnvCloudDropReasonsNameTheMount(t *testing.T) {
	t.Parallel()

	for name, field := range map[string]string{
		cloudReportsEnv:  "Values.CloudReportsHostPath",
		cloudReleasesEnv: "Values.CloudReleasesHostPath",
	} {
		if reason := composeOnlyEnv[name]; !strings.Contains(reason, field) || !strings.Contains(reason, "only when") {
			t.Errorf("composeOnlyEnv[%s] = %q, want a reason naming %s and 'only when'", name, reason, field)
		}
	}
}

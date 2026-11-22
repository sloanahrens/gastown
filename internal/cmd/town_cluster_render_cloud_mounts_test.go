package cmd

import (
	"bytes"
	"reflect"
	"slices"
	"testing"

	k8sdaemon "github.com/steveyegge/gastown/deploy/k8s/daemon"
)

// TestTownClusterRenderCloudInputMounts holds a render naming both cloud
// directories to the two read-only hostPath mounts and the two daemon
// environment names, and a render naming neither to the same Deployment with
// those entries taken out: the whole Deployment, so a stray empty volume, mount
// or variable fails it (gt-9n1jmp).
func TestTownClusterRenderCloudInputMounts(t *testing.T) {
	t.Parallel()
	inputs := []struct{ path, volume, env string }{
		{"/Users/Shared/gt-cloud/reports", k8sdaemon.CloudReportsVolume, "GT_CLOUD_REPORTS_DIR"},
		{"/Users/Shared/gt-cloud/releases", k8sdaemon.CloudReleasesVolume, "GT_CLOUD_RELEASES_DIR"},
	}
	render := func(reports, releases string) k8sdaemon.Deployment {
		t.Helper()
		f := clusterRenderFixture(t)
		f.cloudReportsHostPath, f.cloudReleasesHostPath = reports, releases
		var out bytes.Buffer
		if err := runClusterRender(clusterRenderTestDeps(t, &out), f); err != nil {
			t.Fatalf("render (%q, %q): %v", reports, releases, err)
		}
		return renderedDeployment(t, f.out, "-deployment-daemon.yaml")
	}
	with, plain := render(inputs[0].path, inputs[1].path), render("", "")
	pod := &with.Spec.Template.Spec
	if len(pod.Containers) == 0 {
		t.Fatalf("the flagged render has no container: %+v", with)
	}
	for _, in := range inputs {
		i := slices.IndexFunc(pod.Volumes, func(v k8sdaemon.Volume) bool { return v.Name == in.volume })
		if i < 0 || pod.Volumes[i].HostPath == nil || pod.Volumes[i].HostPath.Path != in.path || pod.Volumes[i].HostPath.Type != "Directory" {
			t.Errorf("volume %s = %+v, want a Directory hostPath %s", in.volume, pod.Volumes, in.path)
		}
		j := slices.IndexFunc(pod.Containers[0].VolumeMounts, func(m k8sdaemon.VolumeMount) bool { return m.MountPath == in.path })
		if j < 0 || pod.Containers[0].VolumeMounts[j].Name != in.volume || !pod.Containers[0].VolumeMounts[j].ReadOnly {
			t.Errorf("no read-only mount of %s at %s in %+v", in.volume, in.path, pod.Containers[0].VolumeMounts)
		}
		if k := slices.IndexFunc(pod.Containers[0].Env, func(e k8sdaemon.EnvVar) bool { return e.Name == in.env }); k < 0 || pod.Containers[0].Env[k].Value != in.path {
			t.Errorf("daemon env %s = %+v, want %q", in.env, pod.Containers[0].Env, in.path)
		}
		pod.Volumes = slices.DeleteFunc(pod.Volumes, func(v k8sdaemon.Volume) bool { return v.Name == in.volume })
		pod.Containers[0].VolumeMounts = slices.DeleteFunc(pod.Containers[0].VolumeMounts, func(m k8sdaemon.VolumeMount) bool { return m.MountPath == in.path })
		pod.Containers[0].Env = slices.DeleteFunc(pod.Containers[0].Env, func(e k8sdaemon.EnvVar) bool { return e.Name == in.env })
	}
	if !reflect.DeepEqual(with, plain) {
		t.Errorf("a render with neither flag differs from the flagged render minus its cloud entries:\nflagless: %+v\nflagged, cloud entries removed: %+v", plain, with)
	}
}

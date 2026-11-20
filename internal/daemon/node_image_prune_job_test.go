package daemon

import (
	"encoding/json"
	"slices"
	"testing"
)

// nodePruneJobDoc is the part of the node-prune Job document the tests read, by
// its wire names.
type nodePruneJobDoc struct {
	Metadata struct {
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
	} `json:"metadata"`
	Spec struct {
		BackoffLimit          *int `json:"backoffLimit"`
		ActiveDeadlineSeconds *int `json:"activeDeadlineSeconds"`
		TTLSeconds            *int `json:"ttlSecondsAfterFinished"`
		Template              struct {
			Spec struct {
				RestartPolicy   string `json:"restartPolicy"`
				SecurityContext struct {
					RunAsUser *int `json:"runAsUser"`
				} `json:"securityContext"`
				Containers []struct {
					Command      []string `json:"command"`
					VolumeMounts []struct {
						Name      string `json:"name"`
						MountPath string `json:"mountPath"`
						ReadOnly  bool   `json:"readOnly"`
					} `json:"volumeMounts"`
				} `json:"containers"`
				Volumes []struct {
					Name     string `json:"name"`
					HostPath struct {
						Path string `json:"path"`
						Type string `json:"type"`
					} `json:"hostPath"`
				} `json:"volumes"`
			} `json:"spec"`
		} `json:"template"`
	} `json:"spec"`
}

// nodePruneFlagValues returns the value after each occurrence of flag in argv.
func nodePruneFlagValues(argv []string, flag string) []string {
	var values []string
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == flag {
			values = append(values, argv[i+1])
		}
	}
	return values
}

func TestNodeImagePruneManifestArgs(t *testing.T) {
	t.Parallel()
	spares := []string{"sha256:aaa", "sha256:bbb"}

	dry, err := nodeImagePruneManifest("prune", "image:1", 3, false, spares)
	if err != nil {
		t.Fatalf("a dry run manifest: %v", err)
	}
	var dryDoc nodePruneJobDoc
	if err := json.Unmarshal(dry, &dryDoc); err != nil {
		t.Fatalf("the dry run manifest is not JSON: %v", err)
	}
	dryArgv := dryDoc.Spec.Template.Spec.Containers[0].Command
	if slices.Contains(dryArgv, "--apply") {
		t.Errorf("a dry run holds --apply: %v", dryArgv)
	}
	if got := nodePruneFlagValues(dryArgv, "--spare"); !slices.Equal(got, spares) {
		t.Errorf("the --spare values are %v, want each spare as its own argument %v", got, spares)
	}
	if got := nodePruneFlagValues(dryArgv, "--keep"); !slices.Equal(got, []string{"3"}) {
		t.Errorf("the --keep values are %v, want [3]", got)
	}

	applied, err := nodeImagePruneManifest("prune", "image:1", 3, true, spares)
	if err != nil {
		t.Fatalf("an apply manifest: %v", err)
	}
	var appliedDoc nodePruneJobDoc
	if err := json.Unmarshal(applied, &appliedDoc); err != nil {
		t.Fatalf("the apply manifest is not JSON: %v", err)
	}
	appliedArgv := appliedDoc.Spec.Template.Spec.Containers[0].Command
	n := 0
	for _, a := range appliedArgv {
		if a == "--apply" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("an apply manifest holds --apply %d times, want once: %v", n, appliedArgv)
	}
}

func TestNodeImagePruneManifestShape(t *testing.T) {
	t.Parallel()
	manifest, err := nodeImagePruneManifest("prune", "image:1", 3, true, nil)
	if err != nil {
		t.Fatalf("the manifest: %v", err)
	}
	var doc nodePruneJobDoc
	if err := json.Unmarshal(manifest, &doc); err != nil {
		t.Fatalf("the manifest is not JSON: %v", err)
	}
	if doc.Metadata.Namespace != "gt" {
		t.Errorf("namespace is %q, want gt", doc.Metadata.Namespace)
	}
	if doc.Spec.BackoffLimit == nil || *doc.Spec.BackoffLimit != 0 {
		t.Errorf("backoffLimit is %v, want 0", doc.Spec.BackoffLimit)
	}
	if doc.Spec.ActiveDeadlineSeconds == nil || *doc.Spec.ActiveDeadlineSeconds != 900 {
		t.Errorf("activeDeadlineSeconds is %v, want 900", doc.Spec.ActiveDeadlineSeconds)
	}
	if doc.Spec.TTLSeconds == nil || *doc.Spec.TTLSeconds != 900 {
		t.Errorf("ttlSecondsAfterFinished is %v, want 900", doc.Spec.TTLSeconds)
	}
	pod := doc.Spec.Template.Spec
	if pod.RestartPolicy != "Never" {
		t.Errorf("restartPolicy is %q, want Never", pod.RestartPolicy)
	}
	if pod.SecurityContext.RunAsUser == nil || *pod.SecurityContext.RunAsUser != 0 {
		t.Errorf("runAsUser is %v, want 0", pod.SecurityContext.RunAsUser)
	}

	wantVolumes := map[string]struct{ path, typ string }{
		"containerd-sock": {"/run/containerd/containerd.sock", "Socket"},
		"k3s":             {"/usr/local/bin/k3s", "File"},
	}
	if len(pod.Volumes) != len(wantVolumes) {
		t.Fatalf("the pod has %d volumes, want %d", len(pod.Volumes), len(wantVolumes))
	}
	for _, v := range pod.Volumes {
		want, ok := wantVolumes[v.Name]
		if !ok || v.HostPath.Path != want.path || v.HostPath.Type != want.typ {
			t.Errorf("volume %q is hostPath %q type %q, want a hostPath of the measured node path", v.Name, v.HostPath.Path, v.HostPath.Type)
		}
	}

	if len(pod.Containers) != 1 {
		t.Fatalf("the pod has %d containers, want 1", len(pod.Containers))
	}
	mounts := pod.Containers[0].VolumeMounts
	if len(mounts) != 2 {
		t.Fatalf("the container has %d mounts, want 2", len(mounts))
	}
	for _, m := range mounts {
		switch m.Name {
		case "containerd-sock":
			if m.MountPath != "/run/containerd/containerd.sock" {
				t.Errorf("the socket is mounted at %q, want the same path as the node", m.MountPath)
			}
		case "k3s":
			if m.MountPath != "/host/k3s" || !m.ReadOnly {
				t.Errorf("the k3s binary is mounted at %q read-only %v, want /host/k3s read-only", m.MountPath, m.ReadOnly)
			}
		default:
			t.Errorf("unexpected mount %q", m.Name)
		}
	}
}

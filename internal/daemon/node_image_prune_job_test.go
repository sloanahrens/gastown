package daemon

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/polecat/cachevol"
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
					Image        string   `json:"image"`
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
	spares := []string{"sha256:" + strings.Repeat("a", 64), "sha256:" + strings.Repeat("b", 64)}
	const (
		name  = "prune-17"
		image = "registry.gt/node-prune:2c1d"
	)

	dry, err := nodeImagePruneManifest(name, image, 3, false, spares)
	if err != nil {
		t.Fatalf("a dry run manifest: %v", err)
	}
	var dryDoc nodePruneJobDoc
	if err := json.Unmarshal(dry, &dryDoc); err != nil {
		t.Fatalf("the dry run manifest is not JSON: %v", err)
	}
	if dryDoc.Metadata.Name != name {
		t.Errorf("the Job name is %q, want the name it was given %q", dryDoc.Metadata.Name, name)
	}
	if got := dryDoc.Spec.Template.Spec.Containers[0].Image; got != image {
		t.Errorf("the container image is %q, want the image it was given %q", got, image)
	}
	dryArgv := dryDoc.Spec.Template.Spec.Containers[0].Command
	if len(dryArgv) < 2 || dryArgv[0] != cachevol.HelperPath || dryArgv[1] != "node-prune" {
		t.Fatalf("the command starts %v, want exactly the helper %q and node-prune as its first two elements", dryArgv, cachevol.HelperPath)
	}
	for i, a := range dryArgv {
		if a == "sh" || a == "-c" || strings.HasSuffix(a, "sh") {
			t.Errorf("the command element %d is %q, want no shell anywhere in the command", i, a)
		}
	}
	for flag, want := range map[string]string{
		"--crictl-bin": "/host/k3s",
		"--crictl-arg": "crictl",
		"--endpoint":   "unix:///run/containerd/containerd.sock",
	} {
		if got := nodePruneFlagValues(dryArgv, flag); !slices.Equal(got, []string{want}) {
			t.Errorf("the %s values are %v, want [%s]", flag, got, want)
		}
	}
	if slices.Contains(dryArgv, "--apply") {
		t.Errorf("a dry run holds --apply: %v", dryArgv)
	}
	if got := nodePruneFlagValues(dryArgv, "--spare"); !slices.Equal(got, spares) {
		t.Errorf("the --spare values are %v, want each spare as its own argument %v", got, spares)
	}
	if got := nodePruneFlagValues(dryArgv, "--keep"); !slices.Equal(got, []string{"3"}) {
		t.Errorf("the --keep values are %v, want [3]", got)
	}

	applied, err := nodeImagePruneManifest(name, image, 3, true, spares)
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

func TestNodeImagePruneManifestRefuses(t *testing.T) {
	t.Parallel()
	validSpare := "sha256:" + strings.Repeat("a", 64)
	tests := []struct {
		name    string
		podName string
		image   string
		keep    int
		spare   []string
		wantArg string
	}{
		{name: "a valid call builds", podName: "prune", image: "image:1", keep: 3, spare: []string{validSpare}},
		{name: "an empty name", podName: "", image: "image:1", keep: 3, wantArg: "name"},
		{name: "an empty image", podName: "prune", image: "", keep: 3, wantArg: "image"},
		{name: "keep 0", podName: "prune", image: "image:1", keep: 0, wantArg: "keep"},
		{name: "keep -1", podName: "prune", image: "image:1", keep: -1, wantArg: "keep"},
		{name: "an empty spare", podName: "prune", image: "image:1", keep: 3, spare: []string{""}, wantArg: "spare"},
		{name: "a spare with no hex digits", podName: "prune", image: "image:1", keep: 3, spare: []string{"sha256:aaa"}, wantArg: "spare"},
		{name: "a spare with 63 hex digits", podName: "prune", image: "image:1", keep: 3, spare: []string{"sha256:" + strings.Repeat("a", 63)}, wantArg: "spare"},
		{name: "a spare with upper-case hex", podName: "prune", image: "image:1", keep: 3, spare: []string{"sha256:" + strings.Repeat("A", 64)}, wantArg: "spare"},
		{name: "a spare with one extra character after 64 hex digits", podName: "prune", image: "image:1", keep: 3, spare: []string{validSpare + "0"}, wantArg: "spare"},
		{name: "a spare starting with a dash", podName: "prune", image: "image:1", keep: 3, spare: []string{"-" + validSpare}, wantArg: "spare"},
		{name: "a spare that is a flag", podName: "prune", image: "image:1", keep: 3, spare: []string{"--apply"}, wantArg: "spare"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			manifest, err := nodeImagePruneManifest(tc.podName, tc.image, tc.keep, false, tc.spare)
			if tc.wantArg == "" {
				if err != nil {
					t.Fatalf("a valid call is refused: %v", err)
				}
				if len(manifest) == 0 {
					t.Fatalf("a valid call builds no manifest")
				}
				return
			}
			if err == nil {
				t.Fatalf("the call is accepted, want an error naming %q", tc.wantArg)
			}
			if !strings.Contains(err.Error(), tc.wantArg) {
				t.Errorf("the error %q does not name the argument %q", err, tc.wantArg)
			}
		})
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

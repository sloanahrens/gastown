package k8s

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// The control cluster's config is the contract the runbook starts from: two
// workers in the pools the manifests select on, and the runner pool tainted so
// nothing else lands on it.
func TestControlKindConfig(t *testing.T) {
	t.Parallel()
	cfg := decodeDocs(t, "control/kind-config.yaml")[0]
	nodes, ok := cfg["nodes"].([]any)
	if !ok || len(nodes) != 3 {
		t.Fatalf("nodes = %v, want a control-plane and two workers", cfg["nodes"])
	}
	images := map[string]bool{}
	pools := map[string]bool{}
	var tainted string
	for _, n := range nodes {
		m, _ := n.(map[string]any)
		labels, _ := m["labels"].(map[string]any)
		pool, _ := labels["gt.dev/pool"].(string)
		img, _ := m["image"].(string)
		if !strings.HasPrefix(img, "kindest/node:v") || !strings.Contains(img, "@sha256:") {
			t.Errorf("node %v image = %q, want kindest/node:v<version>@sha256:<digest>", m["role"], img)
		}
		images[img] = true
		if m["role"] == "worker" {
			pools[pool] = true
		}
		if patches, ok := m["kubeadmConfigPatches"].([]any); ok && len(patches) > 0 {
			text, _ := patches[0].(string)
			if strings.Contains(text, "key: gt.dev/pool") && strings.Contains(text, "value: privileged") && strings.Contains(text, "effect: NoSchedule\n") {
				tainted = pool
			}
		}
	}
	if len(images) != 1 {
		t.Errorf("nodes run %d different images, want one pinned image", len(images))
	}
	if !pools["core"] || !pools["privileged"] {
		t.Errorf("worker pools = %v, want core and privileged", pools)
	}
	if tainted != "privileged" {
		t.Errorf("the NoSchedule taint is on pool %q, want privileged", tainted)
	}
}

func readControlRunbook(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("control/README.md")
	if err != nil {
		t.Fatalf("reading the control runbook: %v", err)
	}
	return string(raw)
}

// The control cluster runs kind on gt-control's Docker engine; there is no
// separate gt-kind VM, so the runbook must not send anyone to one.
func TestControlRunbookNamesGtControlAsTheVM(t *testing.T) {
	t.Parallel()
	lines := strings.Split(readControlRunbook(t), "\n")
	if len(lines) < 1 || !strings.Contains(lines[0], "gt-control") {
		t.Fatalf("the runbook's first line must name gt-control as the VM: %q", lines[:min(len(lines), 1)])
	}
	for i, line := range lines {
		if strings.Contains(line, "gt-kind") {
			t.Errorf("line %d still names the gt-kind VM: %q", i+1, line)
		}
	}
}

func TestControlRunbookStatesCoreBudget(t *testing.T) {
	t.Parallel()
	text := readControlRunbook(t)
	for _, want := range []string{"24 cores", "Docker Desktop 8, gt-sbx 8, gt-control 12", "28 against 24", "cloud VM is separate"} {
		if !strings.Contains(text, want) {
			t.Errorf("the runbook lacks the budget figure %q", want)
		}
	}
	// The VM figures are attached to their names and add up to the total the
	// runbook states.
	sum := 0
	for _, m := range regexp.MustCompile(`(?:Docker Desktop|gt-sbx|gt-control) (\d+)`).FindAllStringSubmatch(text, -1) {
		n, _ := strconv.Atoi(m[1])
		sum += n
	}
	if !strings.Contains(text, fmt.Sprintf("%d against 24", sum)) {
		t.Errorf("the VM figures sum to %d, which the runbook's total does not state", sum)
	}
}

// A kind rebuild drops the node containers, so the local-path provisioner's
// storage is mounted from the gt-control VM's filesystem, which outlives it.
func TestControlKindConfigMountsDurableVolumes(t *testing.T) {
	t.Parallel()
	cfg := decodeDocs(t, "control/kind-config.yaml")[0]
	nodes, _ := cfg["nodes"].([]any)
	mountsByNode := map[string][][2]string{}
	for _, n := range nodes {
		m, _ := n.(map[string]any)
		labels, _ := m["labels"].(map[string]any)
		name, _ := labels["gt.dev/pool"].(string)
		if name == "" {
			name, _ = m["role"].(string)
		}
		mounts, _ := m["extraMounts"].([]any)
		for _, raw := range mounts {
			mm, _ := raw.(map[string]any)
			host, _ := mm["hostPath"].(string)
			container, _ := mm["containerPath"].(string)
			mountsByNode[name] = append(mountsByNode[name], [2]string{host, container})
		}
	}
	want := [2]string{"/var/lib/gt-control/volumes", "/var/local-path-provisioner"}
	if got := mountsByNode["core"]; len(got) != 1 || got[0] != want {
		t.Errorf("core worker mounts = %v, want %v", got, want)
	}
	for _, name := range []string{"control-plane", "privileged"} {
		if got := mountsByNode[name]; len(got) != 0 {
			t.Errorf("%s node mounts = %v, want none: only the core node carries the durable volume", name, got)
		}
	}
}

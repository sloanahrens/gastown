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

// The VM name gt-control is misleading, so the runbook says in its first lines
// which VM hosts the control cluster and what gt-control really is.
func TestControlRunbookNamesGtKindNotGtControl(t *testing.T) {
	t.Parallel()
	lines := strings.Split(readControlRunbook(t), "\n")
	if len(lines) < 3 || !strings.Contains(lines[0], "gt-kind") {
		t.Fatalf("the runbook's first line must name gt-kind as the VM: %q", lines[:min(len(lines), 1)])
	}
	if !strings.Contains(lines[2], "gt-control") || !strings.Contains(lines[2], "sweep runner") {
		t.Errorf("the first paragraph must say gt-control is the sweep runner's engine: %q", lines[2])
	}
}

func TestControlRunbookStatesCoreBudget(t *testing.T) {
	t.Parallel()
	text := readControlRunbook(t)
	for _, want := range []string{"24 cores", "Docker Desktop 8, gt-sbx 12, gt-control 8, gt-kind 6", "34 against 24"} {
		if !strings.Contains(text, want) {
			t.Errorf("the runbook lacks the budget figure %q", want)
		}
	}
	// The four VM figures are attached to their names and add up to the total
	// the runbook states.
	sum := 0
	for _, m := range regexp.MustCompile(`(?:Docker Desktop|gt-sbx|gt-control|gt-kind) (\d+)[,.]? ?`).FindAllStringSubmatch(text, -1) {
		n, _ := strconv.Atoi(m[1])
		sum += n
	}
	if !strings.Contains(text, fmt.Sprintf("%d against 24", sum)) {
		t.Errorf("the VM figures sum to %d, which the runbook's total does not state", sum)
	}
}

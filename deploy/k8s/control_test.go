package k8s

import (
	"os"
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
	pools := map[string]bool{}
	var tainted string
	for _, n := range nodes {
		m, _ := n.(map[string]any)
		labels, _ := m["labels"].(map[string]any)
		pool, _ := labels["gt.dev/pool"].(string)
		if m["role"] == "worker" {
			pools[pool] = true
		}
		if patches, ok := m["kubeadmConfigPatches"].([]any); ok && len(patches) > 0 {
			text, _ := patches[0].(string)
			if strings.Contains(text, "key: gt.dev/pool") && strings.Contains(text, "effect: NoSchedule\n") {
				tainted = pool
			}
		}
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
	lines := strings.SplitN(readControlRunbook(t), "\n", 5)
	head := strings.Join(lines[:min(len(lines), 5)], "\n")
	for _, want := range []string{"gt-kind", "gt-control", "sweep runner"} {
		if !strings.Contains(head, want) {
			t.Errorf("the runbook's opening lines lack %q:\n%s", want, head)
		}
	}
}

func TestControlRunbookStatesCoreBudget(t *testing.T) {
	t.Parallel()
	text := readControlRunbook(t)
	for _, want := range []string{"24 cores", "Docker Desktop 8", "gt-sbx 12", "gt-control 8", "gt-kind 6"} {
		if !strings.Contains(text, want) {
			t.Errorf("the runbook lacks the budget figure %q", want)
		}
	}
}

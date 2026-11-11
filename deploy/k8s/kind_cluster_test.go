package k8s

import (
	"testing"

	"gopkg.in/yaml.v3"
)

// clusterConfig decodes the checked-in kind cluster config. The placeholders
// for the cluster name and the gateway port are plain strings, so the template
// parses as it is; up.sh fills them and passes the file to kind.
func clusterConfig(t *testing.T) map[string]any {
	t.Helper()
	docs := decodeDocs(t, "kind/cluster.yaml")
	if len(docs) != 1 {
		t.Fatalf("kind/cluster.yaml has %d documents, want 1", len(docs))
	}
	return docs[0]
}

// patchDoc decodes one kubeadmConfigPatches string as a YAML document, so a
// patch whose text is malformed fails here rather than in a create nobody in
// the unit tier runs.
func patchDoc(t *testing.T, raw any) map[string]any {
	t.Helper()
	s, ok := raw.(string)
	if !ok {
		t.Fatalf("kubeadmConfigPatches entry = %v (%T), want a YAML string", raw, raw)
	}
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(s), &doc); err != nil {
		t.Fatalf("kubeadmConfigPatches entry is not valid YAML: %v", err)
	}
	return doc
}

// The kubelet keeps its own image GC on for every node (kind collects no
// images by default, gt-ppgga), so a rebuilt cluster prunes from its first
// start instead of relying on the Mac-side prune job.
func TestKindClusterConfigEnablesKubeletImageGC(t *testing.T) {
	t.Parallel()
	patches, ok := clusterConfig(t)["kubeadmConfigPatches"].([]any)
	if !ok {
		t.Fatal("kind/cluster.yaml has no top-level kubeadmConfigPatches; a per-node patch would leave the other nodes' kubelets with GC off")
	}
	var kubelet map[string]any
	for _, raw := range patches {
		if p := patchDoc(t, raw); p["kind"] == "KubeletConfiguration" {
			kubelet = p
		}
	}
	if kubelet == nil {
		t.Fatal("kubeadmConfigPatches has no KubeletConfiguration entry")
	}
	if got := kubelet["imageGCHighThresholdPercent"]; got != 80 {
		t.Errorf("imageGCHighThresholdPercent = %v, want 80", got)
	}
	if got := kubelet["imageGCLowThresholdPercent"]; got != 70 {
		t.Errorf("imageGCLowThresholdPercent = %v, want 70", got)
	}
	if got := kubelet["imageMaximumGCAge"]; got != "72h" {
		t.Errorf("imageMaximumGCAge = %v, want 72h", got)
	}
}

// kindNode is the part of one nodes[] entry the layout test judges.
type kindNode struct {
	role   string
	pool   string
	taints []any
	doc    map[string]any
}

// kindNodes reads the node list. A node whose patch is not valid YAML fails
// here, once, rather than as a scattering of nil comparisons below.
func kindNodes(t *testing.T, doc map[string]any) []kindNode {
	t.Helper()
	raw, ok := doc["nodes"].([]any)
	if !ok || len(raw) != 3 {
		t.Fatalf("nodes = %v, want three (control-plane, core, sandbox)", doc["nodes"])
	}
	nodes := make([]kindNode, 0, len(raw))
	for _, item := range raw {
		m, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("node = %v (%T), want a map", item, item)
		}
		role, _ := m["role"].(string)
		n := kindNode{role: role, doc: m}
		if labels, ok := m["labels"].(map[string]any); ok {
			n.pool, _ = labels["gt.dev/pool"].(string)
		}
		if patches, ok := m["kubeadmConfigPatches"].([]any); ok {
			for _, raw := range patches {
				if reg, ok := patchDoc(t, raw)["nodeRegistration"].(map[string]any); ok {
					n.taints, _ = reg["taints"].([]any)
				}
			}
		}
		nodes = append(nodes, n)
	}
	return nodes
}

// The node layout is what up.sh used to write in a heredoc: the pool labels and
// the sandbox taint decide which workloads share a node, and the port mapping is
// the only path from the Mac to the gateway.
func TestKindClusterConfigHoldsNodeLayout(t *testing.T) {
	t.Parallel()
	doc := clusterConfig(t)
	if got := doc["name"]; got != "__CLUSTER__" {
		t.Errorf("name = %v, want the __CLUSTER__ placeholder up.sh fills", got)
	}
	if got := doc["apiVersion"]; got != "kind.x-k8s.io/v1alpha4" {
		t.Errorf("apiVersion = %v, want kind.x-k8s.io/v1alpha4", got)
	}

	nodes := kindNodes(t, doc)
	roles := map[string]int{}
	pools := map[string]int{}
	byPool := map[string]*kindNode{}
	var controlPlane *kindNode
	for i := range nodes {
		n := &nodes[i]
		roles[n.role]++
		pools[n.pool]++
		byPool[n.pool] = n
		if n.role == "control-plane" {
			controlPlane = n
		}
	}
	if roles["control-plane"] != 1 || roles["worker"] != 2 {
		t.Errorf("roles = %v, want one control-plane and two workers", roles)
	}
	if pools["core"] != 1 || pools["sandbox"] != 1 {
		t.Errorf("pool labels = %v, want exactly one core and one sandbox worker", pools)
	}
	for pool := range pools {
		if pool == "" {
			continue // the control plane carries no pool label
		}
		if pool != "core" && pool != "sandbox" {
			t.Errorf("node pool label = %q, want only core and sandbox", pool)
		}
	}
	if controlPlane == nil {
		t.Fatal("no control-plane node")
	}
	if len(controlPlane.taints) != 0 {
		t.Errorf("control-plane taints = %v, want none", controlPlane.taints)
	}
	if core := byPool["core"]; core == nil {
		t.Fatal("no core worker")
	} else if len(core.taints) != 0 {
		t.Errorf("core worker taints = %v; only the sandbox worker may be tainted", core.taints)
	}
	sandbox := byPool["sandbox"]
	if sandbox == nil {
		t.Fatal("no sandbox worker")
	}
	if len(sandbox.taints) != 1 {
		t.Fatalf("sandbox worker taints = %v, want exactly the NoSchedule taint", sandbox.taints)
	}
	taint, ok := sandbox.taints[0].(map[string]any)
	if !ok {
		t.Fatalf("taint = %v (%T), want a map", sandbox.taints[0], sandbox.taints[0])
	}
	if taint["key"] != "gt.dev/pool" || taint["value"] != "sandbox" || taint["effect"] != "NoSchedule" {
		t.Errorf("taint = %v, want gt.dev/pool=sandbox:NoSchedule", taint)
	}

	mappings, ok := controlPlane.doc["extraPortMappings"].([]any)
	if !ok || len(mappings) != 1 {
		t.Fatalf("control-plane extraPortMappings = %v, want one", controlPlane.doc["extraPortMappings"])
	}
	mapping, ok := mappings[0].(map[string]any)
	if !ok {
		t.Fatalf("port mapping = %v (%T), want a map", mappings[0], mappings[0])
	}
	if mapping["containerPort"] != 30443 {
		t.Errorf("containerPort = %v, want 30443", mapping["containerPort"])
	}
	if mapping["hostPort"] != "__GATEWAY_PORT__" {
		t.Errorf("hostPort = %v, want the __GATEWAY_PORT__ placeholder up.sh fills", mapping["hostPort"])
	}
	if mapping["listenAddress"] != "127.0.0.1" {
		t.Errorf("listenAddress = %v, want 127.0.0.1", mapping["listenAddress"])
	}
}

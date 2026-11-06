package k8s

import "testing"

// Forgejo tolerates the disk-pressure NoSchedule taint and nothing else, in both
// namespaces it runs in. The record is the only copy in the repo: the live
// Deployments are patched by hand, so this test holds the text to the decision
// (gt-gg9hv.59).
func TestForgejoTolerationsPatch(t *testing.T) {
	t.Parallel()
	docs := decodeDocs(t, "forgejo-tolerations.yaml")
	if len(docs) != 2 {
		t.Fatalf("forgejo-tolerations.yaml has %d documents, want one per Forgejo namespace (2)", len(docs))
	}
	seen := map[string]bool{}
	for _, d := range docs {
		if got := d["kind"]; got != "Deployment" {
			t.Errorf("document kind = %v, want Deployment", got)
		}
		name, _ := at(t, d, "metadata", "name").(string)
		ns, _ := at(t, d, "metadata", "namespace").(string)
		if name != "forgejo" {
			t.Errorf("Deployment name = %q, want forgejo", name)
		}
		seen[ns] = true

		tols, ok := at(t, d, "spec", "template", "spec", "tolerations").([]any)
		if !ok {
			t.Fatalf("%s: tolerations is not a list", ns)
		}
		if len(tols) != 1 {
			t.Errorf("%s: %d tolerations, want exactly the disk-pressure one", ns, len(tols))
			continue
		}
		tol, ok := tols[0].(map[string]any)
		if !ok {
			t.Fatalf("%s: toleration = %v (%T), want a map", ns, tols[0], tols[0])
		}
		if got := tol["key"]; got != "node.kubernetes.io/disk-pressure" {
			t.Errorf("%s: toleration key = %v, want node.kubernetes.io/disk-pressure", ns, got)
		}
		if got := tol["operator"]; got != "Exists" {
			t.Errorf("%s: toleration operator = %v, want Exists", ns, got)
		}
		if got := tol["effect"]; got != "NoSchedule" {
			t.Errorf("%s: toleration effect = %v, want NoSchedule", ns, got)
		}
		if got, has := tol["tolerationSeconds"]; has {
			t.Errorf("%s: toleration sets tolerationSeconds = %v; the decision tolerates the taint without a bound", ns, got)
		}
	}
	for _, ns := range []string{"forgejo", "forgejo-trial"} {
		if !seen[ns] {
			t.Errorf("no Forgejo Deployment in namespace %s", ns)
		}
	}
}

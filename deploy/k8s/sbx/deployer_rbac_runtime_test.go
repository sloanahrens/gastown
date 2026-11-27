package sbx

import (
	"context"
	"io"
	"os"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/steveyegge/gastown/deploy/k8s"
)

// deployerRBACPath and deployerRBACPrefix are the manifest the operator applies
// in the sitting, which no rendered set embeds, and its object-name prefix.
const (
	deployerRBACPath   = "../sbx-deployer-rbac.yaml"
	deployerRBACPrefix = "gt-sbx-deployer"
)

// TestSbxDeployerRuntime holds sbxRuntime to the manifest: every object in it is
// IsRuntime on a rendered set, the prefix rows are exactly its objects, and a
// name with that prefix it does not hold is still reported.
func TestSbxDeployerRuntime(t *testing.T) {
	t.Parallel()
	sandbox := readFixture(t)
	set, err := renderSet(context.Background(), renderInputs(sandbox, readChartFixture(t), new([]runnerCall)), k8s.SbxStatic, digestOf(sandbox))
	if err != nil {
		t.Fatalf("renderSet(): %v", err)
	}
	raw, err := os.ReadFile(deployerRBACPath)
	if err != nil {
		t.Fatalf("reading %s: %v", deployerRBACPath, err)
	}
	var refs []ObjectRef
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	for err == nil {
		var doc struct {
			Kind     string    `yaml:"kind"`
			Metadata ObjectRef `yaml:"metadata"`
		}
		if err = dec.Decode(&doc); err != nil {
			break
		}
		doc.Metadata.Kind = doc.Kind
		refs = append(refs, doc.Metadata)
	}
	if err != io.EOF {
		t.Fatalf("%s: %v", deployerRBACPath, err)
	}
	rows := slices.DeleteFunc(slices.Clone(set.Runtime), func(ref ObjectRef) bool { return !strings.HasPrefix(ref.Name, deployerRBACPrefix) })
	for _, ref := range refs {
		if !set.IsRuntime(ref) {
			t.Errorf("%+v is in %s but IsRuntime reports it as extra", ref, deployerRBACPath)
		}
	}
	if len(rows) != len(refs) || slices.ContainsFunc(refs, func(r ObjectRef) bool { return !slices.Contains(rows, r) }) {
		t.Errorf("sbxRuntime deployer rows %+v are not the objects in %s", rows, deployerRBACPath)
	}
	for _, ref := range []ObjectRef{{Kind: "ClusterRole", Name: deployerRBACPrefix + "-ghost"}, {Kind: "Role", Namespace: openshellName, Name: deployerRBACPrefix + "-ghost"}} {
		if set.IsRuntime(ref) {
			t.Errorf("%+v is not in %s but IsRuntime ignores it", ref, deployerRBACPath)
		}
	}
}

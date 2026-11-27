package sbx

import (
	"context"
	"maps"
	"os"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/steveyegge/gastown/deploy/k8s"
)

// deployerManifest is the gt-sbx deployer RBAC, which sits beside this package.
const deployerManifest = "../sbx-deployer-rbac.yaml"

// deployerDoc is the part of one manifest document the coverage reads.
type deployerDoc struct {
	Kind     string
	Metadata struct{ Name, Namespace string }
	Rules    []struct {
		APIGroups []string `yaml:"apiGroups"`
		Resources []string `yaml:"resources"`
		Verbs     []string `yaml:"verbs"`
	}
}

// deployerGrants flattens raw to "object group/resource verb". A document with
// no kind and a stream with no grant fail the test, so unread input never passes.
func deployerGrants(t *testing.T, raw []byte) map[string]bool {
	t.Helper()
	grants := map[string]bool{}
	for _, part := range splitDocuments(raw) {
		var doc deployerDoc
		if err := yaml.Unmarshal(part, &doc); err != nil {
			t.Fatalf("%s: %v", deployerManifest, err)
		}
		if doc.Kind == "" {
			t.Fatalf("%s holds a document with no kind", deployerManifest)
		}
		object := doc.Kind + "/" + doc.Metadata.Name
		if doc.Metadata.Namespace != "" {
			object = doc.Kind + "/" + doc.Metadata.Namespace + "/" + doc.Metadata.Name
		}
		for _, rule := range doc.Rules {
			for _, group := range rule.APIGroups {
				if group == "" {
					group = "core"
				}
				for _, resource := range rule.Resources {
					for _, verb := range rule.Verbs {
						grants[object+" "+group+"/"+resource+" "+verb] = true
					}
				}
			}
		}
	}
	if len(grants) == 0 {
		t.Fatalf("%s holds no grant", deployerManifest)
	}
	return grants
}

// deployerKind is a kind's group, plural resource (core for the empty group) and cluster scope.
type deployerKind struct {
	resource string
	cluster  bool
}

// deployerKinds is every kind the gt-sbx render accepts or its fixture set places; an absent kind cannot be checked, so the coverage fails it by name.
var deployerKinds = map[string]deployerKind{
	"ConfigMap":                {"core/configmaps", false},
	"ClusterRole":              {"rbac.authorization.k8s.io/clusterroles", true},
	"ClusterRoleBinding":       {"rbac.authorization.k8s.io/clusterrolebindings", true},
	"CustomResourceDefinition": {"apiextensions.k8s.io/customresourcedefinitions", true},
	"Deployment":               {"apps/deployments", false},
	"Job":                      {"batch/jobs", false},
	"Namespace":                {"core/namespaces", true},
	"PersistentVolumeClaim":    {"core/persistentvolumeclaims", false},
	"PriorityClass":            {"scheduling.k8s.io/priorityclasses", true},
	"Role":                     {"rbac.authorization.k8s.io/roles", false},
	"RoleBinding":              {"rbac.authorization.k8s.io/rolebindings", false},
	"Service":                  {"core/services", false},
	"ServiceAccount":           {"core/serviceaccounts", false},
	"StatefulSet":              {"apps/statefulsets", false},
}

// deployerViewKinds are the rendered kinds the built-in view ClusterRole lets the read user list; the manifest binds view by name and cannot show its rules.
var deployerViewKinds = []string{"ConfigMap", "Deployment", "Job", "PersistentVolumeClaim", "Service", "ServiceAccount", "StatefulSet"}

// deployerProblems reports each kind the manifest does not cover, naming it first. A cluster-scoped kind needs get, create and patch in the policy ClusterRole; a namespaced kind needs them in the openshell Role and, when the agent-sandbox release marks it namespaced, in the agent-sandbox-system Role.
func deployerProblems(grants map[string]bool, kinds, rendered []string, table map[string]deployerKind) []string {
	var problems []string
	for _, kind := range kinds {
		resource, ok := table[kind]
		if !ok {
			problems = append(problems, kind+": no row in the kind-to-resource table")
			continue
		}
		roles := []string{"ClusterRole/gt-sbx-deployer-policy"}
		if !resource.cluster {
			roles = []string{"Role/openshell/gt-sbx-deployer-apply"}
			if scoped, ok := agentSandboxObjectScopes[kind]; ok && !scoped {
				roles = append(roles, "Role/agent-sandbox-system/gt-sbx-deployer-apply")
			}
		}
		for _, role := range roles {
			for _, verb := range []string{"get", "create", "patch"} {
				if !grants[role+" "+resource.resource+" "+verb] {
					problems = append(problems, kind+": "+role+" grants no "+verb+" on "+resource.resource)
				}
			}
		}
	}
	for _, kind := range rendered {
		if resource, ok := table[kind]; ok && !grants["ClusterRole/gt-sbx-deployer-reader-extra "+resource.resource+" list"] && !slices.Contains(deployerViewKinds, kind) {
			problems = append(problems, kind+": the read user cannot list "+resource.resource)
		}
	}
	return problems
}

// TestSbxDeployerCoverage holds the manifest to every kind the gt-sbx render accepts and its fixture set places, then to naming each kind the table or an edited manifest leaves uncovered.
func TestSbxDeployerCoverage(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(deployerManifest)
	if err != nil {
		t.Fatalf("reading %s: %v", deployerManifest, err)
	}
	sandbox := readFixture(t)
	var calls []runnerCall
	set, err := renderSet(context.Background(), renderInputs(sandbox, readChartFixture(t), &calls), k8s.SbxStatic, digestOf(sandbox))
	if err != nil {
		t.Fatalf("renderSet(): %v", err)
	}
	kinds := append(slices.Sorted(maps.Keys(openshellObjectScopes)), slices.Sorted(maps.Keys(agentSandboxObjectScopes))...)
	var rendered []string
	for _, object := range set.Objects {
		kinds, rendered = append(kinds, object.Kind), append(rendered, object.Kind)
	}
	slices.Sort(kinds)
	kinds = slices.DeleteFunc(slices.Compact(kinds), func(kind string) bool { return kind == "Secret" })
	if problems := deployerProblems(deployerGrants(t, raw), kinds, rendered, deployerKinds); len(problems) > 0 {
		t.Fatalf("%s does not cover the render: %q", deployerManifest, problems)
	}
	rows := []struct {
		name            string
		kinds, rendered []string
		table           map[string]deployerKind
		edit            func(string) string
		want            string
	}{
		{"invented cluster-scoped kind", []string{"Widget"}, nil, map[string]deployerKind{"Widget": {"example.com/widgets", true}}, nil, "Widget: ClusterRole/gt-sbx-deployer-policy grants no get"},
		{"invented namespaced kind", []string{"Gadget"}, nil, map[string]deployerKind{"Gadget": {"example.com/gadgets", false}}, nil, "Gadget: Role/openshell/gt-sbx-deployer-apply grants no get"},
		{"kind with no table row", []string{"Doohickey"}, nil, nil, nil, "Doohickey: no row in the kind-to-resource table"},
		{"policy grant removed", []string{"CustomResourceDefinition"}, nil, deployerKinds, func(s string) string { return strings.Replace(s, "apiextensions.k8s.io", "planted.example.com", 1) }, "CustomResourceDefinition: ClusterRole/gt-sbx-deployer-policy grants no get"},
		{"sandbox grant removed", []string{"Deployment"}, nil, deployerKinds, func(s string) string {
			return strings.ReplaceAll(s, `resources: ["deployments", "statefulsets"]`, `resources: ["statefulsets"]`)
		}, "Deployment: Role/agent-sandbox-system/gt-sbx-deployer-apply grants no get"},
		{"read grant removed", nil, []string{"PriorityClass"}, deployerKinds, func(s string) string { return strings.ReplaceAll(s, "scheduling.k8s.io", "planted.example.com") }, "PriorityClass: the read user cannot list"},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			source := string(raw)
			if row.edit != nil {
				source = row.edit(source)
			}
			problems := deployerProblems(deployerGrants(t, []byte(source)), row.kinds, row.rendered, row.table)
			if !slices.ContainsFunc(problems, func(problem string) bool { return strings.HasPrefix(problem, row.want) }) {
				t.Fatalf("problems = %q, want one starting %q", problems, row.want)
			}
		})
	}
}

package k8s

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// sbxDeployerRBACFile is the gt-sbx deployer manifest TestSbxDeployerRBAC holds
// to the exact grant table sbxDeployerWant names.
const sbxDeployerRBACFile = "sbx-deployer-rbac.yaml"

// sbxDeployerRule carries resourceNames so a rule narrowed to named objects
// flattens with the names it grants.
type sbxDeployerRule struct {
	APIGroups     []string `yaml:"apiGroups"`
	Resources     []string `yaml:"resources"`
	Verbs         []string `yaml:"verbs"`
	ResourceNames []string `yaml:"resourceNames"`
}

type sbxDeployerDoc struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name string `yaml:"name"`
	} `yaml:"metadata"`
	Rules []sbxDeployerRule `yaml:"rules"`
}

// loadSbxDeployerRBAC parses every document of file, failing with its name.
func loadSbxDeployerRBAC(t *testing.T, file string) []sbxDeployerDoc {
	t.Helper()
	raw, err := os.ReadFile(filepath.FromSlash(file))
	if err != nil {
		t.Fatalf("reading %s: %v", file, err)
	}
	var docs []sbxDeployerDoc
	for _, doc := range splitDocuments(raw) {
		var d sbxDeployerDoc
		if err := yaml.Unmarshal(doc, &d); err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		docs = append(docs, d)
	}
	return docs
}

// sbxDeployerLines flattens every object and every rule combination into one
// line: the object, then its API group, resource, verb and, when the rule has
// one, the resource name.
func sbxDeployerLines(docs []sbxDeployerDoc) []string {
	var lines []string
	for _, d := range docs {
		obj := d.Kind + "/" + d.Metadata.Name
		lines = append(lines, obj)
		for _, r := range d.Rules {
			names := r.ResourceNames
			if len(names) == 0 {
				names = []string{""}
			}
			for _, g := range r.APIGroups {
				for _, res := range r.Resources {
					for _, verb := range r.Verbs {
						for _, name := range names {
							lines = append(lines, strings.TrimSpace(obj+" "+deployerResourceName(g, res)+" "+verb+" "+name))
						}
					}
				}
			}
		}
	}
	return lines
}

// sbxDeployerMissingExtra returns the lines want holds that got lacks and the
// lines got holds that want does not name, so a comparison reports both sides.
func sbxDeployerMissingExtra(got, want []string) (missing, extra []string) {
	missing = slices.DeleteFunc(slices.Clone(want), func(line string) bool { return slices.Contains(got, line) })
	extra = slices.DeleteFunc(slices.Clone(got), func(line string) bool { return slices.Contains(want, line) })
	return missing, extra
}

// sbxDeployerWant is the exact grant table: the six writes on each resource,
// and escalate and bind on clusterroles only.
var sbxDeployerWant = func() []string {
	const role = "ClusterRole/gt-sbx-deployer-policy"
	write := []string{"get", "list", "watch", "create", "update", "patch"}
	lines := []string{role}
	for _, res := range []string{"core/namespaces", "scheduling.k8s.io/priorityclasses", "apiextensions.k8s.io/customresourcedefinitions", "rbac.authorization.k8s.io/clusterrolebindings"} {
		for _, verb := range write {
			lines = append(lines, role+" "+res+" "+verb)
		}
	}
	for _, verb := range append(slices.Clone(write), "escalate", "bind") {
		lines = append(lines, role+" rbac.authorization.k8s.io/clusterroles "+verb)
	}
	return lines
}()

// TestSbxDeployerRBAC holds the manifest to the exact grant table.
func TestSbxDeployerRBAC(t *testing.T) {
	t.Parallel()
	docs := loadSbxDeployerRBAC(t, sbxDeployerRBACFile)
	if missing, extra := sbxDeployerMissingExtra(sbxDeployerLines(docs), sbxDeployerWant); len(missing)+len(extra) > 0 {
		t.Fatalf("%s does not match the table: missing %q, extra %q", sbxDeployerRBACFile, missing, extra)
	}
}

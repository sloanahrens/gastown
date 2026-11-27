package k8s

import (
	"bytes"
	"fmt"
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
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Metadata   struct {
		Name string `yaml:"name"`
	} `yaml:"metadata"`
	Rules   []sbxDeployerRule `yaml:"rules"`
	RoleRef struct {
		APIGroup string `yaml:"apiGroup"`
		Kind     string `yaml:"kind"`
		Name     string `yaml:"name"`
	} `yaml:"roleRef"`
	Subjects []struct {
		APIGroup string `yaml:"apiGroup"`
		Kind     string `yaml:"kind"`
		Name     string `yaml:"name"`
	} `yaml:"subjects"`
}

// sbxDeployerParse parses the documents of the file named name, refusing any
// field these types do not list and a file that holds no document object. Every
// error names the file, so a refusal says which input carried the widening.
func sbxDeployerParse(name string, raw []byte) ([]sbxDeployerDoc, error) {
	var docs []sbxDeployerDoc
	for _, doc := range splitDocuments(raw) {
		dec := yaml.NewDecoder(bytes.NewReader(doc))
		dec.KnownFields(true)
		var d sbxDeployerDoc
		if err := dec.Decode(&d); err != nil {
			return nil, fmt.Errorf("%s: %v", name, err)
		}
		if d.Kind == "" {
			return nil, fmt.Errorf("%s holds a document with no kind", name)
		}
		docs = append(docs, d)
	}
	if len(docs) == 0 {
		return nil, fmt.Errorf("%s holds no object", name)
	}
	return docs, nil
}

// loadSbxDeployerRBAC reads file and parses it with sbxDeployerParse.
func loadSbxDeployerRBAC(t *testing.T, file string) []sbxDeployerDoc {
	t.Helper()
	raw, err := os.ReadFile(filepath.FromSlash(file))
	if err != nil {
		t.Fatalf("reading %s: %v", file, err)
	}
	docs, err := sbxDeployerParse(file, raw)
	if err != nil {
		t.Fatal(err)
	}
	return docs
}

// sbxDeployerLines flattens every object, every rule combination and every
// binding subject into one line: the object, then its API group, resource,
// verb and, when the rule has one, the resource name; a binding subject adds
// the roleRef kind and name and the subject kind and name. A binding with no
// subject yields only its object line.
func sbxDeployerLines(docs []sbxDeployerDoc) []string {
	var lines []string
	for _, d := range docs {
		obj := d.Kind + "/" + d.Metadata.Name
		lines = append(lines, obj)
		roleRef := d.RoleRef.Kind + "/" + d.RoleRef.Name
		for _, s := range d.Subjects {
			lines = append(lines, obj+" "+roleRef+" "+s.Kind+"/"+s.Name)
		}
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
// escalate and bind on clusterroles only, and the binding of the role to the
// User the certificate carries.
var sbxDeployerWant = func() []string {
	const role = "ClusterRole/gt-sbx-deployer-policy"
	write := []string{"get", "list", "watch", "create", "update", "patch"}
	binding := "ClusterRoleBinding/gt-sbx-deployer-policy"
	lines := []string{role, binding, binding + " ClusterRole/gt-sbx-deployer-policy User/gt-deployer-policy"}
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

// TestSbxDeployerRBACRefusesUnreadInput holds the loader to refusing a field the
// types do not list and a file that holds no object.
func TestSbxDeployerRBACRefusesUnreadInput(t *testing.T) {
	t.Parallel()
	const role = "apiVersion: rbac.authorization.k8s.io/v1\nkind: ClusterRole\nmetadata:\n  name: gt-sbx-deployer-policy\n"
	rows := []struct{ file, doc string }{
		{"aggregation-rule.yaml", role + "aggregationRule:\n  clusterRoleSelectors: []\n"},
		{"non-resource-urls.yaml", role + "rules:\n  - nonResourceURLs: [\"/healthz\"]\n    verbs: [\"get\"]\n"},
		{"empty.yaml", ""},
		{"comments-only.yaml", "# no object here\n"},
		{"no-kind.yaml", "apiVersion: v1\nmetadata:\n  name: gt-sbx-deployer-policy\n"},
	}
	for _, row := range rows {
		t.Run(row.file, func(t *testing.T) {
			t.Parallel()
			if _, err := sbxDeployerParse(row.file, []byte(row.doc)); err == nil || !strings.Contains(err.Error(), row.file) {
				t.Fatalf("%s: got %v, want a refusal naming the file", row.file, err)
			}
		})
	}
}

// TestSbxDeployerRBACReportsWidenings plants one edit in the manifest's own
// bytes and holds the comparison to reporting it on the side the edit moved:
// an added grant or subject is extra, a dropped one is missing.
func TestSbxDeployerRBACReportsWidenings(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(sbxDeployerRBACFile)
	if err != nil {
		t.Fatalf("reading %s: %v", sbxDeployerRBACFile, err)
	}
	const role = "ClusterRole/gt-sbx-deployer-policy"
	rows := []struct {
		what, old, new, want string
		dropped              bool
	}{
		{"added verb", `verbs: ["escalate", "bind"]`, `verbs: ["escalate", "bind", "delete"]`, role + " rbac.authorization.k8s.io/clusterroles delete", false},
		{"dropped grant", "\n  - apiGroups: [\"rbac.authorization.k8s.io\"]\n    resources: [\"clusterroles\"]\n    verbs: [\"escalate\", \"bind\"]\n", "\n", role + " rbac.authorization.k8s.io/clusterroles escalate", true},
		{"second binding subject", "  - kind: User\n    name: gt-deployer-policy", "  - kind: User\n    name: gt-deployers\n  - kind: User\n    name: gt-deployer-policy", "ClusterRoleBinding/gt-sbx-deployer-policy ClusterRole/gt-sbx-deployer-policy User/gt-deployers", false},
		{"wildcard resource", `resources: ["namespaces"]`, `resources: ["namespaces", "*"]`, role + " core/* get", false},
		{"wildcard verb", `verbs: ["escalate", "bind"]`, `verbs: ["escalate", "bind", "*"]`, role + " rbac.authorization.k8s.io/clusterroles *", false},
	}
	for _, row := range rows {
		t.Run(row.what, func(t *testing.T) {
			t.Parallel()
			edited := strings.Replace(string(raw), row.old, row.new, 1)
			if edited == string(raw) {
				t.Fatalf("%q matched nothing in %s", row.old, sbxDeployerRBACFile)
			}
			docs, err := sbxDeployerParse(sbxDeployerRBACFile, []byte(edited))
			if err != nil {
				t.Fatal(err)
			}
			missing, extra := sbxDeployerMissingExtra(sbxDeployerLines(docs), sbxDeployerWant)
			got, side := extra, "extra"
			if row.dropped {
				got, side = missing, "missing"
			}
			if !slices.Contains(got, row.want) {
				t.Fatalf("%q went unreported; %s %q", row.want, side, got)
			}
		})
	}
}

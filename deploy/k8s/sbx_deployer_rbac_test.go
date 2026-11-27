package k8s

import (
	"bytes"
	"errors"
	"fmt"
	"io"
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
		Name      string `yaml:"name"`
		Namespace string `yaml:"namespace"`
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

// sbxDeployerParse reads the whole stream, one document at a time until io.EOF,
// refusing a field these types do not list, a document with no kind and a stream
// that holds no document; every error names the file.
func sbxDeployerParse(name string, raw []byte) ([]sbxDeployerDoc, error) {
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	var docs []sbxDeployerDoc
	// io.EOF ends the loop; the check after it accepts io.EOF and refuses any other decode error.
	var err error
	for err == nil {
		var d sbxDeployerDoc
		if err = dec.Decode(&d); err != nil {
			break
		}
		if d.Kind == "" {
			return nil, fmt.Errorf("%s holds a document with no kind", name)
		}
		docs = append(docs, d)
	}
	if !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%s: %v", name, err)
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
// the roleRef kind and name and the subject kind and name. A namespaced object
// is kind/namespace/name, a cluster-scoped object kind/name. A binding with no
// subject yields only its object line.
func sbxDeployerLines(docs []sbxDeployerDoc) []string {
	var lines []string
	for _, d := range docs {
		obj := d.Kind + "/" + d.Metadata.Name
		if d.Metadata.Namespace != "" {
			obj = d.Kind + "/" + d.Metadata.Namespace + "/" + d.Metadata.Name
		}
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

// sbxDeployerWant is the exact grant table: the policy ClusterRole's six writes
// on the cluster kinds, escalate and bind on clusterroles only, and its binding
// to the User the policy certificate carries; then the apply Role in each
// namespace that holds the rendered set, with the six writes on the render's
// namespaced kinds, escalate and bind on roles, reads on replicasets and
// controllerrevisions, and its RoleBinding to the User gt-deployer-apply.
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
	for _, ns := range []string{"openshell", "agent-sandbox-system"} {
		applyRole, applyBinding := "Role/"+ns+"/gt-sbx-deployer-apply", "RoleBinding/"+ns+"/gt-sbx-deployer-apply"
		lines = append(lines, applyRole, applyBinding, applyBinding+" Role/gt-sbx-deployer-apply User/gt-deployer-apply")
		for _, res := range []string{"core/serviceaccounts", "core/services", "core/persistentvolumeclaims", "core/configmaps", "apps/deployments", "apps/statefulsets", "batch/jobs", "rbac.authorization.k8s.io/roles", "rbac.authorization.k8s.io/rolebindings"} {
			for _, verb := range write {
				lines = append(lines, applyRole+" "+res+" "+verb)
			}
		}
		for _, verb := range []string{"escalate", "bind"} {
			lines = append(lines, applyRole+" rbac.authorization.k8s.io/roles "+verb)
		}
		for _, res := range []string{"apps/replicasets", "apps/controllerrevisions"} {
			for _, verb := range []string{"get", "list"} {
				lines = append(lines, applyRole+" "+res+" "+verb)
			}
		}
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
// types do not list, a document with no kind and a stream with no document, naming the file.
func TestSbxDeployerRBACRefusesUnreadInput(t *testing.T) {
	t.Parallel()
	const role = "apiVersion: rbac.authorization.k8s.io/v1\nkind: ClusterRole\nmetadata:\n  name: gt-sbx-deployer-policy\n"
	rows := []struct{ file, doc string }{
		{"aggregation-rule.yaml", role + "aggregationRule:\n  clusterRoleSelectors: []\n"},
		{"non-resource-urls.yaml", role + "rules:\n  - nonResourceURLs: [\"/healthz\"]\n    verbs: [\"get\"]\n"},
		{"empty.yaml", ""},
		{"comments-only.yaml", "# no object here\n"},
		{"no-kind.yaml", "apiVersion: v1\nmetadata:\n  name: gt-sbx-deployer-policy\n"},
		{"empty-after-separator.yaml", role + "---\n"},
		{"text-after-doc-end.yaml", role + "...\nstray text\n"},
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

// TestSbxDeployerRBACReportsEdits plants one edit in the manifest's own bytes and
// holds the comparison to reporting it on the side the edit moved.
func TestSbxDeployerRBACReportsEdits(t *testing.T) {
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

// TestSbxDeployerRBACDistinguishesNamespaces holds a namespaced object's line to
// kind/namespace/name: one Role name in two namespaces is two objects, a grant in
// the wrong namespace is reported on both sides, and a subject's namespace stays
// an unlisted field, refused with the file named.
func TestSbxDeployerRBACDistinguishesNamespaces(t *testing.T) {
	t.Parallel()
	role := func(ns string) string {
		return "apiVersion: rbac.authorization.k8s.io/v1\nkind: Role\nmetadata:\n  name: gt-sbx-deployer\n  namespace: " + ns + "\nrules:\n  - apiGroups: [\"\"]\n    resources: [\"configmaps\"]\n    verbs: [\"get\"]\n"
	}
	load := func(t *testing.T, file, doc string) []string {
		t.Helper()
		docs, err := sbxDeployerParse(file, []byte(doc))
		if err != nil {
			t.Fatal(err)
		}
		return sbxDeployerLines(docs)
	}
	t.Run("one name in two namespaces", func(t *testing.T) {
		t.Parallel()
		got := load(t, "two-namespaces.yaml", role("openshell")+"---\n"+role("agent-sandbox-system"))
		want := []string{
			"Role/openshell/gt-sbx-deployer", "Role/openshell/gt-sbx-deployer core/configmaps get",
			"Role/agent-sandbox-system/gt-sbx-deployer", "Role/agent-sandbox-system/gt-sbx-deployer core/configmaps get",
		}
		if !slices.Equal(got, want) {
			t.Fatalf("got %q, want %q", got, want)
		}
	})
	t.Run("changed namespace", func(t *testing.T) {
		t.Parallel()
		openshell := load(t, "openshell.yaml", role("openshell"))
		missing, extra := sbxDeployerMissingExtra(load(t, "agent-sandbox-system.yaml", role("agent-sandbox-system")), openshell)
		if want := []string{"Role/openshell/gt-sbx-deployer", "Role/openshell/gt-sbx-deployer core/configmaps get"}; !slices.Equal(missing, want) {
			t.Fatalf("missing %q, want %q", missing, want)
		}
		if want := []string{"Role/agent-sandbox-system/gt-sbx-deployer", "Role/agent-sandbox-system/gt-sbx-deployer core/configmaps get"}; !slices.Equal(extra, want) {
			t.Fatalf("extra %q, want %q", extra, want)
		}
	})
	t.Run("subject namespace refused", func(t *testing.T) {
		t.Parallel()
		const file = "subject-namespace.yaml"
		doc := "apiVersion: v1\nkind: ClusterRoleBinding\nmetadata:\n  name: gt-sbx-deployer\nsubjects:\n  - kind: ServiceAccount\n    name: gt-sbx-deployer\n    namespace: openshell\n"
		if _, err := sbxDeployerParse(file, []byte(doc)); err == nil || !strings.Contains(err.Error(), file) {
			t.Fatalf("got %v, want a refusal naming %s", err, file)
		}
	})
}

// TestSbxDeployerRBACReadsEveryDocument holds the loader to the whole stream: a
// non-bare separator still begins a document kubectl applies, reported as extra.
func TestSbxDeployerRBACReadsEveryDocument(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(sbxDeployerRBACFile)
	if err != nil {
		t.Fatalf("reading %s: %v", sbxDeployerRBACFile, err)
	}
	const second = "ClusterRoleBinding/gt-sbx-deployer-extra"
	rows := []struct{ what, doc string }{
		{"separator with comment", "--- # c\nkind: ClusterRoleBinding\nmetadata:\n  name: gt-sbx-deployer-extra\n"},
		{"separator with tag", "--- !!map\nkind: ClusterRoleBinding\nmetadata:\n  name: gt-sbx-deployer-extra\n"},
		{"flow mapping on separator line", "--- {kind: ClusterRoleBinding, metadata: {name: gt-sbx-deployer-extra}}\n"},
	}
	for _, row := range rows {
		t.Run(row.what, func(t *testing.T) {
			t.Parallel()
			docs, err := sbxDeployerParse(sbxDeployerRBACFile, append(slices.Clone(raw), row.doc...))
			if err != nil {
				t.Fatal(err)
			}
			if _, extra := sbxDeployerMissingExtra(sbxDeployerLines(docs), sbxDeployerWant); !slices.Contains(extra, second) {
				t.Fatalf("second document went unread; extra %q", extra)
			}
		})
	}
}

// sbxDeployerApplyUser is the User the apply credential's certificate carries.
const sbxDeployerApplyUser = "gt-deployer-apply"

// sbxDeployerApplyProblems reports every way the apply credential reaches past
// its bound: a ClusterRoleBinding that names it, a binding whose roleRef the
// manifest does not hold, and a reached rule that grants secrets or a removing
// verb.
func sbxDeployerApplyProblems(docs []sbxDeployerDoc) []string {
	lines := sbxDeployerLines(docs)
	var problems []string
	for _, line := range lines {
		f := strings.Fields(line)
		if len(f) != 3 || f[2] != "User/"+sbxDeployerApplyUser {
			continue
		}
		parts := strings.Split(f[0], "/")
		if parts[0] == "ClusterRoleBinding" {
			problems = append(problems, f[0]+" binds User "+sbxDeployerApplyUser+"; the apply credential is bound by a RoleBinding in its namespace")
			continue
		}
		role := f[1]
		if len(parts) == 3 {
			role = "Role/" + parts[1] + "/" + strings.TrimPrefix(f[1], "Role/")
		}
		if !slices.Contains(lines, role) {
			problems = append(problems, f[0]+" refs "+f[1]+", which the manifest does not hold")
			continue
		}
		for _, rule := range lines {
			rf := strings.Fields(rule)
			if len(rf) < 3 || rf[0] != role {
				continue
			}
			if strings.HasSuffix(rf[1], "/secrets") {
				problems = append(problems, role+" grants "+rf[1])
			}
			if slices.Contains(deployerRemoveVerbs, rf[2]) {
				problems = append(problems, role+" grants "+rf[2]+" on "+rf[1])
			}
		}
	}
	return problems
}

// TestSbxDeployerRBACApplyReach holds the apply credential to its bound: no
// binding that names it is a ClusterRoleBinding or refs an object the manifest
// does not hold, and no rule it reaches grants secrets or a removing verb; the
// unmutated manifest passes and each planted refusal is reported.
func TestSbxDeployerRBACApplyReach(t *testing.T) {
	t.Parallel()
	if problems := sbxDeployerApplyProblems(loadSbxDeployerRBAC(t, sbxDeployerRBACFile)); len(problems) > 0 {
		t.Fatalf("%s as written is refused: %q", sbxDeployerRBACFile, problems)
	}
	raw, err := os.ReadFile(sbxDeployerRBACFile)
	if err != nil {
		t.Fatalf("reading %s: %v", sbxDeployerRBACFile, err)
	}
	rows := []struct{ what, old, new, want string }{
		{"a ClusterRoleBinding for the apply User", "kind: RoleBinding", "kind: ClusterRoleBinding", "ClusterRoleBinding/openshell/gt-sbx-deployer-apply binds User gt-deployer-apply"},
		{"a secrets rule", `    verbs: ["get", "list"]`, "    verbs: [\"get\", \"list\"]\n  - apiGroups: [\"\"]\n    resources: [\"secrets\"]\n    verbs: [\"get\"]", "Role/openshell/gt-sbx-deployer-apply grants core/secrets"},
		{"a delete verb", `    verbs: ["get", "list"]`, `    verbs: ["get", "list", "delete"]`, "Role/openshell/gt-sbx-deployer-apply grants delete on apps/replicasets"},
		{"a roleRef the manifest does not hold", "kind: Role\nmetadata:\n  name: gt-sbx-deployer-apply", "kind: Role\nmetadata:\n  name: gt-sbx-deployer-apply-absent", "RoleBinding/openshell/gt-sbx-deployer-apply refs Role/gt-sbx-deployer-apply, which the manifest does not hold"},
	}
	for _, row := range rows {
		t.Run(row.what, func(t *testing.T) {
			t.Parallel()
			edited := strings.Replace(string(raw), row.old, row.new, 1)
			if edited == string(raw) {
				t.Fatalf("%q matched nothing in %s", row.old, sbxDeployerRBACFile)
			}
			planted, err := sbxDeployerParse(sbxDeployerRBACFile, []byte(edited))
			if err != nil {
				t.Fatal(err)
			}
			if problems := sbxDeployerApplyProblems(planted); !slices.ContainsFunc(problems, func(p string) bool { return strings.Contains(p, row.want) }) {
				t.Fatalf("with %s the reach was not refused; want a problem containing %q, got %q", row.what, row.want, problems)
			}
		})
	}
}

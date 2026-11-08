package daemon

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

// The daemon pod makes the sweep's kubectl calls: it creates and deletes the
// Job, the pods and the claim, execs into them, and reads the capacity and
// priority the launch needs. deploy/k8s/sweep-rbac.yaml holds the Roles and
// ClusterRole those calls run under; this package binds the pod's service
// account to them and copies no rule. The tests below hold the render to the
// reads internal/sweeprunner/testdata/rbac-reads.golden records, read as files
// because the import rule keeps those packages out of the renderer.
const (
	sweepRBACFile  = "deploy/k8s/sweep-rbac.yaml"
	sweepReadsFile = "internal/sweeprunner/testdata/rbac-reads.golden"
)

// rbacRule is one rule of a Role or ClusterRole.
type rbacRule struct {
	APIGroups []string `yaml:"apiGroups"`
	Resources []string `yaml:"resources"`
	Verbs     []string `yaml:"verbs"`
}

// rbacDoc is the part of one manifest document the coverage check reads.
type rbacDoc struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name      string `yaml:"name"`
		Namespace string `yaml:"namespace"`
	} `yaml:"metadata"`
	Rules []rbacRule `yaml:"rules"`
}

// repoFile reads a file named from the repository root, so the check reads the
// manifest and the golden beside the packages that own them.
func repoFile(t *testing.T, rel string) []byte {
	t.Helper()
	path := filepath.Join(packageDir(t), "..", "..", "..", filepath.FromSlash(rel))
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", rel, err)
	}
	return data
}

// loadSweepRBAC parses every document of the manifest.
func loadSweepRBAC(t *testing.T) []rbacDoc {
	t.Helper()
	dec := yaml.NewDecoder(bytes.NewReader(repoFile(t, sweepRBACFile)))
	var docs []rbacDoc
	for {
		var doc rbacDoc
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("%s is not valid YAML: %v", sweepRBACFile, err)
		}
		if doc.Kind != "" {
			docs = append(docs, doc)
		}
	}
	if len(docs) == 0 {
		t.Fatalf("%s holds no objects, so the coverage check would pass for the wrong reason", sweepRBACFile)
	}
	return docs
}

// loadSweepReads reads the golden's lines, skipping comments and blanks.
func loadSweepReads(t *testing.T) []string {
	t.Helper()
	var lines []string
	for _, line := range strings.Split(string(repoFile(t, sweepReadsFile)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if slices.Contains(lines, line) {
			t.Fatalf("%s lists %q twice", sweepReadsFile, line)
		}
		lines = append(lines, line)
	}
	if len(lines) == 0 {
		t.Fatalf("%s holds no reads, so the coverage check would pass for the wrong reason", sweepReadsFile)
	}
	return lines
}

// boundRole is one Role or ClusterRole the render binds the pod's service
// account to.
type boundRole struct {
	kind      string
	name      string
	namespace string
}

// daemonBoundRoles lists the roles the rendered bindings give the daemon's
// service account. A binding that names another subject, or is not a Role, is
// left out, so the coverage check fails on the calls it would have covered.
func daemonBoundRoles(m Manifests) []boundRole {
	account := m.Namespace.Metadata.Name
	subject := func(subjects []Subject) bool {
		return slices.ContainsFunc(subjects, func(s Subject) bool {
			return s.Kind == "ServiceAccount" && s.Name == ServiceAccountName && s.Namespace == account
		})
	}
	var bound []boundRole
	for _, b := range []RoleBinding{m.SweepRoleBinding, m.CacheTrimRoleBinding} {
		if b.RoleRef.Kind == "Role" && subject(b.Subjects) {
			bound = append(bound, boundRole{kind: "Role", name: b.RoleRef.Name, namespace: b.Metadata.Namespace})
		}
	}
	if m.SweepReaderBinding.RoleRef.Kind == "ClusterRole" && subject(m.SweepReaderBinding.Subjects) {
		bound = append(bound, boundRole{kind: "ClusterRole", name: m.SweepReaderBinding.RoleRef.Name})
	}
	return bound
}

// findRBAC returns the document of the kind, name and namespace, or nil.
func findRBAC(docs []rbacDoc, kind, name, namespace string) *rbacDoc {
	for i := range docs {
		d := &docs[i]
		if d.Kind == kind && d.Metadata.Name == name && d.Metadata.Namespace == namespace {
			return d
		}
	}
	return nil
}

// rbacAllows reports whether a rule grants verb on group's resource.
func rbacAllows(rules []rbacRule, group, resource, verb string) bool {
	for _, r := range rules {
		if slices.Contains(r.APIGroups, group) && slices.Contains(r.Resources, resource) && slices.Contains(r.Verbs, verb) {
			return true
		}
	}
	return false
}

// rbacResourceGroup is the API group each resource the sweep reads belongs to.
// A resource missing here is an error: the check does not guess a group for a
// call it has not been taught.
var rbacResourceGroup = map[string]string{
	"nodes":                  "",
	"nodes/proxy":            "",
	"pods":                   "",
	"pods/exec":              "",
	"persistentvolumeclaims": "",
	"jobs":                   "batch",
	"priorityclasses":        "scheduling.k8s.io",
}

// rbacVerbsOf is what RBAC verbs each kubectl verb needs on its resource: get
// with no name is a list, apply reads then creates or patches, wait reads,
// lists and watches, delete reads, lists and deletes, and cp copies through
// pods/exec as kubectl does. raw is one GET, so get alone.
var rbacVerbsOf = map[string][]string{
	"get":    {"get", "list"},
	"raw":    {"get"},
	"apply":  {"get", "create", "patch"},
	"create": {"create"},
	"wait":   {"get", "list", "watch"},
	"delete": {"get", "list", "delete"},
	"exec":   {"create"},
	"cp":     {"create"},
}

// uncoveredSweepReads reports every golden line whose verbs are not granted in
// its scope by the roles the daemon's service account is bound to, and every
// binding whose role the manifest does not define. A scope named "namespace" is
// the sweep's own namespace, SweepNamespace; any other scope is a namespace
// name. A cluster-wide read may only use the ClusterRole's rules.
func uncoveredSweepReads(docs []rbacDoc, bound []boundRole, golden []string) ([]string, error) {
	var findings []string
	var clusterRules []rbacRule
	namespaced := map[string][]rbacRule{}
	for _, b := range bound {
		role := findRBAC(docs, b.kind, b.name, b.namespace)
		if role == nil {
			findings = append(findings, fmt.Sprintf("%s defines no %s %q in %q, which the daemon's service account is bound to", sweepRBACFile, b.kind, b.name, b.namespace))
			continue
		}
		if b.kind == "ClusterRole" {
			clusterRules = append(clusterRules, role.Rules...)
			continue
		}
		namespaced[b.namespace] = append(namespaced[b.namespace], role.Rules...)
	}
	for _, line := range golden {
		fields := strings.Fields(line)
		if len(fields) != 3 {
			return nil, fmt.Errorf("%s: %q is not `<verb> <resource> <scope>`", sweepReadsFile, line)
		}
		verb, resource, scope := fields[0], fields[1], fields[2]
		group, known := rbacResourceGroup[resource]
		if !known {
			return nil, fmt.Errorf("%s: resource %q has no API group in rbacResourceGroup", sweepReadsFile, resource)
		}
		verbs, known := rbacVerbsOf[verb]
		if !known {
			return nil, fmt.Errorf("%s: verb %q has no RBAC mapping", sweepReadsFile, verb)
		}
		rules, source := clusterRules, "the ClusterRole the daemon's service account is bound to"
		if scope != "cluster" {
			ns := scope
			if scope == "namespace" {
				ns = SweepNamespace
			}
			rules = append(slices.Clone(rules), namespaced[ns]...)
			source = fmt.Sprintf("the roles the daemon's service account is bound to in %q", ns)
		}
		for _, v := range verbs {
			if !rbacAllows(rules, group, resource, v) {
				findings = append(findings, fmt.Sprintf("%q: no rule in %s grants %q on %s/%s, so removing it strands the call", line, source, v, group, resource))
			}
		}
	}
	return findings, nil
}

// TestServiceAccountCoversEverySweepRead holds the rendered bindings and the
// manifest together: every read the sweep makes is granted through a role the
// daemon's service account is bound to, and removing a rule reports the call it
// leaves uncovered.
func TestServiceAccountCoversEverySweepRead(t *testing.T) {
	t.Parallel()
	docs := loadSweepRBAC(t)
	golden := loadSweepReads(t)
	bound := daemonBoundRoles(mustBuild(t, goldenValues(t)))
	if len(bound) == 0 {
		t.Fatal("the render binds the daemon's service account to no role, so every sweep read is uncovered")
	}

	findings, err := uncoveredSweepReads(docs, bound, golden)
	if err != nil {
		t.Fatalf("the coverage check cannot read a golden line: %v", err)
	}
	for _, finding := range findings {
		t.Error(finding)
	}

	// A rule removed from a bound Role must fail, naming the call it strands.
	broken := slices.Clone(docs)
	for i := range broken {
		if broken[i].Kind == "Role" && broken[i].Metadata.Name == CacheTrimRole {
			broken[i].Rules = slices.DeleteFunc(slices.Clone(broken[i].Rules), func(r rbacRule) bool {
				return slices.Contains(r.Resources, "pods/exec")
			})
		}
	}
	findings, err = uncoveredSweepReads(broken, bound, []string{"exec pods/exec forgejo"})
	if err != nil {
		t.Fatalf("the coverage check cannot read the fixture: %v", err)
	}
	if !slices.ContainsFunc(findings, func(f string) bool { return strings.Contains(f, "exec pods/exec forgejo") }) {
		t.Errorf("removing the pods/exec rule reported %q, want a finding naming the call", findings)
	}
}

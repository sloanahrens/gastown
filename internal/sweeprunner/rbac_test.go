package sweeprunner

import (
	"bytes"
	"encoding/json"
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

// The sweep's cluster access is written down in deploy/k8s/sweep-rbac.yaml and
// held to the kubectl reads the Kubernetes backend renders. The golden file
// lists one line per distinct read as `<verb> <resource> <scope>`; the tests
// below derive those lines from the rendered plan, and prove the manifest
// grants each one (gt-gg9hv.55).
const (
	sweepRBACManifest = "../../deploy/k8s/sweep-rbac.yaml"
	sweepReadsGolden  = "testdata/rbac-reads.golden"
	sweepReaderRole   = "gt-sweep-reader"
	sweepRunnerNS     = "gt-sweeps"
	sweepRunnerRole   = "sweep-runner"
)

// rbacResourceGroup is the API group each resource the sweep's kubectl calls
// touch belongs to. A resource missing here is an error: the check does not
// guess a group for a call it has not been taught.
var rbacResourceGroup = map[string]string{
	"nodes":                  "",
	"pods":                   "",
	"pods/exec":              "",
	"persistentvolumeclaims": "",
	"jobs":                   "batch",
	"priorityclasses":        "scheduling.k8s.io",
}

// rbacCLIResource is the resource a kubectl command names by its singular
// form on the command line, such as `delete pod`.
var rbacCLIResource = map[string]string{
	"pod":                   "pods",
	"job":                   "jobs",
	"persistentvolumeclaim": "persistentvolumeclaims",
}

// rbacKindResource is the resource an applied document's Kind names.
var rbacKindResource = map[string]string{
	"Pod":                   "pods",
	"Job":                   "jobs",
	"PersistentVolumeClaim": "persistentvolumeclaims",
}

// rbacVerbsOf is what RBAC verbs each kubectl verb needs on its resource. A get
// with no object name is a list, so the golden's get needs both verbs; apply
// reads the object, then patches it or creates it; cp copies through the pod's
// exec subresource, as kubectl does.
var rbacVerbsOf = map[string][]string{
	"get":    {"get", "list"},
	"apply":  {"get", "create", "patch"},
	"create": {"create"},
	"wait":   {"get", "watch"},
	"delete": {"delete"},
	"exec":   {"create"},
	"cp":     {"create"},
}

// rbacRead is one kubectl read the rendered plan makes, named by the golden
// line it contributes and the argv it came from, so a mismatch names the call.
type rbacRead struct {
	line string
	argv []string
}

// rbacReads returns the golden line for each resource one kubectl call touches.
// A call it cannot read (not a kubectl argv against a context, a verb or
// resource it has no mapping for, an applied document with an unknown Kind) is
// an error, so an unmapped call fails the test instead of passing unchecked.
func rbacReads(argv []string, stdin []byte) ([]string, error) {
	if len(argv) < 4 || argv[0] != kubectl || argv[1] != "--context" {
		return nil, fmt.Errorf("%q is not a kubectl call against a context", argv)
	}
	verb, args := argv[3], argv[4:]
	scope := "namespace"
	if slices.Contains(args, "-A") {
		scope = "cluster"
	}
	var resources []string
	switch verb {
	case "get":
		// `get job NAME` names the singular on the command line; `get nodes,pods`
		// names the plural. Both resolve through the same table.
		for _, r := range strings.Split(args[0], ",") {
			if plural, ok := rbacCLIResource[r]; ok {
				r = plural
			}
			resources = append(resources, r)
		}
	case "apply", "create":
		var doc struct {
			Kind string `json:"kind"`
		}
		if err := json.Unmarshal(stdin, &doc); err != nil {
			return nil, fmt.Errorf("%q: the applied document does not parse: %w", argv, err)
		}
		r, ok := rbacKindResource[doc.Kind]
		if !ok {
			return nil, fmt.Errorf("%q: no RBAC resource for applied Kind %q", argv, doc.Kind)
		}
		resources = []string{r}
	case "wait":
		for _, a := range args {
			if strings.HasPrefix(a, "-") {
				continue
			}
			singular, _, _ := strings.Cut(a, "/")
			r, ok := rbacCLIResource[singular]
			if !ok {
				return nil, fmt.Errorf("%q: no RBAC resource for %q", argv, a)
			}
			resources = append(resources, r)
			break
		}
	case "delete":
		r, ok := rbacCLIResource[args[0]]
		if !ok {
			return nil, fmt.Errorf("%q: no RBAC resource for %q", argv, args[0])
		}
		resources = []string{r}
	case "exec", "cp":
		resources = []string{"pods/exec"}
	default:
		return nil, fmt.Errorf("%q: verb %q has no RBAC mapping", argv, verb)
	}
	lines := make([]string, 0, len(resources))
	for _, r := range resources {
		if _, ok := rbacResourceGroup[r]; !ok {
			return nil, fmt.Errorf("%q: resource %q has no API group in rbacResourceGroup", argv, r)
		}
		lines = append(lines, verb+" "+r+" "+scope)
	}
	return lines, nil
}

// plannedKubectlSteps is every step of a plan that runs kubectl, in the order
// the caller runs them.
func plannedKubectlSteps(plan Plan) []Step {
	var steps []Step
	if plan.Preflight != nil {
		steps = append(steps, plan.Preflight.Step)
	}
	for _, g := range plan.Guards {
		steps = append(steps, g.Step)
	}
	steps = append(steps, plan.Apply...)
	steps = append(steps, plan.Steps...)
	if plan.Poll != nil {
		steps = append(steps, plan.Poll.Status)
		steps = append(steps, plan.Poll.Collect...)
	}
	return steps
}

// renderedReads maps each distinct golden line the plan needs to the first
// call that needs it.
func renderedReads(t *testing.T, plan Plan) map[string]rbacRead {
	t.Helper()
	reads := map[string]rbacRead{}
	for _, step := range plannedKubectlSteps(plan) {
		lines, err := rbacReads(step.Argv, step.Stdin)
		if err != nil {
			t.Fatalf("the rendered plan has a call the RBAC check cannot read: %v", err)
		}
		for _, line := range lines {
			if _, seen := reads[line]; !seen {
				reads[line] = rbacRead{line: line, argv: step.Argv}
			}
		}
	}
	return reads
}

// loadReadsGolden reads the golden file's lines, skipping blanks and comments,
// and fails on a line repeated.
func loadReadsGolden(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.FromSlash(sweepReadsGolden))
	if err != nil {
		t.Fatalf("reading %s: %v", sweepReadsGolden, err)
	}
	var lines []string
	for _, l := range strings.Split(string(raw), "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		if slices.Contains(lines, l) {
			t.Fatalf("%s lists %q twice", sweepReadsGolden, l)
		}
		lines = append(lines, l)
	}
	return lines
}

// rbacDoc is the part of one manifest document the RBAC checks read.
type rbacDoc struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name        string            `yaml:"name"`
		Namespace   string            `yaml:"namespace"`
		Annotations map[string]string `yaml:"annotations"`
	} `yaml:"metadata"`
	Rules   []rbacRule `yaml:"rules"`
	RoleRef struct {
		Kind string `yaml:"kind"`
		Name string `yaml:"name"`
	} `yaml:"roleRef"`
	Subjects []struct {
		Kind      string `yaml:"kind"`
		Name      string `yaml:"name"`
		Namespace string `yaml:"namespace"`
	} `yaml:"subjects"`
}

type rbacRule struct {
	APIGroups []string `yaml:"apiGroups"`
	Resources []string `yaml:"resources"`
	Verbs     []string `yaml:"verbs"`
}

// loadSweepRBAC parses every document of the manifest. A file that is not valid
// YAML fails here.
func loadSweepRBAC(t *testing.T) []rbacDoc {
	t.Helper()
	raw, err := os.ReadFile(filepath.FromSlash(sweepRBACManifest))
	if err != nil {
		t.Fatalf("reading %s: %v", sweepRBACManifest, err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	var docs []rbacDoc
	for {
		var doc rbacDoc
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			return docs
		}
		if err != nil {
			t.Fatalf("%s is not valid YAML: %v", sweepRBACManifest, err)
		}
		if doc.Kind != "" {
			docs = append(docs, doc)
		}
	}
}

// findRBAC returns the document of the given kind, name and namespace, or nil
// when the manifest does not hold it.
func findRBAC(docs []rbacDoc, kind, name, namespace string) *rbacDoc {
	for i := range docs {
		d := &docs[i]
		if d.Kind == kind && d.Metadata.Name == name && d.Metadata.Namespace == namespace {
			return d
		}
	}
	return nil
}

// rbacAllows reports whether one of the rules grants verb on group's resource.
// A wildcard is not understood, so a rule that uses one grants nothing here.
func rbacAllows(rules []rbacRule, group, resource, verb string) bool {
	for _, r := range rules {
		if slices.Contains(r.APIGroups, group) && slices.Contains(r.Resources, resource) && slices.Contains(r.Verbs, verb) {
			return true
		}
	}
	return false
}

// TestSweepRBACManifestParses holds the manifest to a parseable set of the
// objects the sweep runs with: the namespace, the runner's service account,
// the token Secret, the namespaced Role and its binding, and the read-only
// ClusterRole with its binding to the runner.
func TestSweepRBACManifestParses(t *testing.T) {
	t.Parallel()
	docs := loadSweepRBAC(t)
	if len(docs) == 0 {
		t.Fatalf("%s holds no objects", sweepRBACManifest)
	}
	want := []struct{ kind, name, namespace string }{
		{"Namespace", sweepRunnerNS, ""},
		{"ClusterRole", sweepReaderRole, ""},
		{"ClusterRoleBinding", sweepReaderRole, ""},
	}
	for _, w := range want {
		if findRBAC(docs, w.kind, w.name, w.namespace) == nil {
			t.Errorf("%s has no %s %q", sweepRBACManifest, w.kind, w.name)
		}
	}
	binding := findRBAC(docs, "ClusterRoleBinding", sweepReaderRole, "")
	if binding == nil {
		return
	}
	if binding.RoleRef.Kind != "ClusterRole" || binding.RoleRef.Name != sweepReaderRole {
		t.Errorf("ClusterRoleBinding %s refs %s %q, want ClusterRole %q", sweepReaderRole, binding.RoleRef.Kind, binding.RoleRef.Name, sweepReaderRole)
	}
	if len(binding.Subjects) != 1 || binding.Subjects[0].Kind != "ServiceAccount" ||
		binding.Subjects[0].Name != sweepRunnerRole || binding.Subjects[0].Namespace != sweepRunnerNS {
		t.Errorf("ClusterRoleBinding %s binds %+v, want ServiceAccount %s/sweep-runner", sweepReaderRole, binding.Subjects, sweepRunnerNS)
	}
}

// TestSweepRBACGoldenMatchesTheRenderedPlan renders the sample Spec and holds
// the golden file to the reads it makes. A kubectl call added to the plan with
// no golden line fails and names the call; a golden line no call needs fails.
func TestSweepRBACGoldenMatchesTheRenderedPlan(t *testing.T) {
	t.Parallel()
	plan, err := Kubernetes{}.Render(goldenSpec())
	if err != nil {
		t.Fatalf("rendering the golden spec: %v", err)
	}
	rendered := renderedReads(t, plan)
	golden := loadReadsGolden(t)
	for line, read := range rendered {
		if !slices.Contains(golden, line) {
			t.Errorf("the rendered plan reads %q (from %q) and %s has no line for it", line, read.argv, sweepReadsGolden)
		}
	}
	for _, line := range golden {
		if _, ok := rendered[line]; !ok {
			t.Errorf("%s has %q, which no rendered call needs", sweepReadsGolden, line)
		}
	}
}

// TestSweepRBACCoversEveryRead proves every golden line is allowed by the
// manifest: a namespaced read by the Role or the ClusterRole, a cluster-wide
// read by the ClusterRole. A missing Role fails every namespaced line by name.
func TestSweepRBACCoversEveryRead(t *testing.T) {
	t.Parallel()
	docs := loadSweepRBAC(t)
	clusterReader := findRBAC(docs, "ClusterRole", sweepReaderRole, "")
	if clusterReader == nil {
		t.Fatalf("%s has no ClusterRole %q", sweepRBACManifest, sweepReaderRole)
	}
	var namespaced []rbacRule
	if role := findRBAC(docs, "Role", sweepRunnerRole, sweepRunnerNS); role != nil {
		namespaced = role.Rules
	}
	for _, line := range loadReadsGolden(t) {
		fields := strings.Fields(line)
		if len(fields) != 3 {
			t.Fatalf("%s: %q is not `<verb> <resource> <scope>`", sweepReadsGolden, line)
		}
		verb, resource, scope := fields[0], fields[1], fields[2]
		group, known := rbacResourceGroup[resource]
		if !known {
			t.Errorf("golden line %q: resource %q has no API group in rbacResourceGroup", line, resource)
			continue
		}
		verbs, known := rbacVerbsOf[verb]
		if !known {
			t.Errorf("golden line %q: verb %q has no RBAC mapping", line, verb)
			continue
		}
		rules := clusterReader.Rules
		source := "ClusterRole " + sweepReaderRole
		if scope == "namespace" {
			rules = append(slices.Clone(namespaced), rules...)
			source = "Role " + sweepRunnerRole + " or ClusterRole " + sweepReaderRole
		}
		for _, v := range verbs {
			if !rbacAllows(rules, group, resource, v) {
				t.Errorf("golden line %q is not allowed: no rule in %s grants %q on %s/%s", line, source, v, group, resource)
			}
		}
	}
}

// TestSweepRBACClusterGrantIsReadOnly keeps the cluster-wide grant to get, list and
// watch on nodes, pods and priorityclasses, each in its own API group.
func TestSweepRBACClusterGrantIsReadOnly(t *testing.T) {
	t.Parallel()
	docs := loadSweepRBAC(t)
	reader := findRBAC(docs, "ClusterRole", sweepReaderRole, "")
	if reader == nil {
		t.Fatalf("%s has no ClusterRole %q", sweepRBACManifest, sweepReaderRole)
	}
	for _, rule := range reader.Rules {
		for _, v := range rule.Verbs {
			if v != "get" && v != "list" && v != "watch" {
				t.Errorf("ClusterRole %s grants %q, want only get, list and watch", sweepReaderRole, v)
			}
		}
		for _, res := range rule.Resources {
			group, known := rbacResourceGroup[res]
			if !known || (res != "nodes" && res != "pods" && res != "priorityclasses") {
				t.Errorf("ClusterRole %s grants resource %q, want only nodes, pods and priorityclasses", sweepReaderRole, res)
				continue
			}
			if !slices.Contains(rule.APIGroups, group) {
				t.Errorf("ClusterRole %s grants %q in %q, want group %q", sweepReaderRole, res, rule.APIGroups, group)
			}
		}
	}
}

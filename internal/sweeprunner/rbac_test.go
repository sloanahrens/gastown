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

// sweepRoleGrant is what the namespaced Role may grant one resource: the API
// group it belongs to and the verbs the sweep needs on it.
type sweepRoleGrant struct {
	group string
	verbs []string
}

// sweepRoleAllowed is the exact set the namespaced Role may grant, one entry
// per resource. Most of it follows from the golden's namespaced reads through
// rbacResourceGroup and rbacVerbsOf; the rest is sweepRoleExtras. It is written
// out here so a widening needs a deliberate edit of this table as well as the
// manifest, and TestSweepRBACRoleAllowedMatchesTheGolden fails if the table,
// the golden and the extras disagree in either direction.
var sweepRoleAllowed = map[string]sweepRoleGrant{
	"jobs":                   {"batch", []string{"get", "list", "create", "patch", "delete", "watch", "update"}},
	"pods":                   {"", []string{"get", "list", "watch", "create", "patch", "delete", "deletecollection"}},
	"pods/exec":              {"", []string{"create"}},
	"pods/log":               {"", []string{"get"}},
	"persistentvolumeclaims": {"", []string{"get", "list", "create", "patch", "delete", "watch", "update"}},
}

// sweepGrant is one API group, resource and RBAC verb the golden needs.
type sweepGrant struct {
	group    string
	resource string
	verb     string
}

// sweepRoleExtra is a grant sweepRoleAllowed makes that no golden line accounts
// for, with why it is allowed. The golden renders the host's kubectl calls; a run
// also has an in-cluster side the golden cannot see.
type sweepRoleExtra struct {
	group    string
	resource string
	verb     string
	reason   string
}

// sweepRoleExtras is the whole of what the Role may grant beyond the golden's
// reads. Each entry says why it is allowed, and the list stays short: the bound is
// the golden plus this list, so a wider Role fails
// TestSweepRBACRoleGrantsOnlyWhatTheSweepNeeds. An entry the golden comes to
// explain is stale and fails TestSweepRBACRoleAllowedMatchesTheGolden.
var sweepRoleExtras = []sweepRoleExtra{
	{
		group: "batch", resource: "jobs", verb: "watch",
		reason: "the live Role (gt-gg9hv.55) grants it for the run's Job lifecycle; the golden renders the host's kubectl calls, so no line accounts for it",
	},
	{
		group: "batch", resource: "jobs", verb: "update",
		reason: "the live Role (gt-gg9hv.55) grants it for the run's Job lifecycle; the golden renders the host's kubectl calls, so no line accounts for it",
	},
	{
		group: "", resource: "pods", verb: "deletecollection",
		reason: "kubectl delete by label selector, the teardown's `delete pod -l`, is a collection delete; the golden's `delete pods namespace` line cannot tell it from the deletes by name",
	},
	{
		group: "", resource: "pods/log", verb: "get",
		reason: "the Job's own log read, which the bead names beside cp and exec; the host copies logs with kubectl cp from the output claim, so the golden has no log line",
	},
	{
		group: "", resource: "persistentvolumeclaims", verb: "list",
		reason: "the live Role (gt-gg9hv.55) grants it; no rendered call lists a claim, and it is kept so the manifest matches the cluster",
	},
	{
		group: "", resource: "persistentvolumeclaims", verb: "watch",
		reason: "the live Role (gt-gg9hv.55) grants it; the host applies, reads through a pod and deletes the claim, so the golden renders no watch",
	},
	{
		group: "", resource: "persistentvolumeclaims", verb: "update",
		reason: "the live Role (gt-gg9hv.55) grants it; the host applies, reads through a pod and deletes the claim, so the golden renders no update",
	},
}

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
// with no object name is a list, so the golden's get needs both verbs; wait
// lists and then watches the object it waits on; apply reads the object, then
// patches it or creates it; cp copies through the pod's exec subresource, as
// kubectl does.
var rbacVerbsOf = map[string][]string{
	"get":    {"get", "list"},
	"apply":  {"get", "create", "patch"},
	"create": {"create"},
	"wait":   {"get", "list", "watch"},
	"delete": {"delete"},
	"exec":   {"create"},
	"cp":     {"create"},
}

// sweepRoleGoldenGrants returns every grant the golden's namespaced lines need,
// resolved through rbacResourceGroup and rbacVerbsOf. A line with no resource
// group or verb mapping is an error, so a call the test has not been taught
// fails instead of passing unchecked.
func sweepRoleGoldenGrants(golden []string) ([]sweepGrant, error) {
	var grants []sweepGrant
	for _, line := range golden {
		fields := strings.Fields(line)
		if len(fields) != 3 {
			return nil, fmt.Errorf("%s: %q is not `<verb> <resource> <scope>`", sweepReadsGolden, line)
		}
		if fields[2] != "namespace" {
			continue
		}
		group, known := rbacResourceGroup[fields[1]]
		if !known {
			return nil, fmt.Errorf("%s: resource %q has no API group in rbacResourceGroup", sweepReadsGolden, fields[1])
		}
		verbs, known := rbacVerbsOf[fields[0]]
		if !known {
			return nil, fmt.Errorf("%s: verb %q has no RBAC mapping", sweepReadsGolden, fields[0])
		}
		for _, verb := range verbs {
			grants = append(grants, sweepGrant{group: group, resource: fields[1], verb: verb})
		}
	}
	return grants, nil
}

// sweepRoleExplained reports whether a golden namespaced line or one of
// sweepRoleExtras accounts for a grant in sweepRoleAllowed.
func sweepRoleExplained(required []sweepGrant, group, resource, verb string) bool {
	for _, g := range required {
		if g.group == group && g.resource == resource && g.verb == verb {
			return true
		}
	}
	for _, e := range sweepRoleExtras {
		if e.group == group && e.resource == resource && e.verb == verb {
			return true
		}
	}
	return false
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

// sweepRoleWideningKind names one way a rule grants more than the bound.
type sweepRoleWideningKind int

const (
	sweepRoleWildcardAPIGroup sweepRoleWideningKind = iota
	sweepRoleWildcardResource
	sweepRoleWildcardVerb
	sweepRoleUnknownResource
	sweepRoleWrongAPIGroup
	sweepRoleVerbOutsideBound
)

func (k sweepRoleWideningKind) String() string {
	switch k {
	case sweepRoleWildcardAPIGroup:
		return "wildcard API group"
	case sweepRoleWildcardResource:
		return "wildcard resource"
	case sweepRoleWildcardVerb:
		return "wildcard verb"
	case sweepRoleUnknownResource:
		return "resource outside the bound"
	case sweepRoleWrongAPIGroup:
		return "resource in the wrong API group"
	case sweepRoleVerbOutsideBound:
		return "verb outside the bound"
	}
	return fmt.Sprintf("unknown widening kind %d", int(k))
}

// sweepRoleWidening is one grant in a rule that the bound does not hold. The
// fields name what the rule grants, so a test asserts on them rather than on
// the wording of a message.
type sweepRoleWidening struct {
	Rule     int // the rule's 1-based position in the Role
	Kind     sweepRoleWideningKind
	APIGroup string
	Resource string
	Verb     string
}

func (w sweepRoleWidening) String() string {
	switch w.Kind {
	case sweepRoleWildcardAPIGroup:
		return fmt.Sprintf("rule %d grants every API group with a wildcard", w.Rule)
	case sweepRoleWildcardResource:
		return fmt.Sprintf("rule %d grants every resource with a wildcard", w.Rule)
	case sweepRoleWildcardVerb:
		return fmt.Sprintf("rule %d grants every verb with a wildcard", w.Rule)
	case sweepRoleUnknownResource:
		return fmt.Sprintf("rule %d grants resource %q, which the sweep does not read", w.Rule, w.Resource)
	case sweepRoleWrongAPIGroup:
		return fmt.Sprintf("rule %d grants %q in API group %q, which is not its group", w.Rule, w.Resource, w.APIGroup)
	case sweepRoleVerbOutsideBound:
		return fmt.Sprintf("rule %d grants %q on %q, which the sweep does not need", w.Rule, w.Verb, w.Resource)
	}
	return fmt.Sprintf("rule %d has an unknown widening", w.Rule)
}

// sweepRoleWidenings reports every grant in a namespaced Role that the bound
// does not hold: a wildcard in any of a rule's apiGroups, resources or verbs,
// and each granted group, resource or verb outside the set. Every rule is read,
// so a rule with several problems reports each of them.
func sweepRoleWidenings(rules []rbacRule, bound map[string]sweepRoleGrant) []sweepRoleWidening {
	var widened []sweepRoleWidening
	for i, rule := range rules {
		n := i + 1
		if slices.Contains(rule.APIGroups, "*") {
			widened = append(widened, sweepRoleWidening{Rule: n, Kind: sweepRoleWildcardAPIGroup, APIGroup: "*"})
		}
		if slices.Contains(rule.Resources, "*") {
			widened = append(widened, sweepRoleWidening{Rule: n, Kind: sweepRoleWildcardResource, Resource: "*"})
		}
		if slices.Contains(rule.Verbs, "*") {
			widened = append(widened, sweepRoleWidening{Rule: n, Kind: sweepRoleWildcardVerb, Verb: "*"})
		}
		for _, resource := range rule.Resources {
			grant, allowed := bound[resource]
			if !allowed {
				widened = append(widened, sweepRoleWidening{Rule: n, Kind: sweepRoleUnknownResource, Resource: resource})
				continue
			}
			for _, group := range rule.APIGroups {
				if group != "*" && group != grant.group {
					widened = append(widened, sweepRoleWidening{Rule: n, Kind: sweepRoleWrongAPIGroup, APIGroup: group, Resource: resource})
				}
			}
			for _, verb := range rule.Verbs {
				if verb != "*" && !slices.Contains(grant.verbs, verb) {
					widened = append(widened, sweepRoleWidening{Rule: n, Kind: sweepRoleVerbOutsideBound, Resource: resource, Verb: verb})
				}
			}
		}
	}
	return widened
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

// TestSweepRBACRoleGrantsOnlyWhatTheSweepNeeds holds the namespaced Role to
// sweepRoleAllowed, so the Role cannot gain a resource, API group or verb that
// neither a golden line nor sweepRoleExtras names. A Role the manifest does not
// hold fails too: a renamed Role must not pass unchecked.
func TestSweepRBACRoleGrantsOnlyWhatTheSweepNeeds(t *testing.T) {
	t.Parallel()
	role := findRBAC(loadSweepRBAC(t), "Role", sweepRunnerRole, sweepRunnerNS)
	if role == nil {
		t.Fatalf("%s has no Role %q in namespace %q", sweepRBACManifest, sweepRunnerRole, sweepRunnerNS)
	}
	for _, widened := range sweepRoleWidenings(role.Rules, sweepRoleAllowed) {
		t.Errorf("Role %s in %s grants more than the sweep uses: %s", sweepRunnerRole, sweepRunnerNS, widened)
	}
}

// TestSweepRBACRoleAllowedMatchesTheGolden ties sweepRoleAllowed to the golden
// in both directions: every namespaced read the golden lists has an entry, and
// every entry is explained by a golden line or by sweepRoleExtras. A verb added
// to the table alone, or a golden line the table does not cover, fails here.
func TestSweepRBACRoleAllowedMatchesTheGolden(t *testing.T) {
	t.Parallel()
	required, err := sweepRoleGoldenGrants(loadReadsGolden(t))
	if err != nil {
		t.Fatalf("deriving the Role's bound from %s: %v", sweepReadsGolden, err)
	}

	// Golden -> table: every namespaced line's grant has an entry.
	for _, g := range required {
		grant, ok := sweepRoleAllowed[g.resource]
		if !ok {
			t.Errorf("%s needs %q, and sweepRoleAllowed has no entry for it", sweepReadsGolden, g.resource)
			continue
		}
		if grant.group != g.group {
			t.Errorf("%s needs %q in API group %q, and sweepRoleAllowed puts it in %q", sweepReadsGolden, g.resource, g.group, grant.group)
		}
		if !slices.Contains(grant.verbs, g.verb) {
			t.Errorf("%s needs %q on %q, and sweepRoleAllowed does not allow it", sweepReadsGolden, g.verb, g.resource)
		}
	}

	// Table -> golden or extras: every allowed verb is explained.
	for resource, grant := range sweepRoleAllowed {
		for _, verb := range grant.verbs {
			if !sweepRoleExplained(required, grant.group, resource, verb) {
				t.Errorf("sweepRoleAllowed allows %q on %q in group %q, and no golden line or sweepRoleExtra explains it", verb, resource, grant.group)
			}
		}
	}

	// Extras: each is in the table with a reason, and none is stale.
	for _, extra := range sweepRoleExtras {
		if strings.TrimSpace(extra.reason) == "" {
			t.Errorf("sweepRoleExtra %q on %q has no reason; an extra beyond the golden must say why it is allowed", extra.verb, extra.resource)
		}
		grant, ok := sweepRoleAllowed[extra.resource]
		if !ok || grant.group != extra.group || !slices.Contains(grant.verbs, extra.verb) {
			t.Errorf("sweepRoleExtra %q on %q in group %q is not in sweepRoleAllowed", extra.verb, extra.resource, extra.group)
		}
		for _, g := range required {
			if g.group == extra.group && g.resource == extra.resource && g.verb == extra.verb {
				t.Errorf("sweepRoleExtra %q on %q is already explained by %s; remove it", extra.verb, extra.resource, sweepReadsGolden)
				break
			}
		}
	}
}

// TestSweepRBACRoleCheckRejectsWidening drives each refusal sweepRoleWidenings
// makes against a clone of the Role the manifest holds, so a check that stopped
// noticing a widened rule fails here rather than on a future manifest edit.
func TestSweepRBACRoleCheckRejectsWidening(t *testing.T) {
	t.Parallel()
	role := findRBAC(loadSweepRBAC(t), "Role", sweepRunnerRole, sweepRunnerNS)
	if role == nil {
		t.Fatalf("%s has no Role %q in namespace %q", sweepRBACManifest, sweepRunnerRole, sweepRunnerNS)
	}
	// withResource returns a clone of the Role's rules with edit applied to the
	// first rule that grants resource, leaving the Role itself untouched.
	withResource := func(resource string, edit func(*rbacRule)) []rbacRule {
		t.Helper()
		rules := slices.Clone(role.Rules)
		for i := range rules {
			if !slices.Contains(rules[i].Resources, resource) {
				continue
			}
			rules[i] = rbacRule{
				APIGroups: slices.Clone(rules[i].APIGroups),
				Resources: slices.Clone(rules[i].Resources),
				Verbs:     slices.Clone(rules[i].Verbs),
			}
			edit(&rules[i])
			return rules
		}
		t.Fatalf("the Role has no rule granting %q", resource)
		return nil
	}
	rows := []struct {
		name  string
		rules []rbacRule
		want  sweepRoleWidening
	}{
		{
			name:  "secrets added to the claim rule",
			rules: withResource("persistentvolumeclaims", func(r *rbacRule) { r.Resources = append(r.Resources, "secrets") }),
			want:  sweepRoleWidening{Kind: sweepRoleUnknownResource, Resource: "secrets"},
		},
		{
			name: "secrets in a rule of their own",
			rules: append(slices.Clone(role.Rules), rbacRule{
				APIGroups: []string{""},
				Resources: []string{"secrets"},
				Verbs:     []string{"get"},
			}),
			want: sweepRoleWidening{Kind: sweepRoleUnknownResource, Resource: "secrets"},
		},
		{
			name:  "wildcard resource",
			rules: withResource("pods", func(r *rbacRule) { r.Resources = []string{"*"} }),
			want:  sweepRoleWidening{Kind: sweepRoleWildcardResource, Resource: "*"},
		},
		{
			name:  "wildcard verb",
			rules: withResource("pods", func(r *rbacRule) { r.Verbs = []string{"*"} }),
			want:  sweepRoleWidening{Kind: sweepRoleWildcardVerb, Verb: "*"},
		},
		{
			name:  "wildcard API group",
			rules: withResource("pods", func(r *rbacRule) { r.APIGroups = []string{"*"} }),
			want:  sweepRoleWidening{Kind: sweepRoleWildcardAPIGroup, APIGroup: "*"},
		},
		{
			name:  "verb outside the set",
			rules: withResource("pods", func(r *rbacRule) { r.Verbs = append(r.Verbs, "impersonate") }),
			want:  sweepRoleWidening{Kind: sweepRoleVerbOutsideBound, Resource: "pods", Verb: "impersonate"},
		},
		{
			name:  "resource in the wrong API group",
			rules: withResource("pods", func(r *rbacRule) { r.APIGroups = []string{"apps"} }),
			want:  sweepRoleWidening{Kind: sweepRoleWrongAPIGroup, APIGroup: "apps", Resource: "pods"},
		},
		{
			name:  "delete added to a resource the golden never deletes",
			rules: withResource("pods/log", func(r *rbacRule) { r.Verbs = append(r.Verbs, "delete") }),
			want:  sweepRoleWidening{Kind: sweepRoleVerbOutsideBound, Resource: "pods/log", Verb: "delete"},
		},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			widened := sweepRoleWidenings(row.rules, sweepRoleAllowed)
			if !slices.ContainsFunc(widened, func(w sweepRoleWidening) bool {
				return w.Kind == row.want.Kind && w.APIGroup == row.want.APIGroup &&
					w.Resource == row.want.Resource && w.Verb == row.want.Verb
			}) {
				t.Fatalf("widening by %s reports %+v, want %s on %+v", row.name, widened, row.want.Kind, row.want)
			}
		})
	}
}

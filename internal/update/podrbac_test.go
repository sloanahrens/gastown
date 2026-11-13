package update

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

// The pod daemon's kubectl calls are written down in deploy/k8s/update-rbac.yaml
// and held to testdata/pod-update-reads.golden, which lists one line per distinct
// call as `verb resource` in the gt namespace. The gt-update Role must grant every
// golden line and nothing else, and its Deployment rule is bound to the two
// Deployments the roll moves (gt-360i18). The gt-update-launch Role grants the
// daemon's Job verbs and nothing else (gt-1qnr01).
const (
	podRBACManifest   = "../../deploy/k8s/update-rbac.yaml"
	podReadsGolden    = "testdata/pod-update-reads.golden"
	podRBACNS         = "gt"
	podRBACName       = "gt-update"
	podRBACLaunchName = "gt-update-launch"
	podRBACDaemonSA   = "daemon"
)

// podRBACGroup is the API group each resource the pod daemon's kubectl calls touch
// belongs to. A resource missing here is a problem: the check does not guess a
// group for a call it has not been taught.
var podRBACGroup = map[string]string{
	"deployments": "apps",
	"pods":        "",
	"pods/exec":   "",
}

// podRBACBoundNames are the resources a rule may bind by resourceNames, and the names
// the gt-update Role binds each to: the two Deployments the roll moves. A rule on a
// bound resource must name exactly these, and a rule on any other resource names none.
var podRBACBoundNames = map[string][]string{
	"deployments": {"daemon", "dashboard"},
}

// podRBACLaunchGrants is every grant the gt-update-launch Role needs: the Job verbs
// the daemon uses to start the roll, and a read and a delete of the Jobs it made.
var podRBACLaunchGrants = []podRBACGrant{
	{group: "batch", resource: "jobs", verb: "create"},
	{group: "batch", resource: "jobs", verb: "get"},
	{group: "batch", resource: "jobs", verb: "list"},
	{group: "batch", resource: "jobs", verb: "watch"},
	{group: "batch", resource: "jobs", verb: "delete"},
}

// podRBACKinds are the only documents the manifest may hold.
var podRBACKinds = []string{"ServiceAccount", "Role", "RoleBinding"}

// podRBACWant is the one document of each kind and name the manifest holds: the
// gt-update ServiceAccount, its Role and RoleBinding, and the launch Role and the
// RoleBinding that binds it to the daemon's ServiceAccount.
var podRBACWant = []struct{ kind, name string }{
	{"ServiceAccount", podRBACName},
	{"Role", podRBACName},
	{"RoleBinding", podRBACName},
	{"Role", podRBACLaunchName},
	{"RoleBinding", podRBACLaunchName},
}

// podRBACSubjects is the one subject each RoleBinding must name, by the binding's name.
var podRBACSubjects = map[string]podRBACSubject{
	podRBACName:       {Kind: "ServiceAccount", Name: podRBACName, Namespace: podRBACNS},
	podRBACLaunchName: {Kind: "ServiceAccount", Name: podRBACDaemonSA, Namespace: podRBACNS},
}

type podRBACMeta struct {
	Name      string `yaml:"name"`
	Namespace string `yaml:"namespace"`
}

type podRBACSubject struct {
	Kind      string `yaml:"kind"`
	Name      string `yaml:"name"`
	Namespace string `yaml:"namespace"`
}

// podRBACRule is one rule of a Role. resourceNames and nonResourceURLs are decoded
// so that the grant check can refuse them: it does not model either.
type podRBACRule struct {
	APIGroups       []string `yaml:"apiGroups"`
	Resources       []string `yaml:"resources"`
	Verbs           []string `yaml:"verbs"`
	ResourceNames   []string `yaml:"resourceNames"`
	NonResourceURLs []string `yaml:"nonResourceURLs"`
}

// podRBACDoc is one document of the manifest, decoded to the fields the checks read.
type podRBACDoc struct {
	Kind     string        `yaml:"kind"`
	Metadata podRBACMeta   `yaml:"metadata"`
	Rules    []podRBACRule `yaml:"rules"`
	RoleRef  struct {
		Kind string `yaml:"kind"`
		Name string `yaml:"name"`
	} `yaml:"roleRef"`
	Subjects []podRBACSubject `yaml:"subjects"`
}

// podRBACGrant is one API group, resource and verb a golden line needs.
type podRBACGrant struct {
	group, resource, verb string
}

// String is the golden line the grant comes from, `verb resource`.
func (g podRBACGrant) String() string {
	return g.verb + " " + g.resource
}

// podRBACRefusal is a fixture edit that a check must refuse, and the substring of
// the problem it must name.
type podRBACRefusal struct {
	name string
	edit func(t *testing.T, docs []podRBACDoc) []podRBACDoc
	want string
}

// parsePodRBAC decodes every document of a manifest, kind-less ones included: the
// shape check refuses a document with no kind rather than letting it pass unread.
func parsePodRBAC(raw []byte) ([]podRBACDoc, error) {
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	var docs []podRBACDoc
	for {
		var doc podRBACDoc
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			return docs, nil
		}
		if err != nil {
			return nil, err
		}
		docs = append(docs, doc)
	}
}

// podRBACLines returns the golden's read lines: every line that is neither blank
// nor a comment.
func podRBACLines(raw []byte) []string {
	var lines []string
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") {
			lines = append(lines, line)
		}
	}
	return lines
}

// podRBACGrantsOf returns the grant each golden line needs. A line that is not
// `verb resource`, or that names a resource with no API group, is a problem and
// yields no grant.
func podRBACGrantsOf(golden []string) ([]podRBACGrant, []string) {
	var grants []podRBACGrant
	var problems []string
	for _, line := range golden {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			problems = append(problems, fmt.Sprintf("golden line %q is not `verb resource`", line))
			continue
		}
		group, known := podRBACGroup[fields[1]]
		if !known {
			problems = append(problems, fmt.Sprintf("golden line %q names resource %q, which has no API group in podRBACGroup", line, fields[1]))
			continue
		}
		grants = append(grants, podRBACGrant{group: group, resource: fields[1], verb: fields[0]})
	}
	return grants, problems
}

// podRBACKindOf returns the first document of kind, or nil when there is none.
func podRBACKindOf(docs []podRBACDoc, kind string) *podRBACDoc {
	for i := range docs {
		if docs[i].Kind == kind {
			return &docs[i]
		}
	}
	return nil
}

// podRBACRuleGrants reports whether rule grants g: its API groups, resources and
// verbs each name g's.
func podRBACRuleGrants(rule podRBACRule, g podRBACGrant) bool {
	return slices.Contains(rule.APIGroups, g.group) &&
		slices.Contains(rule.Resources, g.resource) &&
		slices.Contains(rule.Verbs, g.verb)
}

// podRBACShapeProblems holds the manifest to its shape: one ServiceAccount, one Role
// and one RoleBinding named gt-update, and one Role and one RoleBinding named
// gt-update-launch, all in gt. Each binding's roleRef is the Role of its own name and
// its only subject is the ServiceAccount podRBACSubjects names. Any other document,
// such as a ClusterRole, is refused by its kind, and a document with no kind is
// refused too.
func podRBACShapeProblems(docs []podRBACDoc) []string {
	var problems []string
	counts := map[string]int{}
	for _, doc := range docs {
		if doc.Kind == "" {
			problems = append(problems, "the manifest holds a document with no kind")
			continue
		}
		if !slices.Contains(podRBACKinds, doc.Kind) {
			problems = append(problems, fmt.Sprintf("the manifest holds a %s, and only %s are allowed", doc.Kind, strings.Join(podRBACKinds, ", ")))
			continue
		}
		counts[doc.Kind]++
		if !podRBACWanted(doc.Kind, doc.Metadata.Name) || doc.Metadata.Namespace != podRBACNS {
			problems = append(problems, fmt.Sprintf("the %s is %s/%s, not %s", doc.Kind, doc.Metadata.Namespace, doc.Metadata.Name, podRBACNames(doc.Kind)))
		}
	}
	for _, kind := range podRBACKinds {
		want := 0
		for _, w := range podRBACWant {
			if w.kind == kind {
				want++
			}
		}
		if counts[kind] != want {
			problems = append(problems, fmt.Sprintf("the manifest holds %d %s documents, want %d", counts[kind], kind, want))
		}
	}
	for _, w := range podRBACWant {
		if n := len(podRBACNamed(docs, w.kind, w.name)); n != 1 {
			problems = append(problems, fmt.Sprintf("the manifest holds %d %s %s documents, want 1", n, w.kind, w.name))
		}
	}
	for _, binding := range podRBACNamedKind(docs, "RoleBinding") {
		if binding.RoleRef.Kind != "Role" || binding.RoleRef.Name != binding.Metadata.Name {
			problems = append(problems, fmt.Sprintf("the RoleBinding %s's roleRef is %s %s, not Role %s", binding.Metadata.Name, binding.RoleRef.Kind, binding.RoleRef.Name, binding.Metadata.Name))
		}
		if want := podRBACSubjects[binding.Metadata.Name]; len(binding.Subjects) != 1 || binding.Subjects[0] != want {
			problems = append(problems, fmt.Sprintf("the RoleBinding %s's subjects are not the one ServiceAccount %s/%s", binding.Metadata.Name, want.Namespace, want.Name))
		}
	}
	return problems
}

// podRBACWanted reports whether the manifest may hold a document of kind named name:
// the ServiceAccount is gt-update alone, and each Role and RoleBinding is one of the
// two names.
func podRBACWanted(kind, name string) bool {
	for _, w := range podRBACWant {
		if w.kind == kind && w.name == name {
			return true
		}
	}
	return false
}

// podRBACNames is the namespaced name each kind of document may carry, for a problem.
func podRBACNames(kind string) string {
	if kind == "ServiceAccount" {
		return podRBACNS + "/" + podRBACName
	}
	return podRBACNS + "/" + podRBACName + " or " + podRBACNS + "/" + podRBACLaunchName
}

// podRBACNamedKind returns every document of kind, in manifest order.
func podRBACNamedKind(docs []podRBACDoc, kind string) []podRBACDoc {
	var out []podRBACDoc
	for _, doc := range docs {
		if doc.Kind == kind {
			out = append(out, doc)
		}
	}
	return out
}

// podRBACNamed returns every document of kind named name.
func podRBACNamed(docs []podRBACDoc, kind, name string) []podRBACDoc {
	var out []podRBACDoc
	for _, doc := range podRBACNamedKind(docs, kind) {
		if doc.Metadata.Name == name {
			out = append(out, doc)
		}
	}
	return out
}

// podRBACRoleNamed returns the Role named name, pointing into docs so an edit to it
// reaches the fixture, or nil when the manifest holds none.
func podRBACRoleNamed(docs []podRBACDoc, name string) *podRBACDoc {
	for i := range docs {
		if docs[i].Kind == "Role" && docs[i].Metadata.Name == name {
			return &docs[i]
		}
	}
	return nil
}

// podRBACGrantCoverage names each grant in needed that no rule of role grants.
func podRBACGrantCoverage(role *podRBACDoc, needed []podRBACGrant) []string {
	var problems []string
	for _, g := range needed {
		if !slices.ContainsFunc(role.Rules, func(r podRBACRule) bool { return podRBACRuleGrants(r, g) }) {
			problems = append(problems, fmt.Sprintf("grant %q is not granted by any rule of the Role", g))
		}
	}
	return problems
}

// podRBACCoverageProblems names each golden line that no rule of the Role grants.
func podRBACCoverageProblems(role *podRBACDoc, golden []string) []string {
	if role == nil {
		return []string{"the manifest holds no Role"}
	}
	grants, problems := podRBACGrantsOf(golden)
	return append(problems, podRBACGrantCoverage(role, grants)...)
}

// podRBACGrantProblems names each grant the Role makes that no golden line needs,
// and each golden line the Role does not grant.
func podRBACGrantProblems(role *podRBACDoc, golden []string) []string {
	if role == nil {
		return []string{"the manifest holds no Role"}
	}
	needed, problems := podRBACGrantsOf(golden)
	return append(problems, podRBACRuleProblems(role, needed)...)
}

// podRBACLaunchProblems holds the gt-update-launch Role to its grants: each one is
// granted, and the Role grants nothing else.
func podRBACLaunchProblems(role *podRBACDoc) []string {
	if role == nil {
		return []string{"the manifest holds no Role " + podRBACLaunchName}
	}
	return append(podRBACGrantCoverage(role, podRBACLaunchGrants), podRBACRuleProblems(role, podRBACLaunchGrants)...)
}

// podRBACRuleProblems names each grant the Role makes that needed does not list: a
// wildcard, any group, resource or verb outside needed, Secrets included, and a
// resourceNames restriction the check does not model except on a bound resource,
// which must name exactly podRBACBoundNames.
func podRBACRuleProblems(role *podRBACDoc, needed []podRBACGrant) []string {
	var problems []string
	allowed := make(map[podRBACGrant]bool, len(needed))
	for _, g := range needed {
		allowed[g] = true
	}
	for i, rule := range role.Rules {
		if len(rule.NonResourceURLs) > 0 {
			problems = append(problems, fmt.Sprintf("rule %d restricts nonResourceURLs, which this check does not model", i))
		}
		for _, resource := range rule.Resources {
			bound, isBound := podRBACBoundNames[resource]
			switch {
			case isBound && !podRBACSameSet(rule.ResourceNames, bound):
				problems = append(problems, fmt.Sprintf("rule %d grants %s, which is not bound to resourceNames %v", i, resource, bound))
			case !isBound && len(rule.ResourceNames) > 0:
				problems = append(problems, fmt.Sprintf("rule %d restricts resourceNames on %s, which this check does not model", i, resource))
			}
		}
		for _, group := range rule.APIGroups {
			for _, resource := range rule.Resources {
				for _, verb := range rule.Verbs {
					g := podRBACGrant{group: group, resource: resource, verb: verb}
					switch {
					case group == "*" || resource == "*" || verb == "*":
						problems = append(problems, fmt.Sprintf("rule %d grants a wildcard: group %q, resource %q, verb %q", i, group, resource, verb))
					case !allowed[g]:
						problems = append(problems, fmt.Sprintf("rule %d grants group %q, resource %q, verb %q, which no expected grant needs", i, group, resource, verb))
					}
				}
			}
		}
	}
	return problems
}

// podRBACSameSet reports whether a and b hold the same names, in any order.
func podRBACSameSet(a, b []string) bool {
	a, b = slices.Clone(a), slices.Clone(b)
	slices.Sort(a)
	slices.Sort(b)
	return slices.Equal(a, b)
}

// loadPodRBAC parses the real manifest, failing the test when it is not valid YAML.
func loadPodRBAC(t *testing.T) []podRBACDoc {
	t.Helper()
	raw, err := os.ReadFile(filepath.FromSlash(podRBACManifest))
	if err != nil {
		t.Fatalf("reading %s: %v", podRBACManifest, err)
	}
	docs, err := parsePodRBAC(raw)
	if err != nil {
		t.Fatalf("%s is not valid YAML: %v", podRBACManifest, err)
	}
	return docs
}

// loadPodReadsGolden returns the golden's read lines.
func loadPodReadsGolden(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.FromSlash(podReadsGolden))
	if err != nil {
		t.Fatalf("reading %s: %v", podReadsGolden, err)
	}
	return podRBACLines(raw)
}

// podRBACDocOrFail returns the first document of kind in a fixture, failing the
// test when the fixture holds none, so an edit never runs against nothing.
func podRBACDocOrFail(t *testing.T, docs []podRBACDoc, kind string) *podRBACDoc {
	t.Helper()
	doc := podRBACKindOf(docs, kind)
	if doc == nil {
		t.Fatalf("fixture holds no %s", kind)
	}
	return doc
}

// podRBACRoleOrFail returns the gt-update Role of a fixture, failing the test when the
// fixture holds none, so an edit never runs against nothing.
func podRBACRoleOrFail(t *testing.T, docs []podRBACDoc) *podRBACDoc {
	t.Helper()
	role := podRBACRoleNamed(docs, podRBACName)
	if role == nil {
		t.Fatalf("fixture holds no Role %s", podRBACName)
	}
	return role
}

// podRBACRuleNamed returns the first rule of a fixture's gt-update Role that names
// resource, failing the test when none does.
func podRBACRuleNamed(t *testing.T, docs []podRBACDoc, resource string) *podRBACRule {
	t.Helper()
	role := podRBACRoleOrFail(t, docs)
	for i := range role.Rules {
		if slices.Contains(role.Rules[i].Resources, resource) {
			return &role.Rules[i]
		}
	}
	t.Fatalf("fixture Role has no rule for %s", resource)
	return nil
}

// podRBACRefused fails the test unless one of problems names want.
func podRBACRefused(t *testing.T, name string, problems []string, want string) {
	t.Helper()
	if !slices.ContainsFunc(problems, func(p string) bool { return strings.Contains(p, want) }) {
		t.Errorf("%s: want a problem naming %q, got %q", name, want, problems)
	}
}

// podRBACShapeRefusals are manifest edits the shape check must refuse.
var podRBACShapeRefusals = []podRBACRefusal{
	{
		name: "a ClusterRole",
		edit: func(t *testing.T, docs []podRBACDoc) []podRBACDoc {
			return append(docs, podRBACDoc{Kind: "ClusterRole"})
		},
		want: "ClusterRole",
	},
	{
		name: "a ClusterRoleBinding",
		edit: func(t *testing.T, docs []podRBACDoc) []podRBACDoc {
			return append(docs, podRBACDoc{Kind: "ClusterRoleBinding"})
		},
		want: "ClusterRoleBinding",
	},
	{
		name: "a Role outside gt",
		edit: func(t *testing.T, docs []podRBACDoc) []podRBACDoc {
			podRBACRoleOrFail(t, docs).Metadata.Namespace = "default"
			return docs
		},
		want: "not gt/gt-update",
	},
	{
		name: "a ServiceAccount under another name",
		edit: func(t *testing.T, docs []podRBACDoc) []podRBACDoc {
			podRBACDocOrFail(t, docs, "ServiceAccount").Metadata.Name = "default"
			return docs
		},
		want: "not gt/gt-update",
	},
	{
		name: "no RoleBinding",
		edit: func(t *testing.T, docs []podRBACDoc) []podRBACDoc {
			return slices.DeleteFunc(docs, func(d podRBACDoc) bool { return d.Kind == "RoleBinding" })
		},
		want: "RoleBinding documents",
	},
	{
		name: "no ServiceAccount",
		edit: func(t *testing.T, docs []podRBACDoc) []podRBACDoc {
			return slices.DeleteFunc(docs, func(d podRBACDoc) bool { return d.Kind == "ServiceAccount" })
		},
		want: "ServiceAccount documents",
	},
	{
		name: "a binding to a ClusterRole",
		edit: func(t *testing.T, docs []podRBACDoc) []podRBACDoc {
			podRBACDocOrFail(t, docs, "RoleBinding").RoleRef.Kind = "ClusterRole"
			return docs
		},
		want: "roleRef",
	},
	{
		name: "a binding with a second subject",
		edit: func(t *testing.T, docs []podRBACDoc) []podRBACDoc {
			binding := podRBACDocOrFail(t, docs, "RoleBinding")
			binding.Subjects = append(binding.Subjects, podRBACSubject{Kind: "ServiceAccount", Name: "default", Namespace: "gt"})
			return docs
		},
		want: "subjects",
	},
	{
		name: "a binding to another ServiceAccount",
		edit: func(t *testing.T, docs []podRBACDoc) []podRBACDoc {
			podRBACDocOrFail(t, docs, "RoleBinding").Subjects[0].Name = "default"
			return docs
		},
		want: "subjects",
	},
	{
		name: "the launch binding to another ServiceAccount",
		edit: func(t *testing.T, docs []podRBACDoc) []podRBACDoc {
			podRBACBindingOrFail(t, docs, podRBACLaunchName).Subjects[0].Name = "gt-update"
			return docs
		},
		want: "subjects",
	},
	{
		name: "the launch binding to the gt-update Role",
		edit: func(t *testing.T, docs []podRBACDoc) []podRBACDoc {
			podRBACBindingOrFail(t, docs, podRBACLaunchName).RoleRef.Name = podRBACName
			return docs
		},
		want: "roleRef",
	},
	{
		name: "no launch Role",
		edit: func(t *testing.T, docs []podRBACDoc) []podRBACDoc {
			return slices.DeleteFunc(docs, func(d podRBACDoc) bool { return d.Kind == "Role" && d.Metadata.Name == podRBACLaunchName })
		},
		want: "Role gt-update-launch documents",
	},
}

// podRBACGrantWidenings are Role edits the grant check must refuse. The ClusterRole
// row is a grant the manifest makes beyond the Role, so the test also runs the shape
// check on it.
var podRBACGrantWidenings = []podRBACRefusal{
	{
		name: "a wildcard verb",
		edit: func(t *testing.T, docs []podRBACDoc) []podRBACDoc {
			podRBACRuleNamed(t, docs, "pods").Verbs = []string{"get", "*"}
			return docs
		},
		want: "wildcard",
	},
	{
		name: "a wildcard resource",
		edit: func(t *testing.T, docs []podRBACDoc) []podRBACDoc {
			podRBACRuleNamed(t, docs, "pods").Resources = []string{"*"}
			return docs
		},
		want: "wildcard",
	},
	{
		name: "a wildcard group",
		edit: func(t *testing.T, docs []podRBACDoc) []podRBACDoc {
			podRBACRuleNamed(t, docs, "deployments").APIGroups = []string{"*"}
			return docs
		},
		want: "wildcard",
	},
	{
		name: "a Secrets rule",
		edit: func(t *testing.T, docs []podRBACDoc) []podRBACDoc {
			role := podRBACRoleOrFail(t, docs)
			role.Rules = append(role.Rules, podRBACRule{APIGroups: []string{""}, Resources: []string{"secrets"}, Verbs: []string{"get"}})
			return docs
		},
		want: `"secrets"`,
	},
	{
		name: "a verb no golden line needs",
		edit: func(t *testing.T, docs []podRBACDoc) []podRBACDoc {
			rule := podRBACRuleNamed(t, docs, "pods")
			rule.Verbs = append(rule.Verbs, "delete")
			return docs
		},
		want: `"delete"`,
	},
	{
		name: "a resourceNames restriction",
		edit: func(t *testing.T, docs []podRBACDoc) []podRBACDoc {
			podRBACRuleNamed(t, docs, "pods").ResourceNames = []string{"gt-daemon"}
			return docs
		},
		want: "resourceNames",
	},
	{
		name: "a deployments rule with no resourceNames",
		edit: func(t *testing.T, docs []podRBACDoc) []podRBACDoc {
			podRBACRuleNamed(t, docs, "deployments").ResourceNames = nil
			return docs
		},
		want: "not bound to resourceNames",
	},
	{
		name: "a deployments rule bound to one Deployment of the two",
		edit: func(t *testing.T, docs []podRBACDoc) []podRBACDoc {
			podRBACRuleNamed(t, docs, "deployments").ResourceNames = []string{"daemon"}
			return docs
		},
		want: "not bound to resourceNames",
	},
	{
		name: "a deployments rule bound to another Deployment",
		edit: func(t *testing.T, docs []podRBACDoc) []podRBACDoc {
			podRBACRuleNamed(t, docs, "deployments").ResourceNames = []string{"daemon", "gt-other"}
			return docs
		},
		want: "not bound to resourceNames",
	},
	{
		name: "a ClusterRole",
		edit: func(t *testing.T, docs []podRBACDoc) []podRBACDoc {
			return append(docs, podRBACDoc{Kind: "ClusterRole"})
		},
		want: "ClusterRole",
	},
}

// TestPodUpdateRBACManifestParses holds the manifest to its shape: the real file
// holds exactly the gt-update ServiceAccount, Role and RoleBinding in gt, and each
// edit in podRBACShapeRefusals that breaks that shape is refused by name.
func TestPodUpdateRBACManifestParses(t *testing.T) {
	t.Parallel()
	if problems := podRBACShapeProblems(loadPodRBAC(t)); len(problems) > 0 {
		t.Errorf("%s: %s", podRBACManifest, strings.Join(problems, "; "))
	}
	for _, r := range podRBACShapeRefusals {
		docs := r.edit(t, loadPodRBAC(t))
		podRBACRefused(t, r.name, podRBACShapeProblems(docs), r.want)
	}
}

// TestPodUpdateRBACManifestRefusesKindlessDocument holds the parser to the shape: a
// document with no kind, carrying a Secrets rule, is refused by name. Dropping it
// unread would let a grant pass the checks.
func TestPodUpdateRBACManifestRefusesKindlessDocument(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.FromSlash(podRBACManifest))
	if err != nil {
		t.Fatalf("reading %s: %v", podRBACManifest, err)
	}
	kindless := append(slices.Clone(raw), []byte("---\nmetadata:\n  name: gt-update\n  namespace: gt\nrules:\n  - apiGroups: [\"\"]\n    resources: [\"secrets\"]\n    verbs: [\"get\"]\n")...)
	docs, err := parsePodRBAC(kindless)
	if err != nil {
		t.Fatalf("parsing the manifest with a kind-less document: %v", err)
	}
	podRBACRefused(t, "a kind-less document", podRBACShapeProblems(docs), "no kind")
}

// podRBACRemovalProblems removes each rule of the Role in turn, on a copy, and names
// each golden line the removed rule alone granted that is then left unnamed. A
// manifest with no Role, a golden line that is not `verb resource`, and a rule that
// grants no golden line are problems too: each is named, never skipped.
func podRBACRemovalProblems(docs []podRBACDoc, golden []string) []string {
	role := podRBACRoleNamed(docs, podRBACName)
	if role == nil {
		return []string{"the manifest holds no Role"}
	}
	grants, problems := podRBACGrantsOf(golden)
	for i, rule := range role.Rules {
		var granted []podRBACGrant
		for _, g := range grants {
			if podRBACRuleGrants(rule, g) {
				granted = append(granted, g)
			}
		}
		if len(granted) == 0 {
			problems = append(problems, fmt.Sprintf("rule %d grants no golden line, so removing it proves nothing", i))
			continue
		}
		without := *role
		without.Rules = slices.Delete(slices.Clone(role.Rules), i, i+1)
		unnamed := strings.Join(podRBACCoverageProblems(&without, golden), "\n")
		for _, g := range granted {
			if !strings.Contains(unnamed, fmt.Sprintf("%q", g)) {
				problems = append(problems, fmt.Sprintf("removing rule %d leaves golden line %q unnamed", i, g))
			}
		}
	}
	return problems
}

// TestPodUpdateRBACCoversEveryRead holds the Role to the golden: every golden line
// is granted by a rule. Removing any one rule fails the test, and the failure names
// each golden line that rule alone granted.
func TestPodUpdateRBACCoversEveryRead(t *testing.T) {
	t.Parallel()
	golden := loadPodReadsGolden(t)
	docs := loadPodRBAC(t)
	if problems := podRBACCoverageProblems(podRBACRoleNamed(docs, podRBACName), golden); len(problems) > 0 {
		t.Errorf("%s: %s", podRBACManifest, strings.Join(problems, "; "))
	}
	if problems := podRBACRemovalProblems(docs, golden); len(problems) > 0 {
		t.Errorf("%s: %s", podRBACManifest, strings.Join(problems, "; "))
	}
}

// TestPodUpdateRBACRemovalCheckRefusesBadRole holds the removal check to its refusals:
// a manifest with no Role and a rule that grants no golden line are each named, not
// passed and not a panic.
func TestPodUpdateRBACRemovalCheckRefusesBadRole(t *testing.T) {
	t.Parallel()
	golden := loadPodReadsGolden(t)
	for _, r := range []podRBACRefusal{
		{
			name: "no Role",
			edit: func(t *testing.T, docs []podRBACDoc) []podRBACDoc {
				return slices.DeleteFunc(docs, func(d podRBACDoc) bool { return d.Kind == "Role" })
			},
			want: "no Role",
		},
		{
			name: "a rule granting no golden line",
			edit: func(t *testing.T, docs []podRBACDoc) []podRBACDoc {
				role := podRBACRoleOrFail(t, docs)
				role.Rules = append(role.Rules, podRBACRule{APIGroups: []string{""}, Resources: []string{"configmaps"}, Verbs: []string{"get"}})
				return docs
			},
			want: "grants no golden line",
		},
	} {
		podRBACRefused(t, r.name, podRBACRemovalProblems(r.edit(t, loadPodRBAC(t)), golden), r.want)
	}
}

// TestPodUpdateRBACCoversEveryReadRefusesBadGolden holds the golden reader to its
// refusals: a line that is not `verb resource`, and a resource with no API group,
// each name the problem instead of passing unchecked.
func TestPodUpdateRBACCoversEveryReadRefusesBadGolden(t *testing.T) {
	t.Parallel()
	for _, r := range []struct {
		name, line, want string
	}{
		{name: "a line with no resource", line: "get", want: "is not `verb resource`"},
		{name: "a resource with no API group", line: "get secrets", want: "no API group"},
	} {
		problems := podRBACCoverageProblems(podRBACRoleNamed(loadPodRBAC(t), podRBACName), []string{r.line})
		podRBACRefused(t, r.name, problems, r.want)
	}
}

// podRBACAllProblems holds the whole manifest: its shape, the gt-update Role to the
// golden, and the gt-update-launch Role to its grants.
func podRBACAllProblems(docs []podRBACDoc, golden []string) []string {
	problems := podRBACShapeProblems(docs)
	problems = append(problems, podRBACGrantProblems(podRBACRoleNamed(docs, podRBACName), golden)...)
	return append(problems, podRBACLaunchProblems(podRBACRoleNamed(docs, podRBACLaunchName))...)
}

// TestPodUpdateRBACGrantsNothingElse holds the manifest to the golden and to the
// launch grants: the real file grants nothing outside them, and each widening in
// podRBACGrantWidenings is refused by name.
func TestPodUpdateRBACGrantsNothingElse(t *testing.T) {
	t.Parallel()
	golden := loadPodReadsGolden(t)
	if problems := podRBACAllProblems(loadPodRBAC(t), golden); len(problems) > 0 {
		t.Errorf("%s: %s", podRBACManifest, strings.Join(problems, "; "))
	}
	for _, r := range podRBACGrantWidenings {
		docs := r.edit(t, loadPodRBAC(t))
		podRBACRefused(t, r.name, podRBACAllProblems(docs, golden), r.want)
	}
}

// podRBACLaunchWidenings are edits to the gt-update-launch Role that its grant check
// must refuse: a verb or resource outside the Job grants, a wildcard, or a
// resourceNames restriction on jobs.
var podRBACLaunchWidenings = []podRBACRefusal{
	{
		name: "an update verb on jobs",
		edit: func(t *testing.T, docs []podRBACDoc) []podRBACDoc {
			rule := &podRBACLaunchOrFail(t, docs).Rules[0]
			rule.Verbs = append(rule.Verbs, "update")
			return docs
		},
		want: `"update"`,
	},
	{
		name: "a patch verb on jobs",
		edit: func(t *testing.T, docs []podRBACDoc) []podRBACDoc {
			rule := &podRBACLaunchOrFail(t, docs).Rules[0]
			rule.Verbs = append(rule.Verbs, "patch")
			return docs
		},
		want: `"patch"`,
	},
	{
		name: "a pods rule",
		edit: func(t *testing.T, docs []podRBACDoc) []podRBACDoc {
			role := podRBACLaunchOrFail(t, docs)
			role.Rules = append(role.Rules, podRBACRule{APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"get"}})
			return docs
		},
		want: `"pods"`,
	},
	{
		name: "a Secrets rule",
		edit: func(t *testing.T, docs []podRBACDoc) []podRBACDoc {
			role := podRBACLaunchOrFail(t, docs)
			role.Rules = append(role.Rules, podRBACRule{APIGroups: []string{""}, Resources: []string{"secrets"}, Verbs: []string{"get"}})
			return docs
		},
		want: `"secrets"`,
	},
	{
		name: "a wildcard verb on jobs",
		edit: func(t *testing.T, docs []podRBACDoc) []podRBACDoc {
			podRBACLaunchOrFail(t, docs).Rules[0].Verbs = []string{"*"}
			return docs
		},
		want: "wildcard",
	},
	{
		name: "a jobs rule bound to one Job",
		edit: func(t *testing.T, docs []podRBACDoc) []podRBACDoc {
			podRBACLaunchOrFail(t, docs).Rules[0].ResourceNames = []string{"gt-update-manual"}
			return docs
		},
		want: "resourceNames",
	},
}

// TestPodUpdateLaunchRBAC holds the gt-update-launch Role to the Job grants the daemon
// needs: the real file grants exactly those, and each widening in
// podRBACLaunchWidenings is refused by name.
func TestPodUpdateLaunchRBAC(t *testing.T) {
	t.Parallel()
	if problems := podRBACLaunchProblems(podRBACRoleNamed(loadPodRBAC(t), podRBACLaunchName)); len(problems) > 0 {
		t.Errorf("%s: %s", podRBACManifest, strings.Join(problems, "; "))
	}
	for _, r := range podRBACLaunchWidenings {
		docs := r.edit(t, loadPodRBAC(t))
		podRBACRefused(t, r.name, podRBACLaunchProblems(podRBACRoleNamed(docs, podRBACLaunchName)), r.want)
	}
}

// podRBACLaunchReaches are the three reaches the launch Role's comment must state, in
// the words the comment uses. A Job's pod spec is chosen by its creator, so the daemon
// reaches each of them through a Job (gt-1qnr01).
var podRBACLaunchReaches = []string{
	"run a pod as the gt-update ServiceAccount",
	"mount any Secret in gt",
	"run any image the nodes can pull",
}

// podRBACRestrictionClaim is the wording the header must not use: it says the daemon
// cannot read or patch, which a Job it creates makes false (gt-1qnr01).
const podRBACRestrictionClaim = "cannot read or patch"

// podRBACHeaderText returns the manifest's leading comment block as one line of words:
// the "#" markers dropped and the lines joined, so a phrase that wraps still matches.
func podRBACHeaderText(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.FromSlash(podRBACManifest))
	if err != nil {
		t.Fatalf("reading %s: %v", podRBACManifest, err)
	}
	var lines []string
	for _, line := range strings.Split(string(raw), "\n") {
		if !strings.HasPrefix(line, "#") {
			break
		}
		lines = append(lines, strings.TrimPrefix(line, "#"))
	}
	return strings.Join(strings.Fields(strings.Join(lines, " ")), " ")
}

// TestPodUpdateLaunchReachIsStated holds the launch Role's comment to the three reaches,
// so the statement of what the daemon can reach through a Job cannot be dropped.
func TestPodUpdateLaunchReachIsStated(t *testing.T) {
	t.Parallel()
	header := podRBACHeaderText(t)
	for _, reach := range podRBACLaunchReaches {
		if !strings.Contains(header, reach) {
			t.Errorf("%s: the header does not state the launch Role's reach %q", podRBACManifest, reach)
		}
	}
}

// TestPodUpdateLaunchClaimsNoRestriction refuses a header that claims the daemon cannot
// read or patch: through a Job it can, so the claim would mislead the next reader.
func TestPodUpdateLaunchClaimsNoRestriction(t *testing.T) {
	t.Parallel()
	if header := podRBACHeaderText(t); strings.Contains(header, podRBACRestrictionClaim) {
		t.Errorf("%s: the header claims %q, which a Job the daemon creates makes false", podRBACManifest, podRBACRestrictionClaim)
	}
}

// podRBACBindingOrFail returns the RoleBinding named name in a fixture, failing the
// test when the fixture holds none, so an edit never runs against nothing.
func podRBACBindingOrFail(t *testing.T, docs []podRBACDoc, name string) *podRBACDoc {
	t.Helper()
	for i := range docs {
		if docs[i].Kind == "RoleBinding" && docs[i].Metadata.Name == name {
			return &docs[i]
		}
	}
	t.Fatalf("fixture holds no RoleBinding %s", name)
	return nil
}

// podRBACLaunchOrFail returns the gt-update-launch Role of a fixture, failing the test
// when the fixture holds none.
func podRBACLaunchOrFail(t *testing.T, docs []podRBACDoc) *podRBACDoc {
	t.Helper()
	role := podRBACRoleNamed(docs, podRBACLaunchName)
	if role == nil {
		t.Fatalf("fixture holds no Role %s", podRBACLaunchName)
	}
	return role
}

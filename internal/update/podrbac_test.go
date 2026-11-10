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
// call as `verb resource` in the gt namespace. The Role must grant every golden
// line and nothing else (gt-360i18).
const (
	podRBACManifest = "../../deploy/k8s/update-rbac.yaml"
	podReadsGolden  = "testdata/pod-update-reads.golden"
	podRBACNS       = "gt"
	podRBACName     = "gt-update"
)

// podRBACGroup is the API group each resource the pod daemon's kubectl calls touch
// belongs to. A resource missing here is a problem: the check does not guess a
// group for a call it has not been taught.
var podRBACGroup = map[string]string{
	"deployments": "apps",
	"pods":        "",
	"pods/exec":   "",
}

// podRBACKinds are the only documents the manifest may hold.
var podRBACKinds = []string{"ServiceAccount", "Role", "RoleBinding"}

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

// podRBACShapeProblems holds the manifest to its shape: exactly one ServiceAccount,
// Role and RoleBinding, each named gt-update in gt; the binding's roleRef is the
// Role and its only subject is the ServiceAccount. Any other document, such as a
// ClusterRole, is refused by its kind, and a document with no kind is refused too.
func podRBACShapeProblems(docs []podRBACDoc) []string {
	var problems []string
	counts := map[string]int{}
	for _, doc := range docs {
		counts[doc.Kind]++
		if doc.Kind == "" {
			problems = append(problems, "the manifest holds a document with no kind")
			continue
		}
		if !slices.Contains(podRBACKinds, doc.Kind) {
			problems = append(problems, fmt.Sprintf("the manifest holds a %s, and only %s are allowed", doc.Kind, strings.Join(podRBACKinds, ", ")))
			continue
		}
		if doc.Metadata.Name != podRBACName || doc.Metadata.Namespace != podRBACNS {
			problems = append(problems, fmt.Sprintf("the %s is %s/%s, not %s/%s", doc.Kind, doc.Metadata.Namespace, doc.Metadata.Name, podRBACNS, podRBACName))
		}
	}
	for _, kind := range podRBACKinds {
		if counts[kind] != 1 {
			problems = append(problems, fmt.Sprintf("the manifest holds %d %s documents, want 1", counts[kind], kind))
		}
	}
	if binding := podRBACKindOf(docs, "RoleBinding"); binding != nil {
		if binding.RoleRef.Kind != "Role" || binding.RoleRef.Name != podRBACName {
			problems = append(problems, fmt.Sprintf("the RoleBinding's roleRef is %s %s, not Role %s", binding.RoleRef.Kind, binding.RoleRef.Name, podRBACName))
		}
		want := podRBACSubject{Kind: "ServiceAccount", Name: podRBACName, Namespace: podRBACNS}
		if len(binding.Subjects) != 1 || binding.Subjects[0] != want {
			problems = append(problems, "the RoleBinding's subjects are not the one ServiceAccount gt-update in gt")
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
	for _, g := range grants {
		if !slices.ContainsFunc(role.Rules, func(r podRBACRule) bool { return podRBACRuleGrants(r, g) }) {
			problems = append(problems, fmt.Sprintf("golden line %q is not granted by any rule of the Role", g))
		}
	}
	return problems
}

// podRBACGrantProblems names each grant the Role makes that no golden line needs:
// a wildcard, a resourceNames or nonResourceURLs restriction the check does not
// model, and any group, resource or verb outside the golden, Secrets included.
func podRBACGrantProblems(role *podRBACDoc, golden []string) []string {
	if role == nil {
		return []string{"the manifest holds no Role"}
	}
	needed, problems := podRBACGrantsOf(golden)
	allowed := make(map[podRBACGrant]bool, len(needed))
	for _, g := range needed {
		allowed[g] = true
	}
	for i, rule := range role.Rules {
		if len(rule.ResourceNames) > 0 || len(rule.NonResourceURLs) > 0 {
			problems = append(problems, fmt.Sprintf("rule %d restricts resourceNames or nonResourceURLs, which this check does not model", i))
		}
		for _, group := range rule.APIGroups {
			for _, resource := range rule.Resources {
				for _, verb := range rule.Verbs {
					g := podRBACGrant{group: group, resource: resource, verb: verb}
					switch {
					case group == "*" || resource == "*" || verb == "*":
						problems = append(problems, fmt.Sprintf("rule %d grants a wildcard: group %q, resource %q, verb %q", i, group, resource, verb))
					case !allowed[g]:
						problems = append(problems, fmt.Sprintf("rule %d grants group %q, resource %q, verb %q, which no golden line needs", i, group, resource, verb))
					}
				}
			}
		}
	}
	return problems
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

// podRBACRuleNamed returns the first rule of a fixture's Role that names resource,
// failing the test when none does.
func podRBACRuleNamed(t *testing.T, docs []podRBACDoc, resource string) *podRBACRule {
	t.Helper()
	role := podRBACDocOrFail(t, docs, "Role")
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
			podRBACDocOrFail(t, docs, "Role").Metadata.Namespace = "default"
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
			role := podRBACDocOrFail(t, docs, "Role")
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
	role := podRBACKindOf(docs, "Role")
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
	if problems := podRBACCoverageProblems(podRBACKindOf(docs, "Role"), golden); len(problems) > 0 {
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
				role := podRBACDocOrFail(t, docs, "Role")
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
		problems := podRBACCoverageProblems(podRBACKindOf(loadPodRBAC(t), "Role"), []string{r.line})
		podRBACRefused(t, r.name, problems, r.want)
	}
}

// TestPodUpdateRBACGrantsNothingElse holds the manifest to the golden: the real
// file grants nothing outside it, and each widening in podRBACGrantWidenings is
// refused by name.
func TestPodUpdateRBACGrantsNothingElse(t *testing.T) {
	t.Parallel()
	golden := loadPodReadsGolden(t)
	docs := loadPodRBAC(t)
	problems := append(podRBACShapeProblems(docs), podRBACGrantProblems(podRBACKindOf(docs, "Role"), golden)...)
	if len(problems) > 0 {
		t.Errorf("%s: %s", podRBACManifest, strings.Join(problems, "; "))
	}
	for _, r := range podRBACGrantWidenings {
		docs := r.edit(t, loadPodRBAC(t))
		problems := append(podRBACShapeProblems(docs), podRBACGrantProblems(podRBACKindOf(docs, "Role"), golden)...)
		podRBACRefused(t, r.name, problems, r.want)
	}
}

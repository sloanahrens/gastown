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

// The drift compare's cluster access is a read-only ClusterRole in
// deploy/k8s/drift-rbac.yaml, bound to the daemon's service account in gt. The
// compare reads each in-scope rendered object by name, so the grant is get and
// list on the kinds it reads and nothing else; internal/cmd/testdata/drift-reads.golden
// lists those reads. The tests below hold the manifest to the table and to the
// golden, in the style of TestSweepRBACClusterGrantIsReadOnly.
const (
	driftRBACManifest = "drift-rbac.yaml"
	driftReadsGolden  = "../../internal/cmd/testdata/drift-reads.golden"
	driftReaderRole   = "gt-drift-reader"
	driftDaemonSA     = "daemon"
	driftDaemonNS     = "gt"
)

// driftGranted is the exact set the ClusterRole may grant: one entry per
// resource the drift compare reads, with the API group it belongs to. The verbs
// are driftGrantedVerbs for every resource, because the compare only reads. It is
// written out here so a resource joining the grant needs a deliberate edit of
// this table as well as the manifest, and TestDriftRBACCoverageProblemsReportsEachFailingBranch
// fails if the table, the golden and the manifest disagree in either direction.
var driftGranted = map[string]string{
	"deployments": "apps",
	"configmaps":  "",
}

// driftGrantedVerbs is every verb the ClusterRole may grant. The compare reads
// each object by name, which is get; list is granted beside it because a `get`
// with no object name is a list, so the grant holds the read whichever form the
// compare uses.
var driftGrantedVerbs = []string{"get", "list"}

// driftRBACDoc is the part of one manifest document the checks read.
type driftRBACDoc struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name      string `yaml:"name"`
		Namespace string `yaml:"namespace"`
	} `yaml:"metadata"`
	Rules   []driftRBACRule `yaml:"rules"`
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

type driftRBACRule struct {
	APIGroups []string `yaml:"apiGroups"`
	Resources []string `yaml:"resources"`
	Verbs     []string `yaml:"verbs"`
}

// loadDriftRBAC parses every document of the manifest. A file that is not valid
// YAML fails here, and so does an empty one, which would otherwise pass every
// check for the wrong reason.
func loadDriftRBAC(t *testing.T) []driftRBACDoc {
	t.Helper()
	raw, err := os.ReadFile(filepath.FromSlash(driftRBACManifest))
	if err != nil {
		t.Fatalf("reading %s: %v", driftRBACManifest, err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	var docs []driftRBACDoc
	for {
		var doc driftRBACDoc
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("%s is not valid YAML: %v", driftRBACManifest, err)
		}
		if doc.Kind != "" {
			docs = append(docs, doc)
		}
	}
	if len(docs) == 0 {
		t.Fatalf("%s holds no objects, so the checks would pass for the wrong reason", driftRBACManifest)
	}
	return docs
}

// findDriftRBAC returns the document of the kind, name and namespace, or nil.
func findDriftRBAC(docs []driftRBACDoc, kind, name, namespace string) *driftRBACDoc {
	for i := range docs {
		d := &docs[i]
		if d.Kind == kind && d.Metadata.Name == name && d.Metadata.Namespace == namespace {
			return d
		}
	}
	return nil
}

// driftAllows reports whether one of the rules grants verb on group's resource.
// A wildcard is not understood, so a rule that uses one grants nothing here.
func driftAllows(rules []driftRBACRule, group, resource, verb string) bool {
	for _, r := range rules {
		if slices.Contains(r.APIGroups, group) && slices.Contains(r.Resources, resource) && slices.Contains(r.Verbs, verb) {
			return true
		}
	}
	return false
}

// TestDriftRBACManifestParses holds the manifest to the two objects the compare
// runs with: the read-only ClusterRole, and a ClusterRoleBinding that names it
// and binds the daemon's service account in gt and no other subject.
func TestDriftRBACManifestParses(t *testing.T) {
	t.Parallel()
	docs := loadDriftRBAC(t)
	if findDriftRBAC(docs, "ClusterRole", driftReaderRole, "") == nil {
		t.Fatalf("%s has no ClusterRole %q", driftRBACManifest, driftReaderRole)
	}
	binding := findDriftRBAC(docs, "ClusterRoleBinding", driftReaderRole, "")
	if binding == nil {
		t.Fatalf("%s has no ClusterRoleBinding %q", driftRBACManifest, driftReaderRole)
	}
	if binding.RoleRef.Kind != "ClusterRole" || binding.RoleRef.Name != driftReaderRole {
		t.Errorf("ClusterRoleBinding %s refs %s %q, want ClusterRole %q",
			driftReaderRole, binding.RoleRef.Kind, binding.RoleRef.Name, driftReaderRole)
	}
	want := struct{ Kind, Name, Namespace string }{"ServiceAccount", driftDaemonSA, driftDaemonNS}
	if len(binding.Subjects) != 1 {
		t.Fatalf("ClusterRoleBinding %s binds %d subjects, want only the daemon's service account",
			driftReaderRole, len(binding.Subjects))
	}
	got := binding.Subjects[0]
	if got.Kind != want.Kind || got.Name != want.Name || got.Namespace != want.Namespace {
		t.Errorf("ClusterRoleBinding %s binds %s %s/%s, want %s %s/%s",
			driftReaderRole, got.Kind, got.Namespace, got.Name, want.Kind, want.Namespace, want.Name)
	}
}

// TestDriftRBACGrantIsReadOnly holds the ClusterRole to driftGranted and
// driftGrantedVerbs: no resource outside the table, no verb beyond get and list,
// no wildcard, and every resource in the table granted both verbs.
func TestDriftRBACGrantIsReadOnly(t *testing.T) {
	t.Parallel()
	rules := driftRoleRules(t, loadDriftRBAC(t))
	for _, problem := range driftGrantProblems(rules) {
		t.Error(problem)
	}
}

// driftRoleRules returns the ClusterRole's rules, failing when the manifest has
// no such role.
func driftRoleRules(t *testing.T, docs []driftRBACDoc) []driftRBACRule {
	t.Helper()
	role := findDriftRBAC(docs, "ClusterRole", driftReaderRole, "")
	if role == nil {
		t.Fatalf("%s has no ClusterRole %q", driftRBACManifest, driftReaderRole)
	}
	return role.Rules
}

// driftWideningKind names one way a rule grants more than the bound.
type driftWideningKind int

const (
	driftWildcardAPIGroup driftWideningKind = iota
	driftWildcardResource
	driftWildcardVerb
	driftUnknownResource
	driftWrongAPIGroup
	driftVerbOutsideBound
)

// driftWidening is one grant in a rule that the bound does not hold. The fields
// name what the rule grants, so a test asserts on them rather than on the
// wording of a message.
type driftWidening struct {
	Rule     int // the rule's 1-based position in the ClusterRole
	Kind     driftWideningKind
	APIGroup string
	Resource string
	Verb     string
}

func (w driftWidening) String() string {
	switch w.Kind {
	case driftWildcardAPIGroup:
		return fmt.Sprintf("rule %d grants every API group with a wildcard", w.Rule)
	case driftWildcardResource:
		return fmt.Sprintf("rule %d grants every resource with a wildcard", w.Rule)
	case driftWildcardVerb:
		return fmt.Sprintf("rule %d grants every verb with a wildcard", w.Rule)
	case driftUnknownResource:
		return fmt.Sprintf("rule %d grants resource %q, which the drift compare does not read", w.Rule, w.Resource)
	case driftWrongAPIGroup:
		return fmt.Sprintf("rule %d grants %q in API group %q, which is not its group", w.Rule, w.Resource, w.APIGroup)
	case driftVerbOutsideBound:
		return fmt.Sprintf("rule %d grants %q on %q, which the drift compare never runs", w.Rule, w.Verb, w.Resource)
	}
	return fmt.Sprintf("rule %d has an unknown widening", w.Rule)
}

// driftWidenings reports every grant in the ClusterRole's rules that the bound
// does not hold: a wildcard in any of a rule's apiGroups, resources or verbs,
// and each granted group, resource or verb outside driftGranted and
// driftGrantedVerbs. Every rule is read, so a rule with several problems
// reports each of them.
func driftWidenings(rules []driftRBACRule) []driftWidening {
	var widened []driftWidening
	for i, rule := range rules {
		n := i + 1
		if slices.Contains(rule.APIGroups, "*") {
			widened = append(widened, driftWidening{Rule: n, Kind: driftWildcardAPIGroup, APIGroup: "*"})
		}
		if slices.Contains(rule.Resources, "*") {
			widened = append(widened, driftWidening{Rule: n, Kind: driftWildcardResource, Resource: "*"})
		}
		if slices.Contains(rule.Verbs, "*") {
			widened = append(widened, driftWidening{Rule: n, Kind: driftWildcardVerb, Verb: "*"})
		}
		for _, resource := range rule.Resources {
			group, allowed := driftGranted[resource]
			if !allowed {
				widened = append(widened, driftWidening{Rule: n, Kind: driftUnknownResource, Resource: resource})
				continue
			}
			for _, g := range rule.APIGroups {
				if g != "*" && g != group {
					widened = append(widened, driftWidening{Rule: n, Kind: driftWrongAPIGroup, APIGroup: g, Resource: resource})
				}
			}
			for _, verb := range rule.Verbs {
				if verb != "*" && !slices.Contains(driftGrantedVerbs, verb) {
					widened = append(widened, driftWidening{Rule: n, Kind: driftVerbOutsideBound, Resource: resource, Verb: verb})
				}
			}
		}
	}
	return widened
}

// driftGrantProblems reports every way the rules grant more than the bound, and
// every resource in the bound the rules do not grant both verbs of.
func driftGrantProblems(rules []driftRBACRule) []string {
	var problems []string
	for _, w := range driftWidenings(rules) {
		problems = append(problems, fmt.Sprintf("ClusterRole %s: %s", driftReaderRole, w))
	}
	for resource, group := range driftGranted {
		for _, verb := range driftGrantedVerbs {
			if !driftAllows(rules, group, resource, verb) {
				problems = append(problems, fmt.Sprintf("ClusterRole %s grants no %q on %s/%s, so the drift compare cannot read it",
					driftReaderRole, verb, group, resource))
			}
		}
	}
	return problems
}

// cloneDriftRules returns a copy of rules their slices a test can edit without
// touching the manifest the other tests read.
func cloneDriftRules(rules []driftRBACRule) []driftRBACRule {
	out := slices.Clone(rules)
	for i := range out {
		out[i] = driftRBACRule{
			APIGroups: slices.Clone(out[i].APIGroups),
			Resources: slices.Clone(out[i].Resources),
			Verbs:     slices.Clone(out[i].Verbs),
		}
	}
	return out
}

// TestDriftRBACGuardsRefuseEachWidening drives driftGrantProblems on a clone of
// the ClusterRole with one widening applied, so each refusal the guard makes has
// a row that turns it red: a write verb, a wildcard in each of the three places
// a rule names one, a Secret read, a resource in the wrong API group, and a
// granted verb removed. The unmutated manifest passes.
func TestDriftRBACGuardsRefuseEachWidening(t *testing.T) {
	t.Parallel()
	rules := driftRoleRules(t, loadDriftRBAC(t))
	if problems := driftGrantProblems(rules); len(problems) > 0 {
		t.Fatalf("the unmutated ClusterRole is refused: %q", problems)
	}
	appended := len(rules) + 1
	tests := []struct {
		name string
		edit func(rules []driftRBACRule) []driftRBACRule
		want string
	}{
		{
			name: "a write verb on a granted resource",
			edit: func(rules []driftRBACRule) []driftRBACRule {
				rules[0].Verbs = append(rules[0].Verbs, "update")
				return rules
			},
			want: wideningProblem(driftWidening{Rule: 1, Kind: driftVerbOutsideBound, Resource: "deployments", Verb: "update"}),
		},
		{
			name: "a wildcard verb",
			edit: func(rules []driftRBACRule) []driftRBACRule {
				rules[0].Verbs = append(rules[0].Verbs, "*")
				return rules
			},
			want: wideningProblem(driftWidening{Rule: 1, Kind: driftWildcardVerb, Verb: "*"}),
		},
		{
			name: "a wildcard resource",
			edit: func(rules []driftRBACRule) []driftRBACRule {
				return append(rules, driftRBACRule{APIGroups: []string{""}, Resources: []string{"*"}, Verbs: []string{"get"}})
			},
			want: wideningProblem(driftWidening{Rule: appended, Kind: driftWildcardResource, Resource: "*"}),
		},
		{
			name: "a wildcard API group",
			edit: func(rules []driftRBACRule) []driftRBACRule {
				return append(rules, driftRBACRule{APIGroups: []string{"*"}, Resources: []string{"services"}, Verbs: []string{"get"}})
			},
			want: wideningProblem(driftWidening{Rule: appended, Kind: driftWildcardAPIGroup, APIGroup: "*"}),
		},
		{
			name: "a Secret read",
			edit: func(rules []driftRBACRule) []driftRBACRule {
				return append(rules, driftRBACRule{APIGroups: []string{""}, Resources: []string{"secrets"}, Verbs: []string{"get"}})
			},
			want: wideningProblem(driftWidening{Rule: appended, Kind: driftUnknownResource, Resource: "secrets"}),
		},
		{
			name: "a granted resource in the wrong API group",
			edit: func(rules []driftRBACRule) []driftRBACRule {
				rules[0].APIGroups = []string{"extensions"}
				return rules
			},
			want: wideningProblem(driftWidening{Rule: 1, Kind: driftWrongAPIGroup, APIGroup: "extensions", Resource: "deployments"}),
		},
		{
			name: "a granted verb removed",
			edit: func(rules []driftRBACRule) []driftRBACRule {
				rules[0].Verbs = slices.DeleteFunc(slices.Clone(rules[0].Verbs), func(v string) bool { return v == "list" })
				return rules
			},
			want: fmt.Sprintf("ClusterRole %s grants no %q on apps/deployments, so the drift compare cannot read it", driftReaderRole, "list"),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			problems := driftGrantProblems(tc.edit(cloneDriftRules(rules)))
			if !slices.Contains(problems, tc.want) {
				t.Errorf("with %s the ClusterRole was not refused; want the problem %q, got %q", tc.name, tc.want, problems)
			}
		})
	}
}

// wideningProblem is the problem driftGrantProblems reports for one widening.
func wideningProblem(w driftWidening) string {
	return fmt.Sprintf("ClusterRole %s: %s", driftReaderRole, w)
}

// loadDriftReads reads the golden's lines, skipping blanks and comments, and
// fails on a line repeated or shaped wrong.
func loadDriftReads(t *testing.T) [][2]string {
	t.Helper()
	raw, err := os.ReadFile(filepath.FromSlash(driftReadsGolden))
	if err != nil {
		t.Fatalf("reading %s: %v", driftReadsGolden, err)
	}
	var reads [][2]string
	seen := map[string]bool{}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if seen[line] {
			t.Fatalf("%s lists %q twice", driftReadsGolden, line)
		}
		seen[line] = true
		fields := strings.Fields(line)
		if len(fields) != 2 {
			t.Fatalf("%s: %q is not `<verb> <resource>`", driftReadsGolden, line)
		}
		reads = append(reads, [2]string{fields[0], fields[1]})
	}
	if len(reads) == 0 {
		t.Fatalf("%s holds no reads, so the coverage check would pass for the wrong reason", driftReadsGolden)
	}
	return reads
}

// driftCoverageProblems holds the golden and the ClusterRole together: it
// reports a golden read the grant does not allow, a grant the golden lists no
// read of, a verb the grant may not name, a resource with no API group in
// driftGranted, and an empty golden, so neither side can move alone.
func driftCoverageProblems(reads [][2]string, rules []driftRBACRule) []string {
	if len(reads) == 0 {
		return []string{fmt.Sprintf("%s lists no read, so it covers nothing", driftReadsGolden)}
	}
	listed := map[string]bool{}
	var problems []string
	for _, read := range reads {
		verb, resource := read[0], read[1]
		listed[resource] = true
		if !slices.Contains(driftGrantedVerbs, verb) {
			problems = append(problems, fmt.Sprintf("%s lists verb %q on %q, which the grant may not name", driftReadsGolden, verb, resource))
			continue
		}
		group, known := driftGranted[resource]
		if !known {
			problems = append(problems, fmt.Sprintf("%s lists %q, which has no API group", driftReadsGolden, resource))
			continue
		}
		if !driftAllows(rules, group, resource, verb) {
			problems = append(problems, fmt.Sprintf("ClusterRole %s grants no %q on %s/%s, so the drift compare's read of it is refused",
				driftReaderRole, verb, group, resource))
		}
	}
	for resource := range driftGranted {
		if !listed[resource] {
			problems = append(problems, fmt.Sprintf("ClusterRole %s grants %q, and %s lists no read of it", driftReaderRole, resource, driftReadsGolden))
		}
	}
	return problems
}

// TestDriftRBACCoverageProblemsReportsEachFailingBranch drives
// driftCoverageProblems with one disagreement applied, so each refusal it makes
// has a row that turns it red. The unmutated pair passes.
func TestDriftRBACCoverageProblemsReportsEachFailingBranch(t *testing.T) {
	t.Parallel()
	rules := driftRoleRules(t, loadDriftRBAC(t))
	reads := loadDriftReads(t)
	if problems := driftCoverageProblems(reads, rules); len(problems) > 0 {
		t.Fatalf("the golden and ClusterRole disagree: %q", problems)
	}
	granted := func(verb, resource string) string {
		return fmt.Sprintf("ClusterRole %s grants no %q on %s/%s, so the drift compare's read of it is refused",
			driftReaderRole, verb, driftGranted[resource], resource)
	}
	tests := []struct {
		name  string
		reads [][2]string
		rules []driftRBACRule
		want  string
	}{
		{name: "a golden read the grant does not allow", reads: reads,
			rules: rules[:1], want: granted("get", "configmaps")},
		{name: "a grant the golden does not list", reads: [][2]string{{"get", "deployments"}},
			rules: rules, want: fmt.Sprintf("ClusterRole %s grants %q, and %s lists no read of it", driftReaderRole, "configmaps", driftReadsGolden)},
		{name: "a verb the grant may not name", reads: [][2]string{{"delete", "deployments"}, {"get", "configmaps"}},
			rules: rules, want: fmt.Sprintf("%s lists verb %q on %q, which the grant may not name", driftReadsGolden, "delete", "deployments")},
		{name: "a resource with no API group", reads: [][2]string{{"get", "secrets"}, {"get", "deployments"}, {"get", "configmaps"}},
			rules: rules, want: fmt.Sprintf("%s lists %q, which has no API group", driftReadsGolden, "secrets")},
		{name: "an empty golden", reads: nil,
			rules: rules, want: fmt.Sprintf("%s lists no read, so it covers nothing", driftReadsGolden)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if problems := driftCoverageProblems(tc.reads, tc.rules); !slices.Contains(problems, tc.want) {
				t.Errorf("with %s, want the problem %q, got %q", tc.name, tc.want, problems)
			}
		})
	}
}

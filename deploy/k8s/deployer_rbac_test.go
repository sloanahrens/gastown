package k8s

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The deployer credentials' cluster access is written down in
// deploy/k8s/deployer-rbac.yaml: the apply Role and its binding, the policy
// ClusterRole and its binding, and the read supplement bound to the certificate
// group. The tests below hold each grant to an allowlist, the file to the grants
// the design forbids, and the bindings to the subjects the certificates carry, in
// the style of TestSweepRBACClusterGrantIsReadOnly.
const (
	deployerRBACManifest = "deployer-rbac.yaml"
	deployerApplyRole    = "deployer-apply"
	deployerPolicyRole   = "deployer-policy"
	deployerReaderRole   = "gt-deployer-reader-extra"
	deployerApplyUser    = "gt-deployer-apply"
	deployerPolicyUser   = "gt-deployer-policy"
	deployerGroup        = "gt-deployers"
	deployerNamespace    = "gt"

	deployerRBACGroup      = "rbac.authorization.k8s.io"
	deployerAdmissionGroup = "admissionregistration.k8s.io"
	deployerSchedGroup     = "scheduling.k8s.io"
)

// deployerGrant is what a role may grant one resource: the API group it belongs
// to and the verbs the credential needs on it.
type deployerGrant struct {
	group string
	verbs []string
}

var (
	// deployerApplyVerbs is every verb deployer-apply may grant: it writes the
	// namespaced workload kinds and removes none of them.
	deployerApplyVerbs = []string{"get", "list", "watch", "create", "update", "patch"}
	// deployerPolicyVerbs is every verb deployer-policy may grant on the kinds it
	// writes.
	deployerPolicyVerbs = []string{"get", "list", "watch", "create", "update", "patch"}
	// deployerRoleOwnerVerbs adds the two verbs a credential needs to create a
	// role wider than its own: creating or widening one is escalate, and making
	// it reach a subject is bind.
	deployerRoleOwnerVerbs = []string{"get", "list", "watch", "create", "update", "patch", "escalate", "bind"}
	// deployerReaderVerbs is what the supplement reads with: the drift audit reads
	// and never writes.
	deployerReaderVerbs = []string{"get", "list"}
)

// deployerApplyAllowed is the exact set the Role deployer-apply in gt may grant:
// the five namespaced workload kinds, and nothing in any other group.
var deployerApplyAllowed = map[string]deployerGrant{
	"serviceaccounts":        {"", deployerApplyVerbs},
	"services":               {"", deployerApplyVerbs},
	"persistentvolumeclaims": {"", deployerApplyVerbs},
	"deployments":            {"apps", deployerApplyVerbs},
	"cronjobs":               {"batch", deployerApplyVerbs},
}

// deployerPolicyAllowed is the exact set the ClusterRole deployer-policy may
// grant: RBAC, admission policies, priority classes and namespaces, with
// escalate and bind on the two role kinds only.
var deployerPolicyAllowed = map[string]deployerGrant{
	"roles":                             {deployerRBACGroup, deployerRoleOwnerVerbs},
	"rolebindings":                      {deployerRBACGroup, deployerPolicyVerbs},
	"clusterroles":                      {deployerRBACGroup, deployerRoleOwnerVerbs},
	"clusterrolebindings":               {deployerRBACGroup, deployerPolicyVerbs},
	"validatingadmissionpolicies":       {deployerAdmissionGroup, deployerPolicyVerbs},
	"validatingadmissionpolicybindings": {deployerAdmissionGroup, deployerPolicyVerbs},
	"priorityclasses":                   {deployerSchedGroup, deployerPolicyVerbs},
	"namespaces":                        {"", deployerPolicyVerbs},
}

// deployerReaderAllowed is the exact set the read supplement may grant: the
// cluster-scoped kinds the all-objects drift compare reads that the
// gt-drift-reader ClusterRole does not already name.
var deployerReaderAllowed = map[string]deployerGrant{
	"clusterroles":        {deployerRBACGroup, deployerReaderVerbs},
	"clusterrolebindings": {deployerRBACGroup, deployerReaderVerbs},
	"priorityclasses":     {deployerSchedGroup, deployerReaderVerbs},
	"namespaces":          {"", deployerReaderVerbs},
}

// deployerWorkloadKinds are the kinds only deployer-apply writes; the policy
// credential may name none of them.
var deployerWorkloadKinds = []string{"serviceaccounts", "services", "persistentvolumeclaims", "deployments", "cronjobs"}

// deployerPrivilegedKinds are the RBAC and admission kinds and the Secret kind;
// an apply rule names none of them.
var deployerPrivilegedKinds = []string{
	"roles", "rolebindings", "clusterroles", "clusterrolebindings",
	"validatingadmissionpolicies", "validatingadmissionpolicybindings",
	"secrets",
}

// deployerRemoveVerbs are the verbs that take an object away, which no deployer
// credential runs.
var deployerRemoveVerbs = []string{"delete", "deletecollection"}

// deployerRole is one grant-bearing object the manifest must hold, with the set
// its rules are held to.
type deployerRole struct {
	kind, name, namespace string
	allowed               map[string]deployerGrant
}

// deployerRoles is every grant-bearing object the file must hold. A Role or
// ClusterRole it holds beyond these fails: the allowlist names no grant for it.
var deployerRoles = []deployerRole{
	{"Role", deployerApplyRole, deployerNamespace, deployerApplyAllowed},
	{"ClusterRole", deployerPolicyRole, "", deployerPolicyAllowed},
	{"ClusterRole", deployerReaderRole, "", deployerReaderAllowed},
}

// deployerBinding is one binding the file must hold: the role it refs and the
// single subject the certificate carries.
type deployerBinding struct {
	kind, name, namespace    string
	roleKind, roleName       string
	subjectKind, subjectName string
}

// deployerBindings is every binding the certificates rely on. The two Users are
// the common names their certificates carry; the Group is the organization both
// certificates carry.
var deployerBindings = []deployerBinding{
	{"RoleBinding", deployerApplyRole, deployerNamespace, "Role", deployerApplyRole, "User", deployerApplyUser},
	{"ClusterRoleBinding", deployerPolicyRole, "", "ClusterRole", deployerPolicyRole, "User", deployerPolicyUser},
	{"ClusterRoleBinding", deployerReaderRole, "", "ClusterRole", deployerReaderRole, "Group", deployerGroup},
}

// deployerRBACRule is one rule of a Role or ClusterRole.
type deployerRBACRule struct {
	APIGroups []string `yaml:"apiGroups"`
	Resources []string `yaml:"resources"`
	Verbs     []string `yaml:"verbs"`
}

// deployerRBACDoc is the part of one manifest document the checks read.
type deployerRBACDoc struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name      string `yaml:"name"`
		Namespace string `yaml:"namespace"`
	} `yaml:"metadata"`
	Rules   []deployerRBACRule `yaml:"rules"`
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

// loadDeployerRBAC parses every document of the manifest. A file that is not
// valid YAML fails here, and so does an empty one, which would otherwise pass
// every check for the wrong reason.
func loadDeployerRBAC(t *testing.T) []deployerRBACDoc {
	t.Helper()
	raw, err := os.ReadFile(filepath.FromSlash(deployerRBACManifest))
	if err != nil {
		t.Fatalf("reading %s: %v", deployerRBACManifest, err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	var docs []deployerRBACDoc
	for {
		var doc deployerRBACDoc
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("%s is not valid YAML: %v", deployerRBACManifest, err)
		}
		if doc.Kind != "" {
			docs = append(docs, doc)
		}
	}
	if len(docs) == 0 {
		t.Fatalf("%s holds no objects, so the checks would pass for the wrong reason", deployerRBACManifest)
	}
	return docs
}

// findDeployerRBAC returns the document of the kind, name and namespace, or nil.
func findDeployerRBAC(docs []deployerRBACDoc, kind, name, namespace string) *deployerRBACDoc {
	for i := range docs {
		d := &docs[i]
		if d.Kind == kind && d.Metadata.Name == name && d.Metadata.Namespace == namespace {
			return d
		}
	}
	return nil
}

// deployerRoleRules returns the rules of the Role or ClusterRole named, failing
// when the manifest has no such object.
func deployerRoleRules(t *testing.T, docs []deployerRBACDoc, name string) []deployerRBACRule {
	t.Helper()
	for _, d := range docs {
		if (d.Kind == "Role" || d.Kind == "ClusterRole") && d.Metadata.Name == name {
			return d.Rules
		}
	}
	t.Fatalf("%s has no Role or ClusterRole %q", deployerRBACManifest, name)
	return nil
}

// deployerViolationKind is the kind of one violation.
type deployerViolationKind int

const (
	deployerWildcardAPIGroup deployerViolationKind = iota
	deployerWildcardResource
	deployerWildcardVerb
	deployerUnknownResource
	deployerWrongAPIGroup
	deployerVerbOutsideBound
	deployerMissingGrant
	deployerMissingRole
	deployerUnlistedRole
	deployerSecretsResource
	deployerRemoveVerb
	deployerPolicyWorkloadKind
	deployerApplyPrivilegedKind
	deployerReaderNotSupplement
)

// deployerViolation is one grant in the file the design does not hold. The
// fields name the role, the rule and what it grants, so a test asserts on them
// rather than on the wording of a message.
type deployerViolation struct {
	Role     string
	Rule     int // the rule's 1-based position in the role, 0 for a whole-object refusal
	Kind     deployerViolationKind
	APIGroup string
	Resource string
	Verb     string
}

func (v deployerViolation) String() string {
	switch v.Kind {
	case deployerWildcardAPIGroup:
		return fmt.Sprintf("%s rule %d grants every API group with a wildcard", v.Role, v.Rule)
	case deployerWildcardResource:
		return fmt.Sprintf("%s rule %d grants every resource with a wildcard", v.Role, v.Rule)
	case deployerWildcardVerb:
		return fmt.Sprintf("%s rule %d grants every verb with a wildcard", v.Role, v.Rule)
	case deployerUnknownResource:
		return fmt.Sprintf("%s rule %d grants resource %q, which its allowlist does not name", v.Role, v.Rule, v.Resource)
	case deployerWrongAPIGroup:
		return fmt.Sprintf("%s rule %d grants %q in API group %q, which is not its group", v.Role, v.Rule, v.Resource, v.APIGroup)
	case deployerVerbOutsideBound:
		return fmt.Sprintf("%s rule %d grants %q on %q, which the credential never runs", v.Role, v.Rule, v.Verb, v.Resource)
	case deployerMissingGrant:
		return fmt.Sprintf("%s grants no %q on %s, so the credential cannot write it", v.Role, v.Verb, deployerResourceName(v.APIGroup, v.Resource))
	case deployerMissingRole:
		return fmt.Sprintf("%s has no Role or ClusterRole %q", deployerRBACManifest, v.Role)
	case deployerUnlistedRole:
		return fmt.Sprintf("%s holds a Role or ClusterRole %q, which the allowlist does not name", deployerRBACManifest, v.Role)
	case deployerSecretsResource:
		return fmt.Sprintf("%s rule %d grants resource %q, which no deployer credential reads or writes", v.Role, v.Rule, v.Resource)
	case deployerRemoveVerb:
		return fmt.Sprintf("%s rule %d grants %q on %q, and a deployer removes nothing", v.Role, v.Rule, v.Verb, v.Resource)
	case deployerPolicyWorkloadKind:
		return fmt.Sprintf("%s rule %d grants workload kind %q, which only deployer-apply writes", v.Role, v.Rule, v.Resource)
	case deployerApplyPrivilegedKind:
		return fmt.Sprintf("%s rule %d grants %q, which is RBAC, admission or Secret and belongs to deployer-policy", v.Role, v.Rule, v.Resource)
	case deployerReaderNotSupplement:
		return fmt.Sprintf("%s rule %d grants %q, which the drift read grant already names", v.Role, v.Rule, v.Resource)
	}
	return fmt.Sprintf("%s has an unknown violation", v.Role)
}

// deployerResourceName names a resource with its API group, spelling the empty
// API group "core" so a refusal names a group rather than a bare slash.
func deployerResourceName(group, resource string) string {
	if group == "" {
		return "core/" + resource
	}
	return group + "/" + resource
}

// deployerAllows reports whether one of the rules grants verb on group's
// resource. A wildcard is not understood, so a rule that uses one grants
// nothing here.
func deployerAllows(rules []deployerRBACRule, group, resource, verb string) bool {
	for _, r := range rules {
		if slices.Contains(r.APIGroups, group) && slices.Contains(r.Resources, resource) && slices.Contains(r.Verbs, verb) {
			return true
		}
	}
	return false
}

// deployerWidenings reports every grant in a role's rules that its allowlist
// does not hold: a wildcard in any of the three places a rule names one, a
// resource outside the allowlist, one in the wrong API group, and a verb the
// allowlist does not give it.
func deployerWidenings(rules []deployerRBACRule, role string, allowed map[string]deployerGrant) []deployerViolation {
	var widened []deployerViolation
	for i, rule := range rules {
		n := i + 1
		if slices.Contains(rule.APIGroups, "*") {
			widened = append(widened, deployerViolation{Role: role, Rule: n, Kind: deployerWildcardAPIGroup, APIGroup: "*"})
		}
		if slices.Contains(rule.Resources, "*") {
			widened = append(widened, deployerViolation{Role: role, Rule: n, Kind: deployerWildcardResource, Resource: "*"})
		}
		if slices.Contains(rule.Verbs, "*") {
			widened = append(widened, deployerViolation{Role: role, Rule: n, Kind: deployerWildcardVerb, Verb: "*"})
		}
		for _, resource := range rule.Resources {
			grant, known := allowed[resource]
			if !known {
				widened = append(widened, deployerViolation{Role: role, Rule: n, Kind: deployerUnknownResource, Resource: resource})
				continue
			}
			for _, g := range rule.APIGroups {
				if g != "*" && g != grant.group {
					widened = append(widened, deployerViolation{Role: role, Rule: n, Kind: deployerWrongAPIGroup, APIGroup: g, Resource: resource})
				}
			}
			for _, verb := range rule.Verbs {
				if verb != "*" && !slices.Contains(grant.verbs, verb) {
					widened = append(widened, deployerViolation{Role: role, Rule: n, Kind: deployerVerbOutsideBound, Resource: resource, Verb: verb})
				}
			}
		}
	}
	return widened
}

// deployerMissingGrants reports every resource and verb in a role's allowlist
// that its rules do not grant, so a grant the credential cannot do its job
// without cannot be dropped silently.
func deployerMissingGrants(rules []deployerRBACRule, role string, allowed map[string]deployerGrant) []deployerViolation {
	var missing []deployerViolation
	for _, resource := range slices.Sorted(maps.Keys(allowed)) {
		grant := allowed[resource]
		for _, verb := range grant.verbs {
			if !deployerAllows(rules, grant.group, resource, verb) {
				missing = append(missing, deployerViolation{
					Role: role, Kind: deployerMissingGrant, APIGroup: grant.group, Resource: resource, Verb: verb,
				})
			}
		}
	}
	return missing
}

// deployerGrantViolations holds every grant-bearing object in docs to its
// allowlist, and reports an object the allowlist does not name and one it names
// that is missing.
func deployerGrantViolations(docs []deployerRBACDoc) []deployerViolation {
	var violations []deployerViolation
	named := map[string]bool{}
	for _, role := range deployerRoles {
		d := findDeployerRBAC(docs, role.kind, role.name, role.namespace)
		if d == nil {
			violations = append(violations, deployerViolation{Role: role.name, Kind: deployerMissingRole})
			continue
		}
		named[role.kind+"/"+role.name] = true
		violations = append(violations, deployerWidenings(d.Rules, role.name, role.allowed)...)
		violations = append(violations, deployerMissingGrants(d.Rules, role.name, role.allowed)...)
	}
	for _, d := range docs {
		if d.Kind != "Role" && d.Kind != "ClusterRole" {
			continue
		}
		if !named[d.Kind+"/"+d.Metadata.Name] {
			violations = append(violations, deployerViolation{Role: d.Metadata.Name, Kind: deployerUnlistedRole})
		}
	}
	return violations
}

// deployerGrantProblems reports every way a role's grant leaves its allowlist.
func deployerGrantProblems(docs []deployerRBACDoc) []string {
	var problems []string
	for _, v := range deployerGrantViolations(docs) {
		problems = append(problems, v.String())
	}
	return problems
}

// deployerRefusalViolations reports every grant the design forbids whatever role
// it sits in: a Secret, a wildcard, a removing verb, a workload kind in the
// policy role, an RBAC, admission or Secret kind in the apply role, and a kind
// in the supplement the drift read grant already names.
func deployerRefusalViolations(docs []deployerRBACDoc) []deployerViolation {
	var refusals []deployerViolation
	for _, d := range docs {
		if d.Kind != "Role" && d.Kind != "ClusterRole" {
			continue
		}
		role := d.Metadata.Name
		for i, rule := range d.Rules {
			n := i + 1
			if slices.Contains(rule.APIGroups, "*") {
				refusals = append(refusals, deployerViolation{Role: role, Rule: n, Kind: deployerWildcardAPIGroup, APIGroup: "*"})
			}
			if slices.Contains(rule.Resources, "*") {
				refusals = append(refusals, deployerViolation{Role: role, Rule: n, Kind: deployerWildcardResource, Resource: "*"})
			}
			if slices.Contains(rule.Verbs, "*") {
				refusals = append(refusals, deployerViolation{Role: role, Rule: n, Kind: deployerWildcardVerb, Verb: "*"})
			}
			for _, resource := range rule.Resources {
				switch {
				case resource == "secrets":
					refusals = append(refusals, deployerViolation{Role: role, Rule: n, Kind: deployerSecretsResource, Resource: resource})
				case role == deployerPolicyRole && slices.Contains(deployerWorkloadKinds, resource):
					refusals = append(refusals, deployerViolation{Role: role, Rule: n, Kind: deployerPolicyWorkloadKind, Resource: resource})
				case role == deployerApplyRole && slices.Contains(deployerPrivilegedKinds, resource):
					refusals = append(refusals, deployerViolation{Role: role, Rule: n, Kind: deployerApplyPrivilegedKind, Resource: resource})
				case role == deployerReaderRole:
					if _, already := driftGranted[resource]; already {
						refusals = append(refusals, deployerViolation{Role: role, Rule: n, Kind: deployerReaderNotSupplement, Resource: resource})
					}
				}
				for _, verb := range rule.Verbs {
					if slices.Contains(deployerRemoveVerbs, verb) {
						refusals = append(refusals, deployerViolation{Role: role, Rule: n, Kind: deployerRemoveVerb, Resource: resource, Verb: verb})
					}
				}
			}
		}
	}
	return refusals
}

// deployerRefusalProblems reports every grant the design forbids.
func deployerRefusalProblems(docs []deployerRBACDoc) []string {
	var problems []string
	for _, v := range deployerRefusalViolations(docs) {
		problems = append(problems, v.String())
	}
	return problems
}

// deployerSubjectProblems holds the bindings to deployerBindings: each refs the
// role the credential's grant lives in, binds exactly the one subject its
// certificate carries, and no other binding in the file binds a subject the
// certificates do not carry.
func deployerSubjectProblems(docs []deployerRBACDoc) []string {
	var problems []string
	named := map[string]bool{}
	for _, want := range deployerBindings {
		b := findDeployerRBAC(docs, want.kind, want.name, want.namespace)
		if b == nil {
			problems = append(problems, fmt.Sprintf("%s has no %s %q", deployerRBACManifest, want.kind, want.name))
			continue
		}
		named[want.kind+"/"+want.name] = true
		if b.RoleRef.Kind != want.roleKind || b.RoleRef.Name != want.roleName {
			problems = append(problems, fmt.Sprintf("%s %q refs %s %q, want %s %q",
				want.kind, want.name, b.RoleRef.Kind, b.RoleRef.Name, want.roleKind, want.roleName))
		}
		switch {
		case len(b.Subjects) != 1:
			problems = append(problems, fmt.Sprintf("%s %q binds %d subjects, want only %s %q",
				want.kind, want.name, len(b.Subjects), want.subjectKind, want.subjectName))
		case b.Subjects[0].Kind != want.subjectKind || b.Subjects[0].Name != want.subjectName:
			problems = append(problems, fmt.Sprintf("%s %q binds %s %q, want %s %q",
				want.kind, want.name, b.Subjects[0].Kind, b.Subjects[0].Name, want.subjectKind, want.subjectName))
		}
	}
	for _, d := range docs {
		if d.Kind != "RoleBinding" && d.Kind != "ClusterRoleBinding" {
			continue
		}
		if !named[d.Kind+"/"+d.Metadata.Name] {
			problems = append(problems, fmt.Sprintf("%s holds a %s %q, which binds no subject the certificates carry",
				deployerRBACManifest, d.Kind, d.Metadata.Name))
		}
	}
	return problems
}

// cloneDeployerDocs returns a copy of docs whose rules and subjects a test can
// edit without touching the manifest the other tests read.
func cloneDeployerDocs(docs []deployerRBACDoc) []deployerRBACDoc {
	out := slices.Clone(docs)
	for i := range out {
		rules := make([]deployerRBACRule, len(out[i].Rules))
		for j, r := range out[i].Rules {
			rules[j] = deployerRBACRule{
				APIGroups: slices.Clone(r.APIGroups),
				Resources: slices.Clone(r.Resources),
				Verbs:     slices.Clone(r.Verbs),
			}
		}
		out[i].Rules = rules
		out[i].Subjects = slices.Clone(out[i].Subjects)
	}
	return out
}

// editDeployerRole returns docs with edit applied to every Role or ClusterRole
// named.
func editDeployerRole(docs []deployerRBACDoc, name string, edit func(*deployerRBACDoc)) []deployerRBACDoc {
	for i := range docs {
		if (docs[i].Kind == "Role" || docs[i].Kind == "ClusterRole") && docs[i].Metadata.Name == name {
			edit(&docs[i])
		}
	}
	return docs
}

// appendDeployerRule returns docs with one rule added to the role named, so a
// mutation is one deliberate widening.
func appendDeployerRule(docs []deployerRBACDoc, name string, rule deployerRBACRule) []deployerRBACDoc {
	return editDeployerRole(docs, name, func(d *deployerRBACDoc) { d.Rules = append(d.Rules, rule) })
}

// dropDeployerRoles returns docs with every Role or ClusterRole named removed,
// keeping the bindings, so the missing-role refusal is driven alone.
func dropDeployerRoles(docs []deployerRBACDoc, name string) []deployerRBACDoc {
	return slices.DeleteFunc(docs, func(d deployerRBACDoc) bool {
		return (d.Kind == "Role" || d.Kind == "ClusterRole") && d.Metadata.Name == name
	})
}

// TestDeployerRBACRoles holds each role's grant to its allowlist, then drives
// every refusal that allowlist makes on a clone with one widening applied. The
// unmutated manifest passes.
func TestDeployerRBACRoles(t *testing.T) {
	t.Parallel()
	docs := loadDeployerRBAC(t)
	if problems := deployerGrantProblems(docs); len(problems) > 0 {
		t.Fatalf("the manifest as written is refused: %q", problems)
	}
	applyRules := len(deployerRoleRules(t, docs, deployerApplyRole))
	policyRules := len(deployerRoleRules(t, docs, deployerPolicyRole))
	readerRules := len(deployerRoleRules(t, docs, deployerReaderRole))
	tests := []struct {
		name string
		edit func(docs []deployerRBACDoc) []deployerRBACDoc
		want deployerViolation
	}{
		{
			name: "a resource outside the apply allowlist",
			edit: func(docs []deployerRBACDoc) []deployerRBACDoc {
				return appendDeployerRule(docs, deployerApplyRole, deployerRBACRule{APIGroups: []string{""}, Resources: []string{"configmaps"}, Verbs: []string{"get"}})
			},
			want: deployerViolation{Role: deployerApplyRole, Rule: applyRules + 1, Kind: deployerUnknownResource, Resource: "configmaps"},
		},
		{
			name: "a verb outside the apply allowlist",
			edit: func(docs []deployerRBACDoc) []deployerRBACDoc {
				return editDeployerRole(docs, deployerApplyRole, func(d *deployerRBACDoc) {
					d.Rules[0].Verbs = append(d.Rules[0].Verbs, "delete")
				})
			},
			want: deployerViolation{Role: deployerApplyRole, Rule: 1, Kind: deployerVerbOutsideBound, Resource: "serviceaccounts", Verb: "delete"},
		},
		{
			name: "an apply resource in the wrong API group",
			edit: func(docs []deployerRBACDoc) []deployerRBACDoc {
				return editDeployerRole(docs, deployerApplyRole, func(d *deployerRBACDoc) {
					d.Rules[1].APIGroups = []string{"batch"}
				})
			},
			want: deployerViolation{Role: deployerApplyRole, Rule: 2, Kind: deployerWrongAPIGroup, APIGroup: "batch", Resource: "deployments"},
		},
		{
			name: "a wildcard resource in the policy role",
			edit: func(docs []deployerRBACDoc) []deployerRBACDoc {
				return appendDeployerRule(docs, deployerPolicyRole, deployerRBACRule{APIGroups: []string{""}, Resources: []string{"*"}, Verbs: []string{"get"}})
			},
			want: deployerViolation{Role: deployerPolicyRole, Rule: policyRules + 1, Kind: deployerWildcardResource, Resource: "*"},
		},
		{
			name: "a wildcard API group in the policy role",
			edit: func(docs []deployerRBACDoc) []deployerRBACDoc {
				return appendDeployerRule(docs, deployerPolicyRole, deployerRBACRule{APIGroups: []string{"*"}, Resources: []string{"namespaces"}, Verbs: []string{"get"}})
			},
			want: deployerViolation{Role: deployerPolicyRole, Rule: policyRules + 1, Kind: deployerWildcardAPIGroup, APIGroup: "*"},
		},
		{
			name: "a wildcard verb in the read supplement",
			edit: func(docs []deployerRBACDoc) []deployerRBACDoc {
				return appendDeployerRule(docs, deployerReaderRole, deployerRBACRule{APIGroups: []string{deployerRBACGroup}, Resources: []string{"clusterroles"}, Verbs: []string{"*"}})
			},
			want: deployerViolation{Role: deployerReaderRole, Rule: readerRules + 1, Kind: deployerWildcardVerb, Verb: "*"},
		},
		{
			name: "a granted verb removed",
			edit: func(docs []deployerRBACDoc) []deployerRBACDoc {
				return editDeployerRole(docs, deployerApplyRole, func(d *deployerRBACDoc) {
					d.Rules[0].Verbs = slices.DeleteFunc(slices.Clone(d.Rules[0].Verbs), func(v string) bool { return v == "patch" })
				})
			},
			want: deployerViolation{Role: deployerApplyRole, Kind: deployerMissingGrant, APIGroup: "", Resource: "serviceaccounts", Verb: "patch"},
		},
		{
			name: "a role the manifest drops",
			edit: func(docs []deployerRBACDoc) []deployerRBACDoc {
				return dropDeployerRoles(docs, deployerPolicyRole)
			},
			want: deployerViolation{Role: deployerPolicyRole, Kind: deployerMissingRole},
		},
		{
			name: "a role the allowlist does not name",
			edit: func(docs []deployerRBACDoc) []deployerRBACDoc {
				return editDeployerRole(docs, deployerApplyRole, func(d *deployerRBACDoc) {
					d.Metadata.Name = "deployer-apply-2"
				})
			},
			want: deployerViolation{Role: "deployer-apply-2", Kind: deployerUnlistedRole},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if !slices.Contains(deployerGrantViolations(tc.edit(cloneDeployerDocs(docs))), tc.want) {
				t.Errorf("with %s the role was not refused as %q; got %q",
					tc.name, tc.want, deployerGrantProblems(tc.edit(cloneDeployerDocs(docs))))
			}
		})
	}
}

// TestDeployerRBACRefusals holds the file to the grants the design forbids
// whatever role they sit in, then drives every refusal on a clone with one
// planted grant. The unmutated manifest passes.
func TestDeployerRBACRefusals(t *testing.T) {
	t.Parallel()
	docs := loadDeployerRBAC(t)
	if problems := deployerRefusalProblems(docs); len(problems) > 0 {
		t.Fatalf("the manifest as written is refused: %q", problems)
	}
	applyRules := len(deployerRoleRules(t, docs, deployerApplyRole))
	policyRules := len(deployerRoleRules(t, docs, deployerPolicyRole))
	readerRules := len(deployerRoleRules(t, docs, deployerReaderRole))
	tests := []struct {
		name string
		edit func(docs []deployerRBACDoc) []deployerRBACDoc
		want deployerViolation
	}{
		{
			name: "a secrets rule planted in the apply role",
			edit: func(docs []deployerRBACDoc) []deployerRBACDoc {
				return appendDeployerRule(docs, deployerApplyRole, deployerRBACRule{APIGroups: []string{""}, Resources: []string{"secrets"}, Verbs: []string{"get"}})
			},
			want: deployerViolation{Role: deployerApplyRole, Rule: applyRules + 1, Kind: deployerSecretsResource, Resource: "secrets"},
		},
		{
			name: "a secrets rule planted in the policy role",
			edit: func(docs []deployerRBACDoc) []deployerRBACDoc {
				return appendDeployerRule(docs, deployerPolicyRole, deployerRBACRule{APIGroups: []string{""}, Resources: []string{"secrets"}, Verbs: []string{"create"}})
			},
			want: deployerViolation{Role: deployerPolicyRole, Rule: policyRules + 1, Kind: deployerSecretsResource, Resource: "secrets"},
		},
		{
			name: "a wildcard API group",
			edit: func(docs []deployerRBACDoc) []deployerRBACDoc {
				return appendDeployerRule(docs, deployerPolicyRole, deployerRBACRule{APIGroups: []string{"*"}, Resources: []string{""}, Verbs: []string{"get"}})
			},
			want: deployerViolation{Role: deployerPolicyRole, Rule: policyRules + 1, Kind: deployerWildcardAPIGroup, APIGroup: "*"},
		},
		{
			name: "a wildcard resource",
			edit: func(docs []deployerRBACDoc) []deployerRBACDoc {
				return appendDeployerRule(docs, deployerReaderRole, deployerRBACRule{APIGroups: []string{""}, Resources: []string{"*"}, Verbs: []string{"get"}})
			},
			want: deployerViolation{Role: deployerReaderRole, Rule: readerRules + 1, Kind: deployerWildcardResource, Resource: "*"},
		},
		{
			name: "a wildcard verb",
			edit: func(docs []deployerRBACDoc) []deployerRBACDoc {
				return editDeployerRole(docs, deployerApplyRole, func(d *deployerRBACDoc) {
					d.Rules[0].Verbs = append(d.Rules[0].Verbs, "*")
				})
			},
			want: deployerViolation{Role: deployerApplyRole, Rule: 1, Kind: deployerWildcardVerb, Verb: "*"},
		},
		{
			name: "a delete verb on the apply role",
			edit: func(docs []deployerRBACDoc) []deployerRBACDoc {
				return appendDeployerRule(docs, deployerApplyRole, deployerRBACRule{APIGroups: []string{"apps"}, Resources: []string{"deployments"}, Verbs: []string{"delete"}})
			},
			want: deployerViolation{Role: deployerApplyRole, Rule: applyRules + 1, Kind: deployerRemoveVerb, Resource: "deployments", Verb: "delete"},
		},
		{
			name: "a deletecollection verb on the policy role",
			edit: func(docs []deployerRBACDoc) []deployerRBACDoc {
				return appendDeployerRule(docs, deployerPolicyRole, deployerRBACRule{APIGroups: []string{deployerRBACGroup}, Resources: []string{"rolebindings"}, Verbs: []string{"deletecollection"}})
			},
			want: deployerViolation{Role: deployerPolicyRole, Rule: policyRules + 1, Kind: deployerRemoveVerb, Resource: "rolebindings", Verb: "deletecollection"},
		},
		{
			name: "a workload kind in the policy role",
			edit: func(docs []deployerRBACDoc) []deployerRBACDoc {
				return appendDeployerRule(docs, deployerPolicyRole, deployerRBACRule{APIGroups: []string{"apps"}, Resources: []string{"deployments"}, Verbs: []string{"get"}})
			},
			want: deployerViolation{Role: deployerPolicyRole, Rule: policyRules + 1, Kind: deployerPolicyWorkloadKind, Resource: "deployments"},
		},
		{
			name: "an RBAC kind in the apply role",
			edit: func(docs []deployerRBACDoc) []deployerRBACDoc {
				return appendDeployerRule(docs, deployerApplyRole, deployerRBACRule{APIGroups: []string{deployerRBACGroup}, Resources: []string{"roles"}, Verbs: []string{"get"}})
			},
			want: deployerViolation{Role: deployerApplyRole, Rule: applyRules + 1, Kind: deployerApplyPrivilegedKind, Resource: "roles"},
		},
		{
			name: "an admission kind in the apply role",
			edit: func(docs []deployerRBACDoc) []deployerRBACDoc {
				return appendDeployerRule(docs, deployerApplyRole, deployerRBACRule{APIGroups: []string{deployerAdmissionGroup}, Resources: []string{"validatingadmissionpolicies"}, Verbs: []string{"get"}})
			},
			want: deployerViolation{Role: deployerApplyRole, Rule: applyRules + 1, Kind: deployerApplyPrivilegedKind, Resource: "validatingadmissionpolicies"},
		},
		{
			name: "a supplement kind the drift read grant already names",
			edit: func(docs []deployerRBACDoc) []deployerRBACDoc {
				return appendDeployerRule(docs, deployerReaderRole, deployerRBACRule{APIGroups: []string{""}, Resources: []string{"configmaps"}, Verbs: []string{"get"}})
			},
			want: deployerViolation{Role: deployerReaderRole, Rule: readerRules + 1, Kind: deployerReaderNotSupplement, Resource: "configmaps"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if !slices.Contains(deployerRefusalViolations(tc.edit(cloneDeployerDocs(docs))), tc.want) {
				t.Errorf("with %s the grant was not refused as %q; got %q",
					tc.name, tc.want, deployerRefusalProblems(tc.edit(cloneDeployerDocs(docs))))
			}
		})
	}
}

// TestDeployerRBACSubjects holds the bindings to the subjects the certificates
// carry, drives each subject refusal on a clone, and proves Static() lists the
// file's six objects, so the manifest is part of the renderer's static set.
func TestDeployerRBACSubjects(t *testing.T) {
	t.Parallel()
	docs := loadDeployerRBAC(t)
	if problems := deployerSubjectProblems(docs); len(problems) > 0 {
		t.Fatalf("the manifest as written is refused: %q", problems)
	}
	tests := []struct {
		name string
		edit func(docs []deployerRBACDoc) []deployerRBACDoc
		want string
	}{
		{
			name: "a binding the file drops",
			edit: func(docs []deployerRBACDoc) []deployerRBACDoc {
				return slices.DeleteFunc(docs, func(d deployerRBACDoc) bool {
					return d.Kind == "ClusterRoleBinding" && d.Metadata.Name == deployerReaderRole
				})
			},
			want: fmt.Sprintf("has no ClusterRoleBinding %q", deployerReaderRole),
		},
		{
			name: "a roleRef that names another role",
			edit: func(docs []deployerRBACDoc) []deployerRBACDoc {
				for i := range docs {
					if docs[i].Kind == "ClusterRoleBinding" && docs[i].Metadata.Name == deployerPolicyRole {
						docs[i].RoleRef.Name = deployerApplyRole
					}
				}
				return docs
			},
			want: fmt.Sprintf("refs ClusterRole %q", deployerApplyRole),
		},
		{
			name: "a binding with a second subject",
			edit: func(docs []deployerRBACDoc) []deployerRBACDoc {
				for i := range docs {
					if docs[i].Kind == "RoleBinding" && docs[i].Metadata.Name == deployerApplyRole {
						docs[i].Subjects = append(docs[i].Subjects, docs[i].Subjects[0])
					}
				}
				return docs
			},
			want: "binds 2 subjects",
		},
		{
			name: "a subject that names another user",
			edit: func(docs []deployerRBACDoc) []deployerRBACDoc {
				for i := range docs {
					if docs[i].Kind == "ClusterRoleBinding" && docs[i].Metadata.Name == deployerPolicyRole {
						docs[i].Subjects[0].Name = "someone-else"
					}
				}
				return docs
			},
			want: fmt.Sprintf(`binds User "someone-else", want User %q`, deployerPolicyUser),
		},
		{
			name: "a binding the file does not name",
			edit: func(docs []deployerRBACDoc) []deployerRBACDoc {
				binding := findDeployerRBAC(docs, "RoleBinding", deployerApplyRole, deployerNamespace)
				unlisted := *binding
				unlisted.Metadata.Name = "deployer-apply-2"
				return append(docs, unlisted)
			},
			want: `holds a RoleBinding "deployer-apply-2"`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			problems := deployerSubjectProblems(tc.edit(cloneDeployerDocs(docs)))
			if !slices.ContainsFunc(problems, func(p string) bool { return strings.Contains(p, tc.want) }) {
				t.Errorf("with %s the binding was not refused; want a problem containing %q, got %q", tc.name, tc.want, problems)
			}
		})
	}
	t.Run("Static lists the deployer documents", func(t *testing.T) {
		t.Parallel()
		objects, err := Static()
		if err != nil {
			t.Fatalf("Static(): %v", err)
		}
		want := []struct {
			kind, name, namespace string
			cluster               bool
		}{
			{"Role", deployerApplyRole, deployerNamespace, false},
			{"RoleBinding", deployerApplyRole, deployerNamespace, false},
			{"ClusterRole", deployerPolicyRole, "", true},
			{"ClusterRoleBinding", deployerPolicyRole, "", true},
			{"ClusterRole", deployerReaderRole, "", true},
			{"ClusterRoleBinding", deployerReaderRole, "", true},
		}
		for _, w := range want {
			if !slices.ContainsFunc(objects, func(o StaticObject) bool {
				return o.Kind == w.kind && o.Name == w.name && o.Namespace == w.namespace && o.Cluster == w.cluster
			}) {
				t.Errorf("Static() does not list %s %s in %q", w.kind, w.name, w.namespace)
			}
		}
	})
}

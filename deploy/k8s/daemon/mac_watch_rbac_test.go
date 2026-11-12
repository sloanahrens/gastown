package daemon

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The Mac's gt-mac-watch publishes probes.json by running kubectl exec into the
// daemon container, and until gt-a2uug4.22 it did so as cluster admin through
// the operator's kubeconfig. deploy/k8s/mac-watch-rbac.yaml is the narrow grant
// that replaces it: one ServiceAccount in gt, one Role for the exec, and their
// RoleBinding. The tests below hold the file to that one job and refuse any
// widening of it.
const (
	macWatchRBACFile  = "deploy/k8s/mac-watch-rbac.yaml"
	macWatchNamespace = "gt"
	macWatchAccount   = "mac-watch"
	macWatchAPIGroup  = "rbac.authorization.k8s.io"
)

// macWatchMeta is the name and namespace of one manifest document.
type macWatchMeta struct {
	Name      string `yaml:"name"`
	Namespace string `yaml:"namespace"`
}

// macWatchRule is one rule of the Role.
type macWatchRule struct {
	APIGroups []string `yaml:"apiGroups"`
	Resources []string `yaml:"resources"`
	Verbs     []string `yaml:"verbs"`
}

// macWatchRoleRef is the RoleBinding's reference to the Role it grants.
type macWatchRoleRef struct {
	APIGroup string `yaml:"apiGroup"`
	Kind     string `yaml:"kind"`
	Name     string `yaml:"name"`
}

// macWatchSubject is one subject of the RoleBinding.
type macWatchSubject struct {
	Kind      string `yaml:"kind"`
	Name      string `yaml:"name"`
	Namespace string `yaml:"namespace"`
}

// macWatchDoc is the part of one manifest document the check reads.
type macWatchDoc struct {
	Kind     string            `yaml:"kind"`
	Metadata macWatchMeta      `yaml:"metadata"`
	Rules    []macWatchRule    `yaml:"rules"`
	RoleRef  macWatchRoleRef   `yaml:"roleRef"`
	Subjects []macWatchSubject `yaml:"subjects"`
}

// macWatchGrant is one RBAC grant: a verb on a resource of an API group.
type macWatchGrant struct {
	group    string
	resource string
	verb     string
}

// macWatchGrants is every access the publish path takes, and so all the Role may
// grant: the exec lists the pods the daemon Deployment selects, reads the one it
// runs, and writes probes.json through pods/exec. A grant missing here strands
// the job; a grant not here reaches past it.
var macWatchGrants = []macWatchGrant{
	{group: "", resource: "pods", verb: "get"},
	{group: "", resource: "pods", verb: "list"},
	{group: "", resource: "pods/exec", verb: "create"},
}

// parseMacWatchRBAC parses every document of the manifest, and returns an error
// rather than no documents when the manifest is unreadable or empty: a check
// over nothing would pass for the wrong reason.
func parseMacWatchRBAC(data []byte) ([]macWatchDoc, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var docs []macWatchDoc
	for {
		var doc macWatchDoc
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("not valid YAML: %w", err)
		}
		if doc.Kind != "" {
			docs = append(docs, doc)
		}
	}
	if len(docs) == 0 {
		return nil, errors.New("the manifest holds no objects, so the rule check would pass for the wrong reason")
	}
	return docs, nil
}

// macWatchOnly returns the manifest's single document of a kind, reporting when
// there is not exactly one. The second result is false when it reports.
func macWatchOnly(kind string, docs []macWatchDoc, findings *[]string) (macWatchDoc, bool) {
	if len(docs) != 1 {
		*findings = append(*findings, fmt.Sprintf("the manifest holds %d %s objects, and the job needs exactly one", len(docs), kind))
		return macWatchDoc{}, false
	}
	return docs[0], true
}

// macWatchName prints a resource as a finding names it: the core group's
// resources bare, any other group's with its prefix.
func macWatchName(group, resource string) string {
	if group == "" {
		return resource
	}
	return group + "/" + resource
}

// macWatchRoleGranted flattens the Role's rules to the grants they make.
func macWatchRoleGranted(rules []macWatchRule) []macWatchGrant {
	var granted []macWatchGrant
	for _, r := range rules {
		for _, group := range r.APIGroups {
			for _, resource := range r.Resources {
				for _, verb := range r.Verbs {
					granted = append(granted, macWatchGrant{group: group, resource: resource, verb: verb})
				}
			}
		}
	}
	return granted
}

// macWatchRBACFindings reports every way docs is not the manifest the publish
// path needs: a document that is not the one ServiceAccount, Role or
// RoleBinding in gt, a Role grant the job loses, or a grant it does not use,
// wildcards and extra resources included. An empty result is the manifest the
// check holds.
func macWatchRBACFindings(docs []macWatchDoc) []string {
	var findings []string
	byKind := map[string][]macWatchDoc{}
	for _, d := range docs {
		switch d.Kind {
		case "ServiceAccount", "Role", "RoleBinding":
			byKind[d.Kind] = append(byKind[d.Kind], d)
		default:
			findings = append(findings, fmt.Sprintf("the manifest holds a %q, and the job's credential is one ServiceAccount, one Role and one RoleBinding; any other object widens or duplicates the grant", d.Kind))
		}
	}

	namespaced := func(kind string, d macWatchDoc) {
		if d.Metadata.Name != macWatchAccount {
			findings = append(findings, fmt.Sprintf("the %s is named %q, and the job's is named %q", kind, d.Metadata.Name, macWatchAccount))
		}
		if d.Metadata.Namespace != macWatchNamespace {
			findings = append(findings, fmt.Sprintf("the %s is in namespace %q, and the job's grant is in %q", kind, d.Metadata.Namespace, macWatchNamespace))
		}
	}

	if account, ok := macWatchOnly("ServiceAccount", byKind["ServiceAccount"], &findings); ok {
		namespaced("ServiceAccount", account)
	}
	if role, ok := macWatchOnly("Role", byKind["Role"], &findings); ok {
		namespaced("Role", role)
		granted := macWatchRoleGranted(role.Rules)
		for _, want := range macWatchGrants {
			if !slices.Contains(granted, want) {
				findings = append(findings, fmt.Sprintf("the Role has no rule granting %q on %s, so the publish path loses that access", want.verb, macWatchName(want.group, want.resource)))
			}
		}
		for _, got := range granted {
			if !slices.Contains(macWatchGrants, got) {
				findings = append(findings, fmt.Sprintf("the Role grants %q on %s, which the publish path does not use", got.verb, macWatchName(got.group, got.resource)))
			}
		}
	}
	if binding, ok := macWatchOnly("RoleBinding", byKind["RoleBinding"], &findings); ok {
		namespaced("RoleBinding", binding)
		if binding.RoleRef.APIGroup != macWatchAPIGroup || binding.RoleRef.Kind != "Role" || binding.RoleRef.Name != macWatchAccount {
			findings = append(findings, fmt.Sprintf("the RoleBinding's roleRef is %q %q in API group %q, and the job's is Role %q of %q", binding.RoleRef.Kind, binding.RoleRef.Name, binding.RoleRef.APIGroup, macWatchAccount, macWatchAPIGroup))
		}
		if len(binding.Subjects) != 1 {
			findings = append(findings, fmt.Sprintf("the RoleBinding has %d subjects, and the job's credential is one ServiceAccount", len(binding.Subjects)))
		} else if s := binding.Subjects[0]; s.Kind != "ServiceAccount" || s.Name != macWatchAccount || s.Namespace != macWatchNamespace {
			findings = append(findings, fmt.Sprintf("the RoleBinding's subject is %s %s/%s, and the job's is ServiceAccount %s/%s", s.Kind, s.Namespace, s.Name, macWatchNamespace, macWatchAccount))
		}
	}
	return findings
}

// TestMacWatchRBACRules holds deploy/k8s/mac-watch-rbac.yaml to the one job it
// serves: one ServiceAccount, one Role in gt granting only get and list on pods
// and create on pods/exec, and one RoleBinding of that Role to that ServiceAccount.
func TestMacWatchRBACRules(t *testing.T) {
	t.Parallel()
	docs, err := parseMacWatchRBAC(repoFile(t, macWatchRBACFile))
	if err != nil {
		t.Fatalf("reading %s: %v", macWatchRBACFile, err)
	}
	for _, finding := range macWatchRBACFindings(docs) {
		t.Error(finding)
	}
}

// macWatchFixture is the manifest the widening cases start from: the three
// objects carrying the publish path's grants and nothing else.
func macWatchFixture() []macWatchDoc {
	account := macWatchDoc{
		Kind:     "ServiceAccount",
		Metadata: macWatchMeta{Name: macWatchAccount, Namespace: macWatchNamespace},
	}
	role := macWatchDoc{
		Kind:     "Role",
		Metadata: macWatchMeta{Name: macWatchAccount, Namespace: macWatchNamespace},
		Rules: []macWatchRule{
			{APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"get", "list"}},
			{APIGroups: []string{""}, Resources: []string{"pods/exec"}, Verbs: []string{"create"}},
		},
	}
	binding := macWatchDoc{
		Kind:     "RoleBinding",
		Metadata: macWatchMeta{Name: macWatchAccount, Namespace: macWatchNamespace},
		RoleRef:  macWatchRoleRef{APIGroup: macWatchAPIGroup, Kind: "Role", Name: macWatchAccount},
		Subjects: []macWatchSubject{{Kind: "ServiceAccount", Name: macWatchAccount, Namespace: macWatchNamespace}},
	}
	return []macWatchDoc{account, role, binding}
}

// TestMacWatchRBACRefusesWidening drives every refusal of the rule check: each
// case makes one change to a valid manifest and requires it to be reported.
func TestMacWatchRBACRefusesWidening(t *testing.T) {
	t.Parallel()
	if findings := macWatchRBACFindings(macWatchFixture()); len(findings) != 0 {
		t.Fatalf("the base fixture is not the manifest the check holds, so every case below would pass for the wrong reason: %q", findings)
	}

	cases := []struct {
		name  string
		widen func([]macWatchDoc) []macWatchDoc
		want  string
	}{
		{
			name: "a ClusterRole",
			widen: func(d []macWatchDoc) []macWatchDoc {
				return append(d, macWatchDoc{Kind: "ClusterRole", Metadata: macWatchMeta{Name: macWatchAccount}})
			},
			want: `holds a "ClusterRole"`,
		},
		{
			name: "a Namespace",
			widen: func(d []macWatchDoc) []macWatchDoc {
				return append(d, macWatchDoc{Kind: "Namespace", Metadata: macWatchMeta{Name: macWatchNamespace}})
			},
			want: `holds a "Namespace"`,
		},
		{
			name: "a second Role",
			widen: func(d []macWatchDoc) []macWatchDoc {
				return slices.Concat(d, []macWatchDoc{{Kind: "Role", Metadata: macWatchMeta{Name: "wider", Namespace: macWatchNamespace}}})
			},
			want: "holds 2 Role objects",
		},
		{
			name: "a lost ServiceAccount",
			widen: func(d []macWatchDoc) []macWatchDoc {
				return d[1:]
			},
			want: "holds 0 ServiceAccount objects",
		},
		{
			name: "a lost RoleBinding",
			widen: func(d []macWatchDoc) []macWatchDoc {
				return d[:2]
			},
			want: "holds 0 RoleBinding objects",
		},
		{
			name: "a ServiceAccount in another namespace",
			widen: func(d []macWatchDoc) []macWatchDoc {
				d[0].Metadata.Namespace = "other"
				return d
			},
			want: `the ServiceAccount is in namespace "other"`,
		},
		{
			name: "a ServiceAccount named otherwise",
			widen: func(d []macWatchDoc) []macWatchDoc {
				d[0].Metadata.Name = "wider"
				return d
			},
			want: `the ServiceAccount is named "wider"`,
		},
		{
			name: "a Role in another namespace",
			widen: func(d []macWatchDoc) []macWatchDoc {
				d[1].Metadata.Namespace = "other"
				return d
			},
			want: `the Role is in namespace "other"`,
		},
		{
			name: "a Role named otherwise",
			widen: func(d []macWatchDoc) []macWatchDoc {
				d[1].Metadata.Name = "wider"
				return d
			},
			want: `the Role is named "wider"`,
		},
		{
			name: "a wildcard verb",
			widen: func(d []macWatchDoc) []macWatchDoc {
				d[1].Rules[0].Verbs = []string{"get", "list", "*"}
				return d
			},
			want: `grants "*" on pods`,
		},
		{
			name: "a wildcard resource",
			widen: func(d []macWatchDoc) []macWatchDoc {
				d[1].Rules[0].Resources = []string{"*"}
				return d
			},
			want: `grants "get" on *`,
		},
		{
			name: "a wildcard API group",
			widen: func(d []macWatchDoc) []macWatchDoc {
				d[1].Rules[0].APIGroups = []string{"*"}
				return d
			},
			want: `grants "get" on */pods`,
		},
		{
			name: "an extra verb",
			widen: func(d []macWatchDoc) []macWatchDoc {
				d[1].Rules[0].Verbs = []string{"get", "list", "delete"}
				return d
			},
			want: `grants "delete" on pods`,
		},
		{
			name: "an extra resource",
			widen: func(d []macWatchDoc) []macWatchDoc {
				d[1].Rules = append(d[1].Rules, macWatchRule{APIGroups: []string{""}, Resources: []string{"secrets"}, Verbs: []string{"get"}})
				return d
			},
			want: `grants "get" on secrets`,
		},
		{
			name: "an extra API group",
			widen: func(d []macWatchDoc) []macWatchDoc {
				d[1].Rules = append(d[1].Rules, macWatchRule{APIGroups: []string{"apps"}, Resources: []string{"deployments"}, Verbs: []string{"get"}})
				return d
			},
			want: `grants "get" on apps/deployments`,
		},
		{
			name: "a lost grant",
			widen: func(d []macWatchDoc) []macWatchDoc {
				d[1].Rules[1].Verbs = nil
				return d
			},
			want: `no rule granting "create" on pods/exec`,
		},
		{
			name: "a rule with no API group",
			widen: func(d []macWatchDoc) []macWatchDoc {
				d[1].Rules[0].APIGroups = nil
				return d
			},
			want: `no rule granting "get" on pods`,
		},
		{
			name: "a RoleBinding on a ClusterRole",
			widen: func(d []macWatchDoc) []macWatchDoc {
				d[2].RoleRef.Kind = "ClusterRole"
				return d
			},
			want: `roleRef is "ClusterRole"`,
		},
		{
			name: "a RoleBinding named otherwise",
			widen: func(d []macWatchDoc) []macWatchDoc {
				d[2].Metadata.Name = "wider"
				return d
			},
			want: `the RoleBinding is named "wider"`,
		},
		{
			name: "a RoleBinding in another namespace",
			widen: func(d []macWatchDoc) []macWatchDoc {
				d[2].Metadata.Namespace = "other"
				return d
			},
			want: `the RoleBinding is in namespace "other"`,
		},
		{
			name: "a second subject",
			widen: func(d []macWatchDoc) []macWatchDoc {
				d[2].Subjects = append(d[2].Subjects, macWatchSubject{Kind: "User", Name: "someone"})
				return d
			},
			want: "has 2 subjects",
		},
		{
			name: "a subject in another namespace",
			widen: func(d []macWatchDoc) []macWatchDoc {
				d[2].Subjects[0].Namespace = "other"
				return d
			},
			want: "the RoleBinding's subject is",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			findings := macWatchRBACFindings(tc.widen(macWatchFixture()))
			if !slices.ContainsFunc(findings, func(f string) bool { return strings.Contains(f, tc.want) }) {
				t.Fatalf("findings = %q, want one containing %q", findings, tc.want)
			}
		})
	}
}

// TestMacWatchRBACParseRefusals drives the reader's refusals: a manifest that is
// not YAML, and one that holds no objects, are errors rather than a check over
// nothing that passes.
func TestMacWatchRBACParseRefusals(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		manifest string
		want     string
	}{
		{name: "not YAML", manifest: "kind: [", want: "not valid YAML"},
		{name: "a comment only", manifest: "# nothing to grant\n", want: "holds no objects"},
		{name: "empty documents", manifest: "---\n---\n", want: "holds no objects"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			docs, err := parseMacWatchRBAC([]byte(tc.manifest))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("parseMacWatchRBAC(%q) = %d documents, error %v; want an error containing %q", tc.manifest, len(docs), err, tc.want)
			}
			if docs != nil {
				t.Errorf("parseMacWatchRBAC(%q) returned %d documents beside its error, want none", tc.manifest, len(docs))
			}
		})
	}
}

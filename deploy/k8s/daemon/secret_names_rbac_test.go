package daemon

import (
	"bytes"
	"errors"
	"io"
	"path/filepath"
	"slices"
	"sort"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/steveyegge/gastown/internal/manifestdata"
)

// bulkSecretVerbs returns the verbs of the named-Secrets grant that read
// Secrets in bulk: list and watch return the data, while the deployer's
// preflight needs the names only.
func bulkSecretVerbs(m Manifests) []string {
	var verbs []string
	for _, rule := range m.SecretNamesRole.Rules {
		for _, verb := range rule.Verbs {
			if verb == "list" || verb == "watch" {
				verbs = append(verbs, verb)
			}
		}
	}
	return verbs
}

// secretNameList returns the names of secrets, sorted and without duplicates,
// as the grant's resourceNames must hold them.
func secretNameList(secrets []Secret) []string {
	names := make([]string, 0, len(secrets))
	for _, s := range secrets {
		if !slices.Contains(names, s.Metadata.Name) {
			names = append(names, s.Metadata.Name)
		}
	}
	sort.Strings(names)
	return names
}

// TestSecretNamesSortsAndDeduplicates holds the list the grant is built from,
// including the duplicate case validateSecrets refuses to let Build render.
func TestSecretNamesSortsAndDeduplicates(t *testing.T) {
	t.Parallel()
	secrets := []Secret{
		{Metadata: ObjectMeta{Name: "daemon-b"}},
		{Metadata: ObjectMeta{Name: "daemon-a"}},
		{Metadata: ObjectMeta{Name: "daemon-b"}},
	}
	if got, want := secretNames(secrets), []string{"daemon-a", "daemon-b"}; !slices.Equal(got, want) {
		t.Errorf("secretNames = %q, want the sorted names without the repeat %q", got, want)
	}
	if got := secretNames(nil); len(got) != 0 {
		t.Errorf("secretNames of no Secrets = %q, want none", got)
	}
}

// TestRenderSecretNamesRoleGolden holds the grant a live render writes: one Role
// naming exactly the render's Secrets, sorted, with the get verb alone, and one
// binding of that Role to the gt-deployers group. Run:
// go test ./deploy/k8s/daemon -run TestRenderSecretNamesRoleGolden
func TestRenderSecretNamesRoleGolden(t *testing.T) {
	t.Parallel()
	m := mustBuild(t, goldenValues(t))

	role := m.SecretNamesRole
	if role.Kind != "Role" || role.Metadata.Name != SecretNamesRole || role.Metadata.Namespace != DefaultNamespace {
		t.Fatalf("the grant is a %s %q in %q, want a Role %q in %q",
			role.Kind, role.Metadata.Name, role.Metadata.Namespace, SecretNamesRole, DefaultNamespace)
	}
	if len(role.Rules) != 1 {
		t.Fatalf("the Role carries %d rules, want one", len(role.Rules))
	}
	rule := role.Rules[0]
	if want := []string{""}; !slices.Equal(rule.APIGroups, want) {
		t.Errorf("the rule's apiGroups = %q, want %q (the core API group)", rule.APIGroups, want)
	}
	if want := []string{"secrets"}; !slices.Equal(rule.Resources, want) {
		t.Errorf("the rule's resources = %q, want %q", rule.Resources, want)
	}
	if want := []string{"get"}; !slices.Equal(rule.Verbs, want) {
		t.Errorf("the rule's verbs = %q, want %q", rule.Verbs, want)
	}
	if bulk := bulkSecretVerbs(m); len(bulk) != 0 {
		t.Errorf("the rule carries the bulk verbs %q, want only get", bulk)
	}
	wantNames := secretNameList(m.Secrets)
	if !slices.Equal(rule.ResourceNames, wantNames) {
		t.Errorf("the rule names %q, want the render's Secrets sorted %q", rule.ResourceNames, wantNames)
	}
	// The static sweep runner's Secret belongs to gt-sweeps, not this render.
	if slices.Contains(rule.ResourceNames, "sweep-runner-token") {
		t.Errorf("the rule names the static sweep-runner-token, want the render's Secrets alone")
	}

	binding := m.SecretNamesBinding
	if binding.Kind != "RoleBinding" || binding.Metadata.Namespace != DefaultNamespace {
		t.Fatalf("the grant is a %s %q in %q, want a RoleBinding in %q",
			binding.Kind, binding.Metadata.Name, binding.Metadata.Namespace, DefaultNamespace)
	}
	if want := (RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "Role", Name: SecretNamesRole}); binding.RoleRef != want {
		t.Errorf("the binding's roleRef = %+v, want %+v", binding.RoleRef, want)
	}
	if want := []Subject{{Kind: "Group", Name: DeployerGroup}}; !slices.Equal(binding.Subjects, want) {
		t.Errorf("the binding's subjects = %+v, want the %q group alone", binding.Subjects, DeployerGroup)
	}

	dir := filepath.Join(packageDir(t), "testdata")
	compareGolden(t, dir, "role-deployer-secret-names.yaml", document(t, role))
	compareGolden(t, dir, "rolebinding-deployer-secret-names.yaml", document(t, binding))
}

// TestRenderSecretNamesRoleRefusals holds what the grant never does: a render
// with no Secrets writes neither object, and a bulk verb fails the test whether
// the build or a later edit put it there. Run:
// go test ./deploy/k8s/daemon -run TestRenderSecretNamesRoleRefusals
func TestRenderSecretNamesRoleRefusals(t *testing.T) {
	t.Parallel()

	t.Run("a render with no Secrets writes neither object", func(t *testing.T) {
		t.Parallel()
		// A rehearsal renders only the entries rehearsalSecrets names, so a list
		// holding another entry leaves the render with no Secret at all.
		v := Values{
			Image:     testImage,
			Rehearsal: true,
			Namespace: "gt-rehearsal",
			Secrets:   []manifestdata.SecretFile{{Name: ForgeTokenFile}},
			CPU:       "2",
			Memory:    "4Gi",
		}
		m := mustBuild(t, v)
		if len(m.Secrets) != 0 {
			t.Fatalf("the render holds %d Secrets, want none", len(m.Secrets))
		}
		if m.SecretNamesRole.Kind != "" || m.SecretNamesBinding.Kind != "" {
			t.Errorf("the render holds %q and %q, want neither", m.SecretNamesRole.Kind, m.SecretNamesBinding.Kind)
		}
		var out bytes.Buffer
		if err := Render(&out, m); err != nil {
			t.Fatalf("Render: %v", err)
		}
		dec := yaml.NewDecoder(&out)
		for {
			var doc struct {
				Kind     string `yaml:"kind"`
				Metadata struct {
					Name string `yaml:"name"`
				} `yaml:"metadata"`
			}
			err := dec.Decode(&doc)
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatalf("decoding the stream: %v", err)
			}
			if doc.Kind == "Role" || doc.Kind == "RoleBinding" {
				t.Errorf("a render with no Secrets writes the %s %q", doc.Kind, doc.Metadata.Name)
			}
		}
	})

	t.Run("list or watch fails the test", func(t *testing.T) {
		t.Parallel()
		m := mustBuild(t, goldenValues(t))
		if bulk := bulkSecretVerbs(m); len(bulk) != 0 {
			t.Fatalf("the built grant carries the bulk verbs %q, want only get", bulk)
		}
		for _, verb := range []string{"list", "watch"} {
			planted := m
			planted.SecretNamesRole.Rules = []PolicyRule{{
				APIGroups:     []string{""},
				Resources:     []string{"secrets"},
				Verbs:         []string{"get", verb},
				ResourceNames: m.SecretNamesRole.Rules[0].ResourceNames,
			}}
			if got := bulkSecretVerbs(planted); !slices.Contains(got, verb) {
				t.Errorf("a rule carrying %q is not refused (the guard reports %q)", verb, got)
			}
		}
	})
}

package updatejob

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	k8sdaemon "github.com/steveyegge/gastown/deploy/k8s/daemon"
	"github.com/steveyegge/gastown/internal/imagevalues"
)

// updateAdmissionManifest is the ValidatingAdmissionPolicy and binding that bound
// what a Job the daemon creates may be (gt-j571bu). The daemon's Role allows it to
// create any Job in gt (deploy/k8s/update-rbac.yaml), and a Job's pod spec is chosen
// by its creator, so the policy admits only the Job this package renders. The
// expressions are held to this package's constants, so the renderer and the policy
// cannot drift, and the rendered Job is held to the same constants.
const updateAdmissionManifest = "../update-admission.yaml"

// The names the manifest must use. updateAdmissionUser is the daemon's ServiceAccount
// in k8sdaemon's namespace, the one the daemon's launch Role lives in.
const (
	updateAdmissionName = "gt-update-jobs"
	updateAdmissionAPI  = "admissionregistration.k8s.io/v1"
	updateAdmissionUser = "system:serviceaccount:" + k8sdaemon.DefaultNamespace + ":" + k8sdaemon.ServiceAccountName
)

// admissionImageRegexp is the image the policy admits: the daemon image
// imagevalues names, by digest, from this town's registry and registry account.
func admissionImageRegexp() string {
	return "^forgejo:3000/bot-registry/" + imagevalues.DaemonImage + "@sha256:[0-9a-f]{64}$"
}

// admissionEnvGuard is the expression body that refuses a Secret read through a
// container's environment, for one container bound to c. A volume pin does not reach
// secretKeyRef or envFrom.secretRef, which name any Secret in the namespace.
const admissionEnvGuard = "(!has(c.env) || c.env.all(e, !has(e.valueFrom) || !has(e.valueFrom.secretKeyRef))) && " +
	"(!has(c.envFrom) || c.envFrom.all(f, !has(f.secretRef)))"

// admissionValidations is every validation the policy must carry, in order, built
// from the renderer's constants. The check compares each stored expression to this
// list exactly (whitespace aside), so a weakened comparison, a dropped validation or
// a changed name fails the test and not only a reader's eye.
func admissionValidations() []string {
	image := admissionImageRegexp()
	return []string{
		fmt.Sprintf("object.metadata.name.startsWith('%s')", JobNamePrefix),
		fmt.Sprintf("object.spec.template.spec.serviceAccountName == '%[1]s' && "+
			"(!has(object.spec.template.spec.serviceAccount) || object.spec.template.spec.serviceAccount == '%[1]s')",
			ServiceAccountName),
		"(!has(object.spec.template.spec.hostNetwork) || !object.spec.template.spec.hostNetwork) && " +
			"(!has(object.spec.template.spec.hostPID) || !object.spec.template.spec.hostPID) && " +
			"(!has(object.spec.template.spec.hostIPC) || !object.spec.template.spec.hostIPC)",
		"object.spec.template.spec.containers.all(c, " +
			"!has(c.securityContext) || !has(c.securityContext.privileged) || !c.securityContext.privileged) && " +
			"(!has(object.spec.template.spec.initContainers) || object.spec.template.spec.initContainers.all(c, " +
			"!has(c.securityContext) || !has(c.securityContext.privileged) || !c.securityContext.privileged))",
		fmt.Sprintf("has(object.spec.template.spec.volumes) && size(object.spec.template.spec.volumes) == 2 && "+
			"object.spec.template.spec.volumes.all(v, "+
			"(v.name == '%[1]s' && has(v.persistentVolumeClaim) && v.persistentVolumeClaim.claimName == '%[2]s' && !has(v.hostPath)) || "+
			"(v.name == '%[3]s' && has(v.secret) && v.secret.secretName == '%[4]s' && !has(v.hostPath)))",
			TownStateVolume, k8sdaemon.TownStateClaim, ViewerTokenVolume, ViewerTokenSecret),
		fmt.Sprintf("object.spec.template.spec.containers.all(c, c.image.matches('%[1]s')) && "+
			"(!has(object.spec.template.spec.initContainers) || object.spec.template.spec.initContainers.all(c, c.image.matches('%[1]s')))",
			image),
		fmt.Sprintf("object.spec.template.spec.containers.all(c, %[1]s) && "+
			"(!has(object.spec.template.spec.initContainers) || object.spec.template.spec.initContainers.all(c, %[1]s))",
			admissionEnvGuard),
	}
}

// admissionDoc is one document of the manifest: the fields the policy and the
// binding share, and the spec fields of both. A document of another kind is a
// problem, never a skipped read.
type admissionDoc struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Metadata   struct {
		Name string `yaml:"name"`
	} `yaml:"metadata"`
	Spec admissionSpec `yaml:"spec"`
}

// admissionSpec is the spec of a ValidatingAdmissionPolicy or of its binding. The
// two are decoded into one shape because each document is read with the kind
// checked first.
type admissionSpec struct {
	FailurePolicy     string                    `yaml:"failurePolicy"`
	MatchConstraints  admissionMatchConstraints `yaml:"matchConstraints"`
	MatchConditions   []admissionCondition      `yaml:"matchConditions"`
	Validations       []admissionValidation     `yaml:"validations"`
	PolicyName        string                    `yaml:"policyName"`
	ValidationActions []string                  `yaml:"validationActions"`
	MatchResources    admissionMatchResources   `yaml:"matchResources"`
}

// admissionMatchConstraints is what the policy applies to.
type admissionMatchConstraints struct {
	ResourceRules []admissionResourceRule `yaml:"resourceRules"`
}

// admissionResourceRule is one resource rule of the policy's constraints.
type admissionResourceRule struct {
	APIGroups   []string `yaml:"apiGroups"`
	APIVersions []string `yaml:"apiVersions"`
	Resources   []string `yaml:"resources"`
	Operations  []string `yaml:"operations"`
}

// admissionCondition is a matchCondition: a named expression over the request.
type admissionCondition struct {
	Name       string `yaml:"name"`
	Expression string `yaml:"expression"`
}

// admissionValidation is one validation of the policy.
type admissionValidation struct {
	Expression string `yaml:"expression"`
	Message    string `yaml:"message"`
}

// admissionMatchResources is what a binding applies its policy to.
type admissionMatchResources struct {
	NamespaceSelector *admissionSelector `yaml:"namespaceSelector"`
}

// admissionSelector matches the labelled namespaces.
type admissionSelector struct {
	MatchLabels map[string]string `yaml:"matchLabels"`
}

// parseUpdateAdmission decodes every document of the manifest. An empty manifest is
// an error, and so is a document yaml cannot decode: neither is read as an absent
// policy.
func parseUpdateAdmission(raw []byte) ([]admissionDoc, error) {
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	var docs []admissionDoc
	for {
		var doc admissionDoc
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("decoding document %d: %w", len(docs)+1, err)
		}
		docs = append(docs, doc)
	}
	if len(docs) == 0 {
		return nil, errors.New("the manifest holds no documents")
	}
	return docs, nil
}

// loadUpdateAdmission reads and parses the manifest, failing the test when it cannot
// be read or decoded: a missing policy must not pass as no problems.
func loadUpdateAdmission(t *testing.T) []admissionDoc {
	t.Helper()
	raw, err := os.ReadFile(filepath.FromSlash(updateAdmissionManifest))
	if err != nil {
		t.Fatalf("reading %s: %v", updateAdmissionManifest, err)
	}
	docs, err := parseUpdateAdmission(raw)
	if err != nil {
		t.Fatalf("%s: %v", updateAdmissionManifest, err)
	}
	return docs
}

// admissionDocNamed returns the one document of the given kind, and a problem when
// the manifest holds none or more than one. A second policy would leave an unread
// document, so it is refused.
func admissionDocNamed(docs []admissionDoc, kind string) (admissionDoc, []string) {
	var found []admissionDoc
	for _, d := range docs {
		if d.Kind == kind {
			found = append(found, d)
		}
	}
	if len(found) == 1 {
		return found[0], nil
	}
	return admissionDoc{}, []string{fmt.Sprintf("the manifest holds %d %s documents, want exactly 1", len(found), kind)}
}

// admissionDocsWithoutKind returns the documents whose kind is not kind, on a new
// slice, so a refusal fixture can drop one document without disturbing its source.
func admissionDocsWithoutKind(docs []admissionDoc, kind string) []admissionDoc {
	var kept []admissionDoc
	for _, d := range docs {
		if d.Kind != kind {
			kept = append(kept, d)
		}
	}
	return kept
}

// admissionText is an expression with its whitespace collapsed, so a rewrapped
// expression still matches and no other difference is forgiven.
func admissionText(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// admissionExprProblems holds one validation expression to its expected text, or
// names the difference. It is used for every validation, so a changed operator, a
// changed literal or a dropped clause is named.
func admissionExprProblems(i int, got, want string) []string {
	if admissionText(got) == admissionText(want) {
		return nil
	}
	return []string{fmt.Sprintf("validation %d is %q, want %q", i+1, admissionText(got), admissionText(want))}
}

// updateAdmissionProblems names every way the manifest departs from the policy the
// daemon's Job creation is bounded by: two v1 documents of the expected kinds and
// names, a policy of Fail on Job CREATE in batch/v1, the validations of
// admissionValidations, and a binding that denies, for the daemon's ServiceAccount,
// the Jobs in namespace gt.
func updateAdmissionProblems(docs []admissionDoc) []string {
	var problems []string
	for _, d := range docs {
		switch d.Kind {
		case "ValidatingAdmissionPolicy", "ValidatingAdmissionPolicyBinding":
		default:
			problems = append(problems, fmt.Sprintf("document kind %q is neither the policy nor its binding", d.Kind))
		}
		if d.APIVersion != updateAdmissionAPI {
			problems = append(problems, fmt.Sprintf("%s apiVersion is %q, want %q", d.Kind, d.APIVersion, updateAdmissionAPI))
		}
		if d.Metadata.Name != updateAdmissionName {
			problems = append(problems, fmt.Sprintf("%s name is %q, want %q", d.Kind, d.Metadata.Name, updateAdmissionName))
		}
	}
	policy, policyProblems := admissionDocNamed(docs, "ValidatingAdmissionPolicy")
	binding, bindingProblems := admissionDocNamed(docs, "ValidatingAdmissionPolicyBinding")
	problems = append(problems, policyProblems...)
	problems = append(problems, bindingProblems...)
	if len(policyProblems) > 0 || len(bindingProblems) > 0 {
		return problems
	}
	problems = append(problems, admissionPolicyProblems(policy)...)
	return append(problems, admissionBindingProblems(binding, policy.Metadata.Name)...)
}

// admissionPolicyProblems names every way the policy's own rules differ from what
// the renderer's constants require.
func admissionPolicyProblems(policy admissionDoc) []string {
	var problems []string
	if got := policy.Spec.FailurePolicy; got != "Fail" {
		problems = append(problems, fmt.Sprintf("failurePolicy is %q, want Fail: an unevaluated policy must refuse the Job, not admit it", got))
	}
	rules := policy.Spec.MatchConstraints.ResourceRules
	if len(rules) != 1 {
		problems = append(problems, fmt.Sprintf("matchConstraints holds %d resource rules, want only Job creation", len(rules)))
	} else {
		rule := rules[0]
		for _, want := range []struct {
			field string
			got   []string
			want  []string
		}{
			{"apiGroups", rule.APIGroups, []string{"batch"}},
			{"apiVersions", rule.APIVersions, []string{"v1"}},
			{"resources", rule.Resources, []string{"jobs"}},
			{"operations", rule.Operations, []string{"CREATE"}},
		} {
			if strings.Join(want.got, ",") != strings.Join(want.want, ",") {
				problems = append(problems, fmt.Sprintf("matchConstraints %s is %v, want %v", want.field, want.got, want.want))
			}
		}
	}
	want := admissionValidations()
	if len(policy.Spec.Validations) != len(want) {
		problems = append(problems, fmt.Sprintf("the policy holds %d validations, want %d", len(policy.Spec.Validations), len(want)))
	}
	for i, v := range policy.Spec.Validations {
		if strings.TrimSpace(v.Message) == "" {
			problems = append(problems, fmt.Sprintf("validation %d has no message naming what it requires", i+1))
		}
		if i < len(want) {
			problems = append(problems, admissionExprProblems(i, v.Expression, want[i])...)
		}
	}
	return problems
}

// admissionBindingProblems names every way the binding fails to hold the policy to
// the daemon's Job creation in gt.
func admissionBindingProblems(binding admissionDoc, policyName string) []string {
	var problems []string
	if binding.Spec.PolicyName != policyName {
		problems = append(problems, fmt.Sprintf("policyName is %q, want the policy %q", binding.Spec.PolicyName, policyName))
	}
	if got := binding.Spec.ValidationActions; len(got) != 1 || got[0] != "Deny" {
		problems = append(problems, fmt.Sprintf("validationActions is %v, want [Deny]: a warning or audit action admits the Job", got))
	}
	selector := binding.Spec.MatchResources.NamespaceSelector
	if selector == nil {
		problems = append(problems, "matchResources holds no namespaceSelector, so the policy would apply in every namespace")
	} else if got := selector.MatchLabels["kubernetes.io/metadata.name"]; got != k8sdaemon.DefaultNamespace {
		problems = append(problems, fmt.Sprintf("matchResources namespaceSelector selects namespace %q, want %q", got, k8sdaemon.DefaultNamespace))
	}
	conditions := binding.Spec.MatchConditions
	if len(conditions) != 1 {
		problems = append(problems, fmt.Sprintf("the binding holds %d matchConditions, want only the daemon's ServiceAccount", len(conditions)))
	} else if want := fmt.Sprintf("request.userInfo.username == '%s'", updateAdmissionUser); admissionText(conditions[0].Expression) != want {
		problems = append(problems, fmt.Sprintf("matchConditions expression is %q, want %q", admissionText(conditions[0].Expression), want))
	}
	return problems
}

// updateAdmissionJobProblems names every way the rendered Job departs from the
// volumes and ServiceAccount the policy's expressions are built from. The expressions
// and the Job are checked against the same constants, so changing either fails the
// test instead of leaving the policy rejecting the daemon's Job. The name prefix needs
// no separate check: admissionValidations builds its expression from JobNamePrefix,
// and the daemon names each Job it creates with that same constant.
func updateAdmissionJobProblems(job Job) []string {
	var problems []string
	pod := job.Spec.Template.Spec
	if pod.ServiceAccountName != ServiceAccountName {
		problems = append(problems, fmt.Sprintf("the Job's pod runs as %q, want %q", pod.ServiceAccountName, ServiceAccountName))
	}
	if len(pod.Volumes) != 2 {
		problems = append(problems, fmt.Sprintf("the Job mounts %d volumes, want the town-state claim and the viewer-token Secret only", len(pod.Volumes)))
	}
	seen := map[string]bool{}
	for _, v := range pod.Volumes {
		switch v.Name {
		case TownStateVolume:
			seen[v.Name] = true
			if v.PersistentVolumeClaim == nil || v.PersistentVolumeClaim.ClaimName != k8sdaemon.TownStateClaim || v.Secret != nil {
				problems = append(problems, fmt.Sprintf("volume %q is not the %q claim", v.Name, k8sdaemon.TownStateClaim))
			}
		case ViewerTokenVolume:
			seen[v.Name] = true
			if v.Secret == nil || v.Secret.SecretName != ViewerTokenSecret || v.PersistentVolumeClaim != nil {
				problems = append(problems, fmt.Sprintf("volume %q is not the %q Secret", v.Name, ViewerTokenSecret))
			}
		default:
			problems = append(problems, fmt.Sprintf("volume %q is neither the town-state claim nor the viewer-token Secret", v.Name))
		}
	}
	for _, name := range []string{TownStateVolume, ViewerTokenVolume} {
		if !seen[name] {
			problems = append(problems, fmt.Sprintf("the Job mounts no %q volume", name))
		}
	}
	return problems
}

// TestUpdateAdmissionPolicy holds the manifest to its shape: the policy and binding
// as deployed admit Job creation in gt only as the daemon's ServiceAccount, and the
// real rendered Job is the Job those expressions admit.
func TestUpdateAdmissionPolicy(t *testing.T) {
	t.Parallel()
	if problems := updateAdmissionProblems(loadUpdateAdmission(t)); len(problems) > 0 {
		t.Errorf("%s: %s", updateAdmissionManifest, strings.Join(problems, "; "))
	}
	if problems := updateAdmissionJobProblems(mustBuild(t, goldenValues()).Job); len(problems) > 0 {
		t.Errorf("the rendered Job: %s", strings.Join(problems, "; "))
	}
}

// updateAdmissionRefusal is an edit to the manifest, or to the rendered Job, that the
// checks must refuse, and the text the refusal must name.
type updateAdmissionRefusal struct {
	name string
	edit func(t *testing.T, docs []admissionDoc) []admissionDoc
	want string
}

// updateAdmissionRefusals is every weakening the checks must name: a document of
// another kind, a renamed or missing policy or binding, an action that only warns, a
// scope wider than Job creation, a validation that admits a wider Job, and a binding
// that reaches another account or namespace.
var updateAdmissionRefusals = []updateAdmissionRefusal{
	{
		name: "an extra document",
		edit: func(t *testing.T, docs []admissionDoc) []admissionDoc {
			return append(docs, admissionDoc{Kind: "MutatingWebhookConfiguration"})
		},
		want: "is neither the policy nor its binding",
	},
	{
		name: "no binding",
		edit: func(t *testing.T, docs []admissionDoc) []admissionDoc {
			return admissionDocsWithoutKind(docs, "ValidatingAdmissionPolicyBinding")
		},
		want: "0 ValidatingAdmissionPolicyBinding documents",
	},
	{
		name: "a second policy",
		edit: func(t *testing.T, docs []admissionDoc) []admissionDoc {
			second := docs[0]
			second.Metadata.Name = "gt-update-jobs-second"
			return append(docs, second)
		},
		want: "2 ValidatingAdmissionPolicy documents",
	},
	{
		name: "a renamed policy",
		edit: func(t *testing.T, docs []admissionDoc) []admissionDoc {
			docs[0].Metadata.Name = "gt-update-jobs-v2"
			return docs
		},
		want: `name is "gt-update-jobs-v2"`,
	},
	{
		name: "another apiVersion",
		edit: func(t *testing.T, docs []admissionDoc) []admissionDoc {
			docs[0].APIVersion = "admissionregistration.k8s.io/v1beta1"
			return docs
		},
		want: "apiVersion",
	},
	{
		name: "failurePolicy Ignore",
		edit: func(t *testing.T, docs []admissionDoc) []admissionDoc {
			docs[0].Spec.FailurePolicy = "Ignore"
			return docs
		},
		want: "failurePolicy",
	},
	{
		name: "an operation besides CREATE",
		edit: func(t *testing.T, docs []admissionDoc) []admissionDoc {
			docs[0].Spec.MatchConstraints.ResourceRules[0].Operations = []string{"CREATE", "UPDATE"}
			return docs
		},
		want: "operations",
	},
	{
		name: "every batch resource",
		edit: func(t *testing.T, docs []admissionDoc) []admissionDoc {
			docs[0].Spec.MatchConstraints.ResourceRules[0].Resources = []string{"*"}
			return docs
		},
		want: "resources",
	},
	{
		name: "another version of the resource",
		edit: func(t *testing.T, docs []admissionDoc) []admissionDoc {
			docs[0].Spec.MatchConstraints.ResourceRules[0].APIVersions = []string{"v1beta1"}
			return docs
		},
		want: "apiVersions",
	},
	{
		name: "a second resource rule",
		edit: func(t *testing.T, docs []admissionDoc) []admissionDoc {
			docs[0].Spec.MatchConstraints.ResourceRules = append(docs[0].Spec.MatchConstraints.ResourceRules,
				docs[0].Spec.MatchConstraints.ResourceRules[0])
			return docs
		},
		want: "resource rules",
	},
	{
		name: "another API group",
		edit: func(t *testing.T, docs []admissionDoc) []admissionDoc {
			docs[0].Spec.MatchConstraints.ResourceRules[0].APIGroups = []string{""}
			return docs
		},
		want: "apiGroups",
	},
	{
		name: "a dropped validation",
		edit: func(t *testing.T, docs []admissionDoc) []admissionDoc {
			docs[0].Spec.Validations = docs[0].Spec.Validations[:len(docs[0].Spec.Validations)-1]
			return docs
		},
		want: "validations, want 7",
	},
	{
		name: "no message on a validation",
		edit: func(t *testing.T, docs []admissionDoc) []admissionDoc {
			docs[0].Spec.Validations[0].Message = " "
			return docs
		},
		want: "has no message",
	},
	{
		name: "another ServiceAccount admitted",
		edit: func(t *testing.T, docs []admissionDoc) []admissionDoc {
			docs[0].Spec.Validations[1].Expression = strings.ReplaceAll(docs[0].Spec.Validations[1].Expression, ServiceAccountName, k8sdaemon.ServiceAccountName)
			return docs
		},
		want: "validation 2",
	},
	{
		name: "a third volume admitted",
		edit: func(t *testing.T, docs []admissionDoc) []admissionDoc {
			docs[0].Spec.Validations[4].Expression = strings.Replace(docs[0].Spec.Validations[4].Expression, "size(object.spec.template.spec.volumes) == 2", "size(object.spec.template.spec.volumes) <= 3", 1)
			return docs
		},
		want: "validation 5",
	},
	{
		name: "an image outside the registry",
		edit: func(t *testing.T, docs []admissionDoc) []admissionDoc {
			docs[0].Spec.Validations[5].Expression = strings.ReplaceAll(docs[0].Spec.Validations[5].Expression, "forgejo:3000/bot-registry/", "")
			return docs
		},
		want: "validation 6",
	},
	{
		name: "a Secret through env",
		edit: func(t *testing.T, docs []admissionDoc) []admissionDoc {
			docs[0].Spec.Validations[6].Expression = strings.ReplaceAll(docs[0].Spec.Validations[6].Expression, "!has(e.valueFrom.secretKeyRef)", "true")
			return docs
		},
		want: "validation 7",
	},
	{
		name: "validationActions Warn",
		edit: func(t *testing.T, docs []admissionDoc) []admissionDoc {
			docs[1].Spec.ValidationActions = []string{"Warn"}
			return docs
		},
		want: "validationActions",
	},
	{
		name: "a binding of another policy",
		edit: func(t *testing.T, docs []admissionDoc) []admissionDoc {
			docs[1].Spec.PolicyName = "gt-update-jobs-v2"
			return docs
		},
		want: "policyName",
	},
	{
		name: "a second binding",
		edit: func(t *testing.T, docs []admissionDoc) []admissionDoc {
			second := docs[1]
			second.Metadata.Name = "gt-update-jobs-second"
			return append(docs, second)
		},
		want: "2 ValidatingAdmissionPolicyBinding documents",
	},
	{
		name: "a warning beside the deny",
		edit: func(t *testing.T, docs []admissionDoc) []admissionDoc {
			docs[1].Spec.ValidationActions = []string{"Deny", "Warn"}
			return docs
		},
		want: "validationActions",
	},
	{
		name: "no namespace selector",
		edit: func(t *testing.T, docs []admissionDoc) []admissionDoc {
			docs[1].Spec.MatchResources.NamespaceSelector = nil
			return docs
		},
		want: "no namespaceSelector",
	},
	{
		name: "another namespace",
		edit: func(t *testing.T, docs []admissionDoc) []admissionDoc {
			docs[1].Spec.MatchResources.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] = "default"
			return docs
		},
		want: "selects namespace",
	},
	{
		name: "no matchConditions",
		edit: func(t *testing.T, docs []admissionDoc) []admissionDoc {
			docs[1].Spec.MatchConditions = nil
			return docs
		},
		want: "matchConditions",
	},
	{
		name: "a second matchCondition",
		edit: func(t *testing.T, docs []admissionDoc) []admissionDoc {
			docs[1].Spec.MatchConditions = append(docs[1].Spec.MatchConditions, docs[1].Spec.MatchConditions[0])
			return docs
		},
		want: "matchConditions",
	},
	{
		name: "the operator's account too",
		edit: func(t *testing.T, docs []admissionDoc) []admissionDoc {
			docs[1].Spec.MatchConditions[0].Expression = "request.userInfo.username == 'kubernetes-admin'"
			return docs
		},
		want: "matchConditions expression",
	},
}

// TestUpdateAdmissionPolicyRefusesWeakenings holds the check to its refusals: each
// edit in updateAdmissionRefusals is named by the check, so a manifest that admits
// more than the rendered Job cannot pass.
func TestUpdateAdmissionPolicyRefusesWeakenings(t *testing.T) {
	t.Parallel()
	for _, r := range updateAdmissionRefusals {
		t.Run(r.name, func(t *testing.T) {
			t.Parallel()
			problems := strings.Join(updateAdmissionProblems(r.edit(t, loadUpdateAdmission(t))), "; ")
			if !strings.Contains(problems, r.want) {
				t.Errorf("%s: problems %q do not name %q", r.name, problems, r.want)
			}
		})
	}
}

// TestUpdateAdmissionParserRefusesBadManifest holds the parser to its refusals: an
// empty manifest and a document yaml cannot decode are errors, not an empty policy.
func TestUpdateAdmissionParserRefusesBadManifest(t *testing.T) {
	t.Parallel()
	for _, r := range []struct{ name, raw string }{
		{name: "no documents", raw: ""},
		{name: "an undecodable document", raw: "kind: [\n"},
	} {
		if _, err := parseUpdateAdmission([]byte(r.raw)); err == nil {
			t.Errorf("%s: parseUpdateAdmission returned no error", r.name)
		}
	}
}

// TestUpdateAdmissionJobRefusesAnotherJob holds the Job check to its refusals: a Job
// with a third volume, another ServiceAccount, another Secret volume or a missing
// town-state claim is named, so a renderer change cannot silently leave the policy
// rejecting the daemon's own Job.
func TestUpdateAdmissionJobRefusesAnotherJob(t *testing.T) {
	t.Parallel()
	for _, r := range []struct {
		name string
		edit func(*Job)
		want string
	}{
		{
			name: "a third volume",
			edit: func(j *Job) {
				j.Spec.Template.Spec.Volumes = append(j.Spec.Template.Spec.Volumes,
					Volume{Name: "extra", PersistentVolumeClaim: &PVCVolumeSource{ClaimName: "extra"}})
			},
			want: "3 volumes",
		},
		{
			name: "another ServiceAccount",
			edit: func(j *Job) { j.Spec.Template.Spec.ServiceAccountName = k8sdaemon.ServiceAccountName },
			want: "runs as",
		},
		{
			name: "another Secret",
			edit: func(j *Job) {
				for i := range j.Spec.Template.Spec.Volumes {
					if v := &j.Spec.Template.Spec.Volumes[i]; v.Name == ViewerTokenVolume && v.Secret != nil {
						v.Secret = &SecretVolumeSource{SecretName: "other"}
					}
				}
			},
			want: "is not the",
		},
		{
			name: "another claim",
			edit: func(j *Job) {
				for i := range j.Spec.Template.Spec.Volumes {
					if v := &j.Spec.Template.Spec.Volumes[i]; v.Name == TownStateVolume && v.PersistentVolumeClaim != nil {
						v.PersistentVolumeClaim = &PVCVolumeSource{ClaimName: "other"}
					}
				}
			},
			want: "is not the",
		},
		{
			name: "no town-state volume",
			edit: func(j *Job) {
				var kept []Volume
				for _, v := range j.Spec.Template.Spec.Volumes {
					if v.Name != TownStateVolume {
						kept = append(kept, v)
					}
				}
				j.Spec.Template.Spec.Volumes = kept
			},
			want: "mounts no",
		},
	} {
		t.Run(r.name, func(t *testing.T) {
			t.Parallel()
			job := mustBuild(t, goldenValues()).Job
			r.edit(&job)
			problems := strings.Join(updateAdmissionJobProblems(job), "; ")
			if !strings.Contains(problems, r.want) {
				t.Errorf("%s: problems %q do not name %q", r.name, problems, r.want)
			}
		})
	}
}

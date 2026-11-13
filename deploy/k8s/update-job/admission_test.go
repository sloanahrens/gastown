package updatejob

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	k8sdaemon "github.com/steveyegge/gastown/deploy/k8s/daemon"
	"github.com/steveyegge/gastown/internal/imagevalues"
)

// updateAdmissionManifest is the ValidatingAdmissionPolicy and binding that bound
// what a Job the daemon creates may be (gt-j571bu). The daemon's Role allows it to
// create any Job in gt (deploy/k8s/update-rbac.yaml), and a Job's pod spec is chosen
// by its creator, so the policy bounds the fields that carry that reach. The
// expressions are built from this package's own output (the containers, their
// environment, the constants), so the renderer and the policy cannot drift, and the
// rendered Job is held to the same bounds.
const updateAdmissionManifest = "../update-admission.yaml"

// The names the manifest must use. updateAdmissionUser is the daemon's ServiceAccount
// in k8sdaemon's namespace, the one the daemon's launch Role lives in.
const (
	updateAdmissionName = "gt-update-jobs"
	updateAdmissionAPI  = "admissionregistration.k8s.io/v1"
	updateAdmissionUser = "system:serviceaccount:" + k8sdaemon.DefaultNamespace + ":" + k8sdaemon.ServiceAccountName
)

// admissionImagePrefix is where the town publishes the daemon image: the registry
// host and account the policy pins. The renderer accepts any repo@sha256 reference,
// so this prefix and the digest shape are the policy's bound, not the renderer's.
const admissionImagePrefix = "forgejo:3000/bot-registry/"

// admissionImagePattern is the image the policy admits: the daemon image
// imagevalues names, by digest, from admissionImagePrefix.
func admissionImagePattern() string {
	return "^" + admissionImagePrefix + imagevalues.DaemonImage + "@sha256:[0-9a-f]{64}$"
}

// admissionImageRE is admissionImagePattern as a Go regexp, for holding the
// rendered Job's image to the same bound the expressions state.
var admissionImageRE = regexp.MustCompile(admissionImagePattern())

// admissionValues is goldenValues with the namespace Build defaults, so the
// containers built from it carry the arguments the daemon's Job does.
func admissionValues() Values {
	v := goldenValues()
	v.Namespace = DefaultNamespace
	return v
}

// celString is s as a CEL string literal. A value holding a quote or backslash
// cannot travel in the policy's single-quoted literals, so it fails the test
// rather than producing a manifest the API server would refuse.
func celString(t *testing.T, s string) string {
	t.Helper()
	if strings.ContainsAny(s, `'\`) {
		t.Fatalf("%q holds a quote or backslash, which a single-quoted CEL literal cannot carry", s)
	}
	return "'" + s + "'"
}

// celList is vs as a CEL list literal.
func celList(t *testing.T, vs []string) string {
	t.Helper()
	lits := make([]string, len(vs))
	for i, v := range vs {
		lits[i] = celString(t, v)
	}
	return strings.Join(lits, ", ")
}

// admissionArgsExpr pins every argument to the renderer's, except the one slot the
// caller chooses: an argument matching commitRE (the green commit) is bound to the
// full-sha shape rather than to one commit.
func admissionArgsExpr(t *testing.T, args []string) string {
	t.Helper()
	parts := []string{fmt.Sprintf("size(c.args) == %d", len(args))}
	for i, a := range args {
		if commitRE.MatchString(a) {
			parts = append(parts, fmt.Sprintf("c.args[%d].matches('^[0-9a-f]{40}$')", i))
			continue
		}
		parts = append(parts, fmt.Sprintf("c.args[%d] == %s", i, celString(t, a)))
	}
	return strings.Join(parts, " && ")
}

// admissionEnvExpr pins the environment to every variable containerEnv sets, with
// its literal value, and refuses envFrom. A volume pin alone does not stop a Secret
// entering through the environment, and GIT_CONFIG_VALUE_0 runs a command, so both
// are pinned rather than only bounded.
func admissionEnvExpr(t *testing.T) string {
	t.Helper()
	env := containerEnv()
	parts := []string{"has(c.env)", fmt.Sprintf("size(c.env) == %d", len(env))}
	for _, e := range env {
		parts = append(parts, fmt.Sprintf("c.env.exists(e, e.name == %s && has(e.value) && e.value == %s)",
			celString(t, e.Name), celString(t, e.Value)))
	}
	return strings.Join(parts, " && ")
}

// admissionNoElevationExpr refuses a privileged container and added Linux
// capabilities. The renderer sets no securityContext, but an empty one is not a
// reason to refuse the Job, so only the elevations are named.
func admissionNoElevationExpr() string {
	return "(!has(c.securityContext) || " +
		"((!has(c.securityContext.privileged) || !c.securityContext.privileged) && " +
		"(!has(c.securityContext.capabilities) || !has(c.securityContext.capabilities.add) || " +
		"size(c.securityContext.capabilities.add) == 0)))"
}

// admissionContainerExpr is the bound on one container bound to c: the renderer's
// name, the admitted image, the renderer's command and arguments, the pinned
// environment, no envFrom and no elevation.
func admissionContainerExpr(t *testing.T, c Container) string {
	t.Helper()
	return strings.Join([]string{
		"c.name == " + celString(t, c.Name),
		"c.image.matches(" + celString(t, admissionImagePattern()) + ")",
		"c.command == [" + celList(t, c.Command) + "]",
		admissionArgsExpr(t, c.Args),
		admissionEnvExpr(t),
		"!has(c.envFrom)",
		admissionNoElevationExpr(),
	}, " && ")
}

// admissionValidations is every validation the policy must carry, in order, built
// from the renderer's own output. The check compares each stored expression to this
// list exactly (whitespace aside), so a weakened comparison, a dropped validation or
// a changed name fails the test and not only a reader's eye.
func admissionValidations(t *testing.T) []string {
	t.Helper()
	v := admissionValues()
	return []string{
		fmt.Sprintf("object.metadata.name.startsWith(%s)", celString(t, JobNamePrefix)),
		fmt.Sprintf("object.spec.template.spec.serviceAccountName == %[1]s && "+
			"(!has(object.spec.template.spec.serviceAccount) || object.spec.template.spec.serviceAccount == %[1]s)",
			celString(t, ServiceAccountName)),
		"(!has(object.spec.template.spec.hostNetwork) || !object.spec.template.spec.hostNetwork) && " +
			"(!has(object.spec.template.spec.hostPID) || !object.spec.template.spec.hostPID) && " +
			"(!has(object.spec.template.spec.hostIPC) || !object.spec.template.spec.hostIPC)",
		"size(object.spec.template.spec.containers) == 1 && " +
			"object.spec.template.spec.containers.all(c, " + admissionContainerExpr(t, updateContainer(v)) + ")",
		"has(object.spec.template.spec.initContainers) && " +
			"size(object.spec.template.spec.initContainers) == 1 && " +
			"object.spec.template.spec.initContainers.all(c, " + admissionContainerExpr(t, imagesContainer(v)) + ")",
		fmt.Sprintf("has(object.spec.template.spec.volumes) && size(object.spec.template.spec.volumes) == 2 && "+
			"object.spec.template.spec.volumes.all(v, "+
			"(v.name == %[1]s && has(v.persistentVolumeClaim) && v.persistentVolumeClaim.claimName == %[2]s && !has(v.hostPath)) || "+
			"(v.name == %[3]s && has(v.secret) && v.secret.secretName == %[4]s && !has(v.hostPath)))",
			celString(t, TownStateVolume), celString(t, k8sdaemon.TownStateClaim),
			celString(t, ViewerTokenVolume), celString(t, ViewerTokenSecret)),
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
func updateAdmissionProblems(t *testing.T, docs []admissionDoc) []string {
	t.Helper()
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
	problems = append(problems, admissionPolicyProblems(t, policy)...)
	return append(problems, admissionBindingProblems(binding, policy.Metadata.Name)...)
}

// admissionPolicyProblems names every way the policy's own rules differ from what
// the renderer's output requires.
func admissionPolicyProblems(t *testing.T, policy admissionDoc) []string {
	t.Helper()
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
	want := admissionValidations(t)
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

// admissionJobContainerProblems holds one rendered container to the policy's
// bounds: the renderer's name, an image the regex admits, the renderer's command,
// arguments whose green-commit slot is a full sha, and exactly the renderer's
// environment.
func admissionJobContainerProblems(got, want Container) []string {
	var problems []string
	if got.Name != want.Name {
		problems = append(problems, fmt.Sprintf("container name %q, want %q", got.Name, want.Name))
	}
	if !admissionImageRE.MatchString(got.Image) {
		problems = append(problems, fmt.Sprintf("container %q image %q is not the daemon image from %s by sha256 digest", got.Name, got.Image, admissionImagePrefix))
	}
	if strings.Join(got.Command, "\x00") != strings.Join(want.Command, "\x00") {
		problems = append(problems, fmt.Sprintf("container %q command is %v, want %v", got.Name, got.Command, want.Command))
	}
	problems = append(problems, admissionJobArgsProblems(got, want)...)
	problems = append(problems, admissionJobEnvProblems(got, want)...)
	return problems
}

// admissionJobArgsProblems holds the Job's arguments to the shape the policy pins:
// every argument equal to the renderer's, but a green-commit slot any full sha.
func admissionJobArgsProblems(got, want Container) []string {
	if len(got.Args) != len(want.Args) {
		return []string{fmt.Sprintf("container %q has %d arguments, want the renderer's %d", got.Name, len(got.Args), len(want.Args))}
	}
	var problems []string
	for i, w := range want.Args {
		if commitRE.MatchString(w) {
			if !commitRE.MatchString(got.Args[i]) {
				problems = append(problems, fmt.Sprintf("container %q argument %d is %q, want a full lowercase sha", got.Name, i, got.Args[i]))
			}
			continue
		}
		if got.Args[i] != w {
			problems = append(problems, fmt.Sprintf("container %q argument %d is %q, want %q", got.Name, i, got.Args[i], w))
		}
	}
	return problems
}

// admissionJobEnvProblems holds the Job's environment to the four variables the
// policy pins, by name and value.
func admissionJobEnvProblems(got, want Container) []string {
	var problems []string
	if len(got.Env) != len(want.Env) {
		problems = append(problems, fmt.Sprintf("container %q has %d environment variables, want the renderer's %d", got.Name, len(got.Env), len(want.Env)))
	}
	byName := map[string]string{}
	for _, e := range got.Env {
		byName[e.Name] = e.Value
	}
	if len(byName) != len(want.Env) {
		problems = append(problems, fmt.Sprintf("container %q names %d distinct environment variables, want %d", got.Name, len(byName), len(want.Env)))
	}
	for _, w := range want.Env {
		if v, ok := byName[w.Name]; !ok {
			problems = append(problems, fmt.Sprintf("container %q sets no %s", got.Name, w.Name))
		} else if v != w.Value {
			problems = append(problems, fmt.Sprintf("container %q sets %s to %q, want %q", got.Name, w.Name, v, w.Value))
		}
	}
	return problems
}

// admissionMapField returns m[key] as a mapping, and whether it was one.
func admissionMapField(m map[string]any, key string) (map[string]any, bool) {
	v, ok := m[key].(map[string]any)
	return v, ok
}

// admissionEncodedProblems names every pod field the policy refuses that the
// renderer's types do not model, read from the encoded Job: the host namespaces, a
// container securityContext or envFrom, and an env valueFrom. A renderer change
// that added one of them would otherwise pass the typed checks and reach the
// cluster, where the policy would refuse the daemon's own Job.
func admissionEncodedProblems(raw []byte) []string {
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return []string{fmt.Sprintf("the Job does not decode: %v", err)}
	}
	spec, ok := admissionMapField(doc, "spec")
	if !ok {
		return []string{"the Job holds no spec"}
	}
	template, ok := admissionMapField(spec, "template")
	if !ok {
		return []string{"the Job holds no spec.template"}
	}
	pod, ok := admissionMapField(template, "spec")
	if !ok {
		return []string{"the Job holds no spec.template.spec"}
	}
	var problems []string
	for _, key := range []string{"hostNetwork", "hostPID", "hostIPC"} {
		if _, found := pod[key]; found {
			problems = append(problems, fmt.Sprintf("the Job's pod sets %s, which the policy refuses", key))
		}
	}
	for _, field := range []string{"containers", "initContainers"} {
		list, ok := pod[field].([]any)
		if !ok {
			problems = append(problems, fmt.Sprintf("the Job's pod %s are not a list", field))
			continue
		}
		for _, item := range list {
			c, ok := item.(map[string]any)
			if !ok {
				problems = append(problems, fmt.Sprintf("a %s entry is not a container", field))
				continue
			}
			for _, key := range []string{"securityContext", "envFrom"} {
				if _, found := c[key]; found {
					problems = append(problems, fmt.Sprintf("container %v sets %s, which the policy refuses", c["name"], key))
				}
			}
			if raw, found := c["env"]; found {
				env, ok := raw.([]any)
				if !ok {
					problems = append(problems, fmt.Sprintf("container %v environment is not a list", c["name"]))
					continue
				}
				for _, e := range env {
					entry, ok := e.(map[string]any)
					if !ok {
						problems = append(problems, fmt.Sprintf("container %v has an environment entry that is not a mapping", c["name"]))
						continue
					}
					if _, found := entry["valueFrom"]; found {
						problems = append(problems, fmt.Sprintf("container %v reads %v through valueFrom, which the policy refuses", c["name"], entry["name"]))
					}
				}
			}
		}
	}
	return problems
}

// updateAdmissionJobProblems names every way the rendered Job departs from what
// the policy's expressions require: the daemon's name, the ServiceAccount, the one
// container and one init container the renderer sets with their image, command,
// arguments and environment, exactly the two volumes, and no pod field the policy
// refuses in the encoded Job.
func updateAdmissionJobProblems(job Job) []string {
	var problems []string
	pod := job.Spec.Template.Spec
	if !strings.HasPrefix(job.Metadata.Name, JobNamePrefix) {
		problems = append(problems, fmt.Sprintf("the Job's name %q does not start %q", job.Metadata.Name, JobNamePrefix))
	}
	if pod.ServiceAccountName != ServiceAccountName {
		problems = append(problems, fmt.Sprintf("the Job's pod runs as %q, want %q", pod.ServiceAccountName, ServiceAccountName))
	}
	v := admissionValues()
	if len(pod.Containers) != 1 {
		problems = append(problems, fmt.Sprintf("the Job's pod runs %d containers, want the one update container", len(pod.Containers)))
	} else {
		problems = append(problems, admissionJobContainerProblems(pod.Containers[0], updateContainer(v))...)
	}
	if len(pod.InitContainers) != 1 {
		problems = append(problems, fmt.Sprintf("the Job's pod runs %d init containers, want the one images init container", len(pod.InitContainers)))
	} else {
		problems = append(problems, admissionJobContainerProblems(pod.InitContainers[0], imagesContainer(v))...)
	}
	problems = append(problems, admissionJobVolumeProblems(pod)...)
	var buf bytes.Buffer
	if err := YAML(&buf, job); err != nil {
		return append(problems, fmt.Sprintf("encoding the Job: %v", err))
	}
	return append(problems, admissionEncodedProblems(buf.Bytes())...)
}

// admissionJobVolumeProblems holds the rendered Job's volumes to the two the policy
// names.
func admissionJobVolumeProblems(pod PodSpec) []string {
	var problems []string
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
	if problems := updateAdmissionProblems(t, loadUpdateAdmission(t)); len(problems) > 0 {
		t.Errorf("%s: %s", updateAdmissionManifest, strings.Join(problems, "; "))
	}
	job := mustBuild(t, goldenValues()).Job
	// The daemon renames the rendered Job before it creates it (updateJobName in
	// internal/daemon): the name the policy admits is that renamed one.
	job.Metadata.Name = JobNamePrefix + "0123456789ab"
	if problems := updateAdmissionJobProblems(job); len(problems) > 0 {
		t.Errorf("the rendered Job: %s", strings.Join(problems, "; "))
	}
}

// updateAdmissionRefusal is an edit to the manifest that the checks must refuse, and
// the text the refusal must name.
type updateAdmissionRefusal struct {
	name string
	edit func(t *testing.T, docs []admissionDoc) []admissionDoc
	want string
}

// updateAdmissionRefusals is every weakening the checks must name: a document of
// another kind, a renamed or missing policy or binding, an action that only warns, a
// scope wider than Job creation, a validation that admits a wider Job or a different
// container, and a binding that reaches another account or namespace.
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
		want: "validations, want 6",
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
		name: "a second container admitted",
		edit: func(t *testing.T, docs []admissionDoc) []admissionDoc {
			docs[0].Spec.Validations[3].Expression = strings.Replace(docs[0].Spec.Validations[3].Expression,
				"size(object.spec.template.spec.containers) == 1", "size(object.spec.template.spec.containers) >= 1", 1)
			return docs
		},
		want: "validation 4",
	},
	{
		name: "an image outside the registry",
		edit: func(t *testing.T, docs []admissionDoc) []admissionDoc {
			docs[0].Spec.Validations[3].Expression = strings.ReplaceAll(docs[0].Spec.Validations[3].Expression, admissionImagePrefix, "")
			return docs
		},
		want: "validation 4",
	},
	{
		name: "another command",
		edit: func(t *testing.T, docs []admissionDoc) []admissionDoc {
			docs[0].Spec.Validations[3].Expression = strings.Replace(docs[0].Spec.Validations[3].Expression,
				"c.command == ['gt']", "true", 1)
			return docs
		},
		want: "validation 4",
	},
	{
		name: "another environment value",
		edit: func(t *testing.T, docs []admissionDoc) []admissionDoc {
			docs[0].Spec.Validations[3].Expression = strings.Replace(docs[0].Spec.Validations[3].Expression,
				"e.value == '/run/gt/xdg'", "true", 1)
			return docs
		},
		want: "validation 4",
	},
	{
		name: "a Secret through envFrom",
		edit: func(t *testing.T, docs []admissionDoc) []admissionDoc {
			docs[0].Spec.Validations[3].Expression = strings.Replace(docs[0].Spec.Validations[3].Expression,
				"!has(c.envFrom)", "true", 1)
			return docs
		},
		want: "validation 4",
	},
	{
		name: "an added capability",
		edit: func(t *testing.T, docs []admissionDoc) []admissionDoc {
			docs[0].Spec.Validations[3].Expression = strings.Replace(docs[0].Spec.Validations[3].Expression,
				"size(c.securityContext.capabilities.add) == 0", "true", 1)
			return docs
		},
		want: "validation 4",
	},
	{
		name: "another init container",
		edit: func(t *testing.T, docs []admissionDoc) []admissionDoc {
			docs[0].Spec.Validations[4].Expression = strings.Replace(docs[0].Spec.Validations[4].Expression,
				"c.name == 'images'", "true", 1)
			return docs
		},
		want: "validation 5",
	},
	{
		name: "a third volume admitted",
		edit: func(t *testing.T, docs []admissionDoc) []admissionDoc {
			docs[0].Spec.Validations[5].Expression = strings.Replace(docs[0].Spec.Validations[5].Expression,
				"size(object.spec.template.spec.volumes) == 2", "size(object.spec.template.spec.volumes) <= 3", 1)
			return docs
		},
		want: "validation 6",
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
			problems := strings.Join(updateAdmissionProblems(t, r.edit(t, loadUpdateAdmission(t))), "; ")
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
// with a third volume, another container or init container, another image, command,
// argument or environment variable, another ServiceAccount, another Secret volume or
// a missing town-state claim is named, so a renderer change cannot silently leave
// the policy rejecting the daemon's own Job.
func TestUpdateAdmissionJobRefusesAnotherJob(t *testing.T) {
	t.Parallel()
	for _, r := range []struct {
		name string
		edit func(*Job)
		want string
	}{
		{
			name: "a second container",
			edit: func(j *Job) {
				j.Spec.Template.Spec.Containers = append(j.Spec.Template.Spec.Containers, Container{Name: "extra"})
			},
			want: "containers",
		},
		{
			name: "a second init container",
			edit: func(j *Job) {
				j.Spec.Template.Spec.InitContainers = append(j.Spec.Template.Spec.InitContainers, Container{Name: "extra"})
			},
			want: "init containers",
		},
		{
			name: "another image",
			edit: func(j *Job) {
				j.Spec.Template.Spec.Containers[0].Image = "registry.example.test/gastown/daemon@sha256:" + strings.Repeat("0123456789abcdef", 4)
			},
			want: "not the daemon image",
		},
		{
			name: "another command",
			edit: func(j *Job) { j.Spec.Template.Spec.Containers[0].Command = []string{"sh"} },
			want: "command",
		},
		{
			name: "an added argument",
			edit: func(j *Job) {
				j.Spec.Template.Spec.Containers[0].Args = append(j.Spec.Template.Spec.Containers[0].Args, "--extra")
			},
			want: "arguments",
		},
		{
			name: "an added environment variable",
			edit: func(j *Job) {
				j.Spec.Template.Spec.Containers[0].Env = append(j.Spec.Template.Spec.Containers[0].Env, EnvVar{Name: "EXTRA", Value: "x"})
			},
			want: "environment",
		},
		{
			name: "another ServiceAccount",
			edit: func(j *Job) { j.Spec.Template.Spec.ServiceAccountName = k8sdaemon.ServiceAccountName },
			want: "runs as",
		},
		{
			name: "a third volume",
			edit: func(j *Job) {
				j.Spec.Template.Spec.Volumes = append(j.Spec.Template.Spec.Volumes,
					Volume{Name: "extra", PersistentVolumeClaim: &PVCVolumeSource{ClaimName: "extra"}})
			},
			want: "3 volumes",
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
		{
			name: "another name",
			edit: func(j *Job) { j.Metadata.Name = "gt-probe" },
			want: "does not start",
		},
	} {
		t.Run(r.name, func(t *testing.T) {
			t.Parallel()
			job := mustBuild(t, goldenValues()).Job
			job.Metadata.Name = JobNamePrefix + "0123456789ab"
			r.edit(&job)
			problems := strings.Join(updateAdmissionJobProblems(job), "; ")
			if !strings.Contains(problems, r.want) {
				t.Errorf("%s: problems %q do not name %q", r.name, problems, r.want)
			}
		})
	}
}

// admissionTestPod returns the decoded Job's pod spec for a mutation.
func admissionTestPod(t *testing.T, doc map[string]any) map[string]any {
	t.Helper()
	spec, ok := admissionMapField(doc, "spec")
	if !ok {
		t.Fatal("the encoded Job holds no spec")
	}
	template, ok := admissionMapField(spec, "template")
	if !ok {
		t.Fatal("the encoded Job holds no spec.template")
	}
	pod, ok := admissionMapField(template, "spec")
	if !ok {
		t.Fatal("the encoded Job holds no spec.template.spec")
	}
	return pod
}

// admissionTestContainer returns one container of the decoded Job's pod spec.
func admissionTestContainer(t *testing.T, pod map[string]any, field string, i int) map[string]any {
	t.Helper()
	list, ok := pod[field].([]any)
	if !ok || i >= len(list) {
		t.Fatalf("the encoded Job holds no %s[%d]", field, i)
	}
	c, ok := list[i].(map[string]any)
	if !ok {
		t.Fatalf("%s[%d] is not a container", field, i)
	}
	return c
}

// TestUpdateAdmissionEncodedJobRefusesAPodField drives the encoded-Job check: a Job
// that carried a host namespace, a container securityContext or envFrom, or an env
// valueFrom is named, so a renderer change adding one of them fails here and not on
// the cluster.
func TestUpdateAdmissionEncodedJobRefusesAPodField(t *testing.T) {
	t.Parallel()
	for _, r := range []struct {
		name string
		edit func(t *testing.T, pod map[string]any)
		want string
	}{
		{
			name: "a host namespace",
			edit: func(t *testing.T, pod map[string]any) { pod["hostNetwork"] = true },
			want: "hostNetwork",
		},
		{
			name: "a container securityContext",
			edit: func(t *testing.T, pod map[string]any) {
				admissionTestContainer(t, pod, "containers", 0)["securityContext"] = map[string]any{"privileged": true}
			},
			want: "securityContext",
		},
		{
			name: "a container envFrom",
			edit: func(t *testing.T, pod map[string]any) {
				admissionTestContainer(t, pod, "containers", 0)["envFrom"] = []any{map[string]any{"secretRef": map[string]any{"name": "other"}}}
			},
			want: "envFrom",
		},
		{
			name: "an env valueFrom",
			edit: func(t *testing.T, pod map[string]any) {
				c := admissionTestContainer(t, pod, "initContainers", 0)
				env, ok := c["env"].([]any)
				if !ok || len(env) == 0 {
					t.Fatal("the init container holds no environment")
				}
				env[0] = map[string]any{"name": "XDG_CONFIG_HOME", "valueFrom": map[string]any{"secretKeyRef": map[string]any{"name": "other", "key": "token"}}}
			},
			want: "valueFrom",
		},
		{
			name: "containers that are not a list",
			edit: func(t *testing.T, pod map[string]any) { pod["containers"] = "not a list" },
			want: "are not a list",
		},
		{
			name: "a container that is not a mapping",
			edit: func(t *testing.T, pod map[string]any) { pod["containers"] = []any{"not a container"} },
			want: "is not a container",
		},
		{
			name: "an environment that is not a list",
			edit: func(t *testing.T, pod map[string]any) {
				admissionTestContainer(t, pod, "containers", 0)["env"] = "not a list"
			},
			want: "environment is not a list",
		},
		{
			name: "an environment entry that is not a mapping",
			edit: func(t *testing.T, pod map[string]any) {
				admissionTestContainer(t, pod, "containers", 0)["env"] = []any{"not an entry"}
			},
			want: "not a mapping",
		},
	} {
		t.Run(r.name, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			if err := YAML(&buf, mustBuild(t, goldenValues()).Job); err != nil {
				t.Fatalf("encoding the Job: %v", err)
			}
			var doc map[string]any
			if err := yaml.Unmarshal(buf.Bytes(), &doc); err != nil {
				t.Fatalf("decoding the Job: %v", err)
			}
			r.edit(t, admissionTestPod(t, doc))
			mutated, err := yaml.Marshal(doc)
			if err != nil {
				t.Fatalf("encoding the mutated Job: %v", err)
			}
			problems := strings.Join(admissionEncodedProblems(mutated), "; ")
			if !strings.Contains(problems, r.want) {
				t.Errorf("%s: problems %q do not name %q", r.name, problems, r.want)
			}
		})
	}
}

// TestUpdateAdmissionEncodedJobRefusesWhatItCannotRead holds the encoded-Job check to
// its refusals: a document that does not decode, and one with no pod spec, are
// problems, never a clean read.
func TestUpdateAdmissionEncodedJobRefusesWhatItCannotRead(t *testing.T) {
	t.Parallel()
	for _, r := range []struct{ name, raw, want string }{
		{name: "an undecodable document", raw: "kind: [\n", want: "does not decode"},
		{name: "no spec", raw: "kind: Job\n", want: "the Job holds no spec"},
		{name: "no template", raw: "spec: {}\n", want: "the Job holds no spec.template"},
		{name: "no pod spec", raw: "spec:\n  template: {}\n", want: "the Job holds no spec.template.spec"},
	} {
		problems := strings.Join(admissionEncodedProblems([]byte(r.raw)), "; ")
		if !strings.Contains(problems, r.want) {
			t.Errorf("%s: problems %q do not name %q", r.name, problems, r.want)
		}
	}
}

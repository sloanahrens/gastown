package updatejob

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode"

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

// PinnedBdGatewayURL is the only gateway address the policy admits: the address the
// daemon hands the Job, not the name it configures its own forwarder with.
// gatewayURLForJob (internal/daemon/update_job.go) resolves the daemon's gateway name
// to an address before the Job is created, and the policy refuses hostAliases, so
// every Job the daemon makes carries an address. The policy repeats this literal, and
// TestAdmissionGatewayPinMatchesRenderer holds the two equal.
const PinnedBdGatewayURL = "http://192.168.5.2:7440"

// admissionGatewayValues is admissionValues with the bd gateway set: the Job the
// daemon renders when bd answers its pre-swap handshake through the gateway
// (gt-15dkai). It is the one Job the policy admits beyond the gateway-off render.
func admissionGatewayValues() Values {
	v := admissionValues()
	v.BdGatewayURL = PinnedBdGatewayURL
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

// admissionEnvExpr pins the environment to every variable containerEnv sets for v,
// each to its one value, and refuses envFrom. The gateway URL is pinned by equality
// because the daemon-class token the Job mounts follows it: any other destination
// would carry that token off the host. A volume pin alone does not stop a Secret
// entering through the environment, and GIT_CONFIG_VALUE_0 runs a command, so both
// are pinned rather than only bounded.
func admissionEnvExpr(t *testing.T, v Values) string {
	t.Helper()
	env := containerEnv(v)
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

// admissionNoExecExpr refuses the fields that run a command of their own: a
// lifecycle hook and the three probes. Pinning command and args alone would leave
// these, and a probe runs in the container as the pod's ServiceAccount.
func admissionNoExecExpr() string {
	return "!has(c.lifecycle) && !has(c.livenessProbe) && " +
		"!has(c.readinessProbe) && !has(c.startupProbe)"
}

// admissionMountsExpr pins every mount to the renderer's: a mount path the
// renderer does not set could put a file from the claim over a binary the pinned
// command runs.
func admissionMountsExpr(t *testing.T, mounts []VolumeMount) string {
	t.Helper()
	parts := []string{fmt.Sprintf("size(c.volumeMounts) == %d", len(mounts))}
	for _, m := range mounts {
		clauses := []string{
			"m.name == " + celString(t, m.Name),
			"m.mountPath == " + celString(t, m.MountPath),
		}
		clauses = append(clauses, "!has(m.subPathExpr)")
		if m.SubPath == "" {
			clauses = append(clauses, "!has(m.subPath)")
		} else {
			clauses = append(clauses, "m.subPath == "+celString(t, m.SubPath))
		}
		if m.ReadOnly {
			clauses = append(clauses, "has(m.readOnly) && m.readOnly")
		} else {
			clauses = append(clauses, "(!has(m.readOnly) || !m.readOnly)")
		}
		parts = append(parts, "c.volumeMounts.exists(m, "+strings.Join(clauses, " && ")+")")
	}
	return strings.Join(parts, " && ")
}

// admissionEnvMountsExpr is one render's environment and mounts as one conjunct, so
// the gateway pair and the gateway mount are admitted together and not one without
// the other.
func admissionEnvMountsExpr(t *testing.T, v Values, c Container) string {
	t.Helper()
	return "(" + admissionEnvExpr(t, v) + " && " + admissionMountsExpr(t, c.VolumeMounts) + ")"
}

// admissionContainerExpr is the bound on one container the renderer builds from off
// and from on: the renderer's name, command and arguments, the admitted image, no
// envFrom, no ports, no elevation, no hook or probe, and then either the gateway-off
// or the gateway-on environment and mounts. The disjunction is parenthesized whole,
// because && binds tighter than || and an unwrapped branch would admit a container
// that skipped every pin before it.
func admissionContainerExpr(t *testing.T, off, on Values, container func(Values) Container) string {
	t.Helper()
	offC, onC := container(off), container(on)
	return strings.Join([]string{
		"c.name == " + celString(t, offC.Name),
		"c.image.matches(" + celString(t, admissionImagePattern()) + ")",
		"c.command == [" + celList(t, offC.Command) + "]",
		admissionArgsExpr(t, offC.Args),
		"!has(c.envFrom)",
		"!has(c.ports)",
		admissionNoElevationExpr(),
		admissionNoExecExpr(),
		"(" + admissionEnvMountsExpr(t, off, offC) + " || " + admissionEnvMountsExpr(t, on, onC) + ")",
	}, " && ")
}

// admissionPodExpr is the bound on the pod fields the policy pins: nodeName,
// nodeSelector, tolerations and runtimeClassName unset, a pod securityContext with
// no run-as identity, group, sysctl or security profile, and the token automount,
// priority class and restart policy the renderer sets. The renderer models only the
// last three, so the rest are the renderer's silence stated as a refusal; the API
// server's empty `securityContext: {}` default is why the clause tests the fields
// and not the object.
func admissionPodExpr(t *testing.T, pod PodSpec) string {
	t.Helper()
	parts := []string{
		"!has(object.spec.template.spec.nodeName)",
		"!has(object.spec.template.spec.nodeSelector)",
		"!has(object.spec.template.spec.tolerations)",
		"!has(object.spec.template.spec.runtimeClassName)",
		"!has(object.spec.template.spec.hostAliases)",
		"!has(object.spec.template.spec.dnsConfig)",
		"!has(object.spec.template.spec.shareProcessNamespace)",
		"!has(object.spec.template.spec.hostUsers)",
		"(!has(object.spec.template.spec.securityContext) || " +
			"(!has(object.spec.template.spec.securityContext.runAsNonRoot) && " +
			"!has(object.spec.template.spec.securityContext.runAsUser) && " +
			"!has(object.spec.template.spec.securityContext.runAsGroup) && " +
			"!has(object.spec.template.spec.securityContext.fsGroup) && " +
			"!has(object.spec.template.spec.securityContext.supplementalGroups) && " +
			"!has(object.spec.template.spec.securityContext.sysctls) && " +
			"!has(object.spec.template.spec.securityContext.seccompProfile) && " +
			"!has(object.spec.template.spec.securityContext.selinuxOptions)))",
	}
	if pod.AutomountServiceAccountToken == nil {
		parts = append(parts, "!has(object.spec.template.spec.automountServiceAccountToken)")
	} else {
		parts = append(parts,
			"has(object.spec.template.spec.automountServiceAccountToken)",
			fmt.Sprintf("object.spec.template.spec.automountServiceAccountToken == %t", *pod.AutomountServiceAccountToken))
	}
	return strings.Join(append(parts,
		"object.spec.template.spec.priorityClassName == "+celString(t, pod.PriorityClassName),
		"object.spec.template.spec.restartPolicy == "+celString(t, pod.RestartPolicy)), " && ")
}

// admissionVolumeClause is one volume the renderer sets, as a clause over v: its
// name, the claim or Secret it is, and no hostPath.
func admissionVolumeClause(t *testing.T, v Volume) string {
	switch {
	case v.PersistentVolumeClaim != nil:
		return fmt.Sprintf("v.name == %s && has(v.persistentVolumeClaim) && v.persistentVolumeClaim.claimName == %s && !has(v.hostPath)",
			celString(t, v.Name), celString(t, v.PersistentVolumeClaim.ClaimName))
	case v.Secret != nil:
		return fmt.Sprintf("v.name == %s && has(v.secret) && v.secret.secretName == %s && !has(v.hostPath)",
			celString(t, v.Name), celString(t, v.Secret.SecretName))
	default:
		t.Fatalf("volume %q is neither a claim nor a Secret, which the policy does not state", v.Name)
		return ""
	}
}

// admissionVolumeExpr is the volume bound: the renderer's volumes for the gateway-off
// render or for the gateway-on one, and nothing else. Each branch pins the count,
// requires every volume to be one of that render's, and requires each of the
// render's volumes by name, so the third volume a gateway Job has is the
// daemon-class gateway Secret and not a second claim under another name.
func admissionVolumeExpr(t *testing.T, off, on Values) string {
	t.Helper()
	branch := func(vols []Volume) string {
		clauses := make([]string, len(vols))
		named := make([]string, len(vols))
		for i, v := range vols {
			clause := admissionVolumeClause(t, v)
			clauses[i] = "(" + clause + ")"
			named[i] = "object.spec.template.spec.volumes.exists(v, " + clause + ")"
		}
		return fmt.Sprintf("(size(object.spec.template.spec.volumes) == %d && "+
			"object.spec.template.spec.volumes.all(v, %s) && %s)",
			len(vols), strings.Join(clauses, " || "), strings.Join(named, " && "))
	}
	return "has(object.spec.template.spec.volumes) && (" +
		branch(volumes(off)) + " || " + branch(volumes(on)) + ")"
}

// admissionValidations is every validation the policy must carry, in order, built
// from the renderer's own output. The check compares each stored expression to this
// list exactly (whitespace aside), so a weakened comparison, a dropped validation or
// a changed name fails the test and not only a reader's eye.
func admissionValidations(t *testing.T) []string {
	t.Helper()
	v := admissionValues()
	gateway := admissionGatewayValues()
	pod := jobSpec(v).Template.Spec
	return []string{
		fmt.Sprintf("object.metadata.name.startsWith(%s)", celString(t, JobNamePrefix)),
		fmt.Sprintf("object.spec.template.spec.serviceAccountName == %[1]s && "+
			"(!has(object.spec.template.spec.serviceAccount) || object.spec.template.spec.serviceAccount == %[1]s)",
			celString(t, ServiceAccountName)),
		"(!has(object.spec.template.spec.hostNetwork) || !object.spec.template.spec.hostNetwork) && " +
			"(!has(object.spec.template.spec.hostPID) || !object.spec.template.spec.hostPID) && " +
			"(!has(object.spec.template.spec.hostIPC) || !object.spec.template.spec.hostIPC)",
		"size(object.spec.template.spec.containers) == 1 && " +
			"object.spec.template.spec.containers.all(c, " + admissionContainerExpr(t, v, gateway, updateContainer) + ")",
		"has(object.spec.template.spec.initContainers) && " +
			"size(object.spec.template.spec.initContainers) == 1 && " +
			"object.spec.template.spec.initContainers.all(c, " + admissionContainerExpr(t, v, gateway, imagesContainer) + ")",
		admissionVolumeExpr(t, v, gateway),
		admissionPodExpr(t, pod),
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
// names, a policy of Fail on Job CREATE in batch/v1 that skips every other account,
// the validations of admissionValidations, and a binding that denies in namespace
// gt.
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
	// matchConditions belong to the policy (admissionregistration.k8s.io/v1 has no
	// such field on the binding), and a condition that skips rather than refuses is
	// what keeps the operator's own Job creation out of these validations.
	conditions := policy.Spec.MatchConditions
	if len(conditions) != 1 {
		problems = append(problems, fmt.Sprintf("the policy holds %d matchConditions, want only the daemon's ServiceAccount", len(conditions)))
	} else if want := fmt.Sprintf("request.userInfo.username == '%s'", updateAdmissionUser); admissionText(conditions[0].Expression) != want {
		problems = append(problems, fmt.Sprintf("matchConditions expression is %q, want %q", admissionText(conditions[0].Expression), want))
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

// admissionBindingProblems names every way the binding fails to hold the policy's
// validations to Job creation in gt: another policy, an action that only warns, a
// wider namespace, or a matchConditions the binding has no field for.
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
	if len(binding.Spec.MatchConditions) != 0 {
		problems = append(problems, "the binding sets matchConditions, which admissionregistration.k8s.io/v1 has no field for: the policy's matchConditions must carry them")
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
	return append(problems, admissionJobMountProblems(got, want)...)
}

// admissionJobMountProblems holds the Job's mounts to the renderer's names, paths,
// subpaths and read-only flags, since the policy pins each of them.
func admissionJobMountProblems(got, want Container) []string {
	var problems []string
	if len(got.VolumeMounts) != len(want.VolumeMounts) {
		return []string{fmt.Sprintf("container %q mounts %d volumes, want the renderer's %d", got.Name, len(got.VolumeMounts), len(want.VolumeMounts))}
	}
	for i, w := range want.VolumeMounts {
		g := got.VolumeMounts[i]
		if g != w {
			problems = append(problems, fmt.Sprintf("container %q mount %d is %+v, want %+v", got.Name, i, g, w))
		}
	}
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
// renderer's types do not model, read from the encoded Job: the host namespaces,
// nodeName, nodeSelector, tolerations and runtimeClassName, the pod
// securityContext's identity, group, sysctl and profile fields, a container
// securityContext, envFrom, ports, lifecycle hook or probe, an env valueFrom, and
// any volume source other than a claim or a Secret. A renderer change that added
// one of them would otherwise pass the typed checks and reach the cluster, where
// the policy would refuse the daemon's own Job.
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
	for _, key := range []string{"hostNetwork", "hostPID", "hostIPC", "nodeName", "nodeSelector", "tolerations", "runtimeClassName", "hostAliases", "dnsConfig", "shareProcessNamespace", "hostUsers"} {
		if _, found := pod[key]; found {
			problems = append(problems, fmt.Sprintf("the Job's pod sets %s, which the policy refuses", key))
		}
	}
	problems = append(problems, admissionEncodedSecurityContextProblems(pod)...)
	problems = append(problems, admissionEncodedVolumeProblems(pod)...)
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
			for _, key := range []string{"securityContext", "envFrom", "ports", "lifecycle", "livenessProbe", "readinessProbe", "startupProbe"} {
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

// admissionEncodedSecurityContextProblems names each pod securityContext field the
// policy refuses, since the renderer's types do not model a pod securityContext and
// a renderer change adding one would otherwise reach the cluster. The API server's
// empty `{}` default is why the fields are checked, not the object.
func admissionEncodedSecurityContextProblems(pod map[string]any) []string {
	raw, found := pod["securityContext"]
	if !found {
		return nil
	}
	sc, ok := raw.(map[string]any)
	if !ok {
		return []string{"the Job's pod securityContext is not a mapping"}
	}
	var problems []string
	for _, key := range []string{"runAsNonRoot", "runAsUser", "runAsGroup", "fsGroup", "supplementalGroups", "sysctls", "seccompProfile", "selinuxOptions"} {
		if _, found := sc[key]; found {
			problems = append(problems, fmt.Sprintf("the Job's pod securityContext sets %s, which the policy refuses", key))
		}
	}
	return problems
}

// admissionEncodedVolumeProblems holds every volume to the two kinds the policy
// admits. The renderer's Volume type models only a claim and a Secret, so any other
// source key is a renderer change the policy would refuse, hostPath first.
func admissionEncodedVolumeProblems(pod map[string]any) []string {
	raw, found := pod["volumes"]
	if !found {
		return nil
	}
	volumes, ok := raw.([]any)
	if !ok {
		return []string{"the Job's pod volumes are not a list"}
	}
	var problems []string
	for _, item := range volumes {
		v, ok := item.(map[string]any)
		if !ok {
			problems = append(problems, "a volume is not a mapping")
			continue
		}
		keys := make([]string, 0, len(v))
		for key := range v {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		for _, key := range keys {
			switch key {
			case "name", "persistentVolumeClaim", "secret":
			default:
				problems = append(problems, fmt.Sprintf("volume %v sets %s, which the policy does not admit (only a claim and a Secret)", v["name"], key))
			}
		}
	}
	return problems
}

// updateAdmissionJobProblems names every way the rendered Job departs from what
// the policy's expressions require: the daemon's name, the ServiceAccount and the
// other pod fields the renderer sets, the one container and one init container the
// renderer sets for v with their image, command, arguments, environment and mounts,
// exactly the renderer's volumes for v, and no pod field the policy refuses in the
// encoded Job.
func updateAdmissionJobProblems(job Job, v Values) []string {
	var problems []string
	pod := job.Spec.Template.Spec
	if !strings.HasPrefix(job.Metadata.Name, JobNamePrefix) {
		problems = append(problems, fmt.Sprintf("the Job's name %q does not start %q", job.Metadata.Name, JobNamePrefix))
	}
	if pod.ServiceAccountName != ServiceAccountName {
		problems = append(problems, fmt.Sprintf("the Job's pod runs as %q, want %q", pod.ServiceAccountName, ServiceAccountName))
	}
	problems = append(problems, admissionJobPodProblems(pod, jobSpec(v).Template.Spec)...)
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
	problems = append(problems, admissionJobVolumeProblems(pod, v)...)
	var buf bytes.Buffer
	if err := YAML(&buf, job); err != nil {
		return append(problems, fmt.Sprintf("encoding the Job: %v", err))
	}
	return append(problems, admissionEncodedProblems(buf.Bytes())...)
}

// admissionJobVolumeProblems holds the rendered Job's volumes to the renderer's for
// v: the town-state claim and the viewer-token Secret, and with the gateway set the
// daemon-class gateway Secret as the third.
func admissionJobVolumeProblems(pod PodSpec, v Values) []string {
	want := volumes(v)
	var problems []string
	if len(pod.Volumes) != len(want) {
		problems = append(problems, fmt.Sprintf("the Job mounts %d volumes, want the renderer's %d", len(pod.Volumes), len(want)))
	}
	seen := map[string]bool{}
	for _, got := range pod.Volumes {
		var named bool
		for _, w := range want {
			if got.Name != w.Name {
				continue
			}
			named, seen[got.Name] = true, true
			problems = append(problems, admissionJobVolumeProblemsFor(got, w)...)
		}
		if !named {
			problems = append(problems, fmt.Sprintf("volume %q is not one the renderer sets", got.Name))
		}
	}
	for _, w := range want {
		if !seen[w.Name] {
			problems = append(problems, fmt.Sprintf("the Job mounts no %q volume", w.Name))
		}
	}
	return problems
}

// admissionJobVolumeProblemsFor holds one volume to the renderer's volume of the same
// name: the same claim, or the same Secret, and not the other kind as well.
func admissionJobVolumeProblemsFor(got, want Volume) []string {
	switch {
	case want.PersistentVolumeClaim != nil:
		if got.PersistentVolumeClaim == nil || got.PersistentVolumeClaim.ClaimName != want.PersistentVolumeClaim.ClaimName || got.Secret != nil {
			return []string{fmt.Sprintf("volume %q is not the %q claim", got.Name, want.PersistentVolumeClaim.ClaimName)}
		}
	case want.Secret != nil:
		if got.Secret == nil || got.Secret.SecretName != want.Secret.SecretName || got.PersistentVolumeClaim != nil {
			return []string{fmt.Sprintf("volume %q is not the %q Secret", got.Name, want.Secret.SecretName)}
		}
	}
	return nil
}

// admissionJobPodProblems holds the Job's token automount, priority class and
// restart policy to the renderer's, since the policy pins each to what the renderer
// sets. The pod fields the renderer does not model are checked on the encoded Job.
func admissionJobPodProblems(got, want PodSpec) []string {
	var problems []string
	if admissionBool(got.AutomountServiceAccountToken) != admissionBool(want.AutomountServiceAccountToken) {
		problems = append(problems, fmt.Sprintf("the Job's pod automountServiceAccountToken is %s, want %s",
			admissionBool(got.AutomountServiceAccountToken), admissionBool(want.AutomountServiceAccountToken)))
	}
	if got.PriorityClassName != want.PriorityClassName {
		problems = append(problems, fmt.Sprintf("the Job's pod priorityClassName is %q, want %q", got.PriorityClassName, want.PriorityClassName))
	}
	if got.RestartPolicy != want.RestartPolicy {
		problems = append(problems, fmt.Sprintf("the Job's pod restartPolicy is %q, want %q", got.RestartPolicy, want.RestartPolicy))
	}
	return problems
}

// admissionBool renders a *bool for a problem message: unset, true or false.
func admissionBool(p *bool) string {
	if p == nil {
		return "unset"
	}
	return strconv.FormatBool(*p)
}

// TestUpdateAdmissionPolicy holds the manifest to its shape: the policy and binding
// as deployed admit Job creation in gt only as the daemon's ServiceAccount, and both
// Jobs the renderer makes (without the gateway, and with it) are the Jobs those
// expressions admit.
func TestUpdateAdmissionPolicy(t *testing.T) {
	t.Parallel()
	if problems := updateAdmissionProblems(t, loadUpdateAdmission(t)); len(problems) > 0 {
		t.Errorf("%s: %s", updateAdmissionManifest, strings.Join(problems, "; "))
	}
	for _, tc := range []struct {
		name   string
		values Values
	}{
		{name: "gateway off", values: admissionValues()},
		{name: "gateway on", values: admissionGatewayValues()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			job := mustBuild(t, tc.values).Job
			// The daemon renames the rendered Job before it creates it (updateJobName
			// in internal/daemon): the name the policy admits is that renamed one.
			job.Metadata.Name = JobNamePrefix + "0123456789ab"
			if problems := updateAdmissionJobProblems(job, tc.values); len(problems) > 0 {
				t.Errorf("the rendered Job: %s", strings.Join(problems, "; "))
			}
		})
	}
}

// TestUpdateAdmissionVolumeExprNamesEveryVolume holds the deployed volume
// validation to one exists clause per renderer volume in each render's branch, so
// a branch that pinned only the count and the set of allowed kinds cannot admit a
// render's volume missing with a second of another kind in its place.
func TestUpdateAdmissionVolumeExprNamesEveryVolume(t *testing.T) {
	t.Parallel()
	expr := admissionPolicyValidationText(t, 5)
	want := map[string]int{}
	for _, v := range volumes(admissionValues()) {
		want[v.Name]++
	}
	for _, v := range volumes(admissionGatewayValues()) {
		want[v.Name]++
	}
	for name, n := range want {
		needle := "object.spec.template.spec.volumes.exists(v, v.name == " + celString(t, name)
		if got := strings.Count(expr, needle); got != n {
			t.Errorf("the volume validation names %q in %d exists clauses, want %d, one for each render that has it: %q",
				name, got, n, expr)
		}
	}
}

// TestUpdateAdmissionPolicyPinsGatewayURL holds BD_GATEWAY_URL to the one address:
// the pinned URL is admitted, and another IPv4 address, a host name, another port
// and an https URL are each refused. The refusals drive the same bound the policy's
// expression states, and the deployed expressions are read to show the pin itself.
func TestUpdateAdmissionPolicyPinsGatewayURL(t *testing.T) {
	t.Parallel()
	admitted := mustBuild(t, admissionGatewayValues()).Job
	admitted.Metadata.Name = JobNamePrefix + "0123456789ab"
	if problems := updateAdmissionJobProblems(admitted, admissionGatewayValues()); len(problems) > 0 {
		t.Errorf("the Job whose BD_GATEWAY_URL is the pinned URL is refused: %s", strings.Join(problems, "; "))
	}
	for _, r := range []struct {
		url string
		why string
	}{
		{"http://192.168.5.3:7440", "another IPv4 address"},
		{"http://host.docker.internal:7440", "a host name"},
		{"http://192.168.5.2:7441", "another port"},
		{"https://192.168.5.2:7440", "an https URL"},
	} {
		t.Run(r.why, func(t *testing.T) {
			t.Parallel()
			values := admissionGatewayValues()
			values.BdGatewayURL = r.url
			job := mustBuild(t, values).Job
			job.Metadata.Name = JobNamePrefix + "0123456789ab"
			problems := strings.Join(updateAdmissionJobProblems(job, admissionGatewayValues()), "; ")
			if !strings.Contains(problems, k8sdaemon.BdGatewayURLEnv) {
				t.Errorf("%s (%q) is admitted: problems %q do not name %s",
					r.why, r.url, problems, k8sdaemon.BdGatewayURLEnv)
			}
		})
	}
	// The renderer's own check accepts all four refused URLs, so only the policy's
	// pin stands between the daemon-class token and another destination.
	for _, i := range []int{3, 4} {
		expr := admissionPolicyValidationText(t, i)
		pin := "e.name == " + celString(t, k8sdaemon.BdGatewayURLEnv) + " && has(e.value) && e.value == " + celString(t, PinnedBdGatewayURL)
		if !strings.Contains(expr, pin) {
			t.Errorf("the deployed validation %d does not pin %s to %q: %q", i+1, k8sdaemon.BdGatewayURLEnv, PinnedBdGatewayURL, expr)
		}
	}
}

// TestAdmissionGatewayPinMatchesRenderer holds the policy's gateway literal to the
// one Go constant the tests render, in both container validations, so the two copies
// cannot drift. It fails when the literal in the manifest is another URL.
func TestAdmissionGatewayPinMatchesRenderer(t *testing.T) {
	t.Parallel()
	pin := regexp.MustCompile(`e\.name == '` + k8sdaemon.BdGatewayURLEnv + `' && has\(e\.value\) && e\.value == '([^']*)'`)
	for _, i := range []int{3, 4} {
		expr := admissionPolicyValidationText(t, i)
		matches := pin.FindAllStringSubmatch(expr, -1)
		if len(matches) != 1 {
			t.Errorf("the deployed validation %d pins %s in %d clauses, want 1: %q", i+1, k8sdaemon.BdGatewayURLEnv, len(matches), expr)
			continue
		}
		if got := matches[0][1]; got != PinnedBdGatewayURL {
			t.Errorf("the deployed validation %d pins %s to %q, want the renderer's %q", i+1, k8sdaemon.BdGatewayURLEnv, got, PinnedBdGatewayURL)
		}
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
		name: "a lifecycle hook admitted",
		edit: func(t *testing.T, docs []admissionDoc) []admissionDoc {
			docs[0].Spec.Validations[3].Expression = strings.Replace(docs[0].Spec.Validations[3].Expression,
				"!has(c.lifecycle)", "true", 1)
			return docs
		},
		want: "validation 4",
	},
	{
		name: "a probe admitted",
		edit: func(t *testing.T, docs []admissionDoc) []admissionDoc {
			docs[0].Spec.Validations[4].Expression = strings.Replace(docs[0].Spec.Validations[4].Expression,
				"!has(c.readinessProbe)", "true", 1)
			return docs
		},
		want: "validation 5",
	},
	{
		name: "a mount path unpinned",
		edit: func(t *testing.T, docs []admissionDoc) []admissionDoc {
			docs[0].Spec.Validations[3].Expression = strings.Replace(docs[0].Spec.Validations[3].Expression,
				"m.mountPath == '/Users/sloan/gt'", "true", 1)
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
		name: "a fourth volume admitted",
		edit: func(t *testing.T, docs []admissionDoc) []admissionDoc {
			docs[0].Spec.Validations[5].Expression = strings.Replace(docs[0].Spec.Validations[5].Expression,
				"size(object.spec.template.spec.volumes) == 3", "size(object.spec.template.spec.volumes) <= 4", 1)
			return docs
		},
		want: "validation 6",
	},
	{
		name: "a dropped viewer-token volume pin",
		edit: func(t *testing.T, docs []admissionDoc) []admissionDoc {
			clause := " && object.spec.template.spec.volumes.exists(v, " +
				admissionVolumeClause(t, volumes(admissionGatewayValues())[1]) + ")"
			expr := docs[0].Spec.Validations[5].Expression
			if !strings.Contains(expr, clause) {
				t.Fatalf("the volume validation does not carry the viewer-token name pin %q", clause)
			}
			docs[0].Spec.Validations[5].Expression = strings.Replace(expr, clause, "", 1)
			return docs
		},
		want: "validation 6",
	},
	{
		name: "a differently named gateway Secret admitted",
		edit: func(t *testing.T, docs []admissionDoc) []admissionDoc {
			docs[0].Spec.Validations[5].Expression = strings.Replace(docs[0].Spec.Validations[5].Expression,
				"v.secret.secretName == '"+k8sdaemon.BdGatewayDaemonSecretName+"'", "true", 1)
			return docs
		},
		want: "validation 6",
	},
	{
		name: "a gateway URL matched by any value",
		edit: func(t *testing.T, docs []admissionDoc) []admissionDoc {
			pin := "e.value == " + celString(t, PinnedBdGatewayURL)
			expr := docs[0].Spec.Validations[3].Expression
			if !strings.Contains(expr, pin) {
				t.Fatalf("the container validation does not carry the gateway URL pin %q", pin)
			}
			docs[0].Spec.Validations[3].Expression = strings.Replace(expr, pin, "e.value.matches('.*')", 1)
			return docs
		},
		want: "validation 4",
	},
	{
		name: "a seventh environment variable admitted",
		edit: func(t *testing.T, docs []admissionDoc) []admissionDoc {
			docs[0].Spec.Validations[3].Expression = strings.Replace(docs[0].Spec.Validations[3].Expression,
				"size(c.env) == 6", "size(c.env) >= 6", 1)
			return docs
		},
		want: "validation 4",
	},
	{
		name: "a fourth mount admitted",
		edit: func(t *testing.T, docs []admissionDoc) []admissionDoc {
			docs[0].Spec.Validations[3].Expression = strings.Replace(docs[0].Spec.Validations[3].Expression,
				"size(c.volumeMounts) == 3", "size(c.volumeMounts) >= 3", 1)
			return docs
		},
		want: "validation 4",
	},
	{
		name: "the gateway token at another path",
		edit: func(t *testing.T, docs []admissionDoc) []admissionDoc {
			docs[0].Spec.Validations[3].Expression = strings.Replace(docs[0].Spec.Validations[3].Expression,
				"m.mountPath == '"+k8sdaemon.BdGatewayTokenDir+"'", "true", 1)
			return docs
		},
		want: "validation 4",
	},
	{
		name: "a container port admitted",
		edit: func(t *testing.T, docs []admissionDoc) []admissionDoc {
			docs[0].Spec.Validations[3].Expression = strings.Replace(docs[0].Spec.Validations[3].Expression,
				"!has(c.ports)", "true", 1)
			return docs
		},
		want: "validation 4",
	},
	{
		name: "a hostAliases admitted",
		edit: func(t *testing.T, docs []admissionDoc) []admissionDoc {
			docs[0].Spec.Validations[6].Expression = strings.Replace(docs[0].Spec.Validations[6].Expression,
				"!has(object.spec.template.spec.hostAliases)", "true", 1)
			return docs
		},
		want: "validation 7",
	},
	{
		name: "a dnsConfig admitted",
		edit: func(t *testing.T, docs []admissionDoc) []admissionDoc {
			docs[0].Spec.Validations[6].Expression = strings.Replace(docs[0].Spec.Validations[6].Expression,
				"!has(object.spec.template.spec.dnsConfig)", "true", 1)
			return docs
		},
		want: "validation 7",
	},
	{
		name: "a shareProcessNamespace admitted",
		edit: func(t *testing.T, docs []admissionDoc) []admissionDoc {
			docs[0].Spec.Validations[6].Expression = strings.Replace(docs[0].Spec.Validations[6].Expression,
				"!has(object.spec.template.spec.shareProcessNamespace)", "true", 1)
			return docs
		},
		want: "validation 7",
	},
	{
		name: "a hostUsers admitted",
		edit: func(t *testing.T, docs []admissionDoc) []admissionDoc {
			docs[0].Spec.Validations[6].Expression = strings.Replace(docs[0].Spec.Validations[6].Expression,
				"!has(object.spec.template.spec.hostUsers)", "true", 1)
			return docs
		},
		want: "validation 7",
	},
	{
		name: "a nodeName admitted",
		edit: func(t *testing.T, docs []admissionDoc) []admissionDoc {
			docs[0].Spec.Validations[6].Expression = strings.Replace(docs[0].Spec.Validations[6].Expression,
				"!has(object.spec.template.spec.nodeName)", "true", 1)
			return docs
		},
		want: "validation 7",
	},
	{
		name: "a nodeSelector admitted",
		edit: func(t *testing.T, docs []admissionDoc) []admissionDoc {
			docs[0].Spec.Validations[6].Expression = strings.Replace(docs[0].Spec.Validations[6].Expression,
				"!has(object.spec.template.spec.nodeSelector)", "true", 1)
			return docs
		},
		want: "validation 7",
	},
	{
		name: "a toleration admitted",
		edit: func(t *testing.T, docs []admissionDoc) []admissionDoc {
			docs[0].Spec.Validations[6].Expression = strings.Replace(docs[0].Spec.Validations[6].Expression,
				"!has(object.spec.template.spec.tolerations)", "true", 1)
			return docs
		},
		want: "validation 7",
	},
	{
		name: "a runtime class admitted",
		edit: func(t *testing.T, docs []admissionDoc) []admissionDoc {
			docs[0].Spec.Validations[6].Expression = strings.Replace(docs[0].Spec.Validations[6].Expression,
				"!has(object.spec.template.spec.runtimeClassName)", "true", 1)
			return docs
		},
		want: "validation 7",
	},
	{
		name: "a pod runAsUser admitted",
		edit: func(t *testing.T, docs []admissionDoc) []admissionDoc {
			docs[0].Spec.Validations[6].Expression = strings.Replace(docs[0].Spec.Validations[6].Expression,
				"!has(object.spec.template.spec.securityContext.runAsUser)", "true", 1)
			return docs
		},
		want: "validation 7",
	},
	{
		name: "the token automount unpinned",
		edit: func(t *testing.T, docs []admissionDoc) []admissionDoc {
			docs[0].Spec.Validations[6].Expression = strings.Replace(docs[0].Spec.Validations[6].Expression,
				"has(object.spec.template.spec.automountServiceAccountToken)", "true", 1)
			return docs
		},
		want: "validation 7",
	},
	{
		name: "another priority class",
		edit: func(t *testing.T, docs []admissionDoc) []admissionDoc {
			docs[0].Spec.Validations[6].Expression = strings.Replace(docs[0].Spec.Validations[6].Expression,
				"object.spec.template.spec.priorityClassName == 'gt-control-plane'",
				"object.spec.template.spec.priorityClassName == 'system-node-critical'", 1)
			return docs
		},
		want: "validation 7",
	},
	{
		name: "another restart policy",
		edit: func(t *testing.T, docs []admissionDoc) []admissionDoc {
			docs[0].Spec.Validations[6].Expression = strings.Replace(docs[0].Spec.Validations[6].Expression,
				"object.spec.template.spec.restartPolicy == 'Never'",
				"object.spec.template.spec.restartPolicy == 'OnFailure'", 1)
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
			docs[0].Spec.MatchConditions = nil
			return docs
		},
		want: "matchConditions",
	},
	{
		name: "a second matchCondition",
		edit: func(t *testing.T, docs []admissionDoc) []admissionDoc {
			docs[0].Spec.MatchConditions = append(docs[0].Spec.MatchConditions, docs[0].Spec.MatchConditions[0])
			return docs
		},
		want: "matchConditions",
	},
	{
		name: "the operator's account too",
		edit: func(t *testing.T, docs []admissionDoc) []admissionDoc {
			docs[0].Spec.MatchConditions[0].Expression = "request.userInfo.username == 'kubernetes-admin'"
			return docs
		},
		want: "matchConditions expression",
	},
	{
		name: "matchConditions on the binding",
		edit: func(t *testing.T, docs []admissionDoc) []admissionDoc {
			docs[1].Spec.MatchConditions = docs[0].Spec.MatchConditions
			return docs
		},
		want: "the binding sets matchConditions",
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
			name: "another mount path",
			edit: func(j *Job) {
				j.Spec.Template.Spec.Containers[0].VolumeMounts[0].MountPath = "/usr/local/bin"
			},
			want: "mount",
		},
		{
			name: "an added mount",
			edit: func(j *Job) {
				j.Spec.Template.Spec.Containers[0].VolumeMounts = append(j.Spec.Template.Spec.Containers[0].VolumeMounts,
					VolumeMount{Name: TownStateVolume, MountPath: "/tmp"})
			},
			want: "mounts",
		},
		{
			name: "another ServiceAccount",
			edit: func(j *Job) { j.Spec.Template.Spec.ServiceAccountName = k8sdaemon.ServiceAccountName },
			want: "runs as",
		},
		{
			name: "the token automount off",
			edit: func(j *Job) {
				off := false
				j.Spec.Template.Spec.AutomountServiceAccountToken = &off
			},
			want: "automountServiceAccountToken",
		},
		{
			name: "another priority class",
			edit: func(j *Job) { j.Spec.Template.Spec.PriorityClassName = "system-node-critical" },
			want: "priorityClassName",
		},
		{
			name: "another restart policy",
			edit: func(j *Job) { j.Spec.Template.Spec.RestartPolicy = "OnFailure" },
			want: "restartPolicy",
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
			problems := strings.Join(updateAdmissionJobProblems(job, admissionValues()), "; ")
			if !strings.Contains(problems, r.want) {
				t.Errorf("%s: problems %q do not name %q", r.name, problems, r.want)
			}
		})
	}
}

// admissionPolicyValidationText is the deployed policy's validation i with its
// whitespace collapsed. The mutation tests read the clauses from the manifest the
// cluster applies, so a gateway pin the Go mirror also enforces is shown in the
// expression itself, not only in the mirror.
func admissionPolicyValidationText(t *testing.T, i int) string {
	t.Helper()
	policy, problems := admissionDocNamed(loadUpdateAdmission(t), "ValidatingAdmissionPolicy")
	if len(problems) > 0 {
		t.Fatalf("%s", strings.Join(problems, "; "))
	}
	if i >= len(policy.Spec.Validations) {
		t.Fatalf("the policy holds %d validations, want one at %d", len(policy.Spec.Validations), i+1)
	}
	return admissionText(policy.Spec.Validations[i].Expression)
}

// TestUpdateAdmissionPolicyRefusesGatewayMutations holds the gateway Job to the
// policy's bounds: the widening admits the gateway pair, the one Secret and the one
// extra mount, and a Job that adds a second Secret, a hostAlias, a stray variable or
// another mount is a Job the renderer never makes. Each case is named by the check
// that mirrors the expressions and by the deployed expression's own pin, so the
// policy text and the mirror cannot drift apart.
func TestUpdateAdmissionPolicyRefusesGatewayMutations(t *testing.T) {
	t.Parallel()
	for _, r := range []struct {
		name       string
		edit       func(*Job)
		want       string
		validation int
		pins       []string
	}{
		{
			name: "a second Secret volume",
			edit: func(j *Job) {
				j.Spec.Template.Spec.Volumes = append(j.Spec.Template.Spec.Volumes,
					Volume{Name: "extra-secret", Secret: &SecretVolumeSource{SecretName: "another"}})
			},
			want:       "volumes",
			validation: 5,
			pins: []string{
				"size(object.spec.template.spec.volumes) == 3",
				"v.secret.secretName == 'bd-gateway-daemon'",
			},
		},
		{
			name: "a second town-state claim and no viewer-token Secret",
			edit: func(j *Job) {
				// Every volume still names one the policy's clause allows, and the
				// count is the gateway render's, so only the named-volume pins
				// refuse this Job: a count and an `all` alone would admit it.
				j.Spec.Template.Spec.Volumes = []Volume{
					{Name: TownStateVolume, PersistentVolumeClaim: &PVCVolumeSource{ClaimName: k8sdaemon.TownStateClaim}},
					{Name: TownStateVolume, PersistentVolumeClaim: &PVCVolumeSource{ClaimName: k8sdaemon.TownStateClaim}},
					k8sdaemon.BdGatewayVolume(k8sdaemon.BdGatewayDaemonSecretName),
				}
			},
			want:       "mounts no",
			validation: 5,
			pins: []string{
				"object.spec.template.spec.volumes.exists(v, v.name == 'viewer-token'",
				"object.spec.template.spec.volumes.exists(v, v.name == 'bd-gateway-token'",
			},
		},
		{
			name: "a stray environment variable",
			edit: func(j *Job) {
				j.Spec.Template.Spec.Containers[0].Env = append(j.Spec.Template.Spec.Containers[0].Env, EnvVar{Name: "EXTRA", Value: "x"})
			},
			want:       "environment",
			validation: 3,
			pins: []string{
				"size(c.env) == 6",
				"e.name == 'BD_GATEWAY_URL'",
				"e.name == 'BD_GATEWAY_TOKEN_FILE'",
			},
		},
		{
			name: "an extra mount",
			edit: func(j *Job) {
				j.Spec.Template.Spec.Containers[0].VolumeMounts = append(j.Spec.Template.Spec.Containers[0].VolumeMounts,
					VolumeMount{Name: TownStateVolume, MountPath: "/tmp"})
			},
			want:       "mounts",
			validation: 3,
			pins: []string{
				"size(c.volumeMounts) == 3",
				"m.name == 'bd-gateway-token'",
				"m.mountPath == '/run/gt/bd-gateway'",
			},
		},
	} {
		t.Run(r.name, func(t *testing.T) {
			t.Parallel()
			job := mustBuild(t, admissionGatewayValues()).Job
			job.Metadata.Name = JobNamePrefix + "0123456789ab"
			r.edit(&job)
			problems := strings.Join(updateAdmissionJobProblems(job, admissionGatewayValues()), "; ")
			if !strings.Contains(problems, r.want) {
				t.Errorf("%s: problems %q do not name %q", r.name, problems, r.want)
			}
			expr := admissionPolicyValidationText(t, r.validation)
			for _, pin := range r.pins {
				if !strings.Contains(expr, pin) {
					t.Errorf("%s: the deployed validation %d does not carry the pin %q: %q", r.name, r.validation+1, pin, expr)
				}
			}
		})
	}
	// hostAliases has no field in the renderer's PodSpec, so it is added to the
	// encoded Job, the path the policy's clause on it is checked through.
	t.Run("a hostAlias", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		if err := YAML(&buf, mustBuild(t, admissionGatewayValues()).Job); err != nil {
			t.Fatalf("encoding the Job: %v", err)
		}
		var doc map[string]any
		if err := yaml.Unmarshal(buf.Bytes(), &doc); err != nil {
			t.Fatalf("decoding the Job: %v", err)
		}
		admissionTestPod(t, doc)["hostAliases"] = []any{
			map[string]any{"ip": "10.0.0.1", "hostnames": []any{"gateway.example.test"}},
		}
		mutated, err := yaml.Marshal(doc)
		if err != nil {
			t.Fatalf("encoding the mutated Job: %v", err)
		}
		problems := strings.Join(admissionEncodedProblems(mutated), "; ")
		if !strings.Contains(problems, "hostAliases") {
			t.Errorf("a hostAlias: problems %q do not name %q", problems, "hostAliases")
		}
		if expr := admissionPolicyValidationText(t, 6); !strings.Contains(expr, "!has(object.spec.template.spec.hostAliases)") {
			t.Errorf("the deployed validation 7 does not refuse hostAliases: %q", expr)
		}
	})
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
// that carried a host namespace, a node placement or runtime class, a pod or
// container securityContext, envFrom, a port, a lifecycle hook or probe, an env
// valueFrom, or another volume kind is named, so a renderer change adding one of
// them fails here and not on the cluster.
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
			name: "a nodeName",
			edit: func(t *testing.T, pod map[string]any) { pod["nodeName"] = "node-1" },
			want: "nodeName",
		},
		{
			name: "a nodeSelector",
			edit: func(t *testing.T, pod map[string]any) {
				pod["nodeSelector"] = map[string]any{"gt.dev/pool": "core"}
			},
			want: "nodeSelector",
		},
		{
			name: "a toleration",
			edit: func(t *testing.T, pod map[string]any) {
				pod["tolerations"] = []any{map[string]any{"key": "gt.dev/update", "operator": "Exists"}}
			},
			want: "tolerations",
		},
		{
			name: "a runtime class",
			edit: func(t *testing.T, pod map[string]any) { pod["runtimeClassName"] = "gvisor" },
			want: "runtimeClassName",
		},
		{
			name: "a pod securityContext",
			edit: func(t *testing.T, pod map[string]any) {
				pod["securityContext"] = map[string]any{"runAsUser": 0}
			},
			want: "securityContext",
		},
		{
			name: "a container port",
			edit: func(t *testing.T, pod map[string]any) {
				admissionTestContainer(t, pod, "containers", 0)["ports"] = []any{
					map[string]any{"containerPort": 22, "hostPort": 22},
				}
			},
			want: "ports",
		},
		{
			name: "a hostPath volume",
			edit: func(t *testing.T, pod map[string]any) {
				volumes, ok := pod["volumes"].([]any)
				if !ok || len(volumes) == 0 {
					t.Fatal("the Job holds no volumes")
				}
				volumes[0] = map[string]any{"name": TownStateVolume, "hostPath": map[string]any{"path": "/"}}
			},
			want: "hostPath",
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
			name: "a container lifecycle",
			edit: func(t *testing.T, pod map[string]any) {
				admissionTestContainer(t, pod, "containers", 0)["lifecycle"] = map[string]any{
					"postStart": map[string]any{"exec": map[string]any{"command": []any{"kubectl"}}},
				}
			},
			want: "lifecycle",
		},
		{
			name: "a startup probe",
			edit: func(t *testing.T, pod map[string]any) {
				admissionTestContainer(t, pod, "initContainers", 0)["startupProbe"] = map[string]any{
					"exec": map[string]any{"command": []any{"kubectl"}},
				}
			},
			want: "startupProbe",
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

// TestAdmissionExpressionsBalanced is the cheap stand-in for compiling the CEL: the
// API server refuses a policy whose expression has an unclosed group, and the
// other checks here compare the YAML to a Go-built copy of the same text, so a
// parenthesis dropped from both goes unseen. Every expression must open and close
// its parentheses and brackets in order, outside string literals.
func TestAdmissionExpressionsBalanced(t *testing.T) {
	t.Parallel()
	var exprs []string
	for _, doc := range loadUpdateAdmission(t) {
		for _, c := range doc.Spec.MatchConditions {
			exprs = append(exprs, c.Expression)
		}
		for _, v := range doc.Spec.Validations {
			exprs = append(exprs, v.Expression)
		}
	}
	if len(exprs) == 0 {
		t.Fatal("the policy holds no expression to check")
	}
	for i, e := range exprs {
		if err := balanced(e); err != nil {
			t.Errorf("expression %d: %v: %.120s", i, err, e)
		}
	}
	for _, bad := range []string{"(a && (b)", "a)", "(a]", "[(a])", `x == "abc`, `x == """abc""`} {
		if balanced(bad) == nil {
			t.Errorf("balanced(%q) accepted an unbalanced expression", bad)
		}
	}
	for _, good := range []string{
		`has(a.b) && x == "(" && (c || [d])`,
		`x == "\"(" && (y)`,
		`x == 'it\'s (' && (y)`,
		`x == "a\\" && (y)`,
		`x == r"\" && (y)`,
		`x == br"\" && (y)`,
		`x == fooR"\"(" && (y)`,
		`x == r"""a"(b""" && (y)`,
		`x == """a"(b""" && (y)`,
		`x == """a\"(""" && (y)`,
	} {
		if err := balanced(good); err != nil {
			t.Errorf("balanced refused a balanced expression %q: %v", good, err)
		}
	}
}

// balanced reports the first unbalanced parenthesis or bracket in a CEL
// expression. String literals are skipped whole: a single- or double-quoted
// literal ends at its first unescaped quote, a triple-quoted literal at the
// matching triple, and an r or R prefix makes the literal raw, so a backslash is
// not an escape. A literal that never closes is an error.
func balanced(expr string) error {
	rs := []rune(expr)
	var stack []rune
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		switch {
		case r == '\'' || r == '"':
			end, err := stringEnd(rs, i)
			if err != nil {
				return err
			}
			i = end
		case r == '(' || r == '[':
			stack = append(stack, r)
		case r == ')' || r == ']':
			want := map[rune]rune{')': '(', ']': '['}[r]
			if len(stack) == 0 || stack[len(stack)-1] != want {
				return fmt.Errorf("unexpected %q", r)
			}
			stack = stack[:len(stack)-1]
		}
	}
	if len(stack) > 0 {
		return fmt.Errorf("%d group(s) never closed", len(stack))
	}
	return nil
}

// stringEnd returns the index of the last rune of the string literal that opens
// at rs[open], or an error when the literal never closes.
func stringEnd(rs []rune, open int) (int, error) {
	quote := rs[open]
	delim := 1
	if open+2 < len(rs) && rs[open+1] == quote && rs[open+2] == quote {
		delim = 3
	}
	raw := rawPrefix(rs, open)
	for j := open + delim; j < len(rs); j++ {
		switch {
		case !raw && rs[j] == '\\':
			j++ // the escaped rune is part of the literal, not its end
		case closesAt(rs, j, quote, delim):
			return j + delim - 1, nil
		}
	}
	return 0, fmt.Errorf("string at offset %d never closes", open)
}

// closesAt reports whether the delimiter of a literal with quote and delim
// characters starts at rs[j].
func closesAt(rs []rune, j int, quote rune, delim int) bool {
	if j+delim > len(rs) {
		return false
	}
	for k := 0; k < delim; k++ {
		if rs[j+k] != quote {
			return false
		}
	}
	return true
}

// rawPrefix reports whether the literal at rs[open] carries an r or R prefix,
// alone or with b or B. The prefix must start a token, so bar"x" is not raw.
func rawPrefix(rs []rune, open int) bool {
	start := open
	for start > 0 && open-start < 2 && strings.ContainsRune("rRbB", rs[start-1]) {
		start--
	}
	if start == open {
		return false
	}
	if start > 0 && isIdentRune(rs[start-1]) {
		return false
	}
	return strings.ContainsAny(string(rs[start:open]), "rR")
}

// isIdentRune reports whether r can continue a CEL identifier.
func isIdentRune(r rune) bool {
	return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r)
}

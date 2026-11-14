package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	k8sdaemon "github.com/steveyegge/gastown/deploy/k8s/daemon"
	"github.com/steveyegge/gastown/internal/imagevalues"
	"github.com/steveyegge/gastown/internal/update"
)

// dnsLabelPattern is what a Kubernetes namespace or Deployment name must be; a
// value that reaches the kubectl argv is checked against it first.
var dnsLabelPattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// maxDNSLabelLength is the longest DNS label a namespace or Deployment name may be.
const maxDNSLabelLength = 63

// isDNSLabel reports whether name is a DNS label within the length limit.
func isDNSLabel(name string) bool {
	return len(name) <= maxDNSLabelLength && dnsLabelPattern.MatchString(name)
}

// podDaemonSwap puts a digest install's pair in force on a daemon that runs as a
// pod. Apply moves two Deployments to the pair: the daemon's, whose container
// takes the daemon image and whose pod template takes the polecat image
// annotation, and the dashboard's, whose container takes the daemon image. The
// dashboard is optional, so Apply asks the cluster whether its Deployment exists
// and moves it only when it does. It patches what it moves, then waits for each
// rollout, so the pair moves as one step and the caller's wait, restore and
// escalation work as they do for the container daemon. RunningPair reads the
// pair the Deployment runs now, which an install records before it swaps: the
// update Job's init container rewrites the values file with the new pair first.
// Apply refuses a pair whose daemon or polecat is not a digest reference, and a
// swap missing its context, namespace or rollout timeout, before any command
// runs. A failed step is named by the step and its exit, never by the command's
// output, which can hold a secret.
type podDaemonSwap struct {
	// Context is the kubectl context the Deployment runs in. Required unless
	// InCluster, and refused with it.
	Context string
	// InCluster runs kubectl with no --context, in the cluster the pod is in,
	// through the pod's service account.
	InCluster bool
	// Namespace is the namespace the Deployment runs in. Required.
	Namespace string
	// Deployment is the daemon's Deployment, whose daemon container takes the
	// image. Empty means the daemon's own Deployment name, k8sdaemon.Name. The
	// dashboard's Deployment is always k8sdaemon.DashboardName.
	Deployment string
	// Run runs kubectl. Every command Apply makes goes through it.
	Run update.RunFunc
	// RolloutTimeout bounds the wait for the rollout. Required; zero is refused.
	RolloutTimeout time.Duration
	// Out receives the one line Apply prints when the cluster has no dashboard
	// Deployment and the dashboard is not moved. Nil discards it.
	Out io.Writer
	// marshal encodes the patch. Nil means json.Marshal; a test replaces it, since
	// the patch holds only strings and no value of it fails to encode.
	marshal func(any) ([]byte, error)
}

var _ update.ImageSwap = podDaemonSwap{}
var _ update.ImageRunningPair = podDaemonSwap{}

// dashboardAbsentLine is what Apply prints when the cluster has no dashboard
// Deployment, so the operator can see the dashboard was not moved.
const dashboardAbsentLine = "dashboard Deployment not found; not moved"

// runningPairJSONPath reads the pair the daemon's Deployment runs: the daemon
// container's image, then the pod template's polecat image annotation, on two
// lines. The annotation key's dot is escaped, as jsonpath requires. Both halves
// are read in one command, so a pair is one Deployment read.
var runningPairJSONPath = `{.spec.template.spec.containers[?(@.name=="` + k8sdaemon.Name + `")].image}{"\n"}{.spec.template.metadata.annotations.` +
	strings.ReplaceAll(k8sdaemon.PolecatImageAnnotation, ".", `\.`) + `}`

// RunningPair reads the pair the daemon's Deployment runs now: its daemon
// container's image and its pod template's polecat image annotation. An install
// records the result before it swaps, so a failed install puts back the pair the
// cluster ran. The pair names no commit, since a Deployment carries no build.
// A command that fails, prints no pair, or names a half that is not a digest
// reference is an error naming the step; the command's output is not.
// A Deployment with no polecat annotation is refused on purpose: the update flow
// is pair-based, and a roll without the polecat half would stop dispatch, so an
// unannotated daemon is not a supported install target.
func (s podDaemonSwap) RunningPair(ctx context.Context) (update.ImagePair, error) {
	if err := s.validateTarget(); err != nil {
		return update.ImagePair{}, err
	}
	const step = "the running pair read"
	out, err := s.kubectlOutput(ctx, step, "get", "deployment/"+s.deployment(), "-o", "jsonpath="+runningPairJSONPath)
	if err != nil {
		return update.ImagePair{}, err
	}
	daemon, polecat, found := strings.Cut(out, "\n")
	if !found {
		return update.ImagePair{}, fmt.Errorf("%s printed no pair", step)
	}
	pair := update.ImagePair{Daemon: strings.TrimSpace(daemon), Polecat: strings.TrimSpace(polecat)}
	for _, half := range []struct{ what, ref string }{
		{"daemon", pair.Daemon},
		{"polecat", pair.Polecat},
	} {
		if !imagevalues.ValidRef(half.ref) {
			return update.ImagePair{}, fmt.Errorf("%s found %s image %q, which is not a digest reference, so the pair cannot be restored to it", step, half.what, half.ref)
		}
	}
	return pair, nil
}

// Apply patches the daemon's Deployment, then the dashboard's when the cluster has
// one, with the pair, and then waits for each rollout. Both bodies are encoded
// before any command runs, and a failed step stops the swap at that step. The
// caller restores the previous pair through this same Apply, which asks the same
// question and moves whichever Deployments exist.
func (s podDaemonSwap) Apply(ctx context.Context, pair update.ImagePair) error {
	if err := s.validate(pair); err != nil {
		return err
	}
	daemonPatch, err := s.daemonPatch(pair)
	if err != nil {
		return fmt.Errorf("encoding the daemon's pod patch: %w", err)
	}
	dashboardPatch, err := s.dashboardPatch(pair)
	if err != nil {
		return fmt.Errorf("encoding the dashboard's pod patch: %w", err)
	}
	present, err := s.dashboardPresent(ctx)
	if err != nil {
		return err
	}
	daemon, dashboard := s.deployment(), k8sdaemon.DashboardName
	timeout := "--timeout=" + s.RolloutTimeout.String()
	if err := s.kubectl(ctx, "the daemon's patch", "patch", "deployment/"+daemon, "--type=strategic", "-p", daemonPatch); err != nil {
		return err
	}
	if present {
		if err := s.kubectl(ctx, "the dashboard's patch", "patch", "deployment/"+dashboard, "--type=strategic", "-p", dashboardPatch); err != nil {
			return err
		}
	}
	if err := s.kubectl(ctx, "the daemon's rollout status", "rollout", "status", "deployment/"+daemon, timeout); err != nil {
		return err
	}
	if present {
		if err := s.kubectl(ctx, "the dashboard's rollout status", "rollout", "status", "deployment/"+dashboard, timeout); err != nil {
			return err
		}
	} else {
		fmt.Fprintln(s.out(), dashboardAbsentLine)
	}
	return nil
}

// dashboardPresent asks the cluster whether the dashboard's Deployment exists. The
// renderer creates it only for a town with a dashboard, and the cluster is the fact,
// so Apply does not decide from the values. An empty answer is absent, the dashboard's
// own name is present, and any other answer, a nonzero exit or a runner error is an
// error: an unreadable cluster never reads as a dashboard that is not there.
func (s podDaemonSwap) dashboardPresent(ctx context.Context) (bool, error) {
	const step = "the dashboard's existence read"
	out, err := s.kubectlOutput(ctx, step, "get", "deployment/"+k8sdaemon.DashboardName, "-o", "name", "--ignore-not-found")
	if err != nil {
		return false, err
	}
	switch strings.TrimSpace(out) {
	case "":
		return false, nil
	case "deployment.apps/" + k8sdaemon.DashboardName, "deployment/" + k8sdaemon.DashboardName:
		return true, nil
	default:
		return false, fmt.Errorf("%s printed a name other than the dashboard's", step)
	}
}

// out is the writer Apply's line goes to: Out when set, else nowhere.
func (s podDaemonSwap) out() io.Writer {
	if s.Out == nil {
		return io.Discard
	}
	return s.Out
}

// daemonPatch is the strategic-merge patch that puts pair in force on the daemon's
// Deployment: the daemon container's image and the pod template's polecat image
// annotation, which the rendered pod projects into its polecat image file.
func (s podDaemonSwap) daemonPatch(pair update.ImagePair) (string, error) {
	return s.encode(podTemplatePatch(map[string]string{k8sdaemon.PolecatImageAnnotation: pair.Polecat}, k8sdaemon.Name, pair.Daemon))
}

// dashboardPatch is the strategic-merge patch that puts the daemon image of pair on
// the dashboard's container. It sets no annotation, so the pod template's own
// annotations, such as a hand restart's, are kept.
func (s podDaemonSwap) dashboardPatch(pair update.ImagePair) (string, error) {
	return s.encode(podTemplatePatch(nil, k8sdaemon.DashboardName, pair.Daemon))
}

// podTemplatePatch is the body that sets container's image, by name, in a
// Deployment's pod template, and the template's annotations when given. A nil map
// leaves the annotations out, since a null annotation would remove them.
func podTemplatePatch(annotations map[string]string, container, image string) map[string]any {
	template := map[string]any{
		"spec": map[string]any{
			"containers": []map[string]string{{"name": container, "image": image}},
		},
	}
	if annotations != nil {
		template["metadata"] = map[string]any{"annotations": annotations}
	}
	return map[string]any{"spec": map[string]any{"template": template}}
}

// encode is the JSON of a patch body. Map keys encode in sorted order, so the same
// body is the same bytes.
func (s podDaemonSwap) encode(body map[string]any) (string, error) {
	marshal := s.marshal
	if marshal == nil {
		marshal = json.Marshal
	}
	data, err := marshal(body)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// deployment is the Deployment Apply names: the field when set, else the
// daemon's own.
func (s podDaemonSwap) deployment() string {
	if s.Deployment == "" {
		return k8sdaemon.Name
	}
	return s.Deployment
}

// validate refuses a swap whose inputs cannot reach kubectl as intended, before
// any command runs.
func (s podDaemonSwap) validate(pair update.ImagePair) error {
	if err := s.validateTarget(); err != nil {
		return err
	}
	if s.RolloutTimeout <= 0 {
		return errors.New("the pod daemon swap has no rollout timeout")
	}
	if err := checkPodDigest("daemon", pair.Daemon); err != nil {
		return err
	}
	return checkPodDigest("polecat", pair.Polecat)
}

// validateTarget refuses a runner or a Deployment the swap cannot name in a
// kubectl command, before any command runs.
func (s podDaemonSwap) validateTarget() error {
	if s.Run == nil {
		return errors.New("the pod daemon swap has no command runner wired")
	}
	if err := checkPodTarget(s.Context, s.InCluster); err != nil {
		return err
	}
	if err := checkPodNamespace(s.Namespace); err != nil {
		return err
	}
	if !isDNSLabel(s.deployment()) {
		return fmt.Errorf("the pod daemon swap deployment %q is not a Kubernetes name", s.deployment())
	}
	return nil
}

// checkPodContext refuses a kubectl context that is empty or would read as a flag.
func checkPodContext(kubeContext string) error {
	if kubeContext == "" || strings.HasPrefix(kubeContext, "-") {
		return fmt.Errorf("the pod daemon swap names no usable kubectl context %q", kubeContext)
	}
	return nil
}

// checkPodTarget refuses a pod target that names both a kubectl context and the
// in-cluster service account, or neither. Without InCluster the context is
// checked as checkPodContext checks it.
func checkPodTarget(kubeContext string, inCluster bool) error {
	if inCluster {
		if kubeContext != "" {
			return errors.New("the pod daemon swap names both a kubectl context and the in-cluster service account")
		}
		return nil
	}
	return checkPodContext(kubeContext)
}

// podKubectlPrefix is the argv every kubectl command against a pod starts with:
// the context when the target names one, then the namespace. With InCluster the
// context is omitted, so kubectl uses the pod's service account.
func podKubectlPrefix(kubeContext, namespace string, inCluster bool) []string {
	if inCluster {
		return []string{"-n", namespace}
	}
	return []string{"--context", kubeContext, "-n", namespace}
}

// checkPodNamespace refuses a namespace that is not a Kubernetes namespace name.
func checkPodNamespace(namespace string) error {
	if !isDNSLabel(namespace) {
		return fmt.Errorf("the pod daemon swap namespace %q is not a Kubernetes namespace name", namespace)
	}
	return nil
}

// checkPodDigest refuses an image reference that is not a digest reference, since
// a tag names whatever the registry holds at pull time. The what argument,
// "daemon" or "polecat", names the image in the error.
func checkPodDigest(what, ref string) error {
	_, digest, found := strings.Cut(ref, "@")
	if !found || !imagevalues.ValidDigest(digest) {
		return fmt.Errorf("the %s image %q is not a digest reference, so the pod cannot roll out to it", what, ref)
	}
	return nil
}

// kubectl runs one kubectl step against the swap's context and namespace, or in
// the cluster when the swap is InCluster. step names the step, such as "the
// daemon's patch". A nonzero exit or a Run error is named by step; the command's
// output is not.
func (s podDaemonSwap) kubectl(ctx context.Context, step string, args ...string) error {
	_, err := s.kubectlOutput(ctx, step, args...)
	return err
}

// kubectlOutput is kubectl for a step that reads the command's output. The output
// is returned to the caller and is never put in an error.
func (s podDaemonSwap) kubectlOutput(ctx context.Context, step string, args ...string) (string, error) {
	argv := append(podKubectlPrefix(s.Context, s.Namespace, s.InCluster), args...)
	out, exit, err := s.Run(ctx, "", nil, "kubectl", argv...)
	if err != nil {
		return "", fmt.Errorf("running kubectl for %s: %w", step, err)
	}
	if exit != 0 {
		return "", fmt.Errorf("kubectl for %s exited %d", step, exit)
	}
	return out, nil
}

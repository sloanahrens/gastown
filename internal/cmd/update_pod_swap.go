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
// pod. Apply moves up to three Deployments to the pair: the bd gateway's, whose
// container takes the gateway image, then the daemon's, whose container takes the
// daemon image and whose pod template takes the polecat image annotation, and the
// dashboard's, whose container takes the daemon image. The gateway moves first
// and the daemon does not move until the gateway's rollout is complete and its
// pod Ready (its readiness probe is bdgw ready), since the compatibility rule
// lets a gateway change run with the previous daemon for one roll. The gateway
// and the dashboard are optional, so Apply asks the cluster whether each
// Deployment exists and moves it only when it does and the pair names its image.
// It patches what it moves, then waits for each rollout, so the pair moves as one
// step and the caller's wait, restore and escalation work as they do for the
// container daemon. RunningPair reads the pair the Deployment runs now, which an
// install records before it swaps: the update Job's init container rewrites the
// values file with the new pair first. Apply refuses a pair whose daemon, polecat
// or bd gateway is not a digest reference, and a swap missing its context,
// namespace or rollout timeout, before any command runs. A failed step is named
// by the step and its exit, never by the command's output, which can hold a
// secret; a gateway step's failure is returned as update.GatewayStepError, so the
// install names the gateway rather than the daemon rollout that never started.
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

// gatewayRolloutTimeout bounds the wait for the bd gateway's rollout before the
// daemon is patched. The gateway must be new and Ready before the daemon that
// needs it moves, so a gateway that never comes Ready fails the roll and restores
// the previous images while the daemon is still on its old pair; the bound is its
// own constant rather than the daemon's restart bound for that reason.
const gatewayRolloutTimeout = 3 * time.Minute

// runningPairJSONPath reads the pair the daemon's Deployment runs: the daemon
// container's image, then the pod template's polecat image annotation, on two
// lines. The annotation key's dot is escaped, as jsonpath requires. Both halves
// are read in one command, so a pair is one Deployment read.
var runningPairJSONPath = `{.spec.template.spec.containers[?(@.name=="` + k8sdaemon.Name + `")].image}{"\n"}{.spec.template.metadata.annotations.` +
	strings.ReplaceAll(k8sdaemon.PolecatImageAnnotation, ".", `\.`) + `}`

// gatewayImageJSONPath reads the bd gateway Deployment's name and the image its
// container runs, on two lines. The name line is what tells a missing Deployment
// (kubectl prints nothing for --ignore-not-found) from one whose gateway
// container names no image, which is an error, never an absent gateway.
var gatewayImageJSONPath = `{.metadata.name}{"\n"}{.spec.template.spec.containers[?(@.name=="` + k8sdaemon.BdGatewayName + `")].image}`

// RunningPair reads the pair the daemon runs now: its Deployment's daemon
// container image and polecat image annotation, and the bd gateway image the
// cluster runs, which is empty until the town has a bd-gateway Deployment. An
// install records the result before it swaps, so a failed install puts back the
// pair the cluster ran. The pair names no commit, since a Deployment carries no
// build. A command that fails, prints no pair, or names a half that is not a
// digest reference is an error naming the step; the command's output is not.
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
	if pair.Bd, err = s.gatewayImage(ctx); err != nil {
		return update.ImagePair{}, err
	}
	return pair, nil
}

// gatewayImage reads the bd gateway image the cluster runs, empty when the
// cluster has no bd-gateway Deployment yet (the town before the cutover). A
// Deployment that exists but names no digest image is an error: a gateway the
// cluster runs names the image the pair compares and can restore.
func (s podDaemonSwap) gatewayImage(ctx context.Context) (string, error) {
	const step = "the bd gateway image read"
	out, err := s.kubectlOutput(ctx, step, "get", "deployment/"+k8sdaemon.BdGatewayName, "-o", "jsonpath="+gatewayImageJSONPath, "--ignore-not-found")
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(out) == "" {
		return "", nil
	}
	name, image, found := strings.Cut(out, "\n")
	if !found || strings.TrimSpace(name) != k8sdaemon.BdGatewayName {
		return "", fmt.Errorf("%s printed no %s image", step, k8sdaemon.BdGatewayName)
	}
	image = strings.TrimSpace(image)
	if !imagevalues.ValidRef(image) {
		return "", fmt.Errorf("%s found bd gateway image %q, which is not a digest reference, so the pair cannot be restored to it", step, image)
	}
	return image, nil
}

// Apply patches the bd gateway's Deployment when the cluster has one and the pair
// names its image, waits for that rollout, then patches the daemon's Deployment
// and the dashboard's when the cluster has one, and waits for each of those. All
// the bodies are encoded, and the cluster is asked which Deployments exist, before
// any patch runs, and a failed step stops the swap at that step. The caller
// restores the previous pair through this same Apply, which asks the same
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
	gatewayPatch := ""
	if pair.Bd != "" {
		if gatewayPatch, err = s.gatewayPatch(pair); err != nil {
			return fmt.Errorf("encoding the bd gateway's pod patch: %w", err)
		}
	}
	present, err := s.dashboardPresent(ctx)
	if err != nil {
		return err
	}
	gatewayPresent, err := s.gatewayPresent(ctx)
	if err != nil {
		return err
	}
	movesGateway := gatewayPresent && pair.Bd != ""
	if movesGateway {
		// The gateway is new before the daemon that needs it: the daemon's patch
		// waits on this rollout, whose completion requires the bdgw ready probe.
		if err := s.kubectl(ctx, "the bd gateway's patch", "patch", "deployment/"+k8sdaemon.BdGatewayName, "--type=strategic", "-p", gatewayPatch); err != nil {
			return update.GatewayStepError{Err: err}
		}
		timeout := "--timeout=" + gatewayRolloutTimeout.String()
		if err := s.kubectl(ctx, "the bd gateway's rollout status", "rollout", "status", "deployment/"+k8sdaemon.BdGatewayName, timeout); err != nil {
			return update.GatewayStepError{Err: err}
		}
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

// gatewayPresent asks the cluster whether the bd gateway's Deployment exists: the
// town before the cutover has none, and the renderer creates it only for a town
// with a gateway. An empty answer is absent, the gateway's own name is present,
// and any other answer, a nonzero exit or a runner error is an error, so an
// unreadable cluster never reads as a gateway that is not there.
func (s podDaemonSwap) gatewayPresent(ctx context.Context) (bool, error) {
	const step = "the bd gateway's existence read"
	out, err := s.kubectlOutput(ctx, step, "get", "deployment/"+k8sdaemon.BdGatewayName, "-o", "name", "--ignore-not-found")
	if err != nil {
		return false, err
	}
	switch strings.TrimSpace(out) {
	case "":
		return false, nil
	case "deployment.apps/" + k8sdaemon.BdGatewayName, "deployment/" + k8sdaemon.BdGatewayName:
		return true, nil
	default:
		return false, fmt.Errorf("%s printed a name other than the bd gateway's", step)
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

// gatewayPatch is the strategic-merge patch that puts the bd gateway image of pair
// on the gateway's container. It sets no annotation, as the dashboard's does not:
// a gateway roll keeps the annotations the pod template already carries.
func (s podDaemonSwap) gatewayPatch(pair update.ImagePair) (string, error) {
	return s.encode(podTemplatePatch(nil, k8sdaemon.BdGatewayName, pair.Bd))
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
	if err := checkPodDigest("polecat", pair.Polecat); err != nil {
		return err
	}
	if pair.Bd == "" {
		return nil
	}
	return checkPodDigest("bd gateway", pair.Bd)
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

// podCrashNotReadyAfter is how long a daemon pod may stay not Ready after it was
// created before the crash probe calls it crashing: a pod the Deployment started
// minutes ago that never became Ready cannot report the new build.
const podCrashNotReadyAfter = 3 * time.Minute

// podCrashRestartLimit is the restart count at which one container is reported
// crashing. A container that has restarted twice is dying even when the poll
// that reads it catches it between deaths.
const podCrashRestartLimit = 2

// podDaemonCrashing reads the daemon Deployment's newest pod through kubectl and
// reports whether it is crashing and, when it is, why. A container waiting with
// reason CrashLoopBackOff, a container restarted podCrashRestartLimit times or
// more, or a pod still not Ready podCrashNotReadyAfter its creation all read as
// crashing; why names the container and the reason. A read that fails or cannot
// be judged is an error, never a healthy answer: an unreadable pod list says
// nothing about the pod.
//
// The list is the daemon Deployment's own selector, podDaemonSelector, so it is
// the pods that Deployment rolled. A pod the cluster is deleting is skipped: it
// is not the pod that must come up. now stamps the pod's age.
func podDaemonCrashing(ctx context.Context, run update.RunFunc, target podDaemonTarget, now time.Time) (bool, string, error) {
	if run == nil {
		return false, "", errors.New("pod daemon crash probe: no command runner is wired")
	}
	target, err := podCrashTarget(target)
	if err != nil {
		return false, "", err
	}
	ctx, cancel := context.WithTimeout(ctx, podProbeBound)
	defer cancel()
	argv := append(podKubectlPrefix(target.Context, target.Namespace, target.InCluster), "get", "pods", "-l", podDaemonSelector, "-o", "json")
	out, exit, err := run(ctx, "", nil, "kubectl", argv...)
	if err != nil {
		return false, "", fmt.Errorf("pod daemon crash probe: running the daemon pod list: %w", err)
	}
	if exit != 0 {
		// The output is left out of the error: a pod's output can hold a secret.
		return false, "", fmt.Errorf("pod daemon crash probe: the daemon pod list exited %d", exit)
	}
	return judgeDaemonPod(out, now)
}

// podCrashTarget is the probe's target with surrounding space trimmed, or the
// refusal for a value kubectl could not be given as written.
func podCrashTarget(target podDaemonTarget) (podDaemonTarget, error) {
	target.Context = strings.TrimSpace(target.Context)
	target.Namespace = strings.TrimSpace(target.Namespace)
	if target.InCluster {
		if target.Context != "" {
			return podDaemonTarget{}, errors.New("pod daemon crash probe: names both a kube context and the in-cluster service account")
		}
	} else if target.Context == "" || strings.HasPrefix(target.Context, "-") {
		return podDaemonTarget{}, fmt.Errorf("pod daemon crash probe: the kube context %q is not a usable kubectl context", target.Context)
	}
	if !isDNSLabel(target.Namespace) {
		return podDaemonTarget{}, fmt.Errorf("pod daemon crash probe: the namespace %q is not a Kubernetes namespace name", target.Namespace)
	}
	return target, nil
}

// podCrashListing is the part of `kubectl get pods -o json` the crash probe
// reads. It is its own shape rather than the pair check's podListing because it
// judges a different part of a pod: when it was created, whether it is Ready,
// and each container's restart count and waiting state.
type podCrashListing struct {
	Items []podCrashItem `json:"items"`
}

// podCrashItem is one pod as the crash probe reads it.
type podCrashItem struct {
	Metadata struct {
		Name              string  `json:"name"`
		CreationTimestamp string  `json:"creationTimestamp"`
		DeletionTimestamp *string `json:"deletionTimestamp"`
	} `json:"metadata"`
	Status struct {
		Conditions        []podCrashCondition       `json:"conditions"`
		ContainerStatuses []podCrashContainerStatus `json:"containerStatuses"`
	} `json:"status"`
}

// podCrashCondition is one pod condition, as the pod reports it.
type podCrashCondition struct {
	Type   string `json:"type"`
	Status string `json:"status"`
}

// podCrashContainerStatus is one container as the crash probe reads it: whether
// it reports itself ready, how often it has restarted, and the state it waits
// in when it is not running.
type podCrashContainerStatus struct {
	Name         string `json:"name"`
	Ready        bool   `json:"ready"`
	RestartCount int    `json:"restartCount"`
	State        struct {
		Waiting *podCrashWaiting `json:"waiting"`
	} `json:"state"`
}

// podCrashWaiting is a container's waiting state and its reason.
type podCrashWaiting struct {
	Reason string `json:"reason"`
}

// judgeDaemonPod judges the newest live pod in the list kubectl printed: the
// CrashLoopBackOff wait first, then the restart count, then the pod that stayed
// not Ready past podCrashNotReadyAfter its creation. The checks are answered in
// that order, so a pod that both waits in CrashLoopBackOff and has restarted
// reports the reason it is waiting in.
func judgeDaemonPod(out string, now time.Time) (bool, string, error) {
	var list podCrashListing
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		return false, "", errors.New("pod daemon crash probe: the daemon pod list is not JSON")
	}
	pod, created, err := newestDaemonPod(list.Items)
	if err != nil {
		return false, "", err
	}
	for _, status := range pod.Status.ContainerStatuses {
		if waiting := status.State.Waiting; waiting != nil && waiting.Reason == "CrashLoopBackOff" {
			return true, status.Name + ": CrashLoopBackOff", nil
		}
	}
	for _, status := range pod.Status.ContainerStatuses {
		if status.RestartCount >= podCrashRestartLimit {
			return true, fmt.Sprintf("%s: restarted %d times", status.Name, status.RestartCount), nil
		}
	}
	if podCrashReady(pod) {
		return false, "", nil
	}
	age := now.Sub(created)
	if age < podCrashNotReadyAfter {
		return false, "", nil
	}
	return true, fmt.Sprintf("%s: not Ready %s after creation", podCrashContainerName(pod), age.Round(time.Second)), nil
}

// newestDaemonPod is the newest pod in items that the cluster is not deleting,
// and when it was created. The newest pod is the one the Deployment rolled, so a
// reading from an old pod a rollout left behind cannot end a wait for the new
// one. A list with no live pod, or a pod whose creation time does not parse, is
// an error: neither says anything about the pod that must come up.
func newestDaemonPod(items []podCrashItem) (podCrashItem, time.Time, error) {
	var newest podCrashItem
	var newestAt time.Time
	found := false
	for _, pod := range items {
		if pod.Metadata.DeletionTimestamp != nil {
			continue
		}
		created, err := time.Parse(time.RFC3339, pod.Metadata.CreationTimestamp)
		if err != nil {
			return podCrashItem{}, time.Time{}, fmt.Errorf("pod daemon crash probe: the daemon pod %s reports no readable creation time", pod.Metadata.Name)
		}
		if found && !created.After(newestAt) {
			continue
		}
		newest, newestAt, found = pod, created, true
	}
	if !found {
		return podCrashItem{}, time.Time{}, errors.New("pod daemon crash probe: the daemon pod list names no pod to judge")
	}
	return newest, newestAt, nil
}

// podCrashReady reports whether the pod's Ready condition says True. A pod with
// no Ready condition yet is not Ready.
func podCrashReady(pod podCrashItem) bool {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == "Ready" {
			return condition.Status == "True"
		}
	}
	return false
}

// podCrashContainerName names the container the not-Ready reason is about: the
// first container that does not report itself ready, else the first container
// the pod reports, else the pod itself, which reports no container.
func podCrashContainerName(pod podCrashItem) string {
	for _, status := range pod.Status.ContainerStatuses {
		if !status.Ready {
			return status.Name
		}
	}
	if len(pod.Status.ContainerStatuses) > 0 {
		return pod.Status.ContainerStatuses[0].Name
	}
	return "pod " + pod.Metadata.Name
}

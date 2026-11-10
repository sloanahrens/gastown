package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
// pod. Apply sends one strategic-merge patch to the Deployment: the daemon
// container's image and the pod template's polecat image annotation, so the pair
// moves as one rollout. It then waits for the rollout, so the caller's wait,
// restore and escalation work as they do for the container daemon. Apply refuses
// a pair whose daemon or polecat is not a digest reference, and a swap missing
// its context, namespace or rollout timeout, before any command runs. A failed
// step is named by the step and its exit, never by the command's output, which
// can hold a secret.
type podDaemonSwap struct {
	// Context is the kubectl context the Deployment runs in. Required unless
	// InCluster, and refused with it.
	Context string
	// InCluster runs kubectl with no --context, in the cluster the pod is in,
	// through the pod's service account.
	InCluster bool
	// Namespace is the namespace the Deployment runs in. Required.
	Namespace string
	// Deployment is the Deployment whose daemon container takes the image. Empty
	// means the daemon's own Deployment name, k8sdaemon.Name.
	Deployment string
	// Run runs kubectl. Every command Apply makes goes through it.
	Run update.RunFunc
	// RolloutTimeout bounds the wait for the rollout. Required; zero is refused.
	RolloutTimeout time.Duration
	// marshal encodes the patch. Nil means json.Marshal; a test replaces it, since
	// the patch holds only strings and no value of it fails to encode.
	marshal func(any) ([]byte, error)
}

var _ update.ImageSwap = podDaemonSwap{}

// Apply patches the Deployment with the pair, then waits for the rollout. The
// patch and the wait run in that order, and a failure stops the swap at the step
// that failed.
func (s podDaemonSwap) Apply(ctx context.Context, pair update.ImagePair) error {
	if err := s.validate(pair); err != nil {
		return err
	}
	patch, err := s.patch(pair)
	if err != nil {
		return fmt.Errorf("encoding the daemon's pod patch: %w", err)
	}
	deployment := s.deployment()
	if err := s.kubectl(ctx, "patch", "patch", "deployment/"+deployment, "--type=strategic", "-p", patch); err != nil {
		return err
	}
	return s.kubectl(ctx, "rollout status", "rollout", "status", "deployment/"+deployment, "--timeout="+s.RolloutTimeout.String())
}

// patch is the strategic-merge patch that puts pair in force: the daemon
// container's image and the pod template's polecat image annotation, which the
// rendered pod projects into its polecat image file. The container is matched by
// its name, the daemon's own. Map keys encode in sorted order, so the patch is the
// same bytes for the same pair.
func (s podDaemonSwap) patch(pair update.ImagePair) (string, error) {
	marshal := s.marshal
	if marshal == nil {
		marshal = json.Marshal
	}
	body := map[string]any{
		"spec": map[string]any{
			"template": map[string]any{
				"metadata": map[string]any{
					"annotations": map[string]string{k8sdaemon.PolecatImageAnnotation: pair.Polecat},
				},
				"spec": map[string]any{
					"containers": []map[string]string{{"name": k8sdaemon.Name, "image": pair.Daemon}},
				},
			},
		},
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
	if s.RolloutTimeout <= 0 {
		return errors.New("the pod daemon swap has no rollout timeout")
	}
	if err := checkPodDigest("daemon", pair.Daemon); err != nil {
		return err
	}
	return checkPodDigest("polecat", pair.Polecat)
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
// the cluster when the swap is InCluster. A nonzero exit or a Run error is named
// by step; the command's output is not.
func (s podDaemonSwap) kubectl(ctx context.Context, step string, args ...string) error {
	argv := append(podKubectlPrefix(s.Context, s.Namespace, s.InCluster), args...)
	_, exit, err := s.Run(ctx, "", nil, "kubectl", argv...)
	if err != nil {
		return fmt.Errorf("running kubectl for the daemon's %s: %w", step, err)
	}
	if exit != 0 {
		return fmt.Errorf("kubectl for the daemon's %s exited %d", step, exit)
	}
	return nil
}

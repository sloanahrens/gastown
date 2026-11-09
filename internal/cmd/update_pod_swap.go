package cmd

import (
	"context"
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

// podDaemonSwap puts a digest install's pair in force on a daemon that runs as a
// pod. Apply points the Deployment's daemon container at the pair's daemon image
// and waits for the rollout, so the caller's wait, restore and escalation work as
// they do for the container daemon. It moves the daemon half only: the pair's
// polecat image is not touched here. Apply refuses a pair whose daemon is not a
// digest reference, and a swap missing its context, namespace or rollout timeout,
// before any command runs. A failed step is named by the step and its exit, never
// by the command's output, which can hold a secret.
type podDaemonSwap struct {
	// Context is the kubectl context the Deployment runs in. Required.
	Context string
	// Namespace is the namespace the Deployment runs in. Required.
	Namespace string
	// Deployment is the Deployment whose daemon container takes the image. Empty
	// means the daemon's own Deployment name, k8sdaemon.Name.
	Deployment string
	// Run runs kubectl. Every command Apply makes goes through it.
	Run update.RunFunc
	// RolloutTimeout bounds the wait for the rollout. Required; zero is refused.
	RolloutTimeout time.Duration
}

var _ update.ImageSwap = podDaemonSwap{}

// Apply sets the daemon image on the Deployment, then waits for the rollout. The
// two commands run in that order, and a failure stops the swap at the step that
// failed.
func (s podDaemonSwap) Apply(ctx context.Context, pair update.ImagePair) error {
	if err := s.validate(pair); err != nil {
		return err
	}
	deployment := s.deployment()
	if err := s.kubectl(ctx, "set image", "set", "image", "deployment/"+deployment, k8sdaemon.Name+"="+pair.Daemon); err != nil {
		return err
	}
	return s.kubectl(ctx, "rollout status", "rollout", "status", "deployment/"+deployment, "--timeout="+s.RolloutTimeout.String())
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
	if s.Context == "" || strings.HasPrefix(s.Context, "-") {
		return fmt.Errorf("the pod daemon swap names no usable kubectl context %q", s.Context)
	}
	if !dnsLabelPattern.MatchString(s.Namespace) {
		return fmt.Errorf("the pod daemon swap namespace %q is not a Kubernetes namespace name", s.Namespace)
	}
	if !dnsLabelPattern.MatchString(s.deployment()) {
		return fmt.Errorf("the pod daemon swap deployment %q is not a Kubernetes name", s.deployment())
	}
	if s.RolloutTimeout <= 0 {
		return errors.New("the pod daemon swap has no rollout timeout")
	}
	_, digest, found := strings.Cut(pair.Daemon, "@")
	if !found || !imagevalues.ValidDigest(digest) {
		return fmt.Errorf("the daemon image %q is not a digest reference, so the pod cannot roll out to it", pair.Daemon)
	}
	return nil
}

// kubectl runs one kubectl step against the swap's context and namespace. A
// nonzero exit or a Run error is named by step; the command's output is not.
func (s podDaemonSwap) kubectl(ctx context.Context, step string, args ...string) error {
	argv := append([]string{"--context", s.Context, "-n", s.Namespace}, args...)
	_, exit, err := s.Run(ctx, "", nil, "kubectl", argv...)
	if err != nil {
		return fmt.Errorf("running kubectl for the daemon's %s: %w", step, err)
	}
	if exit != 0 {
		return fmt.Errorf("kubectl for the daemon's %s exited %d", step, exit)
	}
	return nil
}

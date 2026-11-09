package cmd

import (
	"context"
	"fmt"
	"strings"

	"github.com/steveyegge/gastown/internal/update"
)

// podDaemonCommit is the commit of the build the daemon running in a pod
// reports. It asks the pod for `gt version`, so the cycle judges the build the
// pod runs and not the daemon state file in the town tree, which a pod does
// not write. The command runs only through run; a refused argument runs nothing.
func podDaemonCommit(ctx context.Context, run update.RunFunc, kubeContext, namespace, deployment string) (string, error) {
	if err := podArgument("kube context", kubeContext); err != nil {
		return "", err
	}
	if err := podArgument("namespace", namespace); err != nil {
		return "", err
	}
	if err := podArgument("deployment", deployment); err != nil {
		return "", err
	}
	out, exit, err := run(ctx, "", nil, "kubectl", "--context", kubeContext, "-n", namespace,
		"exec", "deployment/"+deployment, "--", "gt", "version")
	if err != nil {
		return "", fmt.Errorf("pod daemon commit: running kubectl exec in deployment %s: %w", deployment, err)
	}
	if exit != 0 {
		// The output is left out of the error: a pod's output can hold a secret.
		return "", fmt.Errorf("pod daemon commit: kubectl exec in deployment %s exited %d", deployment, exit)
	}
	// The first-line rule is copied from verifyProbes.GTVersion in
	// update_verify.go; keep the two in step.
	line, _, _ := strings.Cut(out, "\n")
	fields := strings.Fields(line)
	if len(fields) < 3 || fields[0] != "gt" || fields[1] != "version" {
		return "", fmt.Errorf("pod daemon commit: the first line of gt version in deployment %s names no commit", deployment)
	}
	return fields[2], nil
}

// podArgument refuses a kube context, namespace or deployment that is empty or
// blank, or that kubectl would read as a flag.
func podArgument(what, value string) error {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return fmt.Errorf("pod daemon commit: the %s is empty", what)
	}
	if strings.HasPrefix(trimmed, "-") {
		return fmt.Errorf("pod daemon commit: the %s %q starts with a dash and would read as a kubectl flag", what, value)
	}
	return nil
}

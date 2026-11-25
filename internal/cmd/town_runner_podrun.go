package cmd

import (
	"context"
	"errors"
	"fmt"

	"github.com/steveyegge/gastown/internal/runnerroll"
)

// The daemon pod the roll's hold scripts and reads run in. The landing hold lives
// in its town root.
const (
	podRunNamespace  = "gt"
	podRunDeployment = "deploy/daemon"
	podRunContainer  = "daemon"
)

// podExec runs argv with stdin in the daemon pod. It returns what argv printed on
// stdout, kept apart from stderr, and the exit status. A start failure is err.
type podExec func(ctx context.Context, argv []string, stdin []byte) (stdout []byte, exit int, err error)

// newPodRunner returns the roll's Runner over exec. It runs argv in the daemon pod
// as kubectl exec with stdin and returns stdout only. A non-zero exit or a start
// failure is an error with no output in it: the hold scripts print a one-word
// reply that a failure must not echo.
func newPodRunner(exec podExec) runnerroll.Runner {
	return func(ctx context.Context, argv []string, stdin []byte) ([]byte, error) {
		if exec == nil {
			return nil, errors.New("no pod exec is wired, so the roll cannot reach the daemon pod")
		}
		if len(argv) == 0 {
			return nil, errors.New("no argv to run in the daemon pod")
		}
		pod := append([]string{"kubectl", "exec", "-i", podRunDeployment, "-c", podRunContainer, "-n", podRunNamespace, "--"}, argv...)
		stdout, exit, err := exec(ctx, pod, stdin)
		if err != nil {
			return nil, fmt.Errorf("running %s in the daemon pod: %w", argv[0], err)
		}
		if exit != 0 {
			return nil, fmt.Errorf("running %s in the daemon pod: kubectl exited %d", argv[0], exit)
		}
		return stdout, nil
	}
}

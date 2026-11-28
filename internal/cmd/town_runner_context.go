package cmd

import (
	"context"

	"github.com/steveyegge/gastown/internal/update"
)

// runnerKubeClient is the command the pin matches: the update seam carries the
// command name, so an argument list is pinned by whose call it is.
const runnerKubeClient = "kubectl"

// runnerKubeContextArgs is the argument list a cluster-client call runs with.
// Outside a pod a kubectl call names runnerRollKubeContext first, so a current
// context that reaches another cluster cannot receive the change (gt-33ye8p).
// Inside a pod the pod's service account has no named context, so the list is
// unchanged; so is another command's, which is not the client.
//
// A nil getenv reads as outside a pod: naming the context can only reach the
// intended cluster, the safe side of an unknown environment.
func runnerKubeContextArgs(getenv func(string) string, name string, args []string) []string {
	if name != runnerKubeClient || (getenv != nil && getenv(podEnv) != "") {
		return args
	}
	pinned := make([]string, 0, len(args)+2)
	pinned = append(pinned, "--context", runnerRollKubeContext)
	return append(pinned, args...)
}

// runnerKubeContextRun is inner with every cluster-client call pinned to the
// control cluster's context outside a pod. It returns inner's output, exit
// status and error unchanged, a failed call included.
func runnerKubeContextRun(getenv func(string) string, inner update.RunFunc) update.RunFunc {
	return func(ctx context.Context, dir string, env []string, name string, args ...string) (string, int, error) {
		return inner(ctx, dir, env, name, runnerKubeContextArgs(getenv, name, args)...)
	}
}

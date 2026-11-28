package cmd

import (
	"context"
	"errors"
	"slices"
	"testing"
)

// runnerKubeContextGetenv answers from vars alone, so no test reads the host's
// own environment.
func runnerKubeContextGetenv(vars map[string]string) func(string) string {
	return func(name string) string { return vars[name] }
}

// TestRunnerKubeContextArgs: outside a pod a cluster-client call names
// runnerRollKubeContext first; inside a pod, and for another command, the
// argument list is the one passed in.
func TestRunnerKubeContextArgs(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		vars       map[string]string
		cmd        string
		args, want []string
	}{
		{"outside a pod", nil, runnerKubeClient, []string{"apply", "-f", "-"}, []string{"--context", runnerRollKubeContext, "apply", "-f", "-"}},
		{"an empty pod variable", map[string]string{podEnv: ""}, runnerKubeClient, []string{"get", "pods"}, []string{"--context", runnerRollKubeContext, "get", "pods"}},
		{"inside a pod", map[string]string{podEnv: "10.20.30.40"}, runnerKubeClient, []string{"apply", "-f", "-"}, []string{"apply", "-f", "-"}},
		{"another command", nil, "docker", []string{"inspect", "gt"}, []string{"inspect", "gt"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := runnerKubeContextArgs(runnerKubeContextGetenv(tc.vars), tc.cmd, tc.args); !slices.Equal(got, tc.want) {
				t.Errorf("args = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestRunnerKubeContextRun: the pinned run func hands the fake the pinned
// argument list and returns its output, exit status and error unchanged,
// including a call that fails.
func TestRunnerKubeContextRun(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		args, want []string
		out        string
		exit       int
		err        error
	}{
		{"a successful call", []string{"get", "pods"}, []string{"--context", runnerRollKubeContext, "get", "pods"}, "pods listed\n", 0, nil},
		{"a nonzero exit", []string{"patch", "pod"}, []string{"--context", runnerRollKubeContext, "patch", "pod"}, "modified\n", 1, nil},
		{"a call that could not run", []string{"delete", "pod"}, []string{"--context", runnerRollKubeContext, "delete", "pod"}, "", -1, errors.New("kubectl could not start")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var gotArgs []string
			inner := func(_ context.Context, _ string, _ []string, _ string, args ...string) (string, int, error) {
				gotArgs = args
				return tc.out, tc.exit, tc.err
			}
			out, exit, err := runnerKubeContextRun(runnerKubeContextGetenv(nil), inner)(context.Background(), "", nil, runnerKubeClient, tc.args...)
			if out != tc.out || exit != tc.exit || !errors.Is(err, tc.err) {
				t.Errorf("run = (%q, %d, %v), want (%q, %d, %v)", out, exit, err, tc.out, tc.exit, tc.err)
			}
			if !slices.Equal(gotArgs, tc.want) {
				t.Errorf("inner args = %q, want %q", gotArgs, tc.want)
			}
		})
	}
}

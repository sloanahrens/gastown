package cmd

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

// podProbeRun records each command the pod probe runs and answers with one
// fixed result.
type podProbeRun struct {
	calls  [][]string
	output string
	exit   int
	err    error
}

func (f *podProbeRun) run(_ context.Context, _ string, _ []string, name string, args ...string) (string, int, error) {
	f.calls = append(f.calls, append([]string{name}, args...))
	return f.output, f.exit, f.err
}

// TestPodDaemonCommitReadsTheVersionLine: the commit is the field after
// `gt version` on the first line, and the one command run names the context,
// namespace and deployment.
func TestPodDaemonCommitReadsTheVersionLine(t *testing.T) {
	t.Parallel()
	run := &podProbeRun{output: "gt version abc1234 (built 2026-11-08T10:00:00Z)\n"}

	commit, err := podDaemonCommit(context.Background(), run.run, "town-ctx", "gastown", "daemon", false)
	if err != nil {
		t.Fatalf("podDaemonCommit: %v", err)
	}
	if commit != "abc1234" {
		t.Errorf("commit = %q, want abc1234", commit)
	}
	want := []string{"kubectl", "--context", "town-ctx", "-n", "gastown", "exec", "deployment/daemon", "--", "gt", "version"}
	if len(run.calls) != 1 {
		t.Fatalf("commands run = %d, want 1: %v", len(run.calls), run.calls)
	}
	if !slices.Equal(run.calls[0], want) {
		t.Errorf("command = %v, want %v", run.calls[0], want)
	}
}

// TestPodDaemonCommitRunsTheTrimmedArguments: a context, namespace or
// deployment with surrounding space is checked and run as its trimmed value,
// so the command never carries a value the check did not read.
func TestPodDaemonCommitRunsTheTrimmedArguments(t *testing.T) {
	t.Parallel()
	run := &podProbeRun{output: "gt version abc1234\n"}

	if _, err := podDaemonCommit(context.Background(), run.run, " town-ctx ", "\tgastown ", " daemon", false); err != nil {
		t.Fatalf("podDaemonCommit: %v", err)
	}
	want := []string{"kubectl", "--context", "town-ctx", "-n", "gastown", "exec", "deployment/daemon", "--", "gt", "version"}
	if len(run.calls) != 1 {
		t.Fatalf("commands run = %d, want 1: %v", len(run.calls), run.calls)
	}
	if !slices.Equal(run.calls[0], want) {
		t.Errorf("command = %v, want %v", run.calls[0], want)
	}
}

// TestPodDaemonCommitRefusesBadOutput: every answer that does not name a
// commit on the first line is an error, the error names the step that failed,
// and an error never carries the command's output.
func TestPodDaemonCommitRefusesBadOutput(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		run    podProbeRun
		want   string
		secret string
	}{
		{name: "nonzero exit", run: podProbeRun{output: "pod error SECRET-TOKEN", exit: 1}, want: "kubectl exec in deployment daemon exited", secret: "SECRET-TOKEN"},
		{name: "nonzero exit after a version line", run: podProbeRun{output: "gt version abc1234\n", exit: 1}, want: "kubectl exec in deployment daemon exited"},
		{name: "run error", run: podProbeRun{err: errors.New("exec: kubectl not found")}, want: "running kubectl exec in deployment daemon"},
		{name: "run error after a version line", run: podProbeRun{output: "gt version abc1234\n", err: errors.New("exec: signal: killed")}, want: "running kubectl exec in deployment daemon"},
		{name: "empty output", run: podProbeRun{output: ""}, want: "names no commit"},
		{name: "first line is not a version line", run: podProbeRun{output: "panic: boom\n"}, want: "names no commit"},
		{name: "version line names no commit", run: podProbeRun{output: "gt version\n"}, want: "names no commit"},
		{name: "commit only on the second line", run: podProbeRun{output: "banner\ngt version abc1234\n"}, want: "names no commit"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			run := tc.run
			commit, err := podDaemonCommit(context.Background(), run.run, "town-ctx", "gastown", "daemon", false)
			if err == nil {
				t.Fatalf("podDaemonCommit = %q, want an error", commit)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not name the failed step %q", err, tc.want)
			}
			if tc.secret != "" && strings.Contains(err.Error(), tc.secret) {
				t.Errorf("error %q echoes the command output", err)
			}
		})
	}
}

// TestPodDaemonCommitRefusesEmptyArguments: a context, namespace or deployment
// that is empty, blank, or would read as a kubectl flag is refused before any
// command runs.
func TestPodDaemonCommitRefusesEmptyArguments(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name                        string
		kubeContext, ns, deployment string
	}{
		{name: "empty context", kubeContext: "", ns: "gastown", deployment: "daemon"},
		{name: "blank context", kubeContext: "   ", ns: "gastown", deployment: "daemon"},
		{name: "empty namespace", kubeContext: "town-ctx", ns: "", deployment: "daemon"},
		{name: "empty deployment", kubeContext: "town-ctx", ns: "gastown", deployment: ""},
		{name: "blank deployment", kubeContext: "town-ctx", ns: "gastown", deployment: "\t"},
		{name: "context reads as a flag", kubeContext: "--kubeconfig=/x", ns: "gastown", deployment: "daemon"},
		{name: "namespace reads as a flag", kubeContext: "town-ctx", ns: "-A", deployment: "daemon"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			run := &podProbeRun{output: "gt version abc1234\n"}
			if _, err := podDaemonCommit(context.Background(), run.run, tc.kubeContext, tc.ns, tc.deployment, false); err == nil {
				t.Fatal("podDaemonCommit accepted the argument, want an error")
			}
			if len(run.calls) != 0 {
				t.Errorf("commands run = %v, want none", run.calls)
			}
		})
	}
}

// TestPodDaemonCommitInClusterOmitsContext: with inCluster the version read has
// no --context, so kubectl uses the pod's service account. A namespace that is
// blank or reads as a flag, or a context named with inCluster, is refused before
// any command runs.
func TestPodDaemonCommitInClusterOmitsContext(t *testing.T) {
	t.Parallel()

	t.Run("the version read runs in the pod's cluster with no --context", func(t *testing.T) {
		t.Parallel()
		run := &podProbeRun{output: "gt version abc1234\n"}

		commit, err := podDaemonCommit(context.Background(), run.run, "", " gastown ", "daemon", true)
		if err != nil {
			t.Fatalf("podDaemonCommit: %v", err)
		}
		if commit != "abc1234" {
			t.Errorf("commit = %q, want abc1234", commit)
		}
		want := []string{"kubectl", "-n", "gastown", "exec", "deployment/daemon", "--", "gt", "version"}
		if len(run.calls) != 1 || !slices.Equal(run.calls[0], want) {
			t.Errorf("commands = %v, want exactly %v", run.calls, want)
		}
	})

	t.Run("a namespace that is blank or reads as a flag is refused before any command", func(t *testing.T) {
		t.Parallel()
		cases := []struct {
			name, ns string
		}{
			{name: "empty namespace", ns: ""},
			{name: "blank namespace", ns: "   "},
			{name: "namespace reads as a flag", ns: "-A"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				run := &podProbeRun{output: "gt version abc1234\n"}
				if _, err := podDaemonCommit(context.Background(), run.run, "", tc.ns, "daemon", true); err == nil {
					t.Fatal("podDaemonCommit accepted the namespace, want an error")
				}
				if len(run.calls) != 0 {
					t.Errorf("commands run = %v, want none", run.calls)
				}
			})
		}
	})

	t.Run("a context named with inCluster is refused before any command", func(t *testing.T) {
		t.Parallel()
		run := &podProbeRun{output: "gt version abc1234\n"}
		_, err := podDaemonCommit(context.Background(), run.run, "town-ctx", "gastown", "daemon", true)
		if err == nil || !strings.Contains(err.Error(), "both a kube context and the in-cluster") {
			t.Fatalf("podDaemonCommit error = %v, want the context and in-cluster refusal", err)
		}
		if len(run.calls) != 0 {
			t.Errorf("commands run = %v, want none", run.calls)
		}
	})
}

package cmd

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	k8sdaemon "github.com/steveyegge/gastown/deploy/k8s/daemon"
	"github.com/steveyegge/gastown/internal/update"
)

// podVerifyJobProbes is the Job's own verify probes for a pod target. Its
// executable is the build the Job was started from, so GTVersion reports
// jobCommit. The town is green and the image label is the target's.
type podVerifyJobProbes struct {
	jobCommit string
}

func (p podVerifyJobProbes) GTVersion(context.Context) (string, error) {
	return p.jobCommit, nil
}

func (podVerifyJobProbes) DaemonVersion(context.Context) (string, error) {
	return "", errors.New("the Job's daemon probe ran for a pod target")
}

func (podVerifyJobProbes) ImageLabel(context.Context) (string, error) {
	return podTargetCommit, nil
}

func (podVerifyJobProbes) Health(context.Context) (string, error) {
	return "GREEN tick 1m ago", nil
}

// podVerifyPodRun answers the pod's `gt version` with commit, the way kubectl
// exec prints it.
func podVerifyPodRun(commit string) update.RunFunc {
	return func(context.Context, string, []string, string, ...string) (string, int, error) {
		return "gt version " + commit + " built on test\n", 0, nil
	}
}

// podVerifyTarget is the pod the probes read: the gt deployment in the swap's
// context and namespace.
var podVerifyTarget = podDaemonTarget{Context: podSwapContext, Namespace: podSwapNamespace, Deployment: k8sdaemon.Name}

// TestPodVerifyGTVersionReadsThePod holds GTVersion for a pod target to the
// pod's commit: the Job's own executable reports podOldCommit, and the pod runs
// podTargetCommit, so the value returned can only come from the pod.
func TestPodVerifyGTVersionReadsThePod(t *testing.T) {
	t.Parallel()
	probe := podVerifyProbes{
		VerifyProbes: podVerifyJobProbes{jobCommit: podOldCommit},
		target:       podVerifyTarget,
		run:          podVerifyPodRun(podTargetCommit),
	}
	commit, err := probe.GTVersion(context.Background())
	if err != nil {
		t.Fatalf("GTVersion: %v", err)
	}
	if commit != podTargetCommit {
		t.Errorf("GTVersion = %q, want the pod's commit %q (the Job's binary reports %q)", commit, podTargetCommit, podOldCommit)
	}
}

// TestPodVerifyPassesWhenOnlyTheJobBinaryDiffers: the Job runs podOldCommit and
// the pod runs the target podTargetCommit, the state the automatic roll leaves.
// Every check of the verify passes, so the run does not end partial.
func TestPodVerifyPassesWhenOnlyTheJobBinaryDiffers(t *testing.T) {
	t.Parallel()
	probe := podVerifyProbes{
		VerifyProbes: podVerifyJobProbes{jobCommit: podOldCommit},
		target:       podVerifyTarget,
		run:          podVerifyPodRun(podTargetCommit),
	}
	checks := update.Verify(context.Background(), probe, podTargetCommit)
	if len(checks) == 0 {
		t.Fatal("Verify ran no checks")
	}
	for _, c := range checks {
		if !c.OK {
			t.Errorf("check %s failed with the Job's binary at %s and the pod at the target: %s", c.Name, podOldCommit, c.Detail)
		}
	}
}

// TestPodVerifyGTVersionFailsWhenThePodIsBehind: the pod runs podOtherCommit, so
// gt-version fails and its detail names both the commit the pod reports and the
// commit the run wants. The Job's own podOldCommit is not what it reports.
func TestPodVerifyGTVersionFailsWhenThePodIsBehind(t *testing.T) {
	t.Parallel()
	probe := podVerifyProbes{
		VerifyProbes: podVerifyJobProbes{jobCommit: podTargetCommit},
		target:       podVerifyTarget,
		run:          podVerifyPodRun(podOtherCommit),
	}
	var gtVersion *update.Check
	for _, c := range update.Verify(context.Background(), probe, podTargetCommit) {
		if c.Name == "gt-version" {
			gtVersion = &c
		}
	}
	if gtVersion == nil {
		t.Fatal("Verify named no gt-version check")
	}
	if gtVersion.OK {
		t.Fatalf("gt-version passed with the pod at %s, want it to fail against %s", podOtherCommit, podTargetCommit)
	}
	if !strings.Contains(gtVersion.Detail, podOtherCommit) || !strings.Contains(gtVersion.Detail, podTargetCommit) {
		t.Errorf("gt-version detail %q names neither the pod's commit nor the wanted commit", gtVersion.Detail)
	}
}

// TestPodVerifyGTVersionDoesNotFallBackToTheJobBinary holds GTVersion's failure
// branch: a pod that cannot be read makes GTVersion fail, and it never returns
// the Job's own executable in its place. The Job here reports podTargetCommit,
// so a fallback would pass a pod the probe never read. GTVersion and
// DaemonVersion share podCommit, whose exec failure modes
// TestUpdatePodProbeFailsWhenTheExecFails covers.
func TestPodVerifyGTVersionDoesNotFallBackToTheJobBinary(t *testing.T) {
	t.Parallel()
	probe := podVerifyProbes{
		VerifyProbes: podVerifyJobProbes{jobCommit: podTargetCommit},
		target:       podVerifyTarget,
		run: func(context.Context, string, []string, string, ...string) (string, int, error) {
			return "", 1, nil
		},
	}
	commit, err := probe.GTVersion(context.Background())
	if err == nil {
		t.Fatalf("GTVersion = %q, want an error: the pod was not read", commit)
	}
	if commit != "" {
		t.Errorf("GTVersion returned commit %q with the error, want none", commit)
	}
}

// TestPodVerifyGTVersionRunsTheExecOnThePod holds the call: the commit is read
// with kubectl exec into the target deployment, so the probe reads the pod the
// swap moved.
func TestPodVerifyGTVersionRunsTheExecOnThePod(t *testing.T) {
	t.Parallel()
	var name string
	var args []string
	run := func(_ context.Context, _ string, _ []string, n string, a ...string) (string, int, error) {
		name, args = n, append([]string(nil), a...)
		return "gt version " + podTargetCommit + " built on test\n", 0, nil
	}
	probe := podVerifyProbes{VerifyProbes: podVerifyJobProbes{jobCommit: podOldCommit}, target: podVerifyTarget, run: run}
	if _, err := probe.GTVersion(context.Background()); err != nil {
		t.Fatalf("GTVersion: %v", err)
	}
	if name != "kubectl" {
		t.Errorf("GTVersion ran %q, want kubectl", name)
	}
	if !slices.Contains(args, "deployment/"+k8sdaemon.Name) {
		t.Errorf("kubectl args %v name no deployment/%s", args, k8sdaemon.Name)
	}
}

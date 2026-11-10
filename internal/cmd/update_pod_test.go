package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	k8sdaemon "github.com/steveyegge/gastown/deploy/k8s/daemon"
	"github.com/steveyegge/gastown/internal/imagevalues"
	"github.com/steveyegge/gastown/internal/notify"
	"github.com/steveyegge/gastown/internal/notify/notifyfake"
	"github.com/steveyegge/gastown/internal/update"
)

// podDaemonKubectl stands in for kubectl against a daemon that runs as a pod. A
// patch puts the daemon on the build of the daemon digest it names, and an exec
// reports the build the pod runs, so the rollout wait reads the commit the patch
// put in force. running is the build the pod runs before any patch.
type podDaemonKubectl struct {
	calls   [][]string
	commits map[string]string
	running string
}

func (k *podDaemonKubectl) run(_ context.Context, _ string, _ []string, _ string, args ...string) (string, int, error) {
	k.calls = append(k.calls, args)
	switch {
	case containsArg(args, "patch"):
		joined := strings.Join(args, " ")
		for ref, commit := range k.commits {
			if strings.Contains(joined, ref) {
				k.running = commit
			}
		}
	case containsArg(args, "exec"):
		return "gt version " + k.running + " built on test\n", 0, nil
	}
	return "", 0, nil
}

// patchBody decodes the body of the first patch the fake was asked to run.
func (k *podDaemonKubectl) patchBody(t *testing.T) podSwapPatchBody {
	t.Helper()
	for _, args := range k.calls {
		if !containsArg(args, "patch") {
			continue
		}
		for i := 0; i+1 < len(args); i++ {
			if args[i] == "-p" {
				var out podSwapPatchBody
				if err := json.Unmarshal([]byte(args[i+1]), &out); err != nil {
					t.Fatalf("patch body is not JSON: %v", err)
				}
				return out
			}
		}
	}
	t.Fatalf("no patch was run: calls = %v", k.calls)
	return podSwapPatchBody{}
}

// runPodCommand runs gt update with args over seams and returns its output and
// error. Output is captured, so a test asserts on it or ignores it.
func runPodCommand(t *testing.T, seams updateSeams, args ...string) (string, error) {
	t.Helper()
	c := newUpdateCmdWith(seams)
	var out strings.Builder
	c.SetOut(&out)
	c.SetErr(new(strings.Builder))
	c.SetArgs(args)
	err := c.Execute()
	return out.String(), err
}

// wiredInstall is what a run's Run seam received when a digest install was
// asked for: the deps the run was built with.
type wiredInstall struct {
	deps    update.Deps
	compose *rollbackTestSwap
	verify  *rollbackTestSwap
	kube    *podDaemonKubectl
}

// captureWiredInstall runs gt update --images with extra flags, over seams whose
// compose swap, state-file probe and kubectl are fakes, and returns the deps the
// run got. The run's Run seam stands in for the orchestrator: it captures the
// deps and reports success, so no step runs.
func captureWiredInstall(t *testing.T, extra ...string) wiredInstall {
	t.Helper()
	townRoot := t.TempDir()
	values := filepath.Join(townRoot, "images.yaml")
	if err := imagevalues.Write(values, imagevalues.Values{Daemon: podSwapDigestRef, Polecat: podSwapPolecatRef, Commit: rollbackNewCommit}); err != nil {
		t.Fatalf("write the values file: %v", err)
	}
	clock := &rollbackTestClock{now: time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)}
	got := wiredInstall{
		compose: &rollbackTestSwap{},
		verify:  &rollbackTestSwap{},
		kube:    &podDaemonKubectl{running: rollbackNewCommit, commits: map[string]string{podSwapDigestRef: rollbackNewCommit}},
	}
	seams := updateSeams{
		FindTownRoot: func() (string, error) { return townRoot, nil },
		DepsFactory: func(string) (update.Deps, error) {
			return update.Deps{
				Store:  &update.Store{Dir: update.Dir(townRoot)},
				Clock:  clock,
				Sleep:  clock.sleep,
				Verify: got.verify,
			}, nil
		},
		Run: func(_ context.Context, deps update.Deps, _ update.Options) (update.State, error) {
			got.deps = deps
			return update.State{Outcome: update.OutcomeOK}, nil
		},
		ImageSwap: func(string) (update.ImageSwap, error) { return got.compose, nil },
		Notifier:  func(string) notify.Notifier { return notifyfake.New() },
		PodRun:    got.kube.run,
	}
	args := append([]string{"--images", values}, extra...)
	if _, err := runPodCommand(t, seams, args...); err != nil {
		t.Fatalf("gt update %v: %v", args, err)
	}
	return got
}

// TestUpdatePodProbeBoundsTheExec holds the pod probe's bound: the kubectl exec it
// runs carries a deadline no later than podProbeBound, so a hung exec returns
// inside the restart wait. The fake reads the deadline and never waits.
func TestUpdatePodProbeBoundsTheExec(t *testing.T) {
	t.Parallel()
	var deadline time.Time
	hasDeadline := false
	run := func(ctx context.Context, _ string, _ []string, _ string, _ ...string) (string, int, error) {
		deadline, hasDeadline = ctx.Deadline()
		return "gt version " + rollbackNewCommit + " built on test\n", 0, nil
	}
	probe := podVerifyProbes{
		target: podDaemonTarget{Context: podSwapContext, Namespace: podSwapNamespace, Deployment: k8sdaemon.Name},
		run:    run,
	}
	if _, err := probe.DaemonVersion(context.Background()); err != nil {
		t.Fatalf("DaemonVersion: %v", err)
	}
	if !hasDeadline {
		t.Fatal("the pod probe's kubectl exec has no deadline, want one within podProbeBound")
	}
	if left := time.Until(deadline); left > podProbeBound {
		t.Errorf("the exec's deadline is %v away, want at most podProbeBound %v", left, podProbeBound)
	}
}

// TestUpdatePodProbeFailsWhenTheExecFails holds the pod probe's failure branch:
// an exec that exits, cannot run, or prints no commit makes the probe fail, and
// the probe never hands the run a commit it did not read from the pod.
func TestUpdatePodProbeFailsWhenTheExecFails(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		run  update.RunFunc
	}{
		{
			name: "exec exits nonzero",
			run: func(context.Context, string, []string, string, ...string) (string, int, error) {
				return "pod output SECRET-TOKEN", 1, nil
			},
		},
		{
			name: "exec cannot run",
			run: func(context.Context, string, []string, string, ...string) (string, int, error) {
				return "", 0, errors.New("exec: kubectl not found")
			},
		},
		{
			name: "exec prints no version line",
			run: func(context.Context, string, []string, string, ...string) (string, int, error) {
				return "panic: boom\n", 0, nil
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			probe := podVerifyProbes{
				target: podDaemonTarget{Context: podSwapContext, Namespace: podSwapNamespace, Deployment: k8sdaemon.Name},
				run:    tc.run,
			}
			commit, err := probe.DaemonVersion(context.Background())
			if err == nil {
				t.Fatalf("DaemonVersion = %q, want an error", commit)
			}
			if commit != "" {
				t.Errorf("DaemonVersion returned commit %q with the error, want none", commit)
			}
			if strings.Contains(err.Error(), "SECRET-TOKEN") {
				t.Errorf("error %q echoes the pod's output", err)
			}
		})
	}
}

// TestUpdatePodRollbackHelpNamesTheFlags: gt update rollback --help says the pod
// flags move the pair's Deployment, as gt update --help does, so an operator
// reads the flags' effect from the command they run.
func TestUpdatePodRollbackHelpNamesTheFlags(t *testing.T) {
	t.Parallel()
	seams := updateSeams{FindTownRoot: func() (string, error) { return t.TempDir(), nil }}
	out, err := runPodCommand(t, seams, "rollback", "--help")
	if err != nil {
		t.Fatalf("gt update rollback --help: %v", err)
	}
	want := "--pod-context and --pod-namespace, given together, name the Deployment"
	if !strings.Contains(out, want) {
		t.Errorf("rollback help does not name the pod flags; want %q in:\n%s", want, out)
	}
}

func TestUpdatePodFlagsSelectThePodSeams(t *testing.T) {
	t.Parallel()

	t.Run("with both flags the swap and the daemon probe are the pod types", func(t *testing.T) {
		t.Parallel()
		got := captureWiredInstall(t, "--pod-context", podSwapContext, "--pod-namespace", podSwapNamespace)
		inst, ok := got.deps.Installer.(*update.DigestInstaller)
		if !ok {
			t.Fatalf("installer = %T, want the digest installer", got.deps.Installer)
		}
		swap, ok := inst.Images.Swap.(podDaemonSwap)
		if !ok {
			t.Fatalf("daemon swap = %T, want the pod swap", inst.Images.Swap)
		}
		if swap.Context != podSwapContext || swap.Namespace != podSwapNamespace || swap.Deployment != k8sdaemon.Name {
			t.Errorf("pod swap names %s/%s/%s, want %s/%s/%s", swap.Context, swap.Namespace, swap.Deployment, podSwapContext, podSwapNamespace, k8sdaemon.Name)
		}
		if swap.RolloutTimeout != updateRestartWait {
			t.Errorf("rollout timeout = %v, want the restart bound %v", swap.RolloutTimeout, updateRestartWait)
		}
		probe, ok := got.deps.Verify.(podVerifyProbes)
		if !ok {
			t.Fatalf("daemon probe = %T, want the pod probe", got.deps.Verify)
		}
		if commit, err := probe.DaemonVersion(context.Background()); err != nil || commit != rollbackNewCommit {
			t.Errorf("DaemonVersion = %q, %v; want the commit the pod runs, read through kubectl", commit, err)
		}
		if len(got.kube.calls) == 0 {
			t.Error("the pod probe ran no kubectl command")
		}
		if len(got.compose.applied) != 0 {
			t.Errorf("the compose swap was used %d times, want none", len(got.compose.applied))
		}
	})

	t.Run("with the pod flags and no kubectl runner the run is refused before any step", func(t *testing.T) {
		t.Parallel()
		townRoot := t.TempDir()
		ran := false
		seams := updateSeams{
			FindTownRoot: func() (string, error) { return townRoot, nil },
			DepsFactory: func(string) (update.Deps, error) {
				return update.Deps{Store: &update.Store{Dir: update.Dir(townRoot)}, Clock: updateClock{}}, nil
			},
			Run: func(context.Context, update.Deps, update.Options) (update.State, error) {
				ran = true
				return update.State{}, nil
			},
		}
		_, err := runPodCommand(t, seams, "--pod-context", podSwapContext, "--pod-namespace", podSwapNamespace)
		if err == nil || !strings.Contains(err.Error(), "no pod command runner is wired") {
			t.Fatalf("gt update error = %v, want the missing kubectl runner named", err)
		}
		if ran {
			t.Error("the run started, want it refused before any step")
		}
	})

	t.Run("without them the compose swap and the state-file probe are wired", func(t *testing.T) {
		t.Parallel()
		got := captureWiredInstall(t)
		inst, ok := got.deps.Installer.(*update.DigestInstaller)
		if !ok {
			t.Fatalf("installer = %T, want the digest installer", got.deps.Installer)
		}
		if inst.Images.Swap != update.ImageSwap(got.compose) {
			t.Errorf("daemon swap = %T, want the compose swap the town seam returned", inst.Images.Swap)
		}
		if got.deps.Verify != update.VerifyProbes(got.verify) {
			t.Errorf("daemon probe = %T, want the state-file probe the deps factory returned", got.deps.Verify)
		}
		if len(got.kube.calls) != 0 {
			t.Errorf("kubectl ran %d times without the pod flags, want none", len(got.kube.calls))
		}
	})
}

func TestUpdatePodFlagsComeTogether(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{
			name:    "a context without a namespace is refused",
			args:    []string{"--pod-context", podSwapContext},
			wantErr: "must be given together",
		},
		{
			name:    "a namespace without a context is refused",
			args:    []string{"--pod-namespace", podSwapNamespace},
			wantErr: "must be given together",
		},
		{
			name:    "a blank context with a namespace is refused",
			args:    []string{"--pod-context", "   ", "--pod-namespace", podSwapNamespace},
			wantErr: "--pod-context",
		},
		{
			name:    "--pod-in-cluster without a namespace is refused",
			args:    []string{"--pod-in-cluster"},
			wantErr: "needs --pod-namespace",
		},
		{
			name:    "--pod-in-cluster with a context is refused",
			args:    []string{"--pod-in-cluster", "--pod-context", podSwapContext, "--pod-namespace", podSwapNamespace},
			wantErr: "cannot be given together",
		},
		{
			name:    "a context that reads as a kubectl flag is refused",
			args:    []string{"--pod-context", "--cluster=control", "--pod-namespace", podSwapNamespace},
			wantErr: "--pod-context",
		},
		{
			name:    "a namespace that is not a Kubernetes name is refused",
			args:    []string{"--pod-context", podSwapContext, "--pod-namespace", "GT"},
			wantErr: "--pod-namespace",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			townRoot := t.TempDir()
			ran, polled, kubectl := false, 0, 0
			seams := updateSeams{
				FindTownRoot: func() (string, error) { polled++; return townRoot, nil },
				DepsFactory: func(string) (update.Deps, error) {
					polled++
					return update.Deps{}, nil
				},
				Run: func(context.Context, update.Deps, update.Options) (update.State, error) {
					ran = true
					return update.State{}, nil
				},
				PodRun: func(context.Context, string, []string, string, ...string) (string, int, error) {
					kubectl++
					return "", 0, nil
				},
			}
			_, err := runPodCommand(t, seams, tt.args...)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("gt update error = %v, want one containing %q", err, tt.wantErr)
			}
			if ran || polled != 0 || kubectl != 0 {
				t.Errorf("the run started (%v), the town was read %d times, or kubectl ran %d times; want none before the refusal", ran, polled, kubectl)
			}
		})
	}

	t.Run("rollback with the pod flags and no kubectl runner is refused before the lock", func(t *testing.T) {
		t.Parallel()
		townRoot := t.TempDir()
		seams := updateSeams{
			FindTownRoot: func() (string, error) { return townRoot, nil },
			DepsFactory: func(string) (update.Deps, error) {
				return update.Deps{Store: &update.Store{Dir: update.Dir(townRoot)}, Clock: updateClock{}}, nil
			},
		}
		_, err := runPodCommand(t, seams, "rollback", "--pod-context", podSwapContext, "--pod-namespace", podSwapNamespace)
		if err == nil || !strings.Contains(err.Error(), "no pod command runner is wired") {
			t.Fatalf("gt update rollback error = %v, want the missing kubectl runner named", err)
		}
		if _, statErr := os.Stat(update.Dir(townRoot)); statErr == nil {
			t.Errorf("the update state directory %s was created, want the refusal before the lock", update.Dir(townRoot))
		}
	})

	t.Run("rollback refuses one flag before the lock or any read", func(t *testing.T) {
		t.Parallel()
		townRoot := t.TempDir()
		polled := 0
		seams := updateSeams{
			FindTownRoot: func() (string, error) { polled++; return townRoot, nil },
			DepsFactory:  func(string) (update.Deps, error) { polled++; return update.Deps{}, nil },
		}
		_, err := runPodCommand(t, seams, "rollback", "--pod-context", podSwapContext)
		if err == nil || !strings.Contains(err.Error(), "must be given together") {
			t.Fatalf("gt update rollback error = %v, want the together refusal", err)
		}
		if polled != 0 {
			t.Errorf("the town was read %d times, want none", polled)
		}
	})
}

func TestUpdatePodRollbackUsesThePodSeams(t *testing.T) {
	t.Parallel()
	f := newImageRollbackFixture(t, true)
	f.record(t, update.ImageRecord{Current: f.running, Previous: f.previous})
	kube := &podDaemonKubectl{
		running: rollbackNewCommit,
		commits: map[string]string{
			f.previous.Daemon: rollbackOldCommit,
			f.running.Daemon:  rollbackNewCommit,
		},
	}
	clock := &rollbackTestClock{now: time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)}
	composeAsked := 0
	seams := updateSeams{
		FindTownRoot: func() (string, error) { return f.townRoot, nil },
		DepsFactory: func(string) (update.Deps, error) {
			return update.Deps{Clock: clock, Sleep: clock.sleep, Verify: f.swap}, nil
		},
		Images: updateImagesSeams{
			Registry:    func(string) (imagevalues.Registry, error) { return f.registry, nil },
			Credentials: func() (string, string, error) { return testUpdateImagesUser, testUpdateImagesToken, nil },
			HTTPClient:  f.server.Client(),
		},
		ImageSwap: func(string) (update.ImageSwap, error) { composeAsked++; return f.swap, nil },
		Notifier:  func(string) notify.Notifier { return f.notes },
		PodRun:    kube.run,
	}

	out, err := runPodCommand(t, seams, "rollback", "--pod-context", podSwapContext, "--pod-namespace", podSwapNamespace)
	if err != nil {
		t.Fatalf("gt update rollback with the pod flags: %v", err)
	}
	if composeAsked != 0 || len(f.swap.applied) != 0 {
		t.Errorf("the compose swap was wired %d times and applied %d pairs, want none", composeAsked, len(f.swap.applied))
	}
	body := kube.patchBody(t)
	if got := body.Spec.Template.Metadata.Annotations[k8sdaemon.PolecatImageAnnotation]; got != f.previous.Polecat {
		t.Errorf("rollback patch annotation = %q, want the recorded polecat %q", got, f.previous.Polecat)
	}
	if len(body.Spec.Template.Spec.Containers) != 1 || body.Spec.Template.Spec.Containers[0].Image != f.previous.Daemon {
		t.Errorf("rollback patch containers = %+v, want the recorded daemon %q", body.Spec.Template.Spec.Containers, f.previous.Daemon)
	}
	if got := f.ledgerNow(t); !samePairs(got, update.ImageRecord{Current: f.previous, Previous: f.previous}) {
		t.Errorf("ledger after the rollback = %+v, want the restored pair as both current and previous", got)
	}
	if !strings.Contains(out, f.previous.Daemon) || !strings.Contains(out, f.previous.Polecat) {
		t.Errorf("output %q does not name both restored images", out)
	}
}

// TestPodDaemonFromFlagsInCluster: --pod-in-cluster with a namespace names the
// Deployment with no context; without a namespace, or with a context, it is
// refused. The together rule still holds without it.
func TestPodDaemonFromFlagsInCluster(t *testing.T) {
	t.Parallel()

	t.Run("with a namespace the target is in-cluster and names no context", func(t *testing.T) {
		t.Parallel()
		got, err := podDaemonFromFlags("", " "+podSwapNamespace+" ", true)
		if err != nil {
			t.Fatalf("podDaemonFromFlags: %v", err)
		}
		if got == nil {
			t.Fatal("target = nil, want the in-cluster target")
		}
		want := podDaemonTarget{Namespace: podSwapNamespace, InCluster: true, Deployment: k8sdaemon.Name}
		if *got != want {
			t.Errorf("target = %+v, want %+v", *got, want)
		}
	})

	refusals := []struct {
		name            string
		kubeContext, ns string
		wantErr         string
	}{
		{name: "without a namespace it is refused", ns: "", wantErr: "needs --pod-namespace"},
		{name: "with a context it is refused", kubeContext: podSwapContext, ns: podSwapNamespace, wantErr: "cannot be given together"},
		{name: "with a namespace that is not a Kubernetes name it is refused", ns: "GT", wantErr: "--pod-namespace"},
	}
	for _, tc := range refusals {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := podDaemonFromFlags(tc.kubeContext, tc.ns, true)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("podDaemonFromFlags error = %v, want one containing %q", err, tc.wantErr)
			}
			if got != nil {
				t.Errorf("target = %+v, want nil with the error", *got)
			}
		})
	}

	t.Run("without the flag the together rule still holds", func(t *testing.T) {
		t.Parallel()
		if got, err := podDaemonFromFlags("", "", false); got != nil || err != nil {
			t.Errorf("no flags = %+v, %v; want nil and no error", got, err)
		}
		if _, err := podDaemonFromFlags(podSwapContext, "", false); err == nil || !strings.Contains(err.Error(), "must be given together") {
			t.Errorf("context alone error = %v, want the together refusal", err)
		}
		if _, err := podDaemonFromFlags("", podSwapNamespace, false); err == nil || !strings.Contains(err.Error(), "must be given together") {
			t.Errorf("namespace alone error = %v, want the together refusal", err)
		}
	})
}

// TestUpdateRollbackAcceptsPodInCluster: gt update rollback --pod-in-cluster
// --pod-namespace N reaches the pod swap with no --context on any kubectl
// command, and restores the recorded pair to the Deployment.
func TestUpdateRollbackAcceptsPodInCluster(t *testing.T) {
	t.Parallel()
	f := newImageRollbackFixture(t, true)
	f.record(t, update.ImageRecord{Current: f.running, Previous: f.previous})
	kube := &podDaemonKubectl{
		running: rollbackNewCommit,
		commits: map[string]string{
			f.previous.Daemon: rollbackOldCommit,
			f.running.Daemon:  rollbackNewCommit,
		},
	}
	clock := &rollbackTestClock{now: time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)}
	seams := updateSeams{
		FindTownRoot: func() (string, error) { return f.townRoot, nil },
		DepsFactory: func(string) (update.Deps, error) {
			return update.Deps{Clock: clock, Sleep: clock.sleep, Verify: f.swap}, nil
		},
		Images: updateImagesSeams{
			Registry:    func(string) (imagevalues.Registry, error) { return f.registry, nil },
			Credentials: func() (string, string, error) { return testUpdateImagesUser, testUpdateImagesToken, nil },
			HTTPClient:  f.server.Client(),
		},
		ImageSwap: func(string) (update.ImageSwap, error) { return f.swap, nil },
		Notifier:  func(string) notify.Notifier { return f.notes },
		PodRun:    kube.run,
	}

	if _, err := runPodCommand(t, seams, "rollback", "--pod-in-cluster", "--pod-namespace", podSwapNamespace); err != nil {
		t.Fatalf("gt update rollback --pod-in-cluster: %v", err)
	}
	if len(kube.calls) == 0 {
		t.Fatal("kubectl ran no command, want the pod swap to run")
	}
	for _, args := range kube.calls {
		if containsArg(args, "--context") {
			t.Errorf("kubectl %v carries --context, want none in-cluster", args)
		}
		if !slices.Equal(args[:2], []string{"-n", podSwapNamespace}) {
			t.Errorf("kubectl %v does not start with the namespace", args)
		}
	}
	body := kube.patchBody(t)
	if got := body.Spec.Template.Metadata.Annotations[k8sdaemon.PolecatImageAnnotation]; got != f.previous.Polecat {
		t.Errorf("rollback patch annotation = %q, want the recorded polecat %q", got, f.previous.Polecat)
	}
}

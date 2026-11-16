package cmd

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	k8sdaemon "github.com/steveyegge/gastown/deploy/k8s/daemon"
	"github.com/steveyegge/gastown/internal/update"
)

// podCrashNow is the clock every crash probe test judges a pod's age against, so
// a row that says "four minutes after creation" means exactly that.
var podCrashNow = time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)

// podCrashRun is the kubectl seam a crash probe test replaces: it records each
// command and answers with one fixed output, exit and error.
type podCrashRun struct {
	calls  [][]string
	output string
	exit   int
	err    error
}

func (r *podCrashRun) run(_ context.Context, _ string, _ []string, name string, args ...string) (string, int, error) {
	r.calls = append(r.calls, append([]string{name}, args...))
	return r.output, r.exit, r.err
}

// podCrashListJSON is the `kubectl get pods -o json` answer holding pods, each
// already the JSON object the cluster prints, so a row states the shape it tests.
func podCrashListJSON(pods ...string) string {
	return `{"items":[` + strings.Join(pods, ",") + `]}`
}

// podCrashPodJSON is one pod in the list: its name, the creation time it
// reports, its Ready condition status ("" leaves the condition out), whether the
// cluster is deleting it, and its containers' statuses.
func podCrashPodJSON(name, created, ready string, deleting bool, containers ...string) string {
	conditions := `[]`
	if ready != "" {
		conditions = fmt.Sprintf(`[{"type":"Ready","status":%q}]`, ready)
	}
	deletion := ""
	if deleting {
		deletion = `,"deletionTimestamp":"2026-11-16T11:59:00Z"`
	}
	return fmt.Sprintf(`{"metadata":{"name":%q,"creationTimestamp":%q%s},"status":{"conditions":%s,"containerStatuses":[%s]}}`,
		name, created, deletion, conditions, strings.Join(containers, ","))
}

// podCrashContainerJSON is one container's status. A waiting reason of "" leaves
// the state out, as a container that is running reports no waiting state.
func podCrashContainerJSON(name string, ready bool, restarts int, waiting string) string {
	state := `{}`
	if waiting != "" {
		state = fmt.Sprintf(`{"waiting":{"reason":%q}}`, waiting)
	}
	return fmt.Sprintf(`{"name":%q,"ready":%t,"restartCount":%d,"state":%s}`, name, ready, restarts, state)
}

// podCrashDaemonPod is a daemon pod created half a minute before podCrashNow,
// with the daemon container and an optional Ready condition. It is the pod a row
// varies one fact about.
func podCrashDaemonPod(ready string, containers ...string) string {
	return podCrashPodJSON("gastown-daemon-new", "2026-11-16T11:59:30Z", ready, false, containers...)
}

// TestPreRollGuardPodCrashProbe drives the probe over canned pod lists: a
// CrashLoopBackOff wait, a container restarted twice, and a pod still not Ready
// three minutes after its creation all report crashing and name the container,
// while a healthy, recently started or still-young pod does not. A list that
// cannot be judged is an error, never a healthy answer.
func TestPreRollGuardPodCrashProbe(t *testing.T) {
	t.Parallel()
	target := podDaemonTarget{Context: podSwapContext, Namespace: podSwapNamespace, Deployment: k8sdaemon.Name}
	// wantCommand is the one command a judged list runs: the daemon
	// Deployment's own selector, so the read is the pods the Deployment rolled.
	wantCommand := []string{"kubectl", "--context", podSwapContext, "-n", podSwapNamespace,
		"get", "pods", "-l", podDaemonSelector, "-o", "json"}
	tests := []struct {
		name string
		// output is the pod list kubectl prints.
		output string
		// exit and runErr are how the command fails, when it does.
		exit   int
		runErr error
		// crashing and why are the answer a judged list must give.
		crashing bool
		why      string
		// wantErr is the step an unjudgeable list or a failed read must name.
		wantErr string
	}{
		{
			name:     "a container waiting in CrashLoopBackOff",
			output:   podCrashListJSON(podCrashDaemonPod("False", podCrashContainerJSON(k8sdaemon.Name, false, 0, "CrashLoopBackOff"))),
			crashing: true,
			why:      k8sdaemon.Name + ": CrashLoopBackOff",
		},
		{
			name: "a sidecar waiting in CrashLoopBackOff is the one named",
			output: podCrashListJSON(podCrashDaemonPod("False",
				podCrashContainerJSON(k8sdaemon.Name, true, 0, ""),
				podCrashContainerJSON("helper", false, 0, "CrashLoopBackOff"))),
			crashing: true,
			why:      "helper: CrashLoopBackOff",
		},
		{
			name:     "a container restarted twice",
			output:   podCrashListJSON(podCrashDaemonPod("True", podCrashContainerJSON(k8sdaemon.Name, true, 2, ""))),
			crashing: true,
			why:      k8sdaemon.Name + ": restarted 2 times",
		},
		{
			name:     "a container restarted once is not crashing",
			output:   podCrashListJSON(podCrashDaemonPod("True", podCrashContainerJSON(k8sdaemon.Name, true, 1, ""))),
			crashing: false,
		},
		{
			name:     "a healthy pod a while after creation",
			output:   podCrashListJSON(podCrashPodJSON("gastown-daemon-new", "2026-11-16T11:50:00Z", "True", false, podCrashContainerJSON(k8sdaemon.Name, true, 0, ""))),
			crashing: false,
		},
		{
			name:     "a pod not Ready four minutes after creation",
			output:   podCrashListJSON(podCrashPodJSON("gastown-daemon-new", "2026-11-16T11:56:00Z", "False", false, podCrashContainerJSON(k8sdaemon.Name, false, 0, ""))),
			crashing: true,
			why:      k8sdaemon.Name + ": not Ready 4m0s after creation",
		},
		{
			name:     "a pod not Ready two minutes after creation",
			output:   podCrashListJSON(podCrashPodJSON("gastown-daemon-new", "2026-11-16T11:58:00Z", "False", false, podCrashContainerJSON(k8sdaemon.Name, false, 0, ""))),
			crashing: false,
		},
		{
			name:     "a pod not Ready exactly at the bound",
			output:   podCrashListJSON(podCrashPodJSON("gastown-daemon-new", "2026-11-16T11:57:00Z", "False", false, podCrashContainerJSON(k8sdaemon.Name, false, 0, ""))),
			crashing: true,
			why:      k8sdaemon.Name + ": not Ready 3m0s after creation",
		},
		{
			name:     "a pod with no Ready condition yet",
			output:   podCrashListJSON(podCrashPodJSON("gastown-daemon-new", "2026-11-16T11:56:00Z", "", false, podCrashContainerJSON(k8sdaemon.Name, false, 0, ""))),
			crashing: true,
			why:      k8sdaemon.Name + ": not Ready 4m0s after creation",
		},
		{
			name:     "a pod reporting no container names itself",
			output:   podCrashListJSON(podCrashPodJSON("gastown-daemon-pending", "2026-11-16T11:56:00Z", "False", false)),
			crashing: true,
			why:      "pod gastown-daemon-pending: not Ready 4m0s after creation",
		},
		{
			name: "the newest pod is the one judged",
			output: podCrashListJSON(
				podCrashPodJSON("gastown-daemon-old", "2026-11-16T11:58:00Z", "False", false, podCrashContainerJSON(k8sdaemon.Name, false, 0, "CrashLoopBackOff")),
				podCrashPodJSON("gastown-daemon-new", "2026-11-16T11:59:00Z", "True", false, podCrashContainerJSON(k8sdaemon.Name, true, 0, ""))),
			crashing: false,
		},
		{
			name: "a crash-looping newest pod is judged over a healthy old one",
			output: podCrashListJSON(
				podCrashPodJSON("gastown-daemon-old", "2026-11-16T11:58:00Z", "True", false, podCrashContainerJSON(k8sdaemon.Name, true, 0, "")),
				podCrashPodJSON("gastown-daemon-new", "2026-11-16T11:59:00Z", "False", false, podCrashContainerJSON(k8sdaemon.Name, false, 0, "CrashLoopBackOff"))),
			crashing: true,
			why:      k8sdaemon.Name + ": CrashLoopBackOff",
		},
		{
			name: "a pod the cluster is deleting is not judged",
			output: podCrashListJSON(
				podCrashPodJSON("gastown-daemon-live", "2026-11-16T11:59:00Z", "True", false, podCrashContainerJSON(k8sdaemon.Name, true, 0, "")),
				podCrashPodJSON("gastown-daemon-going", "2026-11-16T11:59:30Z", "False", true, podCrashContainerJSON(k8sdaemon.Name, false, 0, "CrashLoopBackOff"))),
			crashing: false,
		},
		{
			name:    "no pod to judge",
			output:  podCrashListJSON(),
			wantErr: "names no pod to judge",
		},
		{
			name:    "only a pod the cluster is deleting",
			output:  podCrashListJSON(podCrashPodJSON("gastown-daemon-going", "2026-11-16T11:59:30Z", "True", true, podCrashContainerJSON(k8sdaemon.Name, true, 0, ""))),
			wantErr: "names no pod to judge",
		},
		{
			name:    "a creation time that does not parse",
			output:  podCrashListJSON(podCrashPodJSON("gastown-daemon-new", "yesterday", "True", false, podCrashContainerJSON(k8sdaemon.Name, true, 0, ""))),
			wantErr: "reports no readable creation time",
		},
		{
			name:    "a list that is not JSON",
			output:  "pod list: SECRET-TOKEN",
			wantErr: "is not JSON",
		},
		{
			name:    "the pod list exits nonzero",
			output:  "pod list: SECRET-TOKEN",
			exit:    1,
			wantErr: "the daemon pod list exited 1",
		},
		{
			name:    "the runner fails",
			output:  "pod list: SECRET-TOKEN",
			runErr:  errors.New("exec: kubectl: not found"),
			wantErr: "running the daemon pod list",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			run := &podCrashRun{output: tt.output, exit: tt.exit, err: tt.runErr}

			crashing, why, err := podDaemonCrashing(context.Background(), run.run, target, podCrashNow)

			if len(run.calls) != 1 {
				t.Fatalf("kubectl ran %d commands, want the one pod list: %v", len(run.calls), run.calls)
			}
			if !slices.Equal(run.calls[0], wantCommand) {
				t.Errorf("command = %v, want %v", run.calls[0], wantCommand)
			}
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("podDaemonCrashing = (%t, %q), want the error %q", crashing, why, tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("error %q does not name the failed step %q", err, tt.wantErr)
				}
				if strings.Contains(err.Error(), "SECRET-TOKEN") {
					t.Errorf("error %q echoes the command output", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("podDaemonCrashing: %v", err)
			}
			if crashing != tt.crashing {
				t.Errorf("crashing = %t, want %t", crashing, tt.crashing)
			}
			if why != tt.why {
				t.Errorf("why = %q, want %q", why, tt.why)
			}
		})
	}
}

// TestPreRollGuardPodCrashProbeRefusesToRunUnwiredOrUnusableInput: a runner that
// is not wired, or a target kubectl could not be given as written, is refused
// before any command runs.
func TestPreRollGuardPodCrashProbeRefusesToRunUnwiredOrUnusableInput(t *testing.T) {
	t.Parallel()
	healthy := podCrashListJSON(podCrashDaemonPod("True", podCrashContainerJSON(k8sdaemon.Name, true, 0, "")))
	targets := []struct {
		name   string
		target podDaemonTarget
	}{
		{name: "an empty context", target: podDaemonTarget{Namespace: podSwapNamespace, Deployment: k8sdaemon.Name}},
		{name: "a context that reads as a flag", target: podDaemonTarget{Context: "--kubeconfig=/x", Namespace: podSwapNamespace, Deployment: k8sdaemon.Name}},
		{name: "an empty namespace", target: podDaemonTarget{Context: podSwapContext, Deployment: k8sdaemon.Name}},
		{name: "a namespace that is not a Kubernetes name", target: podDaemonTarget{Context: podSwapContext, Namespace: "GT", Deployment: k8sdaemon.Name}},
		{name: "a context with the in-cluster service account", target: podDaemonTarget{Context: podSwapContext, Namespace: podSwapNamespace, InCluster: true, Deployment: k8sdaemon.Name}},
	}
	for _, tc := range targets {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			run := &podCrashRun{output: healthy}
			if _, _, err := podDaemonCrashing(context.Background(), run.run, tc.target, podCrashNow); err == nil {
				t.Fatalf("podDaemonCrashing target %+v reported no error", tc.target)
			}
			if len(run.calls) != 0 {
				t.Errorf("kubectl ran %v, want no command", run.calls)
			}
		})
	}

	t.Run("no runner is wired", func(t *testing.T) {
		t.Parallel()
		_, _, err := podDaemonCrashing(context.Background(), nil, podDaemonTarget{Context: podSwapContext, Namespace: podSwapNamespace, Deployment: k8sdaemon.Name}, podCrashNow)
		if err == nil || !strings.Contains(err.Error(), "no command runner is wired") {
			t.Fatalf("podDaemonCrashing with no runner error = %v, want the missing runner named", err)
		}
	})
}

// TestPreRollGuardPodCrashProbeInCluster: an in-cluster probe names only the
// namespace, so kubectl uses the pod's service account.
func TestPreRollGuardPodCrashProbeInCluster(t *testing.T) {
	t.Parallel()
	run := &podCrashRun{output: podCrashListJSON(podCrashDaemonPod("True", podCrashContainerJSON(k8sdaemon.Name, true, 0, "")))}
	target := podDaemonTarget{Namespace: podSwapNamespace, InCluster: true, Deployment: k8sdaemon.Name}

	if _, _, err := podDaemonCrashing(context.Background(), run.run, target, podCrashNow); err != nil {
		t.Fatalf("podDaemonCrashing in-cluster: %v", err)
	}
	want := []string{"kubectl", "-n", podSwapNamespace, "get", "pods", "-l", podDaemonSelector, "-o", "json"}
	if len(run.calls) != 1 || !slices.Equal(run.calls[0], want) {
		t.Errorf("commands = %v, want the one prefixless pod list %v", run.calls, want)
	}
}

// TestPreRollGuardUpdateWiresThePodCrashProbe drives the command's own wiring: a
// pod-target run's verify probes carry the crash probe and still the pair
// checks, and the probe reads the daemon pod list through the run's kubectl
// runner. A run with no pod target builds no crash probe.
func TestPreRollGuardUpdateWiresThePodCrashProbe(t *testing.T) {
	t.Parallel()
	crashLooping := podCrashListJSON(podCrashDaemonPod("False", podCrashContainerJSON(k8sdaemon.Name, false, 0, "CrashLoopBackOff")))

	t.Run("a pod target's probes carry the crash probe and the pair checks", func(t *testing.T) {
		t.Parallel()
		townRoot := t.TempDir()
		kube := &podCrashRun{output: crashLooping}
		var got update.Deps
		seams := updateSeams{
			FindTownRoot: func() (string, error) { return townRoot, nil },
			DepsFactory: func(string) (update.Deps, error) {
				return update.Deps{
					Store: &update.Store{Dir: update.Dir(townRoot)},
					Clock: updateTestClock{now: podCrashNow},
				}, nil
			},
			Run: func(_ context.Context, deps update.Deps, _ update.Options) (update.State, error) {
				got = deps
				return update.State{Outcome: update.OutcomeOK}, nil
			},
			PodRun: kube.run,
		}

		if _, err := runPodCommand(t, seams, "--skip", "install",
			"--pod-context", podSwapContext, "--pod-namespace", podSwapNamespace); err != nil {
			t.Fatalf("gt update --skip install with the pod flags: %v", err)
		}
		crash, ok := got.Verify.(update.CrashProbes)
		if !ok {
			t.Fatalf("verify probe = %T, want the pod run's crash probe", got.Verify)
		}
		if _, ok := got.Verify.(update.PairProbes); !ok {
			t.Fatalf("verify probe = %T, want the pair checks kept beside the crash probe", got.Verify)
		}

		crashing, why, err := crash.PodCrashing(context.Background())
		if err != nil {
			t.Fatalf("PodCrashing: %v", err)
		}
		if !crashing || why != k8sdaemon.Name+": CrashLoopBackOff" {
			t.Errorf("PodCrashing = (%t, %q), want the crash-looping pod reported", crashing, why)
		}
		want := []string{"kubectl", "--context", podSwapContext, "-n", podSwapNamespace,
			"get", "pods", "-l", podDaemonSelector, "-o", "json"}
		if len(kube.calls) != 1 || !slices.Equal(kube.calls[0], want) {
			t.Errorf("kubectl ran %v, want the one daemon pod list %v", kube.calls, want)
		}
	})

	t.Run("a run with no pod target builds no crash probe", func(t *testing.T) {
		t.Parallel()
		townRoot := t.TempDir()
		var got update.Deps
		seams := updateSeams{
			FindTownRoot: func() (string, error) { return townRoot, nil },
			DepsFactory: func(string) (update.Deps, error) {
				return update.Deps{
					Store:  &update.Store{Dir: update.Dir(townRoot)},
					Clock:  updateTestClock{now: podCrashNow},
					Verify: newVerifyProbes(verifyProbeEnv{}),
				}, nil
			},
			Run: func(_ context.Context, deps update.Deps, _ update.Options) (update.State, error) {
				got = deps
				return update.State{Outcome: update.OutcomeOK}, nil
			},
		}

		if _, err := runPodCommand(t, seams, "--skip", "install"); err != nil {
			t.Fatalf("gt update --skip install: %v", err)
		}
		if _, ok := got.Verify.(update.CrashProbes); ok {
			t.Errorf("verify probe = %T, want no crash probe on a run with no daemon Deployment", got.Verify)
		}
	})
}

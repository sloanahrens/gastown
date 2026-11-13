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

// podSwapOldDaemonRef and podSwapOldPolecatRef are the pair the fake's Deployment
// ran before an install: the pair the running pair read returns.
var (
	podSwapOldDaemonRef  = "registry.internal:5000/town/gastown-daemon@sha256:" + strings.Repeat("1", 64)
	podSwapOldPolecatRef = "registry.internal:5000/town/gastown-polecat@sha256:" + strings.Repeat("2", 64)
)

// podDaemonKubectl stands in for kubectl against a daemon that runs as a pod. A
// patch puts the daemon on the build of the daemon digest it names, and an exec
// reports the build the pod runs, so the rollout wait reads the commit the patch
// put in force. running is the build the pod runs before any patch. pods is the
// pod list the pair check reads after the rollout, one entry per daemon pod.
type podDaemonKubectl struct {
	calls   [][]string
	commits map[string]string
	running string
	pods    []podPairPod
	// image and polecat are the pair the daemon Deployment's pod template runs:
	// its daemon container's image and its polecat image annotation. A patch of
	// the daemon Deployment sets them to the pair it carries.
	image, polecat string
	// readErr and readExit fail the running pair read; the knobs above fail a
	// patch, the rollout wait or the pod list instead.
	readErr  error
	readExit int
	// failOnce is a kubectl step, such as "patch deployment/dashboard", whose first
	// command exits 1. failed records that it did.
	failOnce string
	failed   bool
}

func (k *podDaemonKubectl) run(_ context.Context, _ string, _ []string, _ string, args ...string) (string, int, error) {
	k.calls = append(k.calls, args)
	if k.failOnce != "" && !k.failed && strings.Contains(strings.Join(args, " "), k.failOnce) {
		k.failed = true
		return "", 1, nil
	}
	switch {
	case containsArg(args, "patch"):
		k.patched(args[len(args)-1])
	case containsArg(args, "get") && containsArg(args, "deployment/"+k8sdaemon.Name):
		// The running pair read: the Deployment's daemon image and polecat
		// annotation, on the two lines the swap reads.
		if k.readErr != nil || k.readExit != 0 {
			return "SECRET-TOKEN in the pair read", k.readExit, k.readErr
		}
		return k.image + "\n" + k.polecat, 0, nil
	case containsArg(args, "get") && containsArg(args, "deployment/"+k8sdaemon.DashboardName):
		// The dashboard's existence read: the town this fake stands in for has one.
		return "deployment.apps/" + k8sdaemon.DashboardName + "\n", 0, nil
	case containsArg(args, "get"):
		return podListJSON(k.pods...), 0, nil
	case containsArg(args, "exec"):
		return "gt version " + k.running + " built on test\n", 0, nil
	}
	return "", 0, nil
}

// patched applies a daemon Deployment patch to the fake's state: the pair the
// Deployment carries and the build the pod runs for it. A patch of any other
// Deployment, such as the dashboard's, leaves the pair alone.
func (k *podDaemonKubectl) patched(body string) {
	var patch podSwapPatchBody
	if err := json.Unmarshal([]byte(body), &patch); err != nil {
		return
	}
	for _, c := range patch.Spec.Template.Spec.Containers {
		if c.Name == k8sdaemon.Name {
			k.image = c.Image
		}
	}
	if polecat, ok := patch.Spec.Template.Metadata.Annotations[k8sdaemon.PolecatImageAnnotation]; ok {
		k.polecat = polecat
	}
	if commit, ok := k.commits[k.image]; ok {
		k.running = commit
	}
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
	deps     update.Deps
	opts     update.Options
	compose  *rollbackTestSwap
	verify   *rollbackTestSwap
	kube     *podDaemonKubectl
	host     *podHostInstaller
	townRoot string
}

// podHostInstaller stands in for the run's host install and counts the calls. A
// run with a pod target must make none, so a test asserts the count is zero.
type podHostInstaller struct {
	calls int
}

func (h *podHostInstaller) Install(_ context.Context, sha string) (update.InstallResult, error) {
	h.calls++
	return update.InstallResult{Event: "installed", Commit: sha}, nil
}

// captureWiredInstall runs gt update --images with extra flags, over seams whose
// compose swap, state-file probe and kubectl are fakes, and returns the deps the
// run got. The run's Run seam stands in for the orchestrator: it captures the
// deps and reports success, so no step runs.
func captureWiredInstall(t *testing.T, extra ...string) wiredInstall {
	t.Helper()
	townRoot := t.TempDir()
	values := filepath.Join(townRoot, "images.yaml")
	// The polecat digest is the one the fake registry serves at the dev tag, so the
	// pod's pair check passes for a healthy pod.
	if err := imagevalues.Write(values, imagevalues.Values{Daemon: podSwapDigestRef, Polecat: podPairPolecatRef, Commit: rollbackNewCommit}); err != nil {
		t.Fatalf("write the values file: %v", err)
	}
	clock := &rollbackTestClock{now: time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)}
	// The state-file probe reads the daemon the compose swap moved, so the swap's
	// wait sees the commit it applied; a separate fake would never report it.
	compose := &rollbackTestSwap{}
	got := wiredInstall{
		townRoot: townRoot,
		compose:  compose,
		verify:   compose,
		kube: &podDaemonKubectl{
			running: rollbackNewCommit,
			commits: map[string]string{podSwapDigestRef: rollbackNewCommit},
			image:   podSwapOldDaemonRef,
			polecat: podSwapOldPolecatRef,
			pods:    podPairHealthy(),
		},
		host: &podHostInstaller{},
	}
	seams := updateSeams{
		FindTownRoot: func() (string, error) { return townRoot, nil },
		DepsFactory: func(string) (update.Deps, error) {
			return update.Deps{
				Store:     &update.Store{Dir: update.Dir(townRoot)},
				Clock:     clock,
				Sleep:     clock.sleep,
				Verify:    got.verify,
				Installer: got.host,
			}, nil
		},
		Run: func(_ context.Context, deps update.Deps, opts update.Options) (update.State, error) {
			got.deps = deps
			got.opts = opts
			return update.State{Outcome: update.OutcomeOK}, nil
		},
		ImageSwap: func(string) (update.ImageSwap, error) { return got.compose, nil },
		Notifier:  func(string) notify.Notifier { return notifyfake.New() },
		PodRun:    got.kube.run,
		Images:    podPairImages(podPairManifest),
	}
	args := append([]string{"--images", values}, extra...)
	if _, err := runPodCommand(t, seams, args...); err != nil {
		t.Fatalf("gt update %v: %v", args, err)
	}
	return got
}

// TestUpdatePodTargetSkipsTheHostInstall holds the digest install under a pod
// target: the host install is never called, the daemon still moves to the pair,
// and the run's report names the skip. A run without a pod target still calls
// the host install first. A pod-target run without --images is refused, which
// TestUpdatePodTargetWithoutImagesIsRefused holds.
func TestUpdatePodTargetSkipsTheHostInstall(t *testing.T) {
	t.Parallel()

	t.Run("a pod target never calls the host install", func(t *testing.T) {
		t.Parallel()
		got := captureWiredInstall(t, "--pod-context", podSwapContext, "--pod-namespace", podSwapNamespace)
		if _, err := got.deps.Installer.Install(context.Background(), rollbackNewCommit); err != nil {
			t.Fatalf("Install: %v", err)
		}
		if got.host.calls != 0 {
			t.Errorf("host install called %d times, want none under a pod target", got.host.calls)
		}
	})

	t.Run("a pod target still moves the daemon to the pair", func(t *testing.T) {
		t.Parallel()
		got := captureWiredInstall(t, "--pod-context", podSwapContext, "--pod-namespace", podSwapNamespace)
		res, err := got.deps.Installer.Install(context.Background(), rollbackNewCommit)
		if err != nil {
			t.Fatalf("Install: %v", err)
		}
		if verdict := update.ClassifyInstall(res); verdict != update.ResultOK {
			t.Errorf("ClassifyInstall = %q, want %q: %+v", verdict, update.ResultOK, res)
		}
		// patchBody fails the test when no patch was run, so the swap is checked
		// by the call that reads it.
		got.kube.patchBody(t)
	})

	t.Run("the run's report names the skip", func(t *testing.T) {
		t.Parallel()
		got := captureWiredInstall(t, "--pod-context", podSwapContext, "--pod-namespace", podSwapNamespace)
		if got.opts.HostInstallSkipped != podHostInstallReason {
			t.Errorf("HostInstallSkipped = %q, want %q", got.opts.HostInstallSkipped, podHostInstallReason)
		}
		if !got.opts.PodTarget {
			t.Error("PodTarget = false, want true: the run's daemon runs as a pod, so the plan must not read the host's Dolt directories")
		}
		res, err := got.deps.Installer.Install(context.Background(), rollbackNewCommit)
		if err != nil {
			t.Fatalf("Install: %v", err)
		}
		if want := update.HostInstallSkipNote(podHostInstallReason); res.Reason != want {
			t.Errorf("install reason = %q, want %q", res.Reason, want)
		}
	})

	t.Run("a run without a pod target calls the host install first", func(t *testing.T) {
		t.Parallel()
		got := captureWiredInstall(t)
		if got.opts.HostInstallSkipped != "" {
			t.Errorf("HostInstallSkipped = %q, want none without a pod target", got.opts.HostInstallSkipped)
		}
		if got.opts.PodTarget {
			t.Error("PodTarget = true, want false: the run names no pod daemon")
		}
		res, err := got.deps.Installer.Install(context.Background(), rollbackNewCommit)
		if err != nil {
			t.Fatalf("Install: %v", err)
		}
		if got.host.calls != 1 {
			t.Errorf("host install called %d times, want it called once, first", got.host.calls)
		}
		if verdict := update.ClassifyInstall(res); verdict != update.ResultOK {
			t.Errorf("ClassifyInstall = %q, want %q: %+v", verdict, update.ResultOK, res)
		}
	})

}

// TestUpdatePodTargetWithoutImagesStillMarksThePlan: --skip install is the one
// pod-target run allowed without --images, so it carries no host-install skip
// note. The run must still tell the plan its daemon is a pod, or the plan asks
// the host's Dolt probes and the Job fails on a data directory the pod does not
// have.
func TestUpdatePodTargetWithoutImagesStillMarksThePlan(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	var got update.Options
	seams := updateSeams{
		FindTownRoot: func() (string, error) { return townRoot, nil },
		DepsFactory: func(string) (update.Deps, error) {
			return update.Deps{Store: &update.Store{Dir: update.Dir(townRoot)}, Clock: updateClock{}}, nil
		},
		Run: func(_ context.Context, _ update.Deps, opts update.Options) (update.State, error) {
			got = opts
			return update.State{Outcome: update.OutcomeOK}, nil
		},
		PodRun: func(context.Context, string, []string, string, ...string) (string, int, error) {
			return "", 0, nil
		},
	}

	if _, err := runPodCommand(t, seams, "--skip", "install", "--pod-context", podSwapContext, "--pod-namespace", podSwapNamespace); err != nil {
		t.Fatalf("gt update --skip install with the pod flags: %v", err)
	}
	if got.HostInstallSkipped != "" {
		t.Errorf("HostInstallSkipped = %q, want none without --images", got.HostInstallSkipped)
	}
	if !got.PodTarget {
		t.Error("PodTarget = false, want true: the run's daemon runs as a pod, so the plan must not read the host's Dolt directories")
	}
}

// podRunCounts counts the seams a gt update run reaches: the town lookup, the deps
// factory, the orchestrator, the host installer and the pod's kubectl runner. A
// refused run reaches none of them.
type podRunCounts struct {
	townLookups int
	depsCalls   int
	runs        int
	host        *podHostInstaller
	kube        *podDaemonKubectl
}

// runCountedPodCommand runs gt update with args over fakes that count each seam
// above. withImages adds --images with a values file, so a run gets past the
// refusal with its digest install wired; the fake orchestrator never calls it.
func runCountedPodCommand(t *testing.T, withImages bool, args ...string) (podRunCounts, error) {
	t.Helper()
	townRoot := t.TempDir()
	clock := &rollbackTestClock{now: time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)}
	compose := &rollbackTestSwap{}
	got := podRunCounts{
		host: &podHostInstaller{},
		kube: &podDaemonKubectl{
			running: rollbackNewCommit,
			commits: map[string]string{podSwapDigestRef: rollbackNewCommit},
			image:   podSwapOldDaemonRef,
			polecat: podSwapOldPolecatRef,
			pods:    podPairHealthy(),
		},
	}
	seams := updateSeams{
		FindTownRoot: func() (string, error) {
			got.townLookups++
			return townRoot, nil
		},
		DepsFactory: func(string) (update.Deps, error) {
			got.depsCalls++
			return update.Deps{
				Store:     &update.Store{Dir: update.Dir(townRoot)},
				Clock:     clock,
				Sleep:     clock.sleep,
				Verify:    compose,
				Installer: got.host,
			}, nil
		},
		Run: func(context.Context, update.Deps, update.Options) (update.State, error) {
			got.runs++
			return update.State{Outcome: update.OutcomeOK}, nil
		},
		ImageSwap: func(string) (update.ImageSwap, error) { return compose, nil },
		Notifier:  func(string) notify.Notifier { return notifyfake.New() },
		PodRun:    got.kube.run,
		Images:    podPairImages(podPairManifest),
		InPod:     func() bool { return true },
	}
	if withImages {
		values := filepath.Join(townRoot, "images.yaml")
		if err := imagevalues.Write(values, imagevalues.Values{Daemon: podSwapDigestRef, Polecat: podPairPolecatRef, Commit: rollbackNewCommit}); err != nil {
			t.Fatalf("write the values file: %v", err)
		}
		args = append([]string{"--images", values}, args...)
	}
	_, err := runPodCommand(t, seams, args...)
	return got, err
}

// TestUpdatePodTargetWithoutImagesIsRefused holds the refusal: a pod-target run
// without --images that selects the install step stops with the constant's text
// and exit status 1, for --pod-context and for --pod-in-cluster. A --dry-run and an
// explicit --only install are refused too, since both select the install step.
func TestUpdatePodTargetWithoutImagesIsRefused(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		args []string
	}{
		{name: "--pod-context", args: []string{"--pod-context", podSwapContext, "--pod-namespace", podSwapNamespace}},
		{name: "--pod-in-cluster", args: []string{"--pod-in-cluster", "--pod-namespace", podSwapNamespace}},
		{name: "--pod-context with --dry-run", args: []string{"--dry-run", "--pod-context", podSwapContext, "--pod-namespace", podSwapNamespace}},
		{name: "--pod-context with --only install", args: []string{"--only", "install", "--pod-context", podSwapContext, "--pod-namespace", podSwapNamespace}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := runCountedPodCommand(t, false, tt.args...)
			if err == nil {
				t.Fatalf("gt update %v ran, want it refused", tt.args)
			}
			if err.Error() != podImagesRequiredMessage {
				t.Errorf("error = %q, want %q", err.Error(), podImagesRequiredMessage)
			}
			if code := exitCodeForError(err); code != 1 {
				t.Errorf("exit status = %d, want 1", code)
			}
		})
	}
}

// TestUpdatePodTargetRefusalRunsNothing holds that the refusal comes before any
// step: the town lookup, the deps factory, the orchestrator, the host installer
// and the pod's kubectl runner are all unreached.
func TestUpdatePodTargetRefusalRunsNothing(t *testing.T) {
	t.Parallel()
	got, err := runCountedPodCommand(t, false, "--pod-context", podSwapContext, "--pod-namespace", podSwapNamespace)
	if err == nil {
		t.Fatal("gt update with a pod target and no --images ran, want it refused")
	}
	if got.townLookups != 0 {
		t.Errorf("town lookup called %d times, want none", got.townLookups)
	}
	if got.depsCalls != 0 {
		t.Errorf("deps factory called %d times, want none", got.depsCalls)
	}
	if got.runs != 0 {
		t.Errorf("orchestrator called %d times, want none", got.runs)
	}
	if got.host.calls != 0 {
		t.Errorf("host installer called %d times, want none", got.host.calls)
	}
	if len(got.kube.calls) != 0 {
		t.Errorf("pod runner ran %d commands, want none: %v", len(got.kube.calls), got.kube.calls)
	}
}

// TestUpdatePodTargetWithoutInstallRefusalRuns holds the runs the refusal leaves
// alone: a pod target with an image pair, a pod target that skips or does not
// select the install step, and a run with no pod target. Each reaches the
// orchestrator.
func TestUpdatePodTargetWithoutInstallRefusalRuns(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		withImages bool
		args       []string
	}{
		{name: "a pod target with --images", withImages: true, args: []string{"--pod-context", podSwapContext, "--pod-namespace", podSwapNamespace}},
		{name: "--pod-context with --skip install", args: []string{"--skip", "install", "--pod-context", podSwapContext, "--pod-namespace", podSwapNamespace}},
		{name: "--pod-context with --only verify", args: []string{"--only", "verify", "--pod-context", podSwapContext, "--pod-namespace", podSwapNamespace}},
		{name: "--pod-in-cluster with --only verify", args: []string{"--only", "verify", "--pod-in-cluster", "--pod-namespace", podSwapNamespace}},
		{name: "no pod target", args: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := runCountedPodCommand(t, tt.withImages, tt.args...)
			if err != nil {
				t.Fatalf("gt update %v was refused or failed: %v", tt.args, err)
			}
			if got.runs != 1 {
				t.Errorf("orchestrator called %d times, want it called once", got.runs)
			}
		})
	}
}

// TestUpdateImagesSubcommandIsNotRefused holds gt update images, which writes the
// values file the refusal names. It is a subcommand, not a run: runUpdate's refusal
// never sees it, and it takes none of the pod flags.
func TestUpdateImagesSubcommandIsNotRefused(t *testing.T) {
	t.Parallel()
	_, server := startFakeRegistry(t, map[string]*fakeRegistryImage{
		imagevalues.DaemonImage:  matchingContents(testUpdateImagesSHA),
		imagevalues.PolecatImage: matchingContents(testUpdateImagesSHA),
	})
	writes := 0
	out := filepath.Join(t.TempDir(), "images.yaml")
	if _, err := runPodCommand(t, updateSeams{Images: testUpdateImagesSeams(t, server, &writes)}, "images", "--sha", testUpdateImagesSHA, "--out", out); err != nil {
		t.Fatalf("gt update images was refused: %v", err)
	}
	if writes != 1 {
		t.Errorf("the values file was written %d times, want once", writes)
	}
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

// TestUpdatePodHelpSaysThePodInstallNeedsImages: gt update --help says a pod
// target's install needs --images, since a run that would install without one
// is refused. Without it an operator reads the pod flags and expects the host
// install to move the pod's daemon.
func TestUpdatePodHelpSaysThePodInstallNeedsImages(t *testing.T) {
	t.Parallel()
	seams := updateSeams{FindTownRoot: func() (string, error) { return t.TempDir(), nil }}
	out, err := runPodCommand(t, seams, "--help")
	if err != nil {
		t.Fatalf("gt update --help: %v", err)
	}
	want := "needs --images"
	if !strings.Contains(out, want) {
		t.Errorf("gt update help does not say the pod install needs --images; want %q in:\n%s", want, out)
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
		if reading, ok := inst.Images.Running.(podDaemonSwap); !ok {
			t.Errorf("running pair read = %T, want the pod swap: only the Deployment can say which pair ran before", inst.Images.Running)
		} else if reading.Namespace != podSwapNamespace {
			t.Errorf("running pair read names namespace %q, want %q", reading.Namespace, podSwapNamespace)
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
		// --skip install keeps the install step out of the plan, so the pod-target
		// refusal does not fire first and the missing runner is what is refused.
		_, err := runPodCommand(t, seams, "--skip", "install", "--pod-context", podSwapContext, "--pod-namespace", podSwapNamespace)
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
		if _, ok := inst.Images.Running.(update.RecordedRunningPair); !ok {
			t.Errorf("running pair read = %T, want the container daemon's record", inst.Images.Running)
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
				InPod: func() bool { return true },
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

// TestUpdatePodHalfMovedPairIsRestoredToThePrevious holds a pair that moves the
// daemon and then fails on the dashboard: the install fails, and the restore
// moves both Deployments back to the recorded previous pair, which the ledger then
// names as running. The daemon is left on the new pair until the restore, so the
// restore must reach the dashboard too.
func TestUpdatePodHalfMovedPairIsRestoredToThePrevious(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		failOnce string
		wantStep string
	}{
		{name: "a failed dashboard patch", failOnce: "patch deployment/" + k8sdaemon.DashboardName, wantStep: "the dashboard's patch exited 1"},
		{name: "a failed dashboard rollout", failOnce: "rollout status deployment/" + k8sdaemon.DashboardName, wantStep: "the dashboard's rollout status exited 1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := captureWiredInstall(t, "--pod-context", podSwapContext, "--pod-namespace", podSwapNamespace)
			oldDaemon := "registry.internal:5000/town/gastown-daemon@sha256:" + strings.Repeat("d", 64)
			oldPolecat := "registry.internal:5000/town/gastown-polecat@sha256:" + strings.Repeat("a", 64)
			previous := update.ImagePair{Daemon: oldDaemon, Polecat: oldPolecat, Commit: rollbackOldCommit}
			got.kube.commits[oldDaemon] = rollbackOldCommit
			got.kube.image, got.kube.polecat = oldDaemon, oldPolecat
			got.kube.failOnce = tt.failOnce
			ledger := update.FileImageLedger{Dir: update.Dir(got.townRoot)}
			if err := ledger.Write(update.ImageRecord{Current: previous, Previous: previous}); err != nil {
				t.Fatalf("record the previous pair: %v", err)
			}

			res, err := got.deps.Installer.Install(context.Background(), rollbackNewCommit)
			if err != nil {
				t.Fatalf("Install: %v", err)
			}
			if verdict := update.ClassifyInstall(res); verdict == update.ResultOK {
				t.Fatalf("ClassifyInstall = %q, want the failed step to fail the install: %+v", verdict, res)
			}
			if !strings.Contains(res.Reason, tt.wantStep) || !strings.Contains(res.Reason, "restored "+oldDaemon) {
				t.Errorf("install reason = %q, want it to name %q and the restored daemon", res.Reason, tt.wantStep)
			}

			// lastPatch decodes the last patch the run sent to a Deployment. A patch
			// body is the last argument of its command.
			lastPatch := func(deployment string) podSwapPatchBody {
				t.Helper()
				for i := len(got.kube.calls) - 1; i >= 0; i-- {
					args := got.kube.calls[i]
					if containsArg(args, "patch") && containsArg(args, "deployment/"+deployment) {
						var body podSwapPatchBody
						if err := json.Unmarshal([]byte(args[len(args)-1]), &body); err != nil {
							t.Fatalf("patch body is not JSON: %v", err)
						}
						return body
					}
				}
				t.Fatalf("no patch of deployment/%s was run: calls = %v", deployment, got.kube.calls)
				return podSwapPatchBody{}
			}
			if c := lastPatch(k8sdaemon.Name).Spec.Template.Spec.Containers; len(c) != 1 || c[0].Image != oldDaemon {
				t.Errorf("last daemon patch containers = %+v, want the previous daemon %q", c, oldDaemon)
			}
			if annotation := lastPatch(k8sdaemon.Name).Spec.Template.Metadata.Annotations[k8sdaemon.PolecatImageAnnotation]; annotation != oldPolecat {
				t.Errorf("last daemon patch annotation = %q, want the previous polecat %q", annotation, oldPolecat)
			}
			if c := lastPatch(k8sdaemon.DashboardName).Spec.Template.Spec.Containers; len(c) != 1 || c[0].Image != oldDaemon {
				t.Errorf("last dashboard patch containers = %+v, want the previous daemon %q", c, oldDaemon)
			}
			// The restored pair is the one read from the Deployment, which names no
			// commit: the record holds the images the daemon runs.
			restored := update.ImagePair{Daemon: oldDaemon, Polecat: oldPolecat}
			if rec, err := ledger.Read(); err != nil || !samePairs(rec, update.ImageRecord{Current: restored, Previous: restored}) {
				t.Errorf("ledger after the restore = %+v (err %v), want the Deployment's pair as both current and previous", rec, err)
			}
		})
	}
}

// TestPodDaemonFromFlagsInCluster: --pod-in-cluster with a namespace names the
// Deployment with no context; without a namespace, or with a context, it is
// refused. The together rule still holds without it.
func TestPodDaemonFromFlagsInCluster(t *testing.T) {
	t.Parallel()

	t.Run("with a namespace the target is in-cluster and names no context", func(t *testing.T) {
		t.Parallel()
		got, err := podDaemonFromFlags("", " "+podSwapNamespace+" ", true, true)
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
			got, err := podDaemonFromFlags(tc.kubeContext, tc.ns, true, true)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("podDaemonFromFlags error = %v, want one containing %q", err, tc.wantErr)
			}
			if got != nil {
				t.Errorf("target = %+v, want nil with the error", *got)
			}
		})
	}

	t.Run("outside a pod it is refused before the other checks", func(t *testing.T) {
		t.Parallel()
		got, err := podDaemonFromFlags("", podSwapNamespace, true, false)
		if err == nil || !strings.Contains(err.Error(), "runs only inside a pod") {
			t.Fatalf("podDaemonFromFlags error = %v, want the outside-a-pod refusal", err)
		}
		if got != nil {
			t.Errorf("target = %+v, want nil with the error", *got)
		}
	})

	t.Run("without the flag the together rule still holds", func(t *testing.T) {
		t.Parallel()
		if got, err := podDaemonFromFlags("", "", false, false); got != nil || err != nil {
			t.Errorf("no flags = %+v, %v; want nil and no error", got, err)
		}
		if _, err := podDaemonFromFlags(podSwapContext, "", false, false); err == nil || !strings.Contains(err.Error(), "must be given together") {
			t.Errorf("context alone error = %v, want the together refusal", err)
		}
		if _, err := podDaemonFromFlags("", podSwapNamespace, false, false); err == nil || !strings.Contains(err.Error(), "must be given together") {
			t.Errorf("namespace alone error = %v, want the together refusal", err)
		}
	})
}

// TestRunsInPod: a process runs in a pod when the environment names the variable
// Kubernetes sets in every pod, and not otherwise.
func TestRunsInPod(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		value string
		want  bool
	}{
		{name: "unset is not a pod", value: "", want: false},
		{name: "set names a pod", value: "10.0.0.1", want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			getenv := func(name string) string {
				if name == podEnv {
					return tc.value
				}
				return ""
			}
			if got := runsInPod(getenv); got != tc.want {
				t.Errorf("runsInPod = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestPodInClusterPassesTheGuardInAPod: gt update --pod-in-cluster in a pod is
// not refused by the guard; the run goes on to find the town. --skip install
// keeps the images refusal out of this run, so only the in-pod guard is tested.
func TestPodInClusterPassesTheGuardInAPod(t *testing.T) {
	t.Parallel()
	seams := updateSeams{
		FindTownRoot: func() (string, error) { return "", errors.New("the town was reached") },
		InPod:        func() bool { return true },
	}
	_, err := runPodCommand(t, seams, "--skip", "install", "--pod-in-cluster", "--pod-namespace", podSwapNamespace)
	if err == nil || !strings.Contains(err.Error(), "the town was reached") {
		t.Fatalf("gt update --pod-in-cluster in a pod error = %v, want the run to reach the town", err)
	}
}

// TestPodInClusterRefusedOutsideAPod: gt update and gt update rollback given
// --pod-in-cluster outside a pod are refused before the town is found or any
// kubectl command runs, so an ambient kubeconfig never moves a Deployment.
func TestPodInClusterRefusedOutsideAPod(t *testing.T) {
	t.Parallel()
	commands := map[string][]string{
		"update":   {"--pod-in-cluster", "--pod-namespace", podSwapNamespace},
		"rollback": {"rollback", "--pod-in-cluster", "--pod-namespace", podSwapNamespace},
	}
	for name, args := range commands {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			kube := &podDaemonKubectl{}
			seams := updateSeams{
				FindTownRoot: func() (string, error) { return "", errors.New("the town was reached") },
				InPod:        func() bool { return false },
				PodRun:       kube.run,
			}
			_, err := runPodCommand(t, seams, args...)
			if err == nil || !strings.Contains(err.Error(), "runs only inside a pod") {
				t.Fatalf("%s error = %v, want the outside-a-pod refusal", name, err)
			}
			if len(kube.calls) != 0 {
				t.Errorf("kubectl ran %v, want no command", kube.calls)
			}
		})
	}
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
		InPod:     func() bool { return true },
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

// TestUpdatePodInstalledProbeReadsTheDaemonPod: the plan's installed build for a
// pod target is the build the daemon's pod runs, read through kubectl exec, not
// the build of the process running the update (the update Job runs the NEW
// image, so its own build would read as already installed, gt-h36j1y).
func TestUpdatePodInstalledProbeReadsTheDaemonPod(t *testing.T) {
	t.Parallel()
	run := func(context.Context, string, []string, string, ...string) (string, int, error) {
		return "gt version " + rollbackNewCommit + " built on test\n", 0, nil
	}
	own := stubPlanProbes{installed: "the-jobs-own-build"}
	deps, err := podDaemonDeps(updateSeams{PodRun: run}, update.Deps{Probes: own}, &podDaemonTarget{Context: podSwapContext, Namespace: podSwapNamespace, Deployment: k8sdaemon.Name})
	if err != nil {
		t.Fatalf("podDaemonDeps: %v", err)
	}
	got, err := deps.Probes.InstalledSHA(context.Background())
	if err != nil {
		t.Fatalf("InstalledSHA: %v", err)
	}
	if got != rollbackNewCommit {
		t.Errorf("InstalledSHA = %q, want the pod's build %q", got, rollbackNewCommit)
	}
	if main, _ := deps.Probes.MainSHA(context.Background()); main != own.main {
		t.Errorf("MainSHA = %q, want the run's own probe %q", main, own.main)
	}
}

// stubPlanProbes is update.Probes with fixed answers, for the pod probe test.
type stubPlanProbes struct{ installed, main string }

func (s stubPlanProbes) InstalledSHA(context.Context) (string, error) { return s.installed, nil }
func (s stubPlanProbes) MainSHA(context.Context) (string, error)      { return s.main, nil }
func (s stubPlanProbes) ArchiveBehind(context.Context) (int, int, error) {
	return 0, 0, nil
}
func (s stubPlanProbes) BackupToday(context.Context) (bool, error) { return true, nil }
func (s stubPlanProbes) GCDue(context.Context) (bool, string, error) {
	return false, "", nil
}
func (s stubPlanProbes) ImagePublished(context.Context, string) (bool, error) { return true, nil }

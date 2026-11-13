package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	k8sdaemon "github.com/steveyegge/gastown/deploy/k8s/daemon"
	"github.com/steveyegge/gastown/internal/update"
)

// podSwapOutput is the text a fake kubectl prints. It stands in for a command
// output that can hold a secret, so no error may carry it.
const podSwapOutput = "secret-token-in-output"

// podSwapCall is one command the fake runner was asked to run.
type podSwapCall struct {
	name string
	args []string
}

// podSwapFake records the commands a swap runs. The verb field names the
// kubectl subcommand that fails; exit and err are what it returns. A deployment
// set narrows the failure to commands that name that Deployment.
type podSwapFake struct {
	calls      []podSwapCall
	verb       string
	deployment string
	exit       int
	err        error
}

func (f *podSwapFake) run(_ context.Context, _ string, _ []string, name string, args ...string) (string, int, error) {
	f.calls = append(f.calls, podSwapCall{name: name, args: args})
	if f.deployment != "" && !containsArg(args, "deployment/"+f.deployment) {
		return podSwapOutput, 0, nil
	}
	if containsArg(args, f.verb) {
		return podSwapOutput, f.exit, f.err
	}
	return podSwapOutput, 0, nil
}

const (
	podSwapContext   = "control-cluster"
	podSwapNamespace = "gt"
)

// podSwapDigestRef is a daemon digest reference, the form a digest install records.
var podSwapDigestRef = "registry.internal:5000/town/gastown-daemon@sha256:" + strings.Repeat("e", 64)

// podSwapPolecatRef is the polecat half of the pair. The one patch carries it as
// the pod template's annotation.
var podSwapPolecatRef = "registry.internal:5000/town/gastown-polecat@sha256:" + strings.Repeat("f", 64)

// podSwapPair is a complete pair, both halves digests.
var podSwapPair = update.ImagePair{Daemon: podSwapDigestRef, Polecat: podSwapPolecatRef}

// podSwapFor is a swap wired to fake, with every required field set.
func podSwapFor(fake *podSwapFake) podDaemonSwap {
	return podDaemonSwap{
		Context:        podSwapContext,
		Namespace:      podSwapNamespace,
		Run:            fake.run,
		RolloutTimeout: 5 * time.Minute,
	}
}

// podSwapPatchBody is the strategic-merge patch a swap sends, decoded so a test
// reads what the patch sets rather than matching its bytes.
type podSwapPatchBody struct {
	Spec struct {
		Template struct {
			Metadata struct {
				Annotations map[string]string `json:"annotations"`
			} `json:"metadata"`
			Spec struct {
				Containers []struct {
					Name  string `json:"name"`
					Image string `json:"image"`
				} `json:"containers"`
			} `json:"spec"`
		} `json:"template"`
	} `json:"spec"`
}

// TestUpdatePodSwapMovesBothDeploymentsInOrder holds the swap's four commands: the
// daemon's patch, the dashboard's patch, then the wait on each rollout. The daemon's
// patch carries the polecat annotation; the dashboard's carries only the daemon
// image, and no annotation, so the annotations it already has stay.
func TestUpdatePodSwapMovesBothDeploymentsInOrder(t *testing.T) {
	t.Parallel()
	fake := &podSwapFake{}

	if err := podSwapFor(fake).Apply(context.Background(), podSwapPair); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if len(fake.calls) != 4 {
		t.Fatalf("commands = %+v, want the daemon's patch, the dashboard's patch, then both rollout waits", fake.calls)
	}
	prefix := []string{"--context", podSwapContext, "-n", podSwapNamespace}
	patchArgs := func(deployment string) []string {
		return append(slices.Clone(prefix), "patch", "deployment/"+deployment, "--type=strategic", "-p")
	}
	rolloutArgs := func(deployment string) []string {
		return append(slices.Clone(prefix), "rollout", "status", "deployment/"+deployment, "--timeout=5m0s")
	}
	checkPatch := func(t *testing.T, args []string, deployment string) podSwapPatchBody {
		t.Helper()
		shape := patchArgs(deployment)
		if len(args) != len(shape)+1 || !slices.Equal(args[:len(shape)], shape) {
			t.Fatalf("patch argv = %q, want %q and one body", args, shape)
		}
		var body podSwapPatchBody
		if err := json.Unmarshal([]byte(args[len(shape)]), &body); err != nil {
			t.Fatalf("patch body is not JSON: %v", err)
		}
		return body
	}

	daemon := checkPatch(t, fake.calls[0].args, k8sdaemon.Name)
	if got := daemon.Spec.Template.Metadata.Annotations[k8sdaemon.PolecatImageAnnotation]; got != podSwapPolecatRef {
		t.Errorf("polecat annotation = %q, want the pair's polecat digest", got)
	}
	if c := daemon.Spec.Template.Spec.Containers; len(c) != 1 || c[0].Name != k8sdaemon.Name || c[0].Image != podSwapDigestRef {
		t.Errorf("daemon containers = %+v, want %s on the pair's daemon digest alone", c, k8sdaemon.Name)
	}

	dashboard := checkPatch(t, fake.calls[1].args, k8sdaemon.DashboardName)
	if c := dashboard.Spec.Template.Spec.Containers; len(c) != 1 || c[0].Name != k8sdaemon.DashboardName || c[0].Image != podSwapDigestRef {
		t.Errorf("dashboard containers = %+v, want %s on the pair's daemon digest alone", c, k8sdaemon.DashboardName)
	}
	if got := dashboard.Spec.Template.Metadata.Annotations; len(got) != 0 {
		t.Errorf("dashboard annotations = %v, want none, so the pod template's own annotations are kept", got)
	}

	if !slices.Equal(fake.calls[2].args, rolloutArgs(k8sdaemon.Name)) {
		t.Errorf("third command = %q, want the daemon's rollout wait %q", fake.calls[2].args, rolloutArgs(k8sdaemon.Name))
	}
	if !slices.Equal(fake.calls[3].args, rolloutArgs(k8sdaemon.DashboardName)) {
		t.Errorf("fourth command = %q, want the dashboard's rollout wait %q", fake.calls[3].args, rolloutArgs(k8sdaemon.DashboardName))
	}
}

// containsArg reports whether args holds exactly arg.
func containsArg(args []string, arg string) bool {
	for _, a := range args {
		if a == arg {
			return true
		}
	}
	return false
}

func TestPodDaemonSwapAcceptsANameAtTheDNSLabelLimit(t *testing.T) {
	t.Parallel()
	fake := &podSwapFake{}
	swap := podSwapFor(fake)
	swap.Namespace = strings.Repeat("a", 63)
	swap.Deployment = strings.Repeat("b", 63)

	if err := swap.Apply(context.Background(), podSwapPair); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(fake.calls) != 4 {
		t.Errorf("commands = %d, want 4 at the limit", len(fake.calls))
	}
}

func TestPodDaemonSwapUsesTheNamedDeployment(t *testing.T) {
	t.Parallel()
	fake := &podSwapFake{}
	swap := podSwapFor(fake)
	swap.Deployment = "daemon-canary"

	if err := swap.Apply(context.Background(), podSwapPair); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	// The daemon's patch and rollout wait name the configured Deployment; the
	// dashboard's patch and wait name the dashboard's, whatever the daemon is.
	for _, i := range []int{0, 2} {
		if !containsArg(fake.calls[i].args, "deployment/daemon-canary") {
			t.Errorf("command %v does not name the configured Deployment", fake.calls[i].args)
		}
	}
	for _, i := range []int{1, 3} {
		if !containsArg(fake.calls[i].args, "deployment/"+k8sdaemon.DashboardName) {
			t.Errorf("command %v does not name the dashboard Deployment", fake.calls[i].args)
		}
	}
}

func TestPodDaemonSwapRefusesAnUnusableTargetOrPair(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		pair    update.ImagePair
		mutate  func(*podDaemonSwap)
		wantErr string
	}{
		{
			name:    "a daemon tag reference is refused",
			pair:    update.ImagePair{Daemon: "registry.internal:5000/town/gastown-daemon:latest", Polecat: podSwapPolecatRef},
			wantErr: "is not a digest reference",
		},
		{
			name:    "a daemon digest with a short hash is refused",
			pair:    update.ImagePair{Daemon: "registry.internal:5000/town/gastown-daemon@sha256:abc", Polecat: podSwapPolecatRef},
			wantErr: "is not a digest reference",
		},
		{
			name:    "a polecat tag reference is refused, since the annotation would name a tag",
			pair:    update.ImagePair{Daemon: podSwapDigestRef, Polecat: "registry.internal:5000/town/gastown-polecat:latest"},
			wantErr: "polecat image",
		},
		{
			name:    "an empty polecat is refused",
			pair:    update.ImagePair{Daemon: podSwapDigestRef},
			wantErr: "polecat image",
		},
		{
			name:    "an empty context is refused",
			pair:    podSwapPair,
			mutate:  func(s *podDaemonSwap) { s.Context = "" },
			wantErr: "no usable kubectl context",
		},
		{
			name:    "a context that reads as a kubectl flag is refused",
			pair:    podSwapPair,
			mutate:  func(s *podDaemonSwap) { s.Context = "--cluster=control" },
			wantErr: "no usable kubectl context",
		},
		{
			name:    "an empty namespace is refused",
			pair:    podSwapPair,
			mutate:  func(s *podDaemonSwap) { s.Namespace = "" },
			wantErr: "is not a Kubernetes namespace name",
		},
		{
			name:    "a namespace with an uppercase letter is refused",
			pair:    podSwapPair,
			mutate:  func(s *podDaemonSwap) { s.Namespace = "GT" },
			wantErr: "is not a Kubernetes namespace name",
		},
		{
			name:    "a namespace one character past the DNS label limit is refused",
			pair:    podSwapPair,
			mutate:  func(s *podDaemonSwap) { s.Namespace = strings.Repeat("a", 64) },
			wantErr: "is not a Kubernetes namespace name",
		},
		{
			name:    "a deployment that is not a Kubernetes name is refused",
			pair:    podSwapPair,
			mutate:  func(s *podDaemonSwap) { s.Deployment = "daemon/x" },
			wantErr: "is not a Kubernetes name",
		},
		{
			name:    "a deployment one character past the DNS label limit is refused",
			pair:    podSwapPair,
			mutate:  func(s *podDaemonSwap) { s.Deployment = strings.Repeat("a", 64) },
			wantErr: "is not a Kubernetes name",
		},
		{
			name:    "a zero rollout timeout is refused",
			pair:    podSwapPair,
			mutate:  func(s *podDaemonSwap) { s.RolloutTimeout = 0 },
			wantErr: "has no rollout timeout",
		},
		{
			name:    "a negative rollout timeout is refused",
			pair:    podSwapPair,
			mutate:  func(s *podDaemonSwap) { s.RolloutTimeout = -time.Second },
			wantErr: "has no rollout timeout",
		},
		{
			name:    "a swap with no runner is refused",
			pair:    podSwapPair,
			mutate:  func(s *podDaemonSwap) { s.Run = nil },
			wantErr: "no command runner wired",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fake := &podSwapFake{}
			swap := podSwapFor(fake)
			if tt.mutate != nil {
				tt.mutate(&swap)
			}
			err := swap.Apply(context.Background(), tt.pair)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Apply error = %v, want one containing %q", err, tt.wantErr)
			}
			if len(fake.calls) != 0 {
				t.Errorf("commands = %+v, want none before the refusal", fake.calls)
			}
		})
	}
}

func TestPodDaemonSwapStopsWhenThePatchCannotEncode(t *testing.T) {
	t.Parallel()
	fake := &podSwapFake{}
	swap := podSwapFor(fake)
	swap.marshal = func(any) ([]byte, error) { return nil, errors.New("encode refused") }

	err := swap.Apply(context.Background(), podSwapPair)
	if err == nil || !strings.Contains(err.Error(), "encoding the daemon's pod patch") {
		t.Fatalf("Apply error = %v, want the patch encoding named", err)
	}
	if len(fake.calls) != 0 {
		t.Errorf("commands = %+v, want none when the patch cannot be built", fake.calls)
	}
}

// TestPodDaemonSwapStopsWhenTheDashboardPatchCannotEncode holds the second body's
// encoding: the daemon's body encodes, the dashboard's does not, and no command
// runs, so the daemon is not moved before the dashboard's patch is known to build.
func TestPodDaemonSwapStopsWhenTheDashboardPatchCannotEncode(t *testing.T) {
	t.Parallel()
	fake := &podSwapFake{}
	swap := podSwapFor(fake)
	encodes := 0
	swap.marshal = func(v any) ([]byte, error) {
		encodes++
		if encodes > 1 {
			return nil, errors.New("encode refused")
		}
		return json.Marshal(v)
	}

	err := swap.Apply(context.Background(), podSwapPair)
	if err == nil || !strings.Contains(err.Error(), "encoding the dashboard's pod patch") {
		t.Fatalf("Apply error = %v, want the dashboard patch encoding named", err)
	}
	if len(fake.calls) != 0 {
		t.Errorf("commands = %+v, want none when the dashboard patch cannot be built", fake.calls)
	}
}

func TestPodDaemonSwapFailureNamesTheStepNotTheOutput(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		fake      podSwapFake
		wantStep  string
		wantCalls int
	}{
		{
			name:      "a failing daemon patch stops before the dashboard is patched",
			fake:      podSwapFake{verb: "patch", deployment: k8sdaemon.Name, exit: 1},
			wantStep:  "the daemon's patch exited 1",
			wantCalls: 1,
		},
		{
			name:      "a failing dashboard patch stops before the rollout waits",
			fake:      podSwapFake{verb: "patch", deployment: k8sdaemon.DashboardName, exit: 1},
			wantStep:  "the dashboard's patch exited 1",
			wantCalls: 2,
		},
		{
			name:      "a failing daemon rollout stops before the dashboard's wait",
			fake:      podSwapFake{verb: "rollout", deployment: k8sdaemon.Name, exit: 1},
			wantStep:  "the daemon's rollout status exited 1",
			wantCalls: 3,
		},
		{
			name:      "a failing dashboard rollout is named as the dashboard's rollout",
			fake:      podSwapFake{verb: "rollout", deployment: k8sdaemon.DashboardName, exit: 1},
			wantStep:  "the dashboard's rollout status exited 1",
			wantCalls: 4,
		},
		{
			name:      "a daemon patch command that cannot start is named as the daemon's patch",
			fake:      podSwapFake{verb: "patch", deployment: k8sdaemon.Name, err: errors.New("kubectl not found")},
			wantStep:  "the daemon's patch",
			wantCalls: 1,
		},
		{
			name:      "a dashboard patch command that cannot start is named as the dashboard's patch",
			fake:      podSwapFake{verb: "patch", deployment: k8sdaemon.DashboardName, err: errors.New("kubectl not found")},
			wantStep:  "the dashboard's patch",
			wantCalls: 2,
		},
		{
			name:      "a rollout command that cannot start is named as the rollout",
			fake:      podSwapFake{verb: "rollout", deployment: k8sdaemon.Name, err: errors.New("kubectl not found")},
			wantStep:  "the daemon's rollout status",
			wantCalls: 3,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fake := tt.fake
			err := podSwapFor(&fake).Apply(context.Background(), podSwapPair)
			if err == nil || !strings.Contains(err.Error(), tt.wantStep) {
				t.Fatalf("Apply error = %v, want one naming %q", err, tt.wantStep)
			}
			if strings.Contains(err.Error(), podSwapOutput) {
				t.Errorf("Apply error carries the command's output: %v", err)
			}
			if len(fake.calls) != tt.wantCalls {
				t.Errorf("commands = %d, want %d: the swap must stop at the failed step", len(fake.calls), tt.wantCalls)
			}
		})
	}
}

// TestPodDaemonSwapInClusterOmitsContext: an InCluster swap runs the patch and
// the rollout wait with the namespace and no --context, and a swap that names
// neither a context nor InCluster, or both, is refused before any command runs.
func TestPodDaemonSwapInClusterOmitsContext(t *testing.T) {
	t.Parallel()

	t.Run("one Apply patches and waits with the namespace and no --context", func(t *testing.T) {
		t.Parallel()
		fake := &podSwapFake{}
		swap := podSwapFor(fake)
		swap.Context = ""
		swap.InCluster = true

		if err := swap.Apply(context.Background(), podSwapPair); err != nil {
			t.Fatalf("Apply: %v", err)
		}
		if len(fake.calls) != 4 {
			t.Fatalf("commands = %+v, want both patches then both rollout waits", fake.calls)
		}
		for _, c := range fake.calls {
			if containsArg(c.args, "--context") {
				t.Errorf("command %v carries --context, want none in-cluster", c.args)
			}
			if !slices.Equal(c.args[:2], []string{"-n", podSwapNamespace}) {
				t.Errorf("command %v does not start with the namespace", c.args)
			}
		}
		if !containsArg(fake.calls[0].args, "patch") || !containsArg(fake.calls[1].args, "patch") {
			t.Errorf("first two commands %v are not the patches", fake.calls[:2])
		}
		if !containsArg(fake.calls[2].args, "rollout") || !containsArg(fake.calls[3].args, "rollout") {
			t.Errorf("last two commands %v are not the rollout waits", fake.calls[2:])
		}
	})

	t.Run("a swap with neither a context nor InCluster is refused before any command runs", func(t *testing.T) {
		t.Parallel()
		fake := &podSwapFake{}
		swap := podSwapFor(fake)
		swap.Context = ""

		if err := swap.Apply(context.Background(), podSwapPair); err == nil {
			t.Fatal("Apply accepted a swap with no context, want an error")
		}
		if len(fake.calls) != 0 {
			t.Errorf("commands run = %+v, want none", fake.calls)
		}
	})

	t.Run("a swap that names both a context and InCluster is refused before any command runs", func(t *testing.T) {
		t.Parallel()
		fake := &podSwapFake{}
		swap := podSwapFor(fake)
		swap.InCluster = true

		if err := swap.Apply(context.Background(), podSwapPair); err == nil || !strings.Contains(err.Error(), "both a kubectl context and the in-cluster") {
			t.Fatalf("Apply error = %v, want the context and in-cluster refusal", err)
		}
		if len(fake.calls) != 0 {
			t.Errorf("commands run = %+v, want none", fake.calls)
		}
	})
}

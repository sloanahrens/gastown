package cmd

import (
	"bytes"
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
//
// The dashboard's existence read (a get) answers present by default. absent makes
// it answer that the Deployment is not there. readExit and readErr fail the read,
// and readOut, when set, is the read's whole answer.
//
// The running pair read (a get of the daemon Deployment) answers the swap's pair
// by default. pairOut, when set, is its whole answer, and pairExit and pairErr
// fail it.
//
// The bd gateway reads (a get of the gateway Deployment, once for its name and
// once for its image) name no gateway by default, as a cluster before the
// cutover: kubectl prints nothing for --ignore-not-found. gateway, when set, is
// the image the Deployment runs and the name the existence read prints;
// gatewayOut is the image read's whole answer, gatewayNameOut the existence
// read's, and gatewayExit and gatewayErr fail either.
type podSwapFake struct {
	calls          []podSwapCall
	verb           string
	deployment     string
	exit           int
	err            error
	absent         bool
	readExit       int
	readErr        error
	readOut        string
	pairOut        string
	pairExit       int
	pairErr        error
	gateway        string
	gatewayOut     string
	gatewayNameOut string
	gatewayExit    int
	gatewayErr     error
}

func (f *podSwapFake) run(_ context.Context, _ string, _ []string, name string, args ...string) (string, int, error) {
	f.calls = append(f.calls, podSwapCall{name: name, args: args})
	if containsArg(args, "get") && containsArg(args, "deployment/"+k8sdaemon.Name) {
		if f.pairErr != nil || f.pairExit != 0 {
			return podSwapOutput, f.pairExit, f.pairErr
		}
		return f.pairAnswer(), 0, nil
	}
	if containsArg(args, "get") && containsArg(args, "deployment/"+k8sdaemon.BdGatewayName) {
		if f.gatewayErr != nil || f.gatewayExit != 0 {
			return podSwapOutput, f.gatewayExit, f.gatewayErr
		}
		if containsArg(args, "name") {
			return f.gatewayNameAnswer(), 0, nil
		}
		return f.gatewayAnswer(), 0, nil
	}
	if containsArg(args, "get") {
		if f.readErr != nil || f.readExit != 0 {
			return podSwapOutput, f.readExit, f.readErr
		}
		return f.readAnswer(), 0, nil
	}
	if f.deployment != "" && !containsArg(args, "deployment/"+f.deployment) {
		return podSwapOutput, 0, nil
	}
	if containsArg(args, f.verb) {
		return podSwapOutput, f.exit, f.err
	}
	return podSwapOutput, 0, nil
}

// readAnswer is what the existence read prints: the dashboard's name when it
// exists, nothing when it does not, and readOut when a test sets one.
func (f *podSwapFake) readAnswer() string {
	switch {
	case f.readOut != "":
		return f.readOut
	case f.absent:
		return ""
	default:
		return "deployment.apps/" + k8sdaemon.DashboardName + "\n"
	}
}

// pairAnswer is what the running pair read prints: pairOut when a test sets one,
// else the pair podSwapPair names, as the two lines the read's jsonpath prints.
func (f *podSwapFake) pairAnswer() string {
	if f.pairOut != "" {
		return f.pairOut
	}
	return podSwapPair.Daemon + "\n" + podSwapPair.Polecat
}

// gatewayAnswer is what the bd gateway image read prints: gatewayOut when a test
// sets one, nothing when the cluster has no gateway Deployment, else the
// Deployment's name and gateway, the two lines the read's jsonpath prints.
func (f *podSwapFake) gatewayAnswer() string {
	switch {
	case f.gatewayOut != "":
		return f.gatewayOut
	case f.gateway == "":
		return ""
	default:
		return k8sdaemon.BdGatewayName + "\n" + f.gateway
	}
}

// gatewayNameAnswer is what the bd gateway existence read prints: gatewayNameOut
// when a test sets one, nothing when the cluster has no gateway Deployment, else
// the Deployment's name, which kubectl prints for -o name.
func (f *podSwapFake) gatewayNameAnswer() string {
	switch {
	case f.gatewayNameOut != "":
		return f.gatewayNameOut
	case f.gateway == "":
		return ""
	default:
		return "deployment.apps/" + k8sdaemon.BdGatewayName + "\n"
	}
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

// TestUpdatePodSwapMovesBothDeploymentsInOrder holds the swap's six commands: the
// dashboard's existence read, the bd gateway's read (this cluster has no gateway,
// so the gateway is not moved), the daemon's patch, the dashboard's patch, then
// the wait on each rollout. The daemon's patch carries the polecat annotation; the
// dashboard's carries only the daemon image, and no annotation, so the annotations
// it already has stay.
func TestUpdatePodSwapMovesBothDeploymentsInOrder(t *testing.T) {
	t.Parallel()
	fake := &podSwapFake{}

	if err := podSwapFor(fake).Apply(context.Background(), podSwapPair); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if len(fake.calls) != 6 {
		t.Fatalf("commands = %+v, want both existence reads, the daemon's patch, the dashboard's patch, then both rollout waits", fake.calls)
	}
	prefix := []string{"--context", podSwapContext, "-n", podSwapNamespace}
	if want := append(slices.Clone(prefix), "get", "deployment/"+k8sdaemon.DashboardName, "-o", "name", "--ignore-not-found"); !slices.Equal(fake.calls[0].args, want) {
		t.Errorf("first command = %q, want the existence read %q", fake.calls[0].args, want)
	}
	if want := gatewayPresentArgs(prefix); !slices.Equal(fake.calls[1].args, want) {
		t.Errorf("second command = %q, want the bd gateway existence read %q", fake.calls[1].args, want)
	}
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

	daemon := checkPatch(t, fake.calls[2].args, k8sdaemon.Name)
	if got := daemon.Spec.Template.Metadata.Annotations[k8sdaemon.PolecatImageAnnotation]; got != podSwapPolecatRef {
		t.Errorf("polecat annotation = %q, want the pair's polecat digest", got)
	}
	if c := daemon.Spec.Template.Spec.Containers; len(c) != 1 || c[0].Name != k8sdaemon.Name || c[0].Image != podSwapDigestRef {
		t.Errorf("daemon containers = %+v, want %s on the pair's daemon digest alone", c, k8sdaemon.Name)
	}

	dashboard := checkPatch(t, fake.calls[3].args, k8sdaemon.DashboardName)
	if c := dashboard.Spec.Template.Spec.Containers; len(c) != 1 || c[0].Name != k8sdaemon.DashboardName || c[0].Image != podSwapDigestRef {
		t.Errorf("dashboard containers = %+v, want %s on the pair's daemon digest alone", c, k8sdaemon.DashboardName)
	}
	if got := dashboard.Spec.Template.Metadata.Annotations; len(got) != 0 {
		t.Errorf("dashboard annotations = %v, want none, so the pod template's own annotations are kept", got)
	}

	if !slices.Equal(fake.calls[4].args, rolloutArgs(k8sdaemon.Name)) {
		t.Errorf("fifth command = %q, want the daemon's rollout wait %q", fake.calls[4].args, rolloutArgs(k8sdaemon.Name))
	}
	if !slices.Equal(fake.calls[5].args, rolloutArgs(k8sdaemon.DashboardName)) {
		t.Errorf("sixth command = %q, want the dashboard's rollout wait %q", fake.calls[5].args, rolloutArgs(k8sdaemon.DashboardName))
	}
}

// gatewayPresentArgs is the argv of the bd gateway existence read Apply makes: the
// Deployment's name, or nothing when the cluster has no gateway.
func gatewayPresentArgs(prefix []string) []string {
	return append(slices.Clone(prefix),
		"get", "deployment/"+k8sdaemon.BdGatewayName,
		"-o", "name", "--ignore-not-found",
	)
}

// TestPodDaemonSwapSkipsAnAbsentDashboard holds the town with no dashboard: after
// the two reads, only the daemon's patch and rollout wait run, no command names
// the dashboard, and one line says the dashboard was not moved.
func TestPodDaemonSwapSkipsAnAbsentDashboard(t *testing.T) {
	t.Parallel()
	fake := &podSwapFake{absent: true}
	var out bytes.Buffer
	swap := podSwapFor(fake)
	swap.Out = &out

	if err := swap.Apply(context.Background(), podSwapPair); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(fake.calls) != 4 {
		t.Fatalf("commands = %+v, want both existence reads, then the daemon's patch and rollout wait only", fake.calls)
	}
	if !containsArg(fake.calls[2].args, "patch") || !containsArg(fake.calls[2].args, "deployment/"+k8sdaemon.Name) {
		t.Errorf("third command = %q, want the daemon's patch", fake.calls[2].args)
	}
	if !slices.Equal(fake.calls[3].args, append(slices.Clone(fake.calls[2].args[:4]), "rollout", "status", "deployment/"+k8sdaemon.Name, "--timeout=5m0s")) {
		t.Errorf("fourth command = %q, want the daemon's rollout wait", fake.calls[3].args)
	}
	for _, c := range fake.calls[1:] {
		if containsArg(c.args, "deployment/"+k8sdaemon.DashboardName) {
			t.Errorf("command %q names the dashboard, want none once the dashboard is absent", c.args)
		}
	}
	if got, want := out.String(), dashboardAbsentLine+"\n"; got != want {
		t.Errorf("output = %q, want exactly %q", got, want)
	}
}

// TestPodDaemonSwapPrintsNothingWhenTheDashboardIsMoved holds the line to the
// absent town: a present dashboard is moved and says nothing about it.
func TestPodDaemonSwapPrintsNothingWhenTheDashboardIsMoved(t *testing.T) {
	t.Parallel()
	fake := &podSwapFake{}
	var out bytes.Buffer
	swap := podSwapFor(fake)
	swap.Out = &out

	if err := swap.Apply(context.Background(), podSwapPair); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := out.String(); got != "" {
		t.Errorf("output = %q, want none when the dashboard is moved", got)
	}
}

// podSwapBdNext is a bd gateway digest a pair names, distinct from the image the
// fake's cluster runs before the swap.
var podSwapBdNext = "registry.internal:5000/town/gastown-bd@sha256:" + strings.Repeat("5", 64)

// podSwapBdPair is podSwapPair with the gateway image a pod install's values file
// names.
func podSwapBdPair() update.ImagePair {
	pair := podSwapPair
	pair.Bd = podSwapBdNext
	return pair
}

// TestPodDaemonSwapMovesTheGatewayFirst holds the gateway step: when the cluster
// has a bd-gateway Deployment and the pair names its image, Apply patches the
// gateway and waits for its rollout, with the gateway's own bound, before the
// daemon's patch. A cluster with no gateway, or a pair that names no gateway
// image, moves the daemon alone and leaves the gateway as it is.
func TestPodDaemonSwapMovesTheGatewayFirst(t *testing.T) {
	t.Parallel()

	t.Run("a cluster with a gateway moves it before the daemon", func(t *testing.T) {
		t.Parallel()
		prefix := []string{"--context", podSwapContext, "-n", podSwapNamespace}
		fake := &podSwapFake{gateway: "registry.internal:5000/town/gastown-bd@sha256:" + strings.Repeat("4", 64)}

		if err := podSwapFor(fake).Apply(context.Background(), podSwapBdPair()); err != nil {
			t.Fatalf("Apply: %v", err)
		}
		if len(fake.calls) != 8 {
			t.Fatalf("commands = %+v, want both reads, the gateway's patch and wait, then the daemon's, the dashboard's and their waits", fake.calls)
		}
		want := append(slices.Clone(prefix), "patch", "deployment/"+k8sdaemon.BdGatewayName, "--type=strategic", "-p")
		if got := fake.calls[2].args; len(got) != len(want)+1 || !slices.Equal(got[:len(want)], want) {
			t.Fatalf("third command = %q, want the gateway's patch %q and one body", got, want)
		}
		var body podSwapPatchBody
		if err := json.Unmarshal([]byte(fake.calls[2].args[len(want)]), &body); err != nil {
			t.Fatalf("the gateway patch body is not JSON: %v", err)
		}
		if c := body.Spec.Template.Spec.Containers; len(c) != 1 || c[0].Name != k8sdaemon.BdGatewayName || c[0].Image != podSwapBdNext {
			t.Errorf("gateway containers = %+v, want %s on the pair's gateway digest alone", c, k8sdaemon.BdGatewayName)
		}
		if got := body.Spec.Template.Metadata.Annotations; len(got) != 0 {
			t.Errorf("gateway annotations = %v, want none, so the pod template's own annotations are kept", got)
		}
		want = append(slices.Clone(prefix), "rollout", "status", "deployment/"+k8sdaemon.BdGatewayName, "--timeout=3m0s")
		if got := fake.calls[3].args; !slices.Equal(got, want) {
			t.Errorf("fourth command = %q, want the gateway's rollout wait %q", got, want)
		}
		want = append(slices.Clone(prefix), "patch", "deployment/"+k8sdaemon.Name, "--type=strategic", "-p")
		if got := fake.calls[4].args; len(got) != len(want)+1 || !slices.Equal(got[:len(want)], want) {
			t.Errorf("fifth command = %q, want the daemon's patch %q, which waits for the gateway rollout", got, want)
		}
	})

	t.Run("a pair that names no gateway image leaves the cluster's gateway alone", func(t *testing.T) {
		t.Parallel()
		fake := &podSwapFake{gateway: "registry.internal:5000/town/gastown-bd@sha256:" + strings.Repeat("4", 64)}

		if err := podSwapFor(fake).Apply(context.Background(), podSwapPair); err != nil {
			t.Fatalf("Apply: %v", err)
		}
		assertDaemonMovedWithoutGateway(t, fake)
	})

	t.Run("a cluster with no gateway Deployment moves the daemon alone", func(t *testing.T) {
		t.Parallel()
		fake := &podSwapFake{}

		if err := podSwapFor(fake).Apply(context.Background(), podSwapBdPair()); err != nil {
			t.Fatalf("Apply: %v", err)
		}
		assertDaemonMovedWithoutGateway(t, fake)
	})
}

// assertDaemonMovedWithoutGateway holds a swap that leaves the gateway alone: the
// daemon and the dashboard are patched and waited on, and no command patches the
// gateway.
func assertDaemonMovedWithoutGateway(t *testing.T, fake *podSwapFake) {
	t.Helper()
	if len(fake.calls) != 6 {
		t.Fatalf("commands = %+v, want both reads, both patches and both rollout waits", fake.calls)
	}
	patched := 0
	for _, c := range fake.calls {
		if !containsArg(c.args, "patch") {
			continue
		}
		patched++
		if containsArg(c.args, "deployment/"+k8sdaemon.BdGatewayName) {
			t.Errorf("command %q patches the gateway, want it left as it is", c.args)
		}
	}
	if patched != 2 {
		t.Errorf("patches = %d, want the daemon's and the dashboard's", patched)
	}
	for _, c := range fake.calls {
		if containsArg(c.args, "rollout") && containsArg(c.args, "deployment/"+k8sdaemon.BdGatewayName) {
			t.Errorf("command %q waits on the gateway, want no gateway step", c.args)
		}
	}
}

// TestPodDaemonSwapFailsTheGatewayStep holds a gateway step that fails: the
// failure is an update.GatewayStepError naming the step, no daemon patch runs, and
// the command's output is never carried. The daemon's image is therefore not set
// until the gateway's rollout reports ready.
func TestPodDaemonSwapFailsTheGatewayStep(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		fake podSwapFake
		want string
	}{
		{
			name: "a gateway patch that exits nonzero stops before the daemon's patch",
			fake: podSwapFake{verb: "patch", deployment: k8sdaemon.BdGatewayName, exit: 1},
			want: "the bd gateway's patch exited 1",
		},
		{
			name: "a gateway patch the runner cannot start stops before the daemon's patch",
			fake: podSwapFake{verb: "patch", deployment: k8sdaemon.BdGatewayName, err: errors.New("kubectl not found")},
			want: "running kubectl for the bd gateway's patch",
		},
		{
			name: "a gateway rollout that never reports ready stops before the daemon's patch",
			fake: podSwapFake{verb: "rollout", deployment: k8sdaemon.BdGatewayName, exit: 1},
			want: "the bd gateway's rollout status exited 1",
		},
		{
			name: "a gateway rollout the runner cannot start stops before the daemon's patch",
			fake: podSwapFake{verb: "rollout", deployment: k8sdaemon.BdGatewayName, err: errors.New("kubectl not found")},
			want: "running kubectl for the bd gateway's rollout status",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fake := tt.fake
			fake.gateway = "registry.internal:5000/town/gastown-bd@sha256:" + strings.Repeat("4", 64)
			err := podSwapFor(&fake).Apply(context.Background(), podSwapBdPair())
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Apply error = %v, want one naming %q", err, tt.want)
			}
			var gw update.GatewayStepError
			if !errors.As(err, &gw) {
				t.Errorf("Apply error = %T, want update.GatewayStepError so the install names the gateway step", err)
			}
			if strings.Contains(err.Error(), podSwapOutput) {
				t.Errorf("Apply error carries the command's output: %v", err)
			}
			for _, c := range fake.calls {
				if containsArg(c.args, "patch") && containsArg(c.args, "deployment/"+k8sdaemon.Name) {
					t.Errorf("command %q patches the daemon, want the daemon left on its old image until the gateway is Ready", c.args)
				}
			}
		})
	}
}

// TestPodDaemonSwapStopsOnAnUnreadableGatewayRead: a gateway read that fails stops
// the swap before any patch, so an unreadable cluster never reads as a gateway
// that is not there and nothing moves past it.
func TestPodDaemonSwapStopsOnAnUnreadableGatewayRead(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		fake podSwapFake
		want string
	}{
		{
			name: "a read that exits nonzero fails the step",
			fake: podSwapFake{gatewayExit: 1},
			want: "the bd gateway's existence read exited 1",
		},
		{
			name: "a read the runner cannot run fails the step",
			fake: podSwapFake{gatewayErr: errors.New("kubectl not found")},
			want: "running kubectl for the bd gateway's existence read",
		},
		{
			name: "a read that answers with another Deployment fails the step",
			fake: podSwapFake{gatewayNameOut: "deployment.apps/" + k8sdaemon.DashboardName + "\n"},
			want: "printed a name other than the bd gateway's",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fake := tt.fake
			err := podSwapFor(&fake).Apply(context.Background(), podSwapBdPair())
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Apply error = %v, want one containing %q", err, tt.want)
			}
			for _, c := range fake.calls {
				if containsArg(c.args, "patch") {
					t.Errorf("command %q patches a Deployment, want none before the gateway read holds", c.args)
				}
			}
		})
	}
}

// TestPodDaemonSwapRefusesAnUnreadableDashboardRead holds the existence read to its
// refusals: a read that exits nonzero, one the runner cannot run, and one whose
// answer is not the dashboard's name each fail the step before any patch runs, so
// an unreadable cluster never passes as a dashboard that is not there.
func TestPodDaemonSwapRefusesAnUnreadableDashboardRead(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		fake podSwapFake
		want string
	}{
		{
			name: "a read that exits nonzero fails the step",
			fake: podSwapFake{readExit: 1},
			want: "the dashboard's existence read exited 1",
		},
		{
			name: "a read the runner cannot run fails the step",
			fake: podSwapFake{readErr: errors.New("kubectl not found")},
			want: "running kubectl for the dashboard's existence read",
		},
		{
			name: "a read that answers with something other than the dashboard fails, without echoing it",
			fake: podSwapFake{readOut: podSwapOutput + "\n"},
			want: "the dashboard's existence read printed a name other than the dashboard's",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fake := tt.fake
			err := podSwapFor(&fake).Apply(context.Background(), podSwapPair)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Apply error = %v, want one containing %q", err, tt.want)
			}
			if strings.Contains(err.Error(), podSwapOutput) {
				t.Errorf("Apply error carries the command's output: %v", err)
			}
			if len(fake.calls) != 1 {
				t.Errorf("commands = %+v, want only the existence read: no patch may run on either Deployment", fake.calls)
			}
		})
	}
}

// TestPodDaemonSwapReadsTheRunningPair holds the read an install records before
// it swaps: a kubectl get of the daemon's Deployment, in the swap's context and
// namespace, whose two lines are the daemon container's image and the pod
// template's polecat annotation, then a get of the bd-gateway Deployment for the
// gateway image. The pair names no commit, since a Deployment carries no build.
func TestPodDaemonSwapReadsTheRunningPair(t *testing.T) {
	t.Parallel()

	t.Run("a swap with no runner refuses before any command", func(t *testing.T) {
		t.Parallel()
		fake := &podSwapFake{}
		swap := podSwapFor(fake)
		swap.Run = nil
		if _, err := swap.RunningPair(context.Background()); err == nil || !strings.Contains(err.Error(), "no command runner wired") {
			t.Fatalf("RunningPair error = %v, want the missing runner refused", err)
		}
		if len(fake.calls) != 0 {
			t.Errorf("commands = %+v, want none", fake.calls)
		}
	})

	t.Run("the read names both Deployments in the swap's context and namespace", func(t *testing.T) {
		t.Parallel()
		fake := &podSwapFake{}
		pair, err := podSwapFor(fake).RunningPair(context.Background())
		if err != nil {
			t.Fatalf("RunningPair: %v", err)
		}
		if want := (update.ImagePair{Daemon: podSwapPair.Daemon, Polecat: podSwapPair.Polecat}); pair != want {
			t.Errorf("RunningPair = %+v, want %+v with no gateway image", pair, want)
		}
		if len(fake.calls) != 2 {
			t.Fatalf("commands = %+v, want the daemon read and the gateway read", fake.calls)
		}
		prefix := []string{"--context", podSwapContext, "-n", podSwapNamespace}
		want := append(slices.Clone(prefix),
			"get", "deployment/"+k8sdaemon.Name,
			"-o", "jsonpath="+runningPairJSONPath,
		)
		if !slices.Equal(fake.calls[0].args, want) {
			t.Errorf("daemon read argv = %q, want %q", fake.calls[0].args, want)
		}
		want = append(slices.Clone(prefix),
			"get", "deployment/"+k8sdaemon.BdGatewayName,
			"-o", "jsonpath="+gatewayImageJSONPath,
			"--ignore-not-found",
		)
		if !slices.Equal(fake.calls[1].args, want) {
			t.Errorf("gateway read argv = %q, want %q", fake.calls[1].args, want)
		}
	})

	t.Run("a cluster with a bd-gateway Deployment names its image", func(t *testing.T) {
		t.Parallel()
		fake := &podSwapFake{gateway: podSwapBdRef}
		pair, err := podSwapFor(fake).RunningPair(context.Background())
		if err != nil {
			t.Fatalf("RunningPair: %v", err)
		}
		if want := (update.ImagePair{Daemon: podSwapPair.Daemon, Polecat: podSwapPair.Polecat, Bd: podSwapBdRef}); pair != want {
			t.Errorf("RunningPair = %+v, want %+v with the gateway image", pair, want)
		}
	})

	t.Run("in-cluster the reads name no context", func(t *testing.T) {
		t.Parallel()
		fake := &podSwapFake{}
		swap := podSwapFor(fake)
		swap.Context, swap.InCluster = "", true
		if _, err := swap.RunningPair(context.Background()); err != nil {
			t.Fatalf("RunningPair: %v", err)
		}
		for _, call := range fake.calls {
			if want := []string{"-n", podSwapNamespace}; !slices.Equal(call.args[:2], want) {
				t.Errorf("read argv = %q, want it to start with %q", call.args, want)
			}
		}
	})
}

// TestPodSwapRefusesAnUnreadableGatewayImage holds the gateway read's refusals: a
// command that fails or exits nonzero, an answer that names no gateway, and one
// whose image is not a digest are each an error naming the step. An unreadable or
// unfamiliar cluster is never a cluster without a gateway.
func TestPodSwapRefusesAnUnreadableGatewayImage(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		fake podSwapFake
		want string
	}{
		{
			name: "a read that exits nonzero fails the step",
			fake: podSwapFake{gatewayExit: 1},
			want: "the bd gateway image read exited 1",
		},
		{
			name: "a read the runner cannot run fails the step",
			fake: podSwapFake{gatewayErr: errors.New("kubectl not found")},
			want: "running kubectl for the bd gateway image read",
		},
		{
			name: "an answer that names no gateway image fails the step",
			fake: podSwapFake{gatewayOut: k8sdaemon.BdGatewayName},
			want: "printed no bd-gateway image",
		},
		{
			name: "an answer naming another Deployment fails the step",
			fake: podSwapFake{gatewayOut: k8sdaemon.DashboardName + "\n" + podSwapBdRef},
			want: "printed no bd-gateway image",
		},
		{
			name: "a gateway Deployment with no gateway container fails the step",
			fake: podSwapFake{gatewayOut: k8sdaemon.BdGatewayName + "\n"},
			want: "bd gateway image",
		},
		{
			name: "a gateway image that is a tag is refused",
			fake: podSwapFake{gatewayOut: k8sdaemon.BdGatewayName + "\ngt-bd:latest"},
			want: `bd gateway image "gt-bd:latest"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fake := tt.fake
			pair, err := podSwapFor(&fake).RunningPair(context.Background())
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("RunningPair error = %v, want one containing %q", err, tt.want)
			}
			if pair != (update.ImagePair{}) {
				t.Errorf("RunningPair returned %+v with the error, want no pair", pair)
			}
			if strings.Contains(err.Error(), podSwapOutput) {
				t.Errorf("RunningPair error carries the command's output: %v", err)
			}
		})
	}
}

// TestPodSwapRefusesWhenCurrentPairUnreadable holds the read's refusals: a
// command that fails, prints no pair, or names a half that is not a digest
// reference is an error naming the step, never an error carrying the command's
// output, and the install it feeds stops before any swap.
func TestPodSwapRefusesWhenCurrentPairUnreadable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		fake podSwapFake
		want string
	}{
		{
			name: "a read that exits nonzero fails the step",
			fake: podSwapFake{pairExit: 1},
			want: "the running pair read exited 1",
		},
		{
			name: "a read the runner cannot run fails the step",
			fake: podSwapFake{pairErr: errors.New("kubectl not found")},
			want: "running kubectl for the running pair read",
		},
		{
			name: "a read that prints no pair fails the step",
			fake: podSwapFake{pairOut: podSwapOutput},
			want: "the running pair read printed no pair",
		},
		{
			name: "a daemon image that is a tag is refused",
			fake: podSwapFake{pairOut: "gt-daemon:latest\n" + podSwapPolecatRef},
			want: `daemon image "gt-daemon:latest"`,
		},
		{
			name: "a polecat annotation that is missing is refused",
			fake: podSwapFake{pairOut: podSwapDigestRef + "\n"},
			want: "polecat image",
		},
		{
			name: "a polecat annotation that is a tag is refused",
			fake: podSwapFake{pairOut: podSwapDigestRef + "\ngt-polecat:latest"},
			want: `polecat image "gt-polecat:latest"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fake := tt.fake
			pair, err := podSwapFor(&fake).RunningPair(context.Background())
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("RunningPair error = %v, want one containing %q", err, tt.want)
			}
			if pair != (update.ImagePair{}) {
				t.Errorf("RunningPair returned %+v with the error, want no pair", pair)
			}
			if strings.Contains(err.Error(), podSwapOutput) {
				t.Errorf("RunningPair error carries the command's output: %v", err)
			}
		})
	}

	t.Run("the digest install refuses before any swap", func(t *testing.T) {
		t.Parallel()
		got := captureWiredInstall(t, "--pod-context", podSwapContext, "--pod-namespace", podSwapNamespace)
		got.kube.readErr = errors.New("kubectl not found")

		res, err := got.deps.Installer.Install(context.Background(), rollbackNewCommit)
		if err == nil || !strings.Contains(err.Error(), "the pair the daemon runs now") {
			t.Fatalf("Install = %+v, %v; want the unreadable pair refused", res, err)
		}
		if update.ClassifyInstall(res) == update.ResultOK {
			t.Errorf("Install result = %+v, want no successful install", res)
		}
		for _, call := range got.kube.calls {
			if containsArg(call, "patch") {
				t.Errorf("kubectl ran %q, want no patch before the pair is read", call)
			}
		}
		rec, rerr := (update.FileImageLedger{Dir: update.Dir(got.townRoot)}).Read()
		if rerr != nil || rec != (update.ImageRecord{}) {
			t.Errorf("record = %+v, %v; want nothing recorded before the swap", rec, rerr)
		}
	})
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
	if len(fake.calls) != 6 {
		t.Errorf("commands = %d, want 6 at the limit", len(fake.calls))
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
	for _, i := range []int{2, 4} {
		if !containsArg(fake.calls[i].args, "deployment/daemon-canary") {
			t.Errorf("command %v does not name the configured Deployment", fake.calls[i].args)
		}
	}
	for _, i := range []int{3, 5} {
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
			name:    "a bd gateway tag reference is refused, since the patch would name a tag",
			pair:    update.ImagePair{Daemon: podSwapDigestRef, Polecat: podSwapPolecatRef, Bd: "registry.internal:5000/town/gastown-bd:latest"},
			wantErr: `bd gateway image "registry.internal:5000/town/gastown-bd:latest"`,
		},
		{
			name:    "a bd gateway digest with a short hash is refused",
			pair:    update.ImagePair{Daemon: podSwapDigestRef, Polecat: podSwapPolecatRef, Bd: "registry.internal:5000/town/gastown-bd@sha256:abc"},
			wantErr: "bd gateway image",
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

// TestPodDaemonSwapStopsWhenTheGatewayPatchCannotEncode holds the gateway body's
// encoding: the daemon's and the dashboard's bodies encode, the gateway's does
// not, and no command runs, so no Deployment moves before every body is known to
// build.
func TestPodDaemonSwapStopsWhenTheGatewayPatchCannotEncode(t *testing.T) {
	t.Parallel()
	fake := &podSwapFake{}
	swap := podSwapFor(fake)
	encodes := 0
	swap.marshal = func(v any) ([]byte, error) {
		encodes++
		if encodes > 2 {
			return nil, errors.New("encode refused")
		}
		return json.Marshal(v)
	}

	err := swap.Apply(context.Background(), podSwapBdPair())
	if err == nil || !strings.Contains(err.Error(), "encoding the bd gateway's pod patch") {
		t.Fatalf("Apply error = %v, want the gateway patch encoding named", err)
	}
	if len(fake.calls) != 0 {
		t.Errorf("commands = %+v, want none when the gateway patch cannot be built", fake.calls)
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
			wantCalls: 3,
		},
		{
			name:      "a failing dashboard patch stops before the rollout waits",
			fake:      podSwapFake{verb: "patch", deployment: k8sdaemon.DashboardName, exit: 1},
			wantStep:  "the dashboard's patch exited 1",
			wantCalls: 4,
		},
		{
			name:      "a failing daemon rollout stops before the dashboard's wait",
			fake:      podSwapFake{verb: "rollout", deployment: k8sdaemon.Name, exit: 1},
			wantStep:  "the daemon's rollout status exited 1",
			wantCalls: 5,
		},
		{
			name:      "a failing dashboard rollout is named as the dashboard's rollout",
			fake:      podSwapFake{verb: "rollout", deployment: k8sdaemon.DashboardName, exit: 1},
			wantStep:  "the dashboard's rollout status exited 1",
			wantCalls: 6,
		},
		{
			name:      "a daemon patch command that cannot start is named as the daemon's patch",
			fake:      podSwapFake{verb: "patch", deployment: k8sdaemon.Name, err: errors.New("kubectl not found")},
			wantStep:  "the daemon's patch",
			wantCalls: 3,
		},
		{
			name:      "a dashboard patch command that cannot start is named as the dashboard's patch",
			fake:      podSwapFake{verb: "patch", deployment: k8sdaemon.DashboardName, err: errors.New("kubectl not found")},
			wantStep:  "the dashboard's patch",
			wantCalls: 4,
		},
		{
			name:      "a rollout command that cannot start is named as the rollout",
			fake:      podSwapFake{verb: "rollout", deployment: k8sdaemon.Name, err: errors.New("kubectl not found")},
			wantStep:  "the daemon's rollout status",
			wantCalls: 5,
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
		if len(fake.calls) != 6 {
			t.Fatalf("commands = %+v, want both existence reads, both patches, then both rollout waits", fake.calls)
		}
		for _, c := range fake.calls {
			if containsArg(c.args, "--context") {
				t.Errorf("command %v carries --context, want none in-cluster", c.args)
			}
			if !slices.Equal(c.args[:2], []string{"-n", podSwapNamespace}) {
				t.Errorf("command %v does not start with the namespace", c.args)
			}
		}
		if !containsArg(fake.calls[2].args, "patch") || !containsArg(fake.calls[3].args, "patch") {
			t.Errorf("third and fourth commands %v are not the patches", fake.calls[2:4])
		}
		if !containsArg(fake.calls[4].args, "rollout") || !containsArg(fake.calls[5].args, "rollout") {
			t.Errorf("last two commands %v are not the rollout waits", fake.calls[4:])
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

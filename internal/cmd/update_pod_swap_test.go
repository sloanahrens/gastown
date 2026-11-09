package cmd

import (
	"context"
	"errors"
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
// kubectl subcommand that fails; exit and err are what it returns.
type podSwapFake struct {
	calls []podSwapCall
	verb  string
	exit  int
	err   error
}

func (f *podSwapFake) run(_ context.Context, _ string, _ []string, name string, args ...string) (string, int, error) {
	f.calls = append(f.calls, podSwapCall{name: name, args: args})
	for _, a := range args {
		if a == f.verb {
			return podSwapOutput, f.exit, f.err
		}
	}
	return podSwapOutput, 0, nil
}

const (
	podSwapContext   = "control-cluster"
	podSwapNamespace = "gt"
)

// podSwapDigestRef is a daemon digest reference, the form a digest install records.
var podSwapDigestRef = "registry.internal:5000/town/gastown-daemon@sha256:" + strings.Repeat("e", 64)

// podSwapPolecatRef is the polecat half of the pair; no pod swap command may name it.
var podSwapPolecatRef = "registry.internal:5000/town/gastown-polecat@sha256:" + strings.Repeat("f", 64)

// podSwapFor is a swap wired to fake, with every required field set.
func podSwapFor(fake *podSwapFake) podDaemonSwap {
	return podDaemonSwap{
		Context:        podSwapContext,
		Namespace:      podSwapNamespace,
		Run:            fake.run,
		RolloutTimeout: 5 * time.Minute,
	}
}

func TestPodDaemonSwapRunsSetImageThenRolloutStatus(t *testing.T) {
	t.Parallel()
	fake := &podSwapFake{}
	pair := update.ImagePair{Daemon: podSwapDigestRef, Polecat: podSwapPolecatRef}

	if err := podSwapFor(fake).Apply(context.Background(), pair); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	want := []podSwapCall{
		{name: "kubectl", args: []string{"--context", podSwapContext, "-n", podSwapNamespace, "set", "image", "deployment/" + k8sdaemon.Name, k8sdaemon.Name + "=" + podSwapDigestRef}},
		{name: "kubectl", args: []string{"--context", podSwapContext, "-n", podSwapNamespace, "rollout", "status", "deployment/" + k8sdaemon.Name, "--timeout=5m0s"}},
	}
	if len(fake.calls) != len(want) {
		t.Fatalf("commands = %+v, want exactly %d", fake.calls, len(want))
	}
	for i := range want {
		if got, w := strings.Join(fake.calls[i].args, " "), strings.Join(want[i].args, " "); fake.calls[i].name != want[i].name || got != w {
			t.Errorf("command %d = %s %s, want %s %s", i, fake.calls[i].name, got, want[i].name, w)
		}
	}
}

func TestPodDaemonSwapAcceptsANameAtTheDNSLabelLimit(t *testing.T) {
	t.Parallel()
	fake := &podSwapFake{}
	swap := podSwapFor(fake)
	swap.Namespace = strings.Repeat("a", 63)
	swap.Deployment = strings.Repeat("b", 63)

	if err := swap.Apply(context.Background(), update.ImagePair{Daemon: podSwapDigestRef}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(fake.calls) != 2 {
		t.Errorf("commands = %d, want 2 at the limit", len(fake.calls))
	}
}

func TestPodDaemonSwapUsesTheNamedDeployment(t *testing.T) {
	t.Parallel()
	fake := &podSwapFake{}
	swap := podSwapFor(fake)
	swap.Deployment = "daemon-canary"

	if err := swap.Apply(context.Background(), update.ImagePair{Daemon: podSwapDigestRef}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	for _, c := range fake.calls {
		if !strings.Contains(strings.Join(c.args, " "), "deployment/daemon-canary") {
			t.Errorf("command %v does not name the configured Deployment", c.args)
		}
	}
}

func TestPodDaemonSwapRefusesATagAndMissingContext(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		daemon  string
		mutate  func(*podDaemonSwap)
		wantErr string
	}{
		{
			name:    "a tag reference is refused",
			daemon:  "registry.internal:5000/town/gastown-daemon:latest",
			wantErr: "is not a digest reference",
		},
		{
			name:    "a digest with a short hash is refused",
			daemon:  "registry.internal:5000/town/gastown-daemon@sha256:abc",
			wantErr: "is not a digest reference",
		},
		{
			name:    "an empty context is refused",
			daemon:  podSwapDigestRef,
			mutate:  func(s *podDaemonSwap) { s.Context = "" },
			wantErr: "no usable kubectl context",
		},
		{
			name:    "a context that reads as a kubectl flag is refused",
			daemon:  podSwapDigestRef,
			mutate:  func(s *podDaemonSwap) { s.Context = "--cluster=control" },
			wantErr: "no usable kubectl context",
		},
		{
			name:    "an empty namespace is refused",
			daemon:  podSwapDigestRef,
			mutate:  func(s *podDaemonSwap) { s.Namespace = "" },
			wantErr: "is not a Kubernetes namespace name",
		},
		{
			name:    "a namespace with an uppercase letter is refused",
			daemon:  podSwapDigestRef,
			mutate:  func(s *podDaemonSwap) { s.Namespace = "GT" },
			wantErr: "is not a Kubernetes namespace name",
		},
		{
			name:    "a namespace one character past the DNS label limit is refused",
			daemon:  podSwapDigestRef,
			mutate:  func(s *podDaemonSwap) { s.Namespace = strings.Repeat("a", 64) },
			wantErr: "is not a Kubernetes namespace name",
		},
		{
			name:    "a deployment that is not a Kubernetes name is refused",
			daemon:  podSwapDigestRef,
			mutate:  func(s *podDaemonSwap) { s.Deployment = "daemon/x" },
			wantErr: "is not a Kubernetes name",
		},
		{
			name:    "a deployment one character past the DNS label limit is refused",
			daemon:  podSwapDigestRef,
			mutate:  func(s *podDaemonSwap) { s.Deployment = strings.Repeat("a", 64) },
			wantErr: "is not a Kubernetes name",
		},
		{
			name:    "a zero rollout timeout is refused",
			daemon:  podSwapDigestRef,
			mutate:  func(s *podDaemonSwap) { s.RolloutTimeout = 0 },
			wantErr: "has no rollout timeout",
		},
		{
			name:    "a negative rollout timeout is refused",
			daemon:  podSwapDigestRef,
			mutate:  func(s *podDaemonSwap) { s.RolloutTimeout = -time.Second },
			wantErr: "has no rollout timeout",
		},
		{
			name:    "a swap with no runner is refused",
			daemon:  podSwapDigestRef,
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
			err := swap.Apply(context.Background(), update.ImagePair{Daemon: tt.daemon})
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Apply error = %v, want one containing %q", err, tt.wantErr)
			}
			if len(fake.calls) != 0 {
				t.Errorf("commands = %+v, want none before the refusal", fake.calls)
			}
		})
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
			name:      "a failing set-image step stops before the rollout",
			fake:      podSwapFake{verb: "set", exit: 1},
			wantStep:  "the daemon's set image exited 1",
			wantCalls: 1,
		},
		{
			name:      "a failing rollout step is named as the rollout",
			fake:      podSwapFake{verb: "rollout", exit: 1},
			wantStep:  "the daemon's rollout status exited 1",
			wantCalls: 2,
		},
		{
			name:      "a set-image command that cannot start is named as the set image",
			fake:      podSwapFake{verb: "set", err: errors.New("kubectl not found")},
			wantStep:  "the daemon's set image",
			wantCalls: 1,
		},
		{
			name:      "a rollout command that cannot start is named as the rollout",
			fake:      podSwapFake{verb: "rollout", err: errors.New("kubectl not found")},
			wantStep:  "the daemon's rollout status",
			wantCalls: 2,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fake := tt.fake
			err := podSwapFor(&fake).Apply(context.Background(), update.ImagePair{Daemon: podSwapDigestRef})
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

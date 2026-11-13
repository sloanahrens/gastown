package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/steveyegge/gastown/internal/attention"
	agentconfig "github.com/steveyegge/gastown/internal/config"
)

// pendingSandboxNow is the tick every collection in these tests judges the
// pods' ages against, so no test reads the wall clock.
var pendingSandboxNow = time.Date(2026, 11, 12, 22, 0, 0, 0, time.UTC)

// pendingSandboxFixture is one daemon whose sandbox pod read is answered in
// process: the reply to the pods call, and the profiles the town settings hold.
type pendingSandboxFixture struct {
	d        *Daemon
	cli      *fakeCLI
	src      *attentionSources
	reply    cliReply
	runErr   error
	profiles map[string]agentconfig.SandboxProfile
}

func newPendingSandboxFixture(t *testing.T) *pendingSandboxFixture {
	t.Helper()
	f := &pendingSandboxFixture{
		reply: cliReply{stdout: `{"items":[]}`},
		profiles: map[string]agentconfig.SandboxProfile{
			"polecat-k8s": {Driver: agentconfig.SandboxDriverKubernetes},
		},
	}
	var logs bytes.Buffer
	f.cli = newFakeCLIFor(func(c cliCall) cliReply {
		if f.runErr != nil {
			return cliReply{code: 1, stderr: f.runErr.Error()}
		}
		if strings.HasPrefix(cacheTrimVerb(c.args), "get pods ") {
			return f.reply
		}
		return cliReply{}
	})
	d := &Daemon{
		config: &Config{TownRoot: t.TempDir()},
		logger: log.New(&logs, "", 0),
		clock:  clockwork.NewFakeClockAt(pendingSandboxNow),
	}
	d.execCmd = f.cli.run
	d.diskMaint.getenv = func(string) string { return "" }
	d.diskMaint.environ = func() []string {
		return []string{"PATH=/usr/local/bin:/usr/bin", "HOME=/home/polecat"}
	}
	d.diskMaint.sandboxes = func() (map[string]agentconfig.SandboxProfile, error) {
		return f.profiles, nil
	}
	f.d = d
	f.src = &attentionSources{d: d, now: pendingSandboxNow}
	return f
}

// collect runs the production collector the registry serves for the kind.
func (f *pendingSandboxFixture) collect(t *testing.T) ([]attention.Item, error) {
	t.Helper()
	return collectorFor(t, f.src, attention.KindSandboxPending).collect(context.Background())
}

// sandboxPodJSON is one pod of the reply: its namespace, phase, PodScheduled
// condition and creation time, the fields the unschedulable read decides on.
func sandboxPodJSON(name, namespace, phase, scheduled, reason, message string, created time.Time) string {
	return fmt.Sprintf(
		`{"metadata":{"name":%q,"namespace":%q,"creationTimestamp":%q},"status":{"phase":%q,"conditions":[{"type":"PodScheduled","status":%q,"reason":%q,"message":%q}]}}`,
		name, namespace, created.Format(time.RFC3339), phase, scheduled, reason, message)
}

func sandboxPodListJSON(pods ...string) string {
	return `{"items":[` + strings.Join(pods, ",") + `]}`
}

// TestCollectPendingSandboxPods is the bead's first box. A pod Pending and
// unschedulable past a minute raises one high item that names the pod, its age
// and the scheduler's message verbatim; a pod younger than a minute raises
// none; a Pending pod that was scheduled and is pulling its image raises none;
// and the item is gone once the pod runs (gt-1gtq5g).
func TestCollectPendingSandboxPods(t *testing.T) {
	t.Parallel()
	f := newPendingSandboxFixture(t)
	const schedulerMessage = "0/3 nodes are available: 3 Insufficient cpu, 3 Insufficient memory."
	f.reply = cliReply{stdout: sandboxPodListJSON(
		sandboxPodJSON("default--gt-nux", sandboxPodNamespace, "Pending", "False", "Unschedulable", schedulerMessage, pendingSandboxNow.Add(-3*time.Minute)),
		sandboxPodJSON("default--gt-splendid", sandboxPodNamespace, "Pending", "False", "Unschedulable", schedulerMessage, pendingSandboxNow.Add(-30*time.Second)),
		sandboxPodJSON("default--gt-puller", sandboxPodNamespace, "Pending", "True", "", "", pendingSandboxNow.Add(-10*time.Minute)),
		// Gated, not unschedulable: the scheduler has not refused it.
		sandboxPodJSON("default--gt-gated", sandboxPodNamespace, "Pending", "False", "SchedulingGated", "", pendingSandboxNow.Add(-10*time.Minute)),
		sandboxPodJSON("default--gt-live", sandboxPodNamespace, "Running", "True", "", "", pendingSandboxNow.Add(-time.Hour)),
	)}

	items, err := f.collect(t)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("items = %+v, want one (the 30s pod, the image pull, the gated pod and the running pod raise none)", items)
	}
	it := items[0]
	if it.Key != "sandbox-pending:default/default--gt-nux" || it.Kind != attention.KindSandboxPending ||
		it.Severity != attention.SeverityHigh {
		t.Errorf("item = %+v, want a high sandbox-pending item on default/default--gt-nux", it)
	}
	want := "sandbox pod default/default--gt-nux pending 3m, unschedulable: " + schedulerMessage
	if it.Summary != want {
		t.Errorf("summary = %q, want %q", it.Summary, want)
	}

	// The pod runs on the next tick: its item clears.
	f.reply = cliReply{stdout: sandboxPodListJSON(
		sandboxPodJSON("default--gt-nux", sandboxPodNamespace, "Running", "True", "", "", pendingSandboxNow.Add(-4*time.Minute)),
	)}
	items, err = f.collect(t)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Errorf("items after the pod runs = %+v, want none", items)
	}
}

// TestPendingSandboxReadNamesTheSandboxNamespace: the read is the daemon's
// kubectl call against the sandbox kubeconfig and context, in the namespace
// the profiles run in, so the item is about the cluster's pods and not a
// guessed list (gt-1gtq5g).
func TestPendingSandboxReadNamesTheSandboxNamespace(t *testing.T) {
	t.Parallel()
	f := newPendingSandboxFixture(t)
	if _, err := f.collect(t); err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, call := range f.cli.recorded() {
		if verb := cacheTrimVerb(call.args); verb == "get pods -n "+sandboxPodNamespace+" -o json" {
			found = true
		}
	}
	if !found {
		t.Errorf("kubectl calls = %+v, want get pods in namespace %s", f.cli.recorded(), sandboxPodNamespace)
	}
}

// TestPendingSandboxReadsNoPodWithoutAKubernetesProfile: a town whose profiles
// all run the docker driver has no sandbox pod in the cluster, so the collector
// reads nothing and raises nothing rather than reading a namespace it has no
// reason to look in (gt-1gtq5g).
func TestPendingSandboxReadsNoPodWithoutAKubernetesProfile(t *testing.T) {
	t.Parallel()
	f := newPendingSandboxFixture(t)
	f.profiles = map[string]agentconfig.SandboxProfile{
		"docker-seat": {Driver: agentconfig.SandboxDriverDocker},
	}
	items, err := f.collect(t)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Errorf("items = %+v, want none", items)
	}
	if calls := f.cli.recorded(); len(calls) != 0 {
		t.Errorf("the collector ran %d kubectl calls, want none: %+v", len(calls), calls)
	}
}

// TestPendingSandboxCollectorSurvivesAClusterReadFailure is the bead's third
// box. A failed read returns the error and no items, so the collector's own
// previous items stand rather than clearing on an unanswered read; the next
// tick reads again and raises the pod it finds (gt-1gtq5g).
func TestPendingSandboxCollectorSurvivesAClusterReadFailure(t *testing.T) {
	t.Parallel()
	f := newPendingSandboxFixture(t)
	f.runErr = errors.New("the server could not find the requested resource")
	items, err := f.collect(t)
	if err == nil {
		t.Fatalf("collector read a failed kubectl call as clean: items = %+v", items)
	}
	if items != nil {
		t.Errorf("items = %+v beside a failed read, want none", items)
	}

	f.runErr = nil
	f.reply = cliReply{stdout: sandboxPodListJSON(
		sandboxPodJSON("default--gt-nux", sandboxPodNamespace, "Pending", "False", "Unschedulable", "0/3 nodes are available: 3 Insufficient cpu.", pendingSandboxNow.Add(-5*time.Minute)),
	)}
	items, err = f.collect(t)
	if err != nil {
		t.Fatalf("the collection after a failed read: %v", err)
	}
	if len(items) != 1 || items[0].Key != "sandbox-pending:default/default--gt-nux" {
		t.Errorf("items after the read recovers = %+v, want the unschedulable pod's item", items)
	}
}

// TestPendingSandboxReadRefusalsAreErrors: a reply the collector cannot trust
// fails the collection instead of reading as no pod waiting, which would clear
// a real item (gt-1gtq5g).
func TestPendingSandboxReadRefusalsAreErrors(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		reply cliReply
		want  string
	}{
		{
			name:  "a reply that is not JSON",
			reply: cliReply{stdout: `{`},
			want:  "decoding the pod list",
		},
		{
			name:  "a reply with no item list",
			reply: cliReply{stdout: `{}`},
			want:  "names no item list",
		},
		{
			name: "a pod of another namespace",
			reply: cliReply{stdout: sandboxPodListJSON(
				sandboxPodJSON("default--gt-nux", "elsewhere", "Pending", "False", "Unschedulable", "0/3 nodes are available.", pendingSandboxNow.Add(-5*time.Minute)),
			)},
			want: "names the pod default--gt-nux in elsewhere",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newPendingSandboxFixture(t)
			f.reply = tc.reply
			items, err := f.collect(t)
			if err == nil {
				t.Fatalf("collector read %s as clean: items = %+v", tc.name, items)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not name %q", err, tc.want)
			}
			if items != nil {
				t.Errorf("items = %+v beside the refusal, want none", items)
			}
		})
	}
}

// TestPendingSandboxProfilesReadFailureIsAnError: a profile read that cannot be
// made fails the collection, so the previous tick's items stand rather than
// clearing because the town's settings could not be read (gt-1gtq5g).
func TestPendingSandboxProfilesReadFailureIsAnError(t *testing.T) {
	t.Parallel()
	f := newPendingSandboxFixture(t)
	f.d.diskMaint.sandboxes = func() (map[string]agentconfig.SandboxProfile, error) {
		return nil, errors.New("settings/config.json: permission denied")
	}
	items, err := f.collect(t)
	if err == nil {
		t.Fatalf("collector read a failed profile read as clean: items = %+v", items)
	}
	if !strings.Contains(err.Error(), "sandbox profiles") {
		t.Errorf("error %q does not name the profile read", err)
	}
	if items != nil {
		t.Errorf("items = %+v beside the refusal, want none", items)
	}
}

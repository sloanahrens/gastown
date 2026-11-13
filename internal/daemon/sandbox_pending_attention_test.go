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

// pendingSandboxNamespace is the namespace an unset kubernetes_namespace reads,
// the OpenShell chart's namespace (config pins the default in
// TestSandboxProfileKubernetesNamespace).
const pendingSandboxNamespace = "openshell"

// pendingSandboxFixture is one daemon whose sandbox pod read is answered in
// process: the reply to the pods call (or a reply per namespace's pods call),
// the profiles the town settings hold, and the daemon's log.
type pendingSandboxFixture struct {
	d        *Daemon
	cli      *fakeCLI
	src      *attentionSources
	clock    *clockwork.FakeClock
	reply    cliReply
	byVerb   map[string]cliReply
	runErr   error
	profiles map[string]agentconfig.SandboxProfile
	logs     bytes.Buffer
	// hang, when set, holds a pod read until it is closed: a cluster that never
	// answers. hangEntered is told each time a read reaches the cluster.
	hang        chan struct{}
	hangEntered chan struct{}
}

func newPendingSandboxFixture(t *testing.T) *pendingSandboxFixture {
	t.Helper()
	f := &pendingSandboxFixture{
		reply: cliReply{stdout: `{"items":[]}`},
		profiles: map[string]agentconfig.SandboxProfile{
			"polecat-k8s": {Driver: agentconfig.SandboxDriverKubernetes},
		},
	}
	f.cli = newFakeCLIFor(func(c cliCall) cliReply {
		if f.runErr != nil {
			return cliReply{code: 1, stderr: f.runErr.Error()}
		}
		verb := cacheTrimVerb(c.args)
		if hang := f.hang; hang != nil && strings.HasPrefix(verb, "get pods ") {
			f.hangEntered <- struct{}{}
			<-hang
			return cliReply{}
		}
		if reply, ok := f.byVerb[verb]; ok {
			return reply
		}
		if strings.HasPrefix(verb, "get pods ") {
			return f.reply
		}
		return cliReply{}
	})
	f.clock = clockwork.NewFakeClockAt(pendingSandboxNow)
	d := &Daemon{
		config: &Config{TownRoot: t.TempDir()},
		logger: log.New(&f.logs, "", 0),
		clock:  f.clock,
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
		sandboxPodJSON("default--gt-nux", pendingSandboxNamespace, "Pending", "False", "Unschedulable", schedulerMessage, pendingSandboxNow.Add(-3*time.Minute)),
		sandboxPodJSON("default--gt-splendid", pendingSandboxNamespace, "Pending", "False", "Unschedulable", schedulerMessage, pendingSandboxNow.Add(-30*time.Second)),
		sandboxPodJSON("default--gt-puller", pendingSandboxNamespace, "Pending", "True", "", "", pendingSandboxNow.Add(-10*time.Minute)),
		// Gated, not unschedulable: the scheduler has not refused it.
		sandboxPodJSON("default--gt-gated", pendingSandboxNamespace, "Pending", "False", "SchedulingGated", "", pendingSandboxNow.Add(-10*time.Minute)),
		sandboxPodJSON("default--gt-live", pendingSandboxNamespace, "Running", "True", "", "", pendingSandboxNow.Add(-time.Hour)),
	)}

	items, err := f.collect(t)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("items = %+v, want one (the 30s pod, the image pull, the gated pod and the running pod raise none)", items)
	}
	it := items[0]
	if it.Key != "sandbox-pending:openshell/default--gt-nux" || it.Kind != attention.KindSandboxPending ||
		it.Severity != attention.SeverityHigh {
		t.Errorf("item = %+v, want a high sandbox-pending item on openshell/default--gt-nux", it)
	}
	want := "sandbox pod openshell/default--gt-nux pending 3m, unschedulable: " + schedulerMessage
	if it.Summary != want {
		t.Errorf("summary = %q, want %q", it.Summary, want)
	}

	// The pod runs on the next tick: its item clears.
	f.reply = cliReply{stdout: sandboxPodListJSON(
		sandboxPodJSON("default--gt-nux", pendingSandboxNamespace, "Running", "True", "", "", pendingSandboxNow.Add(-4*time.Minute)),
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
// the profile names, so the item is about the cluster's pods and not a guessed
// list (gt-1gtq5g).
func TestPendingSandboxReadNamesTheSandboxNamespace(t *testing.T) {
	t.Parallel()
	f := newPendingSandboxFixture(t)
	if _, err := f.collect(t); err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, call := range f.cli.recorded() {
		if verb := cacheTrimVerb(call.args); verb == "get pods -n "+pendingSandboxNamespace+" -o json --request-timeout=10s" {
			found = true
		}
	}
	if !found {
		t.Errorf("kubectl calls = %+v, want get pods in namespace %s", f.cli.recorded(), pendingSandboxNamespace)
	}
}

// TestPendingSandboxReadsEachProfileNamespaceOnce: every kubernetes profile's
// namespace is read, a namespace two profiles share is read once, and a
// docker profile contributes none. Each namespace's pod is raised from its own
// reply (gt-1gtq5g).
func TestPendingSandboxReadsEachProfileNamespaceOnce(t *testing.T) {
	t.Parallel()
	f := newPendingSandboxFixture(t)
	f.profiles = map[string]agentconfig.SandboxProfile{
		"polecat-k8s":   {Driver: agentconfig.SandboxDriverKubernetes},
		"seat-b":        {Driver: agentconfig.SandboxDriverKubernetes, KubernetesNamespace: "sandboxes-b"},
		"seat-b-second": {Driver: agentconfig.SandboxDriverKubernetes, KubernetesNamespace: "sandboxes-b"},
		"docker-seat":   {Driver: agentconfig.SandboxDriverDocker},
	}
	f.byVerb = map[string]cliReply{
		"get pods -n openshell -o json --request-timeout=10s": {stdout: sandboxPodListJSON(
			sandboxPodJSON("default--gt-nux", "openshell", "Pending", "False", "Unschedulable", "0/3 nodes are available: 3 Insufficient cpu.", pendingSandboxNow.Add(-3*time.Minute)),
		)},
		"get pods -n sandboxes-b -o json --request-timeout=10s": {stdout: sandboxPodListJSON(
			sandboxPodJSON("default--gt-b", "sandboxes-b", "Pending", "False", "Unschedulable", "0/3 nodes are available: 3 Insufficient memory.", pendingSandboxNow.Add(-4*time.Minute)),
		)},
	}

	items, err := f.collect(t)
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, it := range items {
		keys = append(keys, it.Key)
	}
	want := []string{"sandbox-pending:openshell/default--gt-nux", "sandbox-pending:sandboxes-b/default--gt-b"}
	if fmt.Sprint(keys) != fmt.Sprint(want) {
		t.Errorf("item keys = %v, want %v", keys, want)
	}
	reads := map[string]int{}
	for _, call := range f.cli.recorded() {
		if verb := cacheTrimVerb(call.args); strings.HasPrefix(verb, "get pods ") {
			reads[verb]++
		}
	}
	for _, verb := range []string{"get pods -n openshell -o json --request-timeout=10s", "get pods -n sandboxes-b -o json --request-timeout=10s"} {
		if reads[verb] != 1 {
			t.Errorf("%q read %d times, want once; calls = %+v", verb, reads[verb], f.cli.recorded())
		}
	}
	if len(reads) != 2 {
		t.Errorf("pod reads = %v, want exactly the two kubernetes namespaces", reads)
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

// TestPendingSandboxUntimedPodIsLoggedOnce: a Pending unschedulable pod with no
// creation time has no age to judge, so it raises nothing and is not skipped
// without a word: its missing time is logged once per daemon run, and the same
// pod with a real time older than a minute raises its item (gt-1gtq5g).
func TestPendingSandboxUntimedPodIsLoggedOnce(t *testing.T) {
	t.Parallel()
	f := newPendingSandboxFixture(t)
	untimed := `{"metadata":{"name":"default--gt-untimed","namespace":"openshell"},"status":{"phase":"Pending","conditions":[{"type":"PodScheduled","status":"False","reason":"Unschedulable","message":"0/3 nodes are available: 3 Insufficient cpu."}]}}`
	f.reply = cliReply{stdout: sandboxPodListJSON(untimed)}

	for tick := 0; tick < 2; tick++ {
		items, err := f.collect(t)
		if err != nil {
			t.Fatal(err)
		}
		if len(items) != 0 {
			t.Fatalf("tick %d: items = %+v for a pod with no creation time, want none", tick, items)
		}
	}
	const line = "sandbox pod openshell/default--gt-untimed has no creation timestamp; treated as new\n"
	if got := f.logs.String(); got != line {
		t.Errorf("log after two collections = %q, want the line once: %q", got, line)
	}

	f.reply = cliReply{stdout: sandboxPodListJSON(
		sandboxPodJSON("default--gt-untimed", pendingSandboxNamespace, "Pending", "False", "Unschedulable", "0/3 nodes are available: 3 Insufficient cpu.", pendingSandboxNow.Add(-2*time.Minute)),
	)}
	items, err := f.collect(t)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Key != "sandbox-pending:openshell/default--gt-untimed" {
		t.Errorf("items once the pod has a real time = %+v, want its item", items)
	}
}

// untimedSandboxPodJSON is one unschedulable pod whose reply carries no creation
// time, the shape a pod has when the cluster reports none.
func untimedSandboxPodJSON(name, message string) string {
	return fmt.Sprintf(`{"metadata":{"name":%q,"namespace":%q},"status":{"phase":"Pending","conditions":[{"type":"PodScheduled","status":"False","reason":"Unschedulable","message":%q}]}}`,
		name, pendingSandboxNamespace, message)
}

// TestCollectPendingSandboxPodsUntimedAgesFromFirstSeen: an unschedulable pod with no
// usable creation time ages from the first tick that saw it unschedulable. It
// raises nothing at zero or fifty-nine seconds, and one item at sixty-one
// seconds that names when it was first seen (gt-1gtq5g).
func TestCollectPendingSandboxPodsUntimedAgesFromFirstSeen(t *testing.T) {
	t.Parallel()
	f := newPendingSandboxFixture(t)
	const message = "0/3 nodes are available: 3 Insufficient cpu."
	f.reply = cliReply{stdout: sandboxPodListJSON(untimedSandboxPodJSON("default--gt-untimed", message))}
	for _, step := range []struct {
		after time.Duration
		want  int
	}{{0, 0}, {59 * time.Second, 0}, {61 * time.Second, 1}} {
		f.src.now = pendingSandboxNow.Add(step.after)
		items, err := f.collect(t)
		if err != nil {
			t.Fatal(err)
		}
		if len(items) != step.want {
			t.Fatalf("at +%s: items = %+v, want %d", step.after, items, step.want)
		}
		if step.want == 1 {
			want := "sandbox pod openshell/default--gt-untimed pending Unschedulable, age unknown, first seen 2026-11-12T22:00:00Z: " + message
			if items[0].Summary != want {
				t.Errorf("summary = %q, want %q", items[0].Summary, want)
			}
		}
	}
}

// TestCollectPendingSandboxPodsUntimedWindowRestartsWhenPodLeaves: a pod that leaves the
// list drops its first-seen entry, and when it returns untimed its window
// starts again from the tick it returns on (gt-1gtq5g).
func TestCollectPendingSandboxPodsUntimedWindowRestartsWhenPodLeaves(t *testing.T) {
	t.Parallel()
	f := newPendingSandboxFixture(t)
	const key = "openshell/default--gt-untimed"
	const message = "0/3 nodes are available: 3 Insufficient cpu."
	pod := cliReply{stdout: sandboxPodListJSON(untimedSandboxPodJSON("default--gt-untimed", message))}
	gone := cliReply{stdout: `{"items":[]}`}
	t0 := pendingSandboxNow
	for _, step := range []struct {
		after time.Duration
		reply cliReply
	}{{0, pod}, {30 * time.Second, gone}, {100 * time.Second, pod}} {
		f.src.now = t0.Add(step.after)
		f.reply = step.reply
		items, err := f.collect(t)
		if err != nil {
			t.Fatal(err)
		}
		if len(items) != 0 {
			t.Fatalf("at +%s: items = %+v, want none while the window is open or restarting", step.after, items)
		}
	}
	if got := f.d.sandboxPendingFirstSeen[key]; !got.Equal(t0.Add(100 * time.Second)) {
		t.Errorf("first seen after the pod returns = %s, want the return tick %s", got, t0.Add(100*time.Second))
	}
	f.src.now = t0.Add(161 * time.Second)
	items, err := f.collect(t)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || !strings.Contains(items[0].Summary, "first seen 2026-11-12T22:01:40Z") {
		t.Errorf("items at +161s = %+v, want one item first seen at the return tick", items)
	}
}

// TestCollectPendingSandboxPodsUntimedDropsAbsentPod: after a full read the first-seen
// map holds only pods the read listed as unschedulable and untimed, so a pod
// that left is not kept in memory (gt-1gtq5g).
func TestCollectPendingSandboxPodsUntimedDropsAbsentPod(t *testing.T) {
	t.Parallel()
	f := newPendingSandboxFixture(t)
	f.reply = cliReply{stdout: sandboxPodListJSON(untimedSandboxPodJSON("default--gt-untimed", "0/3 nodes are available."))}
	if _, err := f.collect(t); err != nil {
		t.Fatal(err)
	}
	if len(f.d.sandboxPendingFirstSeen) != 1 {
		t.Fatalf("first-seen entries after the pod is listed = %v, want the one pod", f.d.sandboxPendingFirstSeen)
	}
	f.reply = cliReply{stdout: `{"items":[]}`}
	if _, err := f.collect(t); err != nil {
		t.Fatal(err)
	}
	if len(f.d.sandboxPendingFirstSeen) != 0 {
		t.Errorf("first-seen entries after the pod is gone = %v, want none", f.d.sandboxPendingFirstSeen)
	}
}

// TestPendingSandboxFailedReadKeepsFirstSeen: a read that fails keeps the
// first-seen map as it was, so the age keeps running across the failure and
// the item raises when the read recovers past the window (gt-1gtq5g).
func TestPendingSandboxFailedReadKeepsFirstSeen(t *testing.T) {
	t.Parallel()
	f := newPendingSandboxFixture(t)
	const key = "openshell/default--gt-untimed"
	t0 := pendingSandboxNow
	f.reply = cliReply{stdout: sandboxPodListJSON(untimedSandboxPodJSON("default--gt-untimed", "0/3 nodes are available."))}
	if _, err := f.collect(t); err != nil {
		t.Fatal(err)
	}
	f.src.now = t0.Add(30 * time.Second)
	f.runErr = errors.New("the server is unavailable")
	if _, err := f.collect(t); err == nil {
		t.Fatal("collector read a failed kubectl call as clean")
	}
	if got := f.d.sandboxPendingFirstSeen[key]; !got.Equal(t0) {
		t.Fatalf("first seen after a failed read = %s, want the first tick %s kept", got, t0)
	}
	f.runErr = nil
	f.src.now = t0.Add(61 * time.Second)
	items, err := f.collect(t)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Errorf("items after the read recovers = %+v, want the pod's item", items)
	}
}

// TestPendingSandboxTimedPodIgnoresFirstSeen: a pod with a usable creation time
// ages from that time, not from the first-seen map, and an untimed pod beside
// it does not change that (gt-1gtq5g).
func TestPendingSandboxTimedPodIgnoresFirstSeen(t *testing.T) {
	t.Parallel()
	f := newPendingSandboxFixture(t)
	f.reply = cliReply{stdout: sandboxPodListJSON(
		sandboxPodJSON("default--gt-timed", pendingSandboxNamespace, "Pending", "False", "Unschedulable", "0/3 nodes are available.", pendingSandboxNow.Add(-2*time.Minute)),
		untimedSandboxPodJSON("default--gt-untimed", "0/3 nodes are available."),
	)}
	items, err := f.collect(t)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Key != "sandbox-pending:openshell/default--gt-timed" {
		t.Fatalf("items = %+v, want only the timed pod's item", items)
	}
	if !strings.Contains(items[0].Summary, "pending 2m, unschedulable:") {
		t.Errorf("summary = %q, want the age from the pod's own creation time", items[0].Summary)
	}
	if _, ok := f.d.sandboxPendingFirstSeen["openshell/default--gt-timed"]; ok {
		t.Error("first-seen map holds the timed pod; only untimed pods belong there")
	}
}

// TestPendingSandboxUnparseableTimestampIsUntimed: a creation time that does
// not parse is an untimed pod, not a refused list, so the other pods in the
// reply are still read and the bad pod ages from first seen (gt-1gtq5g).
func TestPendingSandboxUnparseableTimestampIsUntimed(t *testing.T) {
	t.Parallel()
	f := newPendingSandboxFixture(t)
	f.reply = cliReply{stdout: `{"items":[` +
		`{"metadata":{"name":"default--gt-badstamp","namespace":"openshell","creationTimestamp":"not-a-time"},"status":{"phase":"Pending","conditions":[{"type":"PodScheduled","status":"False","reason":"Unschedulable","message":"0/3 nodes are available."}]}},` +
		sandboxPodJSON("default--gt-nux", pendingSandboxNamespace, "Pending", "False", "Unschedulable", "0/3 nodes are available.", pendingSandboxNow.Add(-5*time.Minute)) +
		`]}`}
	items, err := f.collect(t)
	if err != nil {
		t.Fatalf("an unparseable creation time refused the whole list: %v", err)
	}
	if len(items) != 1 || items[0].Key != "sandbox-pending:openshell/default--gt-nux" {
		t.Errorf("items = %+v, want only the timed pod's item", items)
	}
	if _, ok := f.d.sandboxPendingFirstSeen["openshell/default--gt-badstamp"]; !ok {
		t.Error("the pod with an unparseable time is not tracked as untimed")
	}
}

// TestPendingSandboxEmptyMessageStillRaises: an unschedulable pod whose
// scheduler gave no message still raises its item, saying so in place of the
// message, so the refusal is not hidden by an empty string (gt-1gtq5g).
func TestPendingSandboxEmptyMessageStillRaises(t *testing.T) {
	t.Parallel()
	f := newPendingSandboxFixture(t)
	f.reply = cliReply{stdout: sandboxPodListJSON(
		sandboxPodJSON("default--gt-quiet", pendingSandboxNamespace, "Pending", "False", "Unschedulable", "", pendingSandboxNow.Add(-5*time.Minute)),
	)}
	items, err := f.collect(t)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("items = %+v, want one for the pod with no scheduler message", items)
	}
	want := "sandbox pod openshell/default--gt-quiet pending 5m and unschedulable; the scheduler gave no message"
	if items[0].Summary != want {
		t.Errorf("summary = %q, want %q", items[0].Summary, want)
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
		sandboxPodJSON("default--gt-nux", pendingSandboxNamespace, "Pending", "False", "Unschedulable", "0/3 nodes are available: 3 Insufficient cpu.", pendingSandboxNow.Add(-5*time.Minute)),
	)}
	items, err = f.collect(t)
	if err != nil {
		t.Fatalf("the collection after a failed read: %v", err)
	}
	if len(items) != 1 || items[0].Key != "sandbox-pending:openshell/default--gt-nux" {
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

// hangPendingSandboxRead runs the attention collection for the kind through
// collectAttention while the cluster never answers its pod list, then moves the
// daemon's clock past the read deadline. It returns the items the tick observed.
// The bounded wait ends the test if the collection does not return once the
// deadline passes.
func hangPendingSandboxRead(t *testing.T, f *pendingSandboxFixture, prev attention.State) []attention.Item {
	t.Helper()
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	f.hang = release
	f.hangEntered = make(chan struct{}, 1)
	collector := collectorFor(t, f.src, attention.KindSandboxPending)
	observed := make(chan []attention.Item, 1)
	go func() {
		observed <- collectAttention(context.Background(), prev, []attentionCollector{collector}, f.d.logger.Printf)
	}()
	blockUntil(t, "the pod read to reach the cluster", f.hangEntered)
	f.clock.Advance(sandboxPendingReadTimeout)
	wait, cancel := context.WithTimeout(context.Background(), signalWait)
	defer cancel()
	select {
	case items := <-observed:
		f.hang = nil
		return items
	case <-wait.Done():
		t.Fatalf("the collection did not return after its %s read deadline", sandboxPendingReadTimeout)
		return nil
	}
}

// TestPendingSandboxCollectorGivesUpOnAHungCluster covers gt-hlo2ac: a sandbox
// cluster that never answers its pod list is cut off at the read deadline. The
// tick keeps the previous items and the first-seen map, logs the timeout, and
// the pod read carries kubectl's own request timeout.
func TestPendingSandboxCollectorGivesUpOnAHungCluster(t *testing.T) {
	t.Parallel()
	f := newPendingSandboxFixture(t)
	const untimed = "openshell/default--gt-untimed"
	firstSeen := pendingSandboxNow.Add(-30 * time.Second)
	f.d.sandboxPendingFirstSeen = map[string]time.Time{untimed: firstSeen}
	previous := attention.Item{
		Key:      sandboxPendingKeyPrefix + "openshell/default--gt-nux",
		Kind:     attention.KindSandboxPending,
		Severity: attention.SeverityHigh,
		Summary:  "sandbox pod openshell/default--gt-nux pending 3m, unschedulable: earlier",
	}

	items := hangPendingSandboxRead(t, f, attention.State{Items: []attention.Item{previous}})

	if len(items) != 1 || items[0].Key != previous.Key || items[0].Summary != previous.Summary {
		t.Errorf("items = %+v, want the previous tick's item kept", items)
	}
	if got := f.d.sandboxPendingFirstSeen; len(got) != 1 || !got[untimed].Equal(firstSeen) {
		t.Errorf("first-seen map = %v, want the previous map kept", got)
	}
	if got := f.logs.String(); !strings.Contains(got, "no answer within 15s") {
		t.Errorf("log = %q, want the read's timeout named", got)
	}
	var pods []cliCall
	for _, c := range f.cli.recorded() {
		if strings.HasPrefix(cacheTrimVerb(c.args), "get pods ") {
			pods = append(pods, c)
		}
	}
	if len(pods) != 1 {
		t.Fatalf("pod reads = %d, want one", len(pods))
	}
	if !strings.Contains(cacheTrimVerb(pods[0].args), "--request-timeout=10s") {
		t.Errorf("pod read args = %q, want --request-timeout=10s for this read", cacheTrimVerb(pods[0].args))
	}
}

// TestPendingSandboxCollectorRecoversAfterATimeout covers gt-hlo2ac: after a
// read that timed out, the next collection with a working cluster raises the
// item for a pod Unschedulable past the window.
func TestPendingSandboxCollectorRecoversAfterATimeout(t *testing.T) {
	t.Parallel()
	f := newPendingSandboxFixture(t)
	const message = "0/3 nodes are available: 3 Insufficient cpu."
	f.reply = cliReply{stdout: sandboxPodListJSON(
		sandboxPodJSON("default--gt-nux", pendingSandboxNamespace, "Pending", "False", "Unschedulable", message, pendingSandboxNow.Add(-3*time.Minute)),
	)}

	if items := hangPendingSandboxRead(t, f, attention.State{}); len(items) != 0 {
		t.Fatalf("the timed-out tick observed %+v, want nothing from an empty previous state", items)
	}

	items, err := f.collect(t)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Key != "sandbox-pending:openshell/default--gt-nux" {
		t.Fatalf("items = %+v, want the unschedulable pod raised after the timeout", items)
	}
	want := "sandbox pod openshell/default--gt-nux pending 3m, unschedulable: " + message
	if items[0].Summary != want {
		t.Errorf("summary = %q, want %q", items[0].Summary, want)
	}
}

package metrics

import (
	"context"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/specdispatch"
)

// seamCall is one bd invocation the recorder answered: the directory the store
// reads (the rig scoping `bd -C <dir>` used to carry) and the argv after "bd".
type seamCall struct {
	dir  string
	args string
}

// bdRecorder is the seam's runner in these tests: it records every bd call
// with the directory of the store that made it and answers the reads the
// metrics sources ask for.
type bdRecorder struct {
	mu    sync.Mutex
	calls []seamCall
}

// env is an Env whose bead store is the real seam over the recorder, so the
// argv and directory bd would receive are the ones production sends.
func (r *bdRecorder) env(t *testing.T) Env {
	t.Helper()
	env := envAt(fixedNow())
	env.Town = queueTestTown
	env.ReadFile = func(path string) ([]byte, error) {
		if path == filepath.Join(queueTestTown, "mayor", "rigs.json") {
			return []byte(`{"version":1,"rigs":{"alpha":{},"beta":{}}}`), nil
		}
		return nil, fs.ErrNotExist
	}
	env.Tail = func(string, int64) ([]byte, error) { return nil, fs.ErrNotExist }
	env.Beads = func(ctx context.Context, dir string) BeadStore {
		return beads.New(dir, beads.WithCallContext(ctx), beads.WithRunner(func(_ context.Context, args []string) ([]byte, []byte, error) {
			return r.answer(dir, args)
		}))
	}
	return env
}

// answer is bd's side of the recorder: the allow-stale probe claims the
// installed bd takes the flag, and each verb answers an empty page.
func (r *bdRecorder) answer(dir string, args []string) ([]byte, []byte, error) {
	r.mu.Lock()
	r.calls = append(r.calls, seamCall{dir: dir, args: strings.Join(args, " ")})
	r.mu.Unlock()
	if len(args) > 0 && args[len(args)-1] == "version" {
		return []byte("bd version 1.2.2\n"), nil, nil
	}
	verb := args[0]
	if verb == "--allow-stale" && len(args) > 1 {
		verb = args[1]
	}
	switch verb {
	case "ready":
		return []byte(`{"data":[],"pagination":{"truncated":false,"returned":0}}`), nil, nil
	case "list", "blocked":
		return []byte("[]"), nil, nil
	}
	return nil, nil, fmt.Errorf("unexpected bd argv %v", args)
}

// reads is the recorded calls without the seam's own allow-stale probe.
func (r *bdRecorder) reads() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, call := range r.calls {
		if strings.HasSuffix(call.args, "version") {
			continue
		}
		out = append(out, call.dir+" | "+call.args)
	}
	sort.Strings(out)
	return out
}

// The queue group reads every rig through the seam and asks for the same bd
// reads it built itself before: the verbs, the filters and the rig scoping are
// unchanged, and each read is scoped to its own rig directory the way `bd -C
// <town>/<rig>` was.
func TestSourceQueueReadsReachBdScopedToTheirRig(t *testing.T) {
	t.Parallel()
	rec := &bdRecorder{}
	if _, err := (queueSource{}).Collect(context.Background(), rec.env(t)); err != nil {
		t.Fatalf("Collect: %v", err)
	}

	labels := append(append([]string(nil), constants.NonDispatchableBeadLabels...), specdispatch.ReadyQueryLabels()...)
	argv := map[string]string{
		"ready":           "--allow-stale ready --json --limit 0",
		"dispatchable":    "--allow-stale ready --json --unassigned --exclude-label " + strings.Join(labels, ",") + " --exclude-type " + strings.Join(specdispatch.NonWorkBeadTypes(), ",") + " --limit 0",
		"waiting_to_land": "--allow-stale list --json --label=gt:ready-to-land --limit=0 --flat",
		"blocked":         "--allow-stale blocked --json",
	}
	var want []string
	for _, rig := range []string{"alpha", "beta"} {
		for _, column := range []string{"ready", "dispatchable", "waiting_to_land", "blocked"} {
			want = append(want, filepath.Join(queueTestTown, rig)+" | "+argv[column])
		}
	}
	sort.Strings(want)
	if got := rec.reads(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("bd reads:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// The host group's bd_latency_ms is one list of one issue in the town's own
// store, the read the old probe timed in bd itself.
func TestSourceHostTimesOneListInTheTownStore(t *testing.T) {
	t.Parallel()
	rec := &bdRecorder{}
	env := rec.env(t)
	env.Run = func(context.Context, []string) ([]byte, error) {
		return nil, fmt.Errorf("the other host probes are not under test")
	}
	got, err := newHostSource().Collect(context.Background(), env)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	want := []string{queueTestTown + " | --allow-stale list --json --limit=1 --flat"}
	if reads := rec.reads(); strings.Join(reads, "\n") != strings.Join(want, "\n") {
		t.Fatalf("bd reads = %v, want %v", reads, want)
	}
	if got["bd_latency_ms"] == nil {
		t.Errorf("bd_latency_ms = nil; a read the store answered must be a reading")
	}
}

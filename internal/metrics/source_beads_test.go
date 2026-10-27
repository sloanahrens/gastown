package metrics

import (
	"context"
	"errors"
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

// seamCall is one bd invocation the recorder answered: the directory the
// source opened the store for and the argv after "bd".
type seamCall struct {
	dir  string
	args []string
}

// bdRecorder is the seam's runner in these tests: it records every bd call
// with the directory the source opened the store for and answers the reads the
// metrics sources ask for.
type bdRecorder struct {
	mu    sync.Mutex
	calls []seamCall
}

// env is an Env whose bead store is the real seam over the recorder, so the
// argv bd receives is the one production sends. The directory recorded is the
// one the source hands the store factory; beads.New(dir) runs bd there, which
// internal/beads pins, so that pair is the scope `bd -C <dir>` used to carry.
func (r *bdRecorder) env(t *testing.T) Env {
	t.Helper()
	return seamEnv(t, r.answer)
}

// seamEnv is an Env whose bead store is the real seam over run: the source
// under test reads through beads.New, with its parsers, its argv and its
// failure classification, and run stands in for the bd process. dir is the
// directory the source opened the store for.
func seamEnv(t *testing.T, run func(dir string, args []string) ([]byte, []byte, error)) Env {
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
		return beads.New(dir, beads.WithCallContext(ctx), beads.WithDirRunner(func(_ context.Context, bdDir string, args []string) ([]byte, []byte, error) {
			return run(bdDir, args)
		}))
	}
	return env
}

// bdVerb is the command word of a recorded bd argv, skipping the --allow-stale
// the seam prepends.
func bdVerb(args []string) string {
	if len(args) > 0 && args[0] == "--allow-stale" {
		args = args[1:]
	}
	if len(args) == 0 {
		return ""
	}
	return args[0]
}

// answer is bd's side of the recorder: the allow-stale probe claims the
// installed bd takes the flag, and each verb answers an empty page.
func (r *bdRecorder) answer(dir string, args []string) ([]byte, []byte, error) {
	r.mu.Lock()
	r.calls = append(r.calls, seamCall{dir: dir, args: append([]string(nil), args...)})
	r.mu.Unlock()
	if len(args) > 0 && args[len(args)-1] == "version" {
		return []byte("bd version 1.2.2\n"), nil, nil
	}
	switch bdVerb(args) {
	case "ready":
		return []byte(`{"data":[],"pagination":{"truncated":false,"returned":0}}`), nil, nil
	case "list", "blocked":
		return []byte("[]"), nil, nil
	}
	return nil, nil, fmt.Errorf("unexpected bd argv %v", args)
}

// reads is every recorded read as bd received it — the full argv, nothing
// stripped — paired with the directory the store was opened for, so the pair
// is the call the source made: `bd -C <dir> <args>`, scoping included. The
// seam's own `--allow-stale version` probes are left out.
func (r *bdRecorder) reads() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, call := range r.calls {
		if len(call.args) > 0 && call.args[len(call.args)-1] == "version" {
			continue
		}
		out = append(out, call.dir+" | "+strings.Join(call.args, " "))
	}
	sort.Strings(out)
	return out
}

// rawReads is every recorded call, the version probes included, for a failure
// message.
func (r *bdRecorder) rawReads() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, call := range r.calls {
		out = append(out, call.dir+" | "+strings.Join(call.args, " "))
	}
	sort.Strings(out)
	return out
}

// The queue group reads every rig through the seam, and the argv below is the
// whole of what bd receives: the verb, the filters, their values and their
// order are the ones the source built itself before the seam, and the rig
// scoping rides the directory the store was opened for, which is what
// `bd -C <town>/<rig>` did. Any change to any argument — a different limit, a
// dropped filter, a new flag the seam starts injecting — turns this red.
//
// The differences from the pre-seam argv are exactly the seam's own, and they
// are pinned here rather than normalised away:
//
//	--allow-stale   prepended when the installed bd takes the flag
//	--flat          appended to a list --json, which bd needs to print JSON
//	--limit=0       the list's limit as the seam spells a flag value; ready
//	                passes --limit 0 as two arguments
//
// The one argument that is not a difference is the one that is absent: the
// seam adds no status filter to the list, so bd list's default scope is the
// query it was before.
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
		t.Fatalf("bd reads:\n%s\nwant:\n%s\nraw:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"), strings.Join(rec.rawReads(), "\n"))
	}
}

// The host group's bd_latency_ms is one list of one issue in the town's own
// store, read twice: the first read warms the seam so the timed second is a
// steady-state read. Both reads are the pre-seam `list --limit 1` with the
// seam's own arguments, pinned whole: --allow-stale prepended, --flat appended
// because that is how bd prints a list as JSON, and the limit spelled as the
// seam's flag value.
func TestSourceHostTimesASteadyStateListInTheTownStore(t *testing.T) {
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
	want := []string{
		queueTestTown + " | --allow-stale list --json --limit=1 --flat",
		queueTestTown + " | --allow-stale list --json --limit=1 --flat",
	}
	if reads := rec.reads(); strings.Join(reads, "\n") != strings.Join(want, "\n") {
		t.Fatalf("bd reads:\n%s\nwant:\n%s\nraw:\n%s", strings.Join(reads, "\n"), strings.Join(want, "\n"), strings.Join(rec.rawReads(), "\n"))
	}
	if got["bd_latency_ms"] == nil {
		t.Errorf("bd_latency_ms = nil; a read the store answered must be a reading")
	}
}

// The seam parses bd's answer now, so a shape its parser refuses reaches the
// queue read as an error and nils only that column: a ready page bd reported
// as cut is not a short board, and a half-written page or prose where JSON was
// asked for is not an empty one. The answers go through the real store over a
// fake bd, so the parsers in the test are the ones production runs.
func TestSourceQueueRefusesAnswersItsParserCannotRead(t *testing.T) {
	t.Parallel()
	readyEmpty := `{"data":[],"pagination":{"returned":0,"truncated":false}}`
	blockedTwo := `[{"id":"gt-a","title":"a","status":"open"},{"id":"gt-b","title":"b","status":"open"}]`
	tests := []struct {
		name   string
		verb   string
		answer string
		want   map[string]any
	}{
		{
			name:   "a ready page bd reported as cut",
			verb:   "ready",
			answer: `{"data":[],"pagination":{"returned":100,"total":250,"truncated":true}}`,
			want:   map[string]any{"ready": nil, "dispatchable": nil, "waiting_to_land": 0, "blocked": 4},
		},
		{
			name:   "a ready answer that is not JSON",
			verb:   "ready",
			answer: `[{"id":`,
			want:   map[string]any{"ready": nil, "dispatchable": nil, "waiting_to_land": 0, "blocked": 4},
		},
		{
			name:   "prose where the ready page was asked for",
			verb:   "ready",
			answer: "error: connection refused\n",
			want:   map[string]any{"ready": nil, "dispatchable": nil, "waiting_to_land": 0, "blocked": 4},
		},
		{
			name:   "a list answer that is not JSON",
			verb:   "list",
			answer: `[{"id":`,
			want:   map[string]any{"ready": 0, "dispatchable": 0, "waiting_to_land": nil, "blocked": 4},
		},
		{
			name:   "prose where the blocked list was asked for",
			verb:   "blocked",
			answer: "error: no Dolt server\n",
			want:   map[string]any{"ready": 0, "dispatchable": 0, "waiting_to_land": 0, "blocked": nil},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := seamEnv(t, func(_ string, args []string) ([]byte, []byte, error) {
				if len(args) > 0 && args[len(args)-1] == "version" {
					return []byte("bd version 1.2.2\n"), nil, nil
				}
				verb := bdVerb(args)
				if verb == tc.verb {
					return []byte(tc.answer), nil, nil
				}
				switch verb {
				case "ready":
					return []byte(readyEmpty), nil, nil
				case "list":
					return []byte("[]"), nil, nil
				case "blocked":
					return []byte(blockedTwo), nil, nil
				}
				return nil, nil, fmt.Errorf("unexpected bd argv %v", args)
			})
			cols, err := (queueSource{}).Collect(context.Background(), env)
			if err != nil {
				t.Fatalf("Collect: %v", err)
			}
			// Two rigs, two beads each for the answers that parse, so only the
			// column whose command answered an unreadable shape is a gap.
			queueWantColumns(t, cols, tc.want)
		})
	}
}

// The timed read bd_latency_ms measures is one bd invocation: the seam retries
// only a failure against its ephemeral test Dolt container, and a town store
// built with beads.New is not that, so a connection-shaped failure ends the
// timed read rather than starting another. The column is a gap and it cost one
// invocation, so a retry can never be inside the number the column reports.
func TestSourceHostTimedReadIsOneBdInvocation(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	lists := 0
	env := seamEnv(t, func(_ string, args []string) ([]byte, []byte, error) {
		if len(args) > 0 && args[len(args)-1] == "version" {
			return []byte("bd version 1.2.2\n"), nil, nil
		}
		mu.Lock()
		lists++
		read := lists
		mu.Unlock()
		if read == 1 {
			return []byte("[]"), nil, nil
		}
		return nil, []byte("Error: failed to open database: invalid connection"), errors.New("exit status 1")
	})
	// The other probes answer, so host_probe_failures counts the bead read alone.
	env.Run = hostFixtureEnv(fixedNow()).env().Run

	got, err := newHostSource().Collect(context.Background(), env)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if got["bd_latency_ms"] != nil || got["host_probe_failures"] != 1 {
		t.Errorf("bd_latency_ms = %v, host_probe_failures = %v, want nil and 1 for a failed timed read", got["bd_latency_ms"], got["host_probe_failures"])
	}
	if lists != 2 {
		t.Errorf("bd ran %d list(s) for the warm-up and the failed timed read, want 2: a town store has no test-container retry class", lists)
	}
}

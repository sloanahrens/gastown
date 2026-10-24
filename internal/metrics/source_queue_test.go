package metrics

import (
	"context"
	"fmt"
	"io/fs"
	"math"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// queueTestTown is the town root every queue test reads under, so the argv a
// fake bd sees and the paths the source reads are both fixed.
const queueTestTown = "/town"

// queueTestLog is the daemon log a queue test serves through Env.Tail.
type queueTestLog struct {
	data []byte
	err  error
	max  int64
}

// queueTestEnv is an Env whose registry, runner and daemon log a test owns.
func queueTestEnv(t *testing.T, files map[string]string, run Runner, log *queueTestLog) Env {
	t.Helper()
	env := envAt(fixedNow())
	env.Town = queueTestTown
	env.ReadFile = func(path string) ([]byte, error) {
		if data, ok := files[path]; ok {
			return []byte(data), nil
		}
		return nil, fs.ErrNotExist
	}
	env.Run = run
	env.Tail = func(path string, maxBytes int64) ([]byte, error) {
		if log == nil {
			return nil, fs.ErrNotExist
		}
		log.max = maxBytes
		return log.data, log.err
	}
	return env
}

// queueTestRigs is a mayor/rigs.json holding the named rigs.
func queueTestRigs(names ...string) map[string]string {
	var b strings.Builder
	b.WriteString(`{"version":1,"rigs":{`)
	for i, name := range names {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, "%q:{}", name)
	}
	b.WriteString("}}")
	return map[string]string{filepath.Join(queueTestTown, "mayor", "rigs.json"): b.String()}
}

// queueBdFake answers one rig's bead reads from a table of row counts and
// failures keyed "<rig>/<column>", and records what it was asked.
type queueBdFake struct {
	rows map[string]int
	fail map[string]bool

	mu              sync.Mutex
	calls           []string
	deadlineMissing bool
}

func (f *queueBdFake) run(ctx context.Context, argv []string) ([]byte, error) {
	if _, ok := ctx.Deadline(); !ok {
		f.mu.Lock()
		f.deadlineMissing = true
		f.mu.Unlock()
	}
	f.mu.Lock()
	f.calls = append(f.calls, strings.Join(argv, " "))
	f.mu.Unlock()
	if len(argv) < 4 || argv[0] != "bd" || argv[1] != "-C" {
		return nil, fmt.Errorf("unexpected argv %v", argv)
	}
	key := filepath.Base(argv[2]) + "/" + queueBdKind(argv)
	if f.fail[key] {
		return nil, fmt.Errorf("bd %s: no Dolt server", key)
	}
	rows := make([]string, f.rows[key])
	for i := range rows {
		rows[i] = fmt.Sprintf(`{"id":"row-%d"}`, i)
	}
	return []byte("[" + strings.Join(rows, ",") + "]"), nil
}

// queueBdKind names the column a bd argv fills, the way the source's own
// flags distinguish them.
func queueBdKind(argv []string) string {
	joined := strings.Join(argv, " ")
	switch {
	case strings.Contains(joined, "--unassigned"):
		return "dispatchable"
	case strings.Contains(joined, "--label gt:ready-to-land"):
		return "waiting_to_land"
	case strings.Contains(joined, "--status blocked"):
		return "blocked"
	}
	return "ready"
}

// queueWantColumns checks each wanted column's value and that the named
// columns are absent.
func queueWantColumns(t *testing.T, cols map[string]any, want map[string]any, absent ...string) {
	t.Helper()
	for name, expected := range want {
		got, ok := cols[name]
		if !ok {
			t.Errorf("%s is missing from the group; want %v", name, expected)
			continue
		}
		if gotFloat, ok := expected.(float64); ok {
			if have, ok := got.(float64); !ok || math.Abs(have-gotFloat) > 1e-9 {
				t.Errorf("%s = %v, want %v", name, got, expected)
			}
			continue
		}
		if got != expected {
			t.Errorf("%s = %v (%T), want %v (%T)", name, got, got, expected, expected)
		}
	}
	for _, name := range absent {
		if prefix, ok := strings.CutSuffix(name, "*"); ok {
			for key := range cols {
				if strings.HasPrefix(key, prefix) {
					t.Errorf("%s is in the group; want every %s* column left out", key, prefix)
				}
			}
			continue
		}
		if _, ok := cols[name]; ok {
			t.Errorf("%s is in the group; want it left out", name)
		}
	}
}

func TestSourceQueueSumsBeadCountsOverRigs(t *testing.T) {
	t.Parallel()
	fake := &queueBdFake{rows: map[string]int{
		"alpha/ready": 3, "alpha/dispatchable": 2, "alpha/waiting_to_land": 1, "alpha/blocked": 4,
		"beta/ready": 5, "beta/dispatchable": 0, "beta/waiting_to_land": 1, "beta/blocked": 2,
	}}
	env := queueTestEnv(t, queueTestRigs("alpha", "beta"), fake.run, nil)
	cols, err := queueSource{}.Collect(context.Background(), env)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	queueWantColumns(t, cols, map[string]any{
		"ready": 8, "dispatchable": 2, "waiting_to_land": 2, "blocked": 6,
	})
	if fake.deadlineMissing {
		t.Error("a bd read ran without a deadline; every call takes queueBdTimeout")
	}
	if want := 2 * len(queueBeadColumns()); len(fake.calls) != want {
		t.Errorf("bd calls = %d, want one per rig per column (%d):\n%s", len(fake.calls), want, strings.Join(fake.calls, "\n"))
	}
}

func TestSourceQueueFailingReadNilsOnlyItsColumn(t *testing.T) {
	t.Parallel()
	fake := &queueBdFake{
		rows: map[string]int{
			"alpha/ready": 3, "alpha/dispatchable": 2, "alpha/waiting_to_land": 1, "alpha/blocked": 4,
			"beta/ready": 5, "beta/dispatchable": 1, "beta/waiting_to_land": 1, "beta/blocked": 2,
		},
		fail: map[string]bool{"beta/ready": true},
	}
	env := queueTestEnv(t, queueTestRigs("alpha", "beta"), fake.run, &queueTestLog{data: []byte(queueTickFixture)})
	cols, err := queueSource{}.Collect(context.Background(), env)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	queueWantColumns(t, cols, map[string]any{
		"ready": nil, "dispatchable": 3, "waiting_to_land": 2, "blocked": 6,
		"dispatch_ticks_1h": 1,
	})
}

// queueTickFixture is a tick line and its duration line in the shape the
// daemon writes them, stamps included.
const queueTickFixture = "2026/10/24 12:30:00 spec_dispatch: tick: 7 candidate(s), roster deepseek-flash 2/3, 1 dispatched, 0 refused, 0 planning, 3 skipped, 0 failed, 0 held by the failed label\n"

func TestSourceQueueParsesDaemonLogColumns(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		log    string
		want   map[string]any
		absent []string
	}{
		{
			name: "the last hour's ticks and the newest roster",
			log: "2026/10/24 11:59:59 spec_dispatch: tick: 9 candidate(s), roster deepseek-flash 0/3, 9 dispatched, 0 refused, 0 planning, 0 skipped, 0 failed, 0 held by the failed label\n" +
				"2026/10/24 12:30:00 spec_dispatch: tick: 7 candidate(s), roster deepseek-flash 2/3, claude-sonnet 1/2, 1 dispatched, 0 refused, 0 planning, 3 skipped, 0 failed, 0 held by the failed label\n" +
				"2026/10/24 12:31:00 spec_dispatch: tick took 4.512s (roster 1.2ms, board 900.5ms, candidates 7 2.1ms, sling 3.4s)\n" +
				"2026/10/24 12:45:00 spec_dispatch: tick: 5 candidate(s), roster deepseek-flash 2/3, claude-sonnet 2/2, 2 dispatched, 1 refused, 0 planning, 1 skipped, 0 failed, 0 held by the failed label\n",
			want: map[string]any{
				"dispatch_ticks_1h": 2, "dispatched_1h": 3, "last_tick_secs": 4.512,
				"seat_live_deepseek-flash": 2, "seat_cap_deepseek-flash": 3,
				"seat_live_claude-sonnet": 2, "seat_cap_claude-sonnet": 2,
			},
		},
		{
			name: "the town total is not a seat, and a closed seat keeps its count",
			log:  "2026/10/24 12:40:00 spec_dispatch: tick: 2 candidate(s), roster claude-sonnet 1/2 (closed until 08:00), total 2/10, 0 dispatched, 0 refused, 0 planning, 0 skipped, 0 failed, 0 held by the failed label\n",
			want: map[string]any{
				"dispatch_ticks_1h": 1, "dispatched_1h": 0,
				"seat_live_claude-sonnet": 1, "seat_cap_claude-sonnet": 2,
			},
			absent: []string{"seat_live_total", "seat_cap_total"},
		},
		{
			name: "no seats reports no seat columns",
			log:  "2026/10/24 12:40:00 spec_dispatch: tick: 0 candidate(s), roster no seats, 0 dispatched, 0 refused, 0 planning, 0 skipped, 0 failed, 0 held by the failed label\n",
			want: map[string]any{
				"dispatch_ticks_1h": 1, "dispatched_1h": 0, "last_tick_secs": nil,
			},
			absent: []string{"seat_*"},
		},
		{
			name: "a line in another shape is not a tick",
			log: "2026/10/24 12:40:00 spec_dispatch: tick: 57 candidate(s), 2 refused, 0 planning\n" +
				"2026/10/24 12:41:00 spec_dispatch: held: the town is parked\n" +
				"2026/10/24 12:42:00 daemon: patrol tick complete\n",
			want:   map[string]any{"dispatch_ticks_1h": 0, "dispatched_1h": 0, "last_tick_secs": nil},
			absent: []string{"seat_*"},
		},
		{
			name: "an empty log is zero ticks, not a failed read",
			log:  "",
			want: map[string]any{"dispatch_ticks_1h": 0, "dispatched_1h": 0, "last_tick_secs": nil},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fake := &queueBdFake{rows: map[string]int{}}
			log := &queueTestLog{data: []byte(tc.log)}
			env := queueTestEnv(t, queueTestRigs("alpha"), fake.run, log)
			cols, err := queueSource{}.Collect(context.Background(), env)
			if err != nil {
				t.Fatalf("Collect: %v", err)
			}
			queueWantColumns(t, cols, tc.want, tc.absent...)
			if log.max != queueLogTailBytes {
				t.Errorf("Tail max = %d, want %d", log.max, queueLogTailBytes)
			}
		})
	}
}

func TestSourceQueueUnreadableLogLeavesLogColumnsNil(t *testing.T) {
	t.Parallel()
	fake := &queueBdFake{rows: map[string]int{"alpha/ready": 4, "alpha/blocked": 1}}
	env := queueTestEnv(t, queueTestRigs("alpha"), fake.run, nil)
	cols, err := queueSource{}.Collect(context.Background(), env)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	queueWantColumns(t, cols, map[string]any{
		"ready": 4, "blocked": 1,
		"dispatch_ticks_1h": nil, "dispatched_1h": nil, "last_tick_secs": nil,
	}, "seat_*")
}

func TestSourceQueueReadsOnlyTheLastLines(t *testing.T) {
	t.Parallel()
	tick := "2026/10/24 12:30:00 spec_dispatch: tick: 1 candidate(s), roster deepseek-flash 1/3, 1 dispatched, 0 refused, 0 planning, 0 skipped, 0 failed, 0 held by the failed label"
	lines := make([]string, queueLogMaxLines+5)
	for i := range lines {
		lines[i] = "2026/10/24 12:30:00 daemon: filling"
	}
	// The first kept line is index 5: the tick there counts, the one before the
	// cut does not.
	lines[4] = tick
	lines[5] = tick
	fake := &queueBdFake{rows: map[string]int{}}
	env := queueTestEnv(t, queueTestRigs("alpha"), fake.run, &queueTestLog{data: []byte(strings.Join(lines, "\n"))})
	cols, err := queueSource{}.Collect(context.Background(), env)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	queueWantColumns(t, cols, map[string]any{"dispatch_ticks_1h": 1, "dispatched_1h": 1})
}

func TestSourceQueueReadsTheTwoFileRegistry(t *testing.T) {
	t.Parallel()
	files := map[string]string{
		filepath.Join(queueTestTown, "mayor", "town.json"): `{"type":"town","version":1,"registry":{"version":1,"rigs":{"alpha":{}}}}`,
	}
	fake := &queueBdFake{rows: map[string]int{"alpha/ready": 2}}
	env := queueTestEnv(t, files, fake.run, nil)
	cols, err := queueSource{}.Collect(context.Background(), env)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	queueWantColumns(t, cols, map[string]any{"ready": 2})
}

func TestSourceQueueUnreadableRegistryIsAnError(t *testing.T) {
	t.Parallel()
	fake := &queueBdFake{rows: map[string]int{}}
	env := queueTestEnv(t, map[string]string{}, fake.run, nil)
	if _, err := (queueSource{}).Collect(context.Background(), env); err == nil {
		t.Fatal("Collect succeeded with no rig registry; want an error")
	}
	if len(fake.calls) != 0 {
		t.Errorf("bd was run %d times with no rigs; want none", len(fake.calls))
	}
}

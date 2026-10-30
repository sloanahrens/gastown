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

	"github.com/steveyegge/gastown/internal/beads"
)

// queueTestTown is the town root every queue test reads under, so the
// directories the fake store is opened for and the paths the source reads are
// both fixed.
const queueTestTown = "/town"

// queueTestLog is the daemon log a queue test serves through Env.Tail.
type queueTestLog struct {
	data []byte
	err  error
	max  int64
}

// queueTestEnv is an Env whose registry, bead store and daemon log a test owns.
func queueTestEnv(t *testing.T, files map[string]string, stores *queueStores, log *queueTestLog) Env {
	t.Helper()
	env := envAt(fixedNow())
	env.Town = queueTestTown
	env.ReadFile = func(path string) ([]byte, error) {
		if data, ok := files[path]; ok {
			return []byte(data), nil
		}
		return nil, fs.ErrNotExist
	}
	env.Beads = stores.open
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

// queueTestRigList is the rigs named, each with itself as its prefix: the
// registry queueTestRigs builds records no beads prefix, so a rig's prefix is
// its name.
func queueTestRigList(names ...string) []queueRig {
	rigs := make([]queueRig, len(names))
	for i, name := range names {
		rigs[i] = queueRig{name: name, prefix: name}
	}
	return rigs
}

// queueStores is the fake bead store the queue tests read: one rig's reads
// answered from a table of issue counts, ephemeral counts and failures keyed
// "<rig>/<column>", with every read recorded.
type queueStores struct {
	rows map[string]int  // the issues the read answers
	wisp map[string]int  // how many of those issues are ephemeral
	fail map[string]bool // a read that fails the way a dead store does

	mu              sync.Mutex
	calls           []string
	dirs            []string
	deadlineMissing bool
}

// open hands out one rig's store and records the directory and deadline it was
// asked for.
func (q *queueStores) open(ctx context.Context, dir string) BeadStore {
	q.mu.Lock()
	defer q.mu.Unlock()
	if _, ok := ctx.Deadline(); !ok {
		q.deadlineMissing = true
	}
	q.dirs = append(q.dirs, dir)
	return &queueStore{owner: q, rig: filepath.Base(dir)}
}

// queueStore serves one rig, naming the column by the seam call the source
// made, the way its bd flags used to.
type queueStore struct {
	owner *queueStores
	rig   string
}

func (s *queueStore) List(opts beads.ListOptions) ([]*beads.Issue, error) {
	if opts.Label != "gt:ready-to-land" || opts.Limit != 0 {
		return nil, fmt.Errorf("unexpected List options %+v", opts)
	}
	return s.serve("waiting_to_land")
}

func (s *queueStore) ReadyFiltered(opts beads.ReadyOptions) ([]*beads.Issue, error) {
	if opts.Limit != 0 {
		return nil, fmt.Errorf("unexpected ready limit %d; the board is read whole", opts.Limit)
	}
	if opts.Unassigned {
		return s.serve("dispatchable")
	}
	return s.serve("ready")
}

func (s *queueStore) Blocked() ([]*beads.Issue, error) { return s.serve("blocked") }

func (s *queueStore) ShowMultiple([]string) (map[string]*beads.Issue, error) {
	return nil, fmt.Errorf("the queue group read beads by id")
}

// serve answers one column's read in this rig.
func (s *queueStore) serve(column string) ([]*beads.Issue, error) {
	key := s.rig + "/" + column
	o := s.owner
	o.mu.Lock()
	o.calls = append(o.calls, key)
	failed, n, wisps := o.fail[key], o.rows[key], o.wisp[key]
	o.mu.Unlock()
	if failed {
		return nil, fmt.Errorf("bd %s: no Dolt server", key)
	}
	issues := make([]*beads.Issue, n)
	for i := range issues {
		issues[i] = &beads.Issue{ID: fmt.Sprintf("%s-%d", column, i), Ephemeral: i < wisps}
	}
	return issues, nil
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
	fake := &queueStores{rows: map[string]int{
		"alpha/ready": 3, "alpha/dispatchable": 2, "alpha/waiting_to_land": 1, "alpha/blocked": 4,
		"beta/ready": 5, "beta/dispatchable": 0, "beta/waiting_to_land": 1, "beta/blocked": 2,
	}}
	env := queueTestEnv(t, queueTestRigs("alpha", "beta"), fake, nil)
	cols, err := queueSource{}.Collect(context.Background(), env)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	queueWantColumns(t, cols, map[string]any{
		"ready": 8, "dispatchable": 2, "waiting_to_land": 2, "blocked": 6,
	})
	if fake.deadlineMissing {
		t.Error("a bead read ran without a deadline; every call takes queueBdTimeout")
	}
	if want := 2 * len(queueBeadColumns()); len(fake.calls) != want {
		t.Errorf("bead reads = %d, want one per rig per column (%d):\n%s", len(fake.calls), want, strings.Join(fake.calls, "\n"))
	}
	// The rig's own directory is the scope: the seam's store for that path is
	// what `bd -C <town>/<rig>` did.
	wantDirs := map[string]bool{"/town/alpha": true, "/town/beta": true}
	for _, dir := range fake.dirs {
		if !wantDirs[dir] {
			t.Errorf("a read opened %q, want one of %v", dir, wantDirs)
		}
	}
}

func TestSourceQueueFailingReadNilsOnlyItsColumn(t *testing.T) {
	t.Parallel()
	fake := &queueStores{
		rows: map[string]int{
			"alpha/ready": 3, "alpha/dispatchable": 2, "alpha/waiting_to_land": 1, "alpha/blocked": 4,
			"beta/ready": 5, "beta/dispatchable": 1, "beta/waiting_to_land": 1, "beta/blocked": 2,
		},
		fail: map[string]bool{"beta/ready": true},
	}
	env := queueTestEnv(t, queueTestRigs("alpha", "beta"), fake, &queueTestLog{data: []byte(queueTickFixture)})
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
			name: "ticks older than an hour report their age and no duration or seats",
			log: "2026/10/24 10:00:00 spec_dispatch: tick: 4 candidate(s), roster deepseek-flash 1/3, 2 dispatched, 0 refused, 0 planning, 0 skipped, 0 failed, 0 held by the failed label\n" +
				"2026/10/24 10:00:05 spec_dispatch: tick took 3.0s (roster 1ms, sling 2s)\n",
			want: map[string]any{
				"dispatch_ticks_1h": 0, "dispatched_1h": 0, "last_tick_secs": nil,
				"dispatch_newest_tick_age_s": 10800.0, "dispatch_unparsed_lines": 0,
			},
			absent: []string{"seat_*"},
		},
		{
			name: "a tick exactly an hour old still counts, the second before it does not",
			log: "2026/10/24 11:59:59 spec_dispatch: tick: 4 candidate(s), roster deepseek-flash 0/3, 5 dispatched, 0 refused, 0 planning, 0 skipped, 0 failed, 0 held by the failed label\n" +
				"2026/10/24 12:00:00 spec_dispatch: tick: 4 candidate(s), roster deepseek-flash 1/3, 2 dispatched, 0 refused, 0 planning, 0 skipped, 0 failed, 0 held by the failed label\n",
			want: map[string]any{
				"dispatch_ticks_1h": 1, "dispatched_1h": 2, "dispatch_newest_tick_age_s": 3600.0,
				"seat_live_deepseek-flash": 1, "seat_cap_deepseek-flash": 3,
			},
		},
		{
			name: "a duration line from before the window is ignored and the newest in-window one wins",
			log: "2026/10/24 11:00:00 spec_dispatch: tick took 9.0s (roster 1ms)\n" +
				"2026/10/24 12:10:00 spec_dispatch: tick took 2.0s (roster 1ms)\n" +
				"2026/10/24 12:20:00 spec_dispatch: tick took 6.5s (roster 1ms)\n",
			want: map[string]any{"last_tick_secs": 6.5, "dispatch_ticks_1h": 0},
		},
		{
			name: "tick lines with no earlier timestamp are counted apart, not summed",
			log: "spec_dispatch: tick: 3 candidate(s), roster deepseek-flash 9/9, 8 dispatched, 0 refused, 0 planning, 0 skipped, 0 failed, 0 held by the failed label\n" +
				"spec_dispatch: tick took 1.0s (roster 1ms)\n" +
				"2026/10/24 12:30:00 spec_dispatch: tick: 4 candidate(s), roster deepseek-flash 1/3, 2 dispatched, 0 refused, 0 planning, 0 skipped, 0 failed, 0 held by the failed label\n",
			want: map[string]any{
				"dispatch_ticks_1h": 1, "dispatched_1h": 2, "dispatch_unparsed_lines": 2,
				"seat_live_deepseek-flash": 1, "seat_cap_deepseek-flash": 3, "last_tick_secs": nil,
			},
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
			fake := &queueStores{}
			log := &queueTestLog{data: []byte(tc.log)}
			env := queueTestEnv(t, queueTestRigs("alpha"), fake, log)
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
	fake := &queueStores{rows: map[string]int{"alpha/ready": 4, "alpha/blocked": 1}}
	env := queueTestEnv(t, queueTestRigs("alpha"), fake, nil)
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
	fake := &queueStores{}
	env := queueTestEnv(t, queueTestRigs("alpha"), fake, &queueTestLog{data: []byte(strings.Join(lines, "\n"))})
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
	fake := &queueStores{rows: map[string]int{"alpha/ready": 2}}
	env := queueTestEnv(t, files, fake, nil)
	cols, err := queueSource{}.Collect(context.Background(), env)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	queueWantColumns(t, cols, map[string]any{"ready": 2})
}

func TestSourceQueueUnreadableRegistryIsAnError(t *testing.T) {
	t.Parallel()
	fake := &queueStores{}
	env := queueTestEnv(t, map[string]string{}, fake, nil)
	if _, err := (queueSource{}).Collect(context.Background(), env); err == nil {
		t.Fatal("Collect succeeded with no rig registry; want an error")
	}
	if len(fake.calls) != 0 {
		t.Errorf("bd was run %d times with no rigs; want none", len(fake.calls))
	}
}

func TestSourceQueueDropsAFirstLineCutByTheByteBound(t *testing.T) {
	t.Parallel()
	tail := "\n2026/10/24 12:30:00 spec_dispatch: tick: 4 candidate(s), roster deepseek-flash 1/3, 2 dispatched, 0 refused, 0 planning, 0 skipped, 0 failed, 0 held by the failed label\n"
	cut := "spec_dispatch: tick: 9 candidate(s), roster deepseek-flash 9/9, 9 dispatched, 0 refused, 0 planning, "
	cut += strings.Repeat("x", queueLogTailBytes-len(cut)-len(tail))
	data := []byte(cut + tail)
	if len(data) != queueLogTailBytes {
		t.Fatalf("test data is %d bytes, want the bound %d", len(data), queueLogTailBytes)
	}
	fake := &queueStores{}
	env := queueTestEnv(t, queueTestRigs("alpha"), fake, &queueTestLog{data: data})
	cols, err := queueSource{}.Collect(context.Background(), env)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	queueWantColumns(t, cols, map[string]any{
		"dispatch_ticks_1h": 1, "dispatched_1h": 2, "dispatch_unparsed_lines": 0,
		"seat_live_deepseek-flash": 1,
	})
}

// The answers the seam's parsers refuse (a cut ready page, a half-written
// page, prose where JSON was asked for) are pinned through the real store in
// TestSourceQueueRefusesAnswersItsParserCannotRead: a fake store can only
// return an error, and an error is the part of that path the source owns.

// An Env with no bead store is a wiring error, not an empty town.
func TestSourceQueueRefusesAnEnvWithNoStore(t *testing.T) {
	t.Parallel()
	env := queueTestEnv(t, queueTestRigs("alpha"), &queueStores{}, nil)
	env.Beads = nil
	if cols, err := (queueSource{}).Collect(context.Background(), env); err == nil || cols != nil {
		t.Fatalf("Collect = %v, %v with no bead store; want a null group and an error", cols, err)
	}
}

// A factory that hands back no store is a failed read of that column, not a
// panic that takes the snapshot down with it.
func TestSourceQueueNilStoreNilsTheColumns(t *testing.T) {
	t.Parallel()
	env := queueTestEnv(t, queueTestRigs("alpha"), &queueStores{}, nil)
	env.Beads = func(context.Context, string) BeadStore { return nil }
	cols, err := queueSource{}.Collect(context.Background(), env)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	queueWantColumns(t, cols, map[string]any{
		"ready": nil, "dispatchable": nil, "waiting_to_land": nil, "blocked": nil,
	})
}

func TestSourceQueueMalformedRigRegistryIsAnError(t *testing.T) {
	t.Parallel()
	files := map[string]string{filepath.Join(queueTestTown, "mayor", "rigs.json"): `{"rigs":`}
	fake := &queueStores{}
	env := queueTestEnv(t, files, fake, nil)
	if _, err := (queueSource{}).Collect(context.Background(), env); err == nil {
		t.Fatal("Collect succeeded with a malformed rig registry; want an error")
	}
	if len(fake.calls) != 0 {
		t.Errorf("bd was run %d times with no readable rigs; want none", len(fake.calls))
	}
}

// blocked is bd blocked, the dependency-blocked beads, and not the ephemeral
// wisp steps that are blocked by design (gt-5jve7).
func TestSourceQueueBlockedCountsDependencyBlockedBeadsNotWisps(t *testing.T) {
	t.Parallel()
	fake := &queueStores{
		rows: map[string]int{"alpha/blocked": 5, "beta/blocked": 1},
		wisp: map[string]int{"alpha/blocked": 2, "beta/blocked": 1},
	}
	env := queueTestEnv(t, queueTestRigs("alpha", "beta"), fake, nil)
	cols, err := queueSource{}.Collect(context.Background(), env)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	queueWantColumns(t, cols, map[string]any{"blocked": 3})
	asked := 0
	for _, call := range fake.calls {
		if call == "alpha/blocked" || call == "beta/blocked" {
			asked++
		}
	}
	if asked != 2 {
		t.Errorf("Blocked was read %d times over two rigs, want 2: %v", asked, fake.calls)
	}
}

func TestSourceQueueBlockedFailureNilsOnlyTheBlockedColumn(t *testing.T) {
	t.Parallel()
	fake := &queueStores{rows: map[string]int{"alpha/ready": 4}, fail: map[string]bool{"alpha/blocked": true}}
	env := queueTestEnv(t, queueTestRigs("alpha"), fake, nil)
	cols, err := queueSource{}.Collect(context.Background(), env)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	queueWantColumns(t, cols, map[string]any{"blocked": nil, "ready": 4})
}

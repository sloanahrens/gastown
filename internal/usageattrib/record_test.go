package usageattrib

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/steveyegge/gastown/internal/dashboard"
	"github.com/steveyegge/gastown/internal/metering/export"
	"github.com/steveyegge/gastown/internal/metering/tokensrc"
)

// msg is one assistant call with a fixed message id.
func msg(id, model, ts string, in, cc, cr, out int64) Message {
	return Message{ID: &id, Model: model, TS: ts, Input: in, CacheCreation: cc, CacheRead: cr, Output: out}
}

// Two sessions, hand-computed in the comments below. Session s-a has a subagent file; s-b
// runs entirely in the DeepSeek peak window (02:00Z is 21:00 CDT, so its cost doubles).
// The DeepSeek message at 18:55Z is off-peak (13:55 CDT), so it costs its base rate.
var (
	sessA = SessionUsage{
		ID:       "s-a",
		Complete: true,
		Main: []Message{
			msg("a1", "claude-opus-5-5", "2026-10-12T18:54:00Z", 10, 20, 30, 40),
			// base cost: 1e6*0.15 + 2e6*0.003 + 5e5*0.60 = 0.456 USD
			msg("a2", "deepseek-flash", "2026-10-12T18:55:00Z", 1000000, 0, 2000000, 500000),
		},
		Subagent: []Message{
			msg("a3", "claude-opus-5-5", "2026-10-12T18:54:34Z", 1, 2, 100, 3),
		},
	}
	sessB = SessionUsage{
		ID:       "s-b",
		Complete: true,
		Main: []Message{
			// peak: 2 * 0.456 = 0.912 USD
			msg("b1", "deepseek-flash", "2026-10-12T02:00:00Z", 1000000, 0, 2000000, 500000),
			msg("b2", "claude-opus-5-5", "2026-10-12T02:00:34Z", 5, 0, 0, 6),
		},
	}
)

// The two sessions together, as Build must total them.
const (
	wantCalls         = 5
	wantInput         = 2000016
	wantCacheCreation = 22
	wantCacheRead     = 4000130
	wantOutput        = 1000049
	wantUncached      = wantInput + wantCacheCreation + wantOutput // 3000087
	wantWallS         = 60 + 34                                    // s-a spans 18:54:00-18:55:00, s-b 02:00:00-02:00:34
	wantCostUSD       = 0.456 + 0.912
)

var (
	testSeat  = "gastown/polecats/dag"
	testStart = time.Date(2026, 10, 12, 18, 53, 57, 0, time.UTC)
	testEnd   = time.Date(2026, 10, 12, 19, 4, 9, 0, time.UTC)
)

// factsIn is a complete work run for gt-abc12 in the given workspace.
func factsIn(ws string) Facts {
	return Facts{
		Bead:      "gt-abc12",
		Kind:      dashboard.UsageKindWork,
		Class:     "polecat",
		Rig:       "gastown",
		Seat:      &testSeat,
		Workspace: ws,
		Start:     testStart,
		End:       testEnd,
		Complete:  true,
	}
}

func approx(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func recordPathFor(t *testing.T, dir string) string {
	t.Helper()
	return recordPath(dir, "gt-abc12", dashboard.UsageKindWork)
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return b
}

func TestBuildRecord(t *testing.T) {
	t.Parallel()
	rec := Build(factsIn("ws-a"), []SessionUsage{sessA, sessB})

	if rec.Bead != "gt-abc12" || rec.Kind != "work" || rec.Class != "polecat" || rec.Rig != "gastown" || rec.Workspace != "ws-a" {
		t.Errorf("identity = %q %q %q %q %q, want gt-abc12 work polecat gastown ws-a", rec.Bead, rec.Kind, rec.Class, rec.Rig, rec.Workspace)
	}
	if rec.Seat == nil || *rec.Seat != testSeat {
		t.Errorf("seat = %v, want %q", rec.Seat, testSeat)
	}
	got := rec.Totals
	if got.Calls != wantCalls || got.Input != wantInput || got.CacheCreation != wantCacheCreation ||
		got.CacheRead != wantCacheRead || got.Output != wantOutput || got.Uncached != wantUncached {
		t.Errorf("totals = %+v, want calls %d input %d cc %d cr %d out %d uncached %d",
			got, wantCalls, wantInput, wantCacheCreation, wantCacheRead, wantOutput, wantUncached)
	}

	opus, ok := rec.Models["claude-opus-5-5"]
	if !ok {
		t.Fatalf("no claude-opus-5-5 row in %v", rec.Models)
	}
	// a1, a3 and b2: calls 3; input 10+1+5; cache creation 20+2; cache read 30+100; output 40+3+6
	if opus.Calls != 3 || opus.Input != 16 || opus.CacheCreation != 22 || opus.CacheRead != 130 || opus.Output != 49 || opus.EstCostUSD != 0 {
		t.Errorf("opus row = %+v, want calls 3 input 16 cc 22 cr 130 out 49 cost 0", opus)
	}
	ds, ok := rec.Models["deepseek-flash"]
	if !ok {
		t.Fatalf("no deepseek-flash row in %v", rec.Models)
	}
	if ds.Calls != 2 || ds.Input != 2000000 || ds.CacheRead != 4000000 || ds.Output != 1000000 || !approx(ds.EstCostUSD, wantCostUSD) {
		t.Errorf("deepseek row = %+v, want calls 2 input 2000000 cr 4000000 out 1000000 cost %v", ds, wantCostUSD)
	}
	if !approx(rec.EstCostUSD, wantCostUSD) {
		t.Errorf("record cost = %v, want %v", rec.EstCostUSD, wantCostUSD)
	}
	if rec.WallS != wantWallS {
		t.Errorf("wall_s = %d, want %d", rec.WallS, wantWallS)
	}
	if rec.Subagents.Calls != 1 || rec.Subagents.Uncached != 6 || !approx(rec.Subagents.Share, 6.0/float64(wantUncached)) {
		t.Errorf("subagents = %+v, want calls 1 uncached 6 share 6/%d", rec.Subagents, wantUncached)
	}
	if !rec.Start.Equal(testStart) || !rec.End.Equal(testEnd) {
		t.Errorf("start, end = %v, %v, want the facts' %v, %v", rec.Start, rec.End, testStart, testEnd)
	}
	if !rec.Complete {
		t.Errorf("complete = false, want true")
	}
}

func TestBuildRecordIncomplete(t *testing.T) {
	t.Parallel()
	badTS := SessionUsage{ID: "s-bad", Complete: true, Main: []Message{
		msg("x1", "deepseek-flash", "not-a-time", 1000000, 0, 0, 0),
	}}
	noID := SessionUsage{ID: "", Complete: true, Main: []Message{msg("x2", "claude-opus-5-5", "2026-10-12T18:54:00Z", 7, 0, 0, 0)}}
	partial := SessionUsage{ID: "s-partial", Complete: false, Main: []Message{msg("x3", "claude-opus-5-5", "2026-10-12T18:54:00Z", 7, 0, 0, 0)}}

	tests := []struct {
		name         string
		facts        Facts
		sessions     []SessionUsage
		wantComplete bool
		wantInput    int64
		wantCost     float64
	}{
		// sessA alone: input 10 + 1000000 + 1 = 1000011, cost 0.456 (the off-peak DeepSeek call)
		{"no sessions", factsIn("ws-a"), nil, false, 0, 0},
		{"run not finished", func() Facts { f := factsIn("ws-a"); f.Complete = false; return f }(), []SessionUsage{sessA}, false, 1000011, 0.456},
		{"session with no id is skipped and not counted", factsIn("ws-a"), []SessionUsage{sessA, noID}, false, 1000011, 0.456},
		{"session read partly", factsIn("ws-a"), []SessionUsage{partial}, false, 7, 0},
		{"unparseable timestamp keeps tokens, drops cost", factsIn("ws-a"), []SessionUsage{badTS}, false, 1000000, 0},
		{"a session passed twice counts once", factsIn("ws-a"), []SessionUsage{sessA, sessA}, true, 1000011, 0.456},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := Build(tt.facts, tt.sessions)
			if rec.Complete != tt.wantComplete {
				t.Errorf("complete = %v, want %v", rec.Complete, tt.wantComplete)
			}
			if rec.Totals.Input != tt.wantInput {
				t.Errorf("input = %d, want %d", rec.Totals.Input, tt.wantInput)
			}
			if !approx(rec.EstCostUSD, tt.wantCost) {
				t.Errorf("cost = %v, want %v", rec.EstCostUSD, tt.wantCost)
			}
			if rec.Models == nil {
				t.Errorf("models is nil, want a map so the record encodes as {}")
			}
		})
	}
}

func TestWriteMergesBySession(t *testing.T) {
	t.Parallel()
	dir := t.TempDir() // the beads directory does not exist yet: Write creates it
	path := recordPathFor(t, dir)

	recA := Build(factsIn("ws-a"), []SessionUsage{sessA})
	if err := Write(dir, recA, []string{"s-a"}); err != nil {
		t.Fatalf("first write: %v", err)
	}
	first := mustRead(t, path)

	if err := Write(dir, recA, []string{"s-a"}); err != nil {
		t.Fatalf("second write of the same session: %v", err)
	}
	if got := mustRead(t, path); string(got) != string(first) {
		t.Errorf("writing a counted session changed the file:\nbefore:\n%s\nafter:\n%s", first, got)
	}

	if fi, err := os.Stat(path); err != nil {
		t.Fatalf("stat: %v", err)
	} else if fi.Mode().Perm() != 0o644 {
		t.Errorf("record mode = %v, want 0644 so the dashboard's user can read it", fi.Mode().Perm())
	}

	recB := Build(factsIn("ws-a"), []SessionUsage{sessB})
	// A session id repeated in one write counts once.
	if err := Write(dir, recB, []string{"s-b", "s-b"}); err != nil {
		t.Fatalf("write of the second session: %v", err)
	}
	var stored storedRecord
	if err := json.Unmarshal(mustRead(t, path), &stored); err != nil {
		t.Fatalf("decoding the merged record: %v", err)
	}
	if stored.Totals.Calls != wantCalls || stored.Totals.Input != wantInput || stored.Totals.Uncached != wantUncached {
		t.Errorf("merged totals = %+v, want the two sessions' sum (calls %d, input %d, uncached %d)",
			stored.Totals, wantCalls, wantInput, wantUncached)
	}
	if !approx(stored.EstCostUSD, wantCostUSD) || stored.WallS != wantWallS {
		t.Errorf("merged cost, wall = %v, %d, want %v, %d", stored.EstCostUSD, stored.WallS, wantCostUSD, wantWallS)
	}
	if want := []string{"s-a", "s-b"}; !equalStrings(stored.Sessions, want) {
		t.Errorf("counted sessions = %v, want %v", stored.Sessions, want)
	}

	// Building both sessions at once must give the same record as merging them one at a time.
	both := Build(factsIn("ws-a"), []SessionUsage{sessA, sessB})
	if stored.Totals != both.Totals || stored.Subagents.Calls != both.Subagents.Calls ||
		!approx(stored.Subagents.Share, both.Subagents.Share) || !approx(stored.Models["deepseek-flash"].EstCostUSD, both.Models["deepseek-flash"].EstCostUSD) {
		t.Errorf("merged record %+v differs from a one-pass build %+v", stored.UsageRecord, both)
	}

	// Writing the first session again after the merge still changes nothing.
	before := mustRead(t, path)
	if err := Write(dir, recA, []string{"s-a"}); err != nil {
		t.Fatalf("rewrite after merge: %v", err)
	}
	if string(mustRead(t, path)) != string(before) {
		t.Errorf("rewriting a session counted by the merge changed the file")
	}
}

func TestMergeTakesWidestBoundsAndNamesNoSeatWhenSeatsDiffer(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := recordPathFor(t, dir)
	if err := Write(dir, Build(factsIn("ws-a"), []SessionUsage{sessA}), []string{"s-a"}); err != nil {
		t.Fatalf("first write: %v", err)
	}

	other := "gastown/polecats/furiosa"
	later := factsIn("ws-a")
	later.Start = testStart.Add(-time.Hour)
	later.End = testEnd.Add(time.Hour)
	later.Seat = &other
	partial := SessionUsage{ID: "s-p", Complete: false, Main: []Message{
		msg("p1", "claude-opus-5-5", "2026-10-12T19:00:00Z", 7, 0, 0, 0),
	}}
	if err := Write(dir, Build(later, []SessionUsage{partial}), []string{"s-p"}); err != nil {
		t.Fatalf("second write: %v", err)
	}

	var stored storedRecord
	if err := json.Unmarshal(mustRead(t, path), &stored); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if !stored.Start.Equal(later.Start) || !stored.End.Equal(later.End) {
		t.Errorf("bounds = %v .. %v, want the widest %v .. %v", stored.Start, stored.End, later.Start, later.End)
	}
	if stored.Seat != nil {
		t.Errorf("seat = %q, want none: the two runs had different seats", *stored.Seat)
	}
	if stored.Complete {
		t.Errorf("complete = true, want false: one merged session was read partly")
	}
	if stored.Totals.Input != 1000011+7 {
		t.Errorf("input = %d, want the partial session's 7 added to sessA's 1000011", stored.Totals.Input)
	}
}

// A record with no workspace is refused even with no record on disk to compare against, so
// its spend can never total as unattributed.
func TestWriteRefusesRecordWithNoWorkspace(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := Write(dir, Build(factsIn(""), []SessionUsage{sessB}), []string{"s-b"}); err == nil {
		t.Fatal("write of a record with no workspace: no error")
	}
	if _, err := os.Stat(recordPathFor(t, dir)); !os.IsNotExist(err) {
		t.Errorf("refused write created the record (stat error %v)", err)
	}
}

// A run whose transcripts could not be listed leaves a record file: no session, incomplete,
// and the reason in its error field. A reason is required.
func TestMarkIncompleteWritesRecordWithReason(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := MarkIncomplete(dir, factsIn("ws-a"), "reading project directory: permission denied"); err != nil {
		t.Fatalf("mark of a run with no record: %v", err)
	}
	var got storedRecord
	if err := json.Unmarshal(mustRead(t, recordPathFor(t, dir)), &got); err != nil {
		t.Fatalf("decoding the record: %v", err)
	}
	if got.Complete || got.Error != "reading project directory: permission denied" || len(got.Sessions) != 0 {
		t.Errorf("record complete %v error %q sessions %v, want incomplete, the reason, and no session",
			got.Complete, got.Error, got.Sessions)
	}
	if err := MarkIncomplete(dir, factsIn("ws-a"), ""); err == nil {
		t.Error("mark with no reason: no error")
	}
}

// A file that already counts sessions keeps them and its totals when a run is marked
// incomplete, and a later write of new sessions keeps the mark.
func TestMarkIncompleteKeepsCountedSessions(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	f := factsIn("ws-a")
	if err := Write(dir, Build(f, []SessionUsage{sessA}), []string{"s-a"}); err != nil {
		t.Fatalf("first write: %v", err)
	}
	var before storedRecord
	if err := json.Unmarshal(mustRead(t, recordPathFor(t, dir)), &before); err != nil {
		t.Fatalf("decoding the record: %v", err)
	}
	if err := MarkIncomplete(dir, f, "boom"); err != nil {
		t.Fatalf("mark: %v", err)
	}
	var marked storedRecord
	if err := json.Unmarshal(mustRead(t, recordPathFor(t, dir)), &marked); err != nil {
		t.Fatalf("decoding the marked record: %v", err)
	}
	if marked.Complete || marked.Error != "boom" || !equalStrings(marked.Sessions, []string{"s-a"}) ||
		!reflect.DeepEqual(marked.Totals, before.Totals) {
		t.Errorf("marked record complete %v error %q sessions %v totals %+v, want incomplete with s-a and its totals %+v",
			marked.Complete, marked.Error, marked.Sessions, marked.Totals, before.Totals)
	}

	if err := Write(dir, Build(f, []SessionUsage{sessB}), []string{"s-b"}); err != nil {
		t.Fatalf("write of a later session: %v", err)
	}
	var after storedRecord
	if err := json.Unmarshal(mustRead(t, recordPathFor(t, dir)), &after); err != nil {
		t.Fatalf("decoding the record after the write: %v", err)
	}
	if after.Complete || after.Error != "boom" || !equalStrings(after.Sessions, []string{"s-a", "s-b"}) {
		t.Errorf("after a later write complete %v error %q sessions %v, want incomplete, boom, and both sessions",
			after.Complete, after.Error, after.Sessions)
	}
}

// MarkIncomplete refuses a run with no workspace, and a file it cannot merge into:
// one it did not write, one with no sessions list, one of another workspace, or a
// path it cannot read. Each refusal leaves the file as it was.
func TestMarkIncompleteRefusesWhatItCannotMerge(t *testing.T) {
	t.Parallel()
	t.Run("run with no workspace", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		if err := MarkIncomplete(dir, factsIn(""), "boom"); err == nil {
			t.Fatal("mark of a run with no workspace: no error")
		}
		if _, err := os.Stat(recordPathFor(t, dir)); !os.IsNotExist(err) {
			t.Errorf("refused mark created the record (stat error %v)", err)
		}
	})
	for _, c := range []struct{ name, content string }{
		{"file this package did not write", "{not json"},
		{"file with no sessions list", "{}"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			path := recordPathFor(t, dir)
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(c.content), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := MarkIncomplete(dir, factsIn("ws-a"), "boom"); err == nil {
				t.Error("mark over the file: no error")
			}
			if got := mustRead(t, path); string(got) != c.content {
				t.Errorf("refused mark changed the file to %q", got)
			}
		})
	}
	t.Run("file of another workspace", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		if err := Write(dir, Build(factsIn("ws-a"), []SessionUsage{sessA}), []string{"s-a"}); err != nil {
			t.Fatalf("first write: %v", err)
		}
		path := recordPathFor(t, dir)
		before := mustRead(t, path)
		if err := MarkIncomplete(dir, factsIn("ws-b"), "boom"); err == nil {
			t.Error("mark of another workspace's record: no error")
		}
		if got := mustRead(t, path); string(got) != string(before) {
			t.Error("refused mark changed another workspace's record")
		}
	})
	t.Run("path that cannot be read", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := recordPathFor(t, dir)
		if err := os.MkdirAll(path, 0o755); err != nil { // a directory where the record file goes
			t.Fatal(err)
		}
		if err := MarkIncomplete(dir, factsIn("ws-a"), "boom"); err == nil {
			t.Error("mark over an unreadable path: no error")
		}
	})
	t.Run("record that will not encode", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		f := factsIn("ws-a")
		f.Start = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC) // a year JSON cannot encode
		if err := MarkIncomplete(dir, f, "boom"); err == nil {
			t.Fatal("mark of a record that will not encode: no error")
		}
		if _, err := os.Stat(recordPathFor(t, dir)); !os.IsNotExist(err) {
			t.Errorf("a record that did not encode left a file behind: %v", err)
		}
	})
	t.Run("beads directory that cannot be made", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		beads := filepath.Join(dir, "beads")
		if err := os.Symlink(filepath.Join(dir, "nowhere"), beads); err != nil { // a dangling symlink
			t.Fatal(err)
		}
		if err := MarkIncomplete(dir, factsIn("ws-a"), "boom"); err == nil {
			t.Fatal("mark with no beads directory it can make: no error")
		}
		if _, err := os.Lstat(beads); err != nil {
			t.Errorf("the beads symlink was changed: %v", err)
		}
	})
}

// Eight runs for one bead end at once. Each merges into the file the others have just
// written, so the lock must hold every read-merge-write whole: all eight sessions counted.
func TestWriteConcurrentSessionsAllCounted(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	const n = 8
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := fmt.Sprintf("c-%d", i)
			s := SessionUsage{ID: id, Complete: true, Main: []Message{
				msg(id, "claude-opus-5-5", "2026-10-12T18:54:00Z", 10, 0, 0, 1),
			}}
			errs[i] = Write(dir, Build(factsIn("ws-a"), []SessionUsage{s}), []string{id})
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("session %d: %v", i, err)
		}
	}
	var stored storedRecord
	if err := json.Unmarshal(mustRead(t, recordPathFor(t, dir)), &stored); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if stored.Totals.Calls != n || stored.Totals.Input != 10*n || len(stored.Sessions) != n {
		t.Errorf("after %d concurrent writes: calls %d, input %d, %d sessions counted; want all %d",
			n, stored.Totals.Calls, stored.Totals.Input, len(stored.Sessions), n)
	}
}

func TestRecordPathCleansBeadName(t *testing.T) {
	t.Parallel()
	got := recordPath("/usage", "gt/../x y", dashboard.UsageKindWork)
	want := filepath.Join("/usage", "beads", "gt_.._x_y.work.json")
	if got != want {
		t.Errorf("recordPath = %q, want %q: a bead name must not reach outside the beads directory", got, want)
	}
}

// treeListing is every entry under root with its size and modification time, or "absent" when
// root does not exist. Two listings are equal only when nothing under root was written.
func treeListing(t *testing.T, root string) string {
	t.Helper()
	if _, err := os.Stat(root); errors.Is(err, fs.ErrNotExist) {
		return "absent"
	}
	var b strings.Builder
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		fmt.Fprintf(&b, "%s\t%d\t%d\n", rel, fi.Size(), fi.ModTime().UnixNano())
		return nil
	})
	if err != nil {
		t.Fatalf("listing %s: %v", root, err)
	}
	return b.String()
}

var errInjectedCreate = errors.New("injected create failure")

// A write whose temporary file cannot be created fails and leaves the earlier record and the
// directory as they were. The failure is injected, so it is the same for root and for a user.
func TestWriteFailureKeepsEarlierRecord(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := recordPathFor(t, dir)
	if err := Write(dir, Build(factsIn("ws-a"), []SessionUsage{sessA}), []string{"s-a"}); err != nil {
		t.Fatalf("first write: %v", err)
	}
	before := mustRead(t, path)
	listing := treeListing(t, dir)

	failing := writer{create: func(string, string) (*os.File, error) { return nil, errInjectedCreate }}
	if err := failing.Write(dir, Build(factsIn("ws-a"), []SessionUsage{sessB}), []string{"s-b"}); !errors.Is(err, errInjectedCreate) {
		t.Fatalf("write with a failing create: error = %v, want the injected failure", err)
	}
	if got := mustRead(t, path); string(got) != string(before) {
		t.Errorf("failed write changed the earlier record:\nbefore:\n%s\nafter:\n%s", before, got)
	}
	if got := treeListing(t, dir); got != listing {
		t.Errorf("failed write changed the directory:\nbefore:\n%s\nafter:\n%s", listing, got)
	}
}

// A write that is refused returns an error naming the mismatch or the sessions already counted,
// and leaves the record and the directory as they were.
func TestWriteRefusesSessionMismatch(t *testing.T) {
	t.Parallel()
	// The same session id with one more message: its accounting differs from what was counted.
	changed := sessA
	changed.Main = append(slices.Clone(sessA.Main), msg("a9", "claude-opus-5-5", "2026-10-12T18:56:00Z", 7, 0, 0, 7))
	tests := []struct {
		name    string
		rec     dashboard.UsageRecord
		ids     []string
		wantErr string
	}{
		{"calls but no session named", Build(factsIn("ws-a"), []SessionUsage{sessB}), nil, "mismatch"},
		{"counted session with other accounting", Build(factsIn("ws-a"), []SessionUsage{changed}), []string{"s-a"}, "mismatch"},
		{"counted and new sessions mixed", Build(factsIn("ws-a"), []SessionUsage{sessA, sessB}), []string{"s-a", "s-b"}, "already counted"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			path := recordPathFor(t, dir)
			if err := Write(dir, Build(factsIn("ws-a"), []SessionUsage{sessA}), []string{"s-a"}); err != nil {
				t.Fatalf("first write: %v", err)
			}
			before := mustRead(t, path)
			listing := treeListing(t, dir)

			err := Write(dir, tt.rec, tt.ids)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("write error = %v, want one containing %q", err, tt.wantErr)
			}
			if got := mustRead(t, path); string(got) != string(before) {
				t.Errorf("refused write changed the record")
			}
			if got := treeListing(t, dir); got != listing {
				t.Errorf("refused write changed the directory")
			}
		})
	}
}

// A repeat of counted sessions is refused when no earlier write counted exactly those sessions,
// since their accounting cannot be checked. The record is left as it is.
func TestWriteRefusesRepeatWithoutItsBatch(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := recordPathFor(t, dir)
	if err := Write(dir, Build(factsIn("ws-a"), []SessionUsage{sessA}), []string{"s-a"}); err != nil {
		t.Fatalf("first write: %v", err)
	}
	// Drop the batches, as a file written before they were kept would lack them.
	var stored map[string]json.RawMessage
	if err := json.Unmarshal(mustRead(t, path), &stored); err != nil {
		t.Fatalf("decoding the record: %v", err)
	}
	delete(stored, "batches")
	data, err := json.Marshal(stored)
	if err != nil {
		t.Fatalf("encoding the record: %v", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("rewriting the record: %v", err)
	}
	listing := treeListing(t, dir)

	err = Write(dir, Build(factsIn("ws-a"), []SessionUsage{sessA}), []string{"s-a"})
	if err == nil || !strings.Contains(err.Error(), "cannot be checked") {
		t.Fatalf("repeat without a batch: error = %v, want one saying it cannot be checked", err)
	}
	if got := treeListing(t, dir); got != listing {
		t.Errorf("refused repeat changed the directory")
	}
}

// A stored record with no models map merges without a panic: its totals are kept, and the new
// session's models are added to the map.
func TestMergeIntoRecordWithoutModels(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := recordPathFor(t, dir)
	if err := Write(dir, Build(factsIn("ws-a"), []SessionUsage{sessA}), []string{"s-a"}); err != nil {
		t.Fatalf("first write: %v", err)
	}
	var stored map[string]json.RawMessage
	if err := json.Unmarshal(mustRead(t, path), &stored); err != nil {
		t.Fatalf("decoding the record: %v", err)
	}
	delete(stored, "models")
	data, err := json.Marshal(stored)
	if err != nil {
		t.Fatalf("encoding the record: %v", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("rewriting the record: %v", err)
	}

	recB := Build(factsIn("ws-a"), []SessionUsage{sessB})
	if err := Write(dir, recB, []string{"s-b"}); err != nil {
		t.Fatalf("write into a record without models: %v", err)
	}
	var after storedRecord
	if err := json.Unmarshal(mustRead(t, path), &after); err != nil {
		t.Fatalf("decoding the merged record: %v", err)
	}
	wantTotals := addTotals(Build(factsIn("ws-a"), []SessionUsage{sessA}).Totals, recB.Totals)
	if after.Totals != wantTotals {
		t.Errorf("merged totals = %+v, want the stored totals plus the new session's %+v", after.Totals, wantTotals)
	}
	if len(after.Models) != len(recB.Models) {
		t.Fatalf("merged models = %v, want the new session's %d rows", after.Models, len(recB.Models))
	}
	for name, row := range recB.Models {
		if after.Models[name] != row {
			t.Errorf("model %s = %+v, want %+v", name, after.Models[name], row)
		}
	}
	if want := []string{"s-a", "s-b"}; !equalStrings(after.Sessions, want) {
		t.Errorf("counted sessions = %v, want %v", after.Sessions, want)
	}
}

func TestWriteRefusesUnreadableOrForeignRecord(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		existing string
	}{
		{"not JSON", `{"bead":`},
		{"no sessions list", `{"bead":"gt-abc12","kind":"work","class":"polecat","rig":"gastown","workspace":"ws-a","totals":{"input":1}}`},
		{"sessions as scan objects", `{"bead":"gt-abc12","kind":"work","class":"polecat","rig":"gastown","workspace":"ws-a","sessions":[{"session_id":"s-a"}]}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := recordPathFor(t, dir)
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(tt.existing), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := Write(dir, Build(factsIn("ws-a"), []SessionUsage{sessA}), []string{"s-a"}); err == nil {
				t.Fatal("merge into an unreadable record: no error")
			}
			if got := mustRead(t, path); string(got) != tt.existing {
				t.Errorf("refused merge overwrote the record: %s", got)
			}
		})
	}
}

func TestWriteRefusesBadInput(t *testing.T) {
	t.Parallel()
	recA := Build(factsIn("ws-a"), []SessionUsage{sessA})
	noBead := recA
	noBead.Bead = ""
	badKind := recA
	badKind.Kind = "polecat"
	otherWorkspace := Build(factsIn("ws-b"), []SessionUsage{sessB})
	otherClass := Build(func() Facts { f := factsIn("ws-a"); f.Class = "deep-review"; return f }(), []SessionUsage{sessB})

	tests := []struct {
		name string
		rec  dashboard.UsageRecord
		ids  []string
	}{
		{"no bead", noBead, []string{"s-a"}},
		{"kind is neither work nor om", badKind, []string{"s-a"}},
		{"no sessions", recA, nil},
		{"no sessions and no calls", Build(factsIn("ws-a"), nil), nil},
		{"empty session id", recA, []string{"s-b", ""}},
		{"record mixes counted and new sessions", Build(factsIn("ws-a"), []SessionUsage{sessA, sessB}), []string{"s-a", "s-b"}},
		{"another workspace's record", otherWorkspace, []string{"s-b"}},
		{"another class's record", otherClass, []string{"s-b"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := recordPathFor(t, dir)
			if err := Write(dir, recA, []string{"s-a"}); err != nil {
				t.Fatalf("seeding the record: %v", err)
			}
			before := mustRead(t, path)
			if err := Write(dir, tt.rec, tt.ids); err == nil {
				t.Fatal("refused input: no error")
			}
			if got := mustRead(t, path); string(got) != string(before) {
				t.Errorf("refused write changed the record")
			}
			entries, err := os.ReadDir(filepath.Join(dir, "beads"))
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 1 {
				t.Errorf("refused write left %d entries, want only the record", len(entries))
			}
		})
	}
}

func TestWorkspaceMapping(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		file    *string // nil: no settings file
		bead    string
		want    string
		wantErr bool
	}{
		{"mapped prefix", ptr(`{"default": "customer-zero", "rigs": {"gt": "ws-a"}}`), "gt-abc12", "ws-a", false},
		{"unmapped prefix takes the file default", ptr(`{"default": "ws-default", "rigs": {"fr": "ws-a"}}`), "zz-abc12", "ws-default", false},
		{"absent file is customer-zero", nil, "gt-abc12", "customer-zero", false},
		{"malformed JSON", ptr(`{"default":`), "gt-abc12", "", true},
		{"empty default", ptr(`{"default": ""}`), "gt-abc12", "", true},
		{"unknown key", ptr(`{"default": "customer-zero", "extra": 1}`), "gt-abc12", "", true},
		{"empty workspace id for a rig", ptr(`{"default": "customer-zero", "rigs": {"gt": ""}}`), "gt-abc12", "", true},
		{"bead with no prefix", nil, "-abc12", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			town := t.TempDir()
			usageDir := filepath.Join(town, ".runtime", "usage")
			if tt.file != nil {
				if err := os.MkdirAll(filepath.Join(town, "settings"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(town, "settings", "workspaces.json"), []byte(*tt.file), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			listing := treeListing(t, town)
			ws, err := WorkspaceOf(town, tt.bead)
			if (err != nil) != tt.wantErr {
				t.Fatalf("WorkspaceOf error = %v, want error %v", err, tt.wantErr)
			}
			if ws != tt.want {
				t.Errorf("workspace = %q, want %q", ws, tt.want)
			}
			// The lookup only reads: the settings file, and any usage directory, are as they were.
			if got := treeListing(t, town); got != listing {
				t.Errorf("workspace lookup changed %s:\nbefore:\n%s\nafter:\n%s", town, listing, got)
			}
			if tt.wantErr {
				if _, statErr := os.Stat(usageDir); !os.IsNotExist(statErr) {
					t.Errorf("a failed workspace lookup created %s", usageDir)
				}
			}
		})
	}
}

func TestRecordIsReadByTheConsumers(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	rec := Build(factsIn("ws-a"), []SessionUsage{sessA, sessB})
	if err := Write(dir, rec, []string{"s-a", "s-b"}); err != nil {
		t.Fatalf("write: %v", err)
	}
	data := mustRead(t, recordPathFor(t, dir))

	// The dashboard's reader decodes each file straight into dashboard.UsageRecord.
	var decoded dashboard.UsageRecord
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("decoding as dashboard.UsageRecord: %v", err)
	}
	if decoded.Totals.Uncached != wantUncached || decoded.Workspace != "ws-a" || decoded.Kind != dashboard.UsageKindWork {
		t.Errorf("decoded record = %+v, want uncached %d in ws-a", decoded.Totals, wantUncached)
	}

	// tokensrc totals the file for its month and workspace.
	fsys := fstest.MapFS{"beads/gt-abc12.work.json": &fstest.MapFile{Data: data}}
	res, err := tokensrc.Tokens(fsys, export.CalendarMonth{Year: 2026, Month: time.October})
	if err != nil {
		t.Fatalf("tokensrc.Tokens: %v", err)
	}
	want := []export.TokenCount{{Workspace: "ws-a", Input: wantInput, Output: wantOutput, CacheCreation: wantCacheCreation, CacheRead: wantCacheRead}}
	if len(res.Workspaces) != 1 || res.Workspaces[0] != want[0] || len(res.Unreadable) != 0 || res.OutOfMonth != 0 {
		t.Errorf("tokensrc result = %+v, want one row %+v", res, want[0])
	}

	// The dashboard's payload is folded from the decoded records by AggregateUsage, the same
	// function the reader calls. internal/cmd's reader itself is unexported and not reachable here.
	now := time.Date(2026, 10, 12, 20, 0, 0, 0, time.UTC)
	u := dashboard.AggregateUsage([]dashboard.UsageRecord{decoded}, now, 0)
	var calls, uncached int64
	for _, row := range u.ByClassModel {
		calls += row.Calls
		uncached += row.Uncached
	}
	if calls != wantCalls || uncached != wantUncached {
		t.Errorf("dashboard by class and model sums to %d calls, %d uncached; want %d, %d", calls, uncached, wantCalls, wantUncached)
	}
	if len(u.TopBeads) != 1 || u.TopBeads[0].Bead != "gt-abc12" || u.TopBeads[0].WallS != wantWallS {
		t.Errorf("dashboard top beads = %+v, want gt-abc12 with wall %d", u.TopBeads, wantWallS)
	}
}

// A record path that cannot be read is an error, and the thing at that path is left alone.
func TestWriteReturnsAReadErrorAndLeavesThePathAlone(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := recordPathFor(t, dir)
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}

	err := Write(dir, Build(factsIn("ws-a"), []SessionUsage{sessA}), []string{"s-a"})
	if err == nil {
		t.Fatal("write over an unreadable record path: no error")
	}
	if info, statErr := os.Stat(path); statErr != nil || !info.IsDir() {
		t.Errorf("the unreadable path was changed: stat %v, err %v", info, statErr)
	}
}

// A record that cannot be encoded is an error before anything is put in place.
func TestWriteRefusesARecordThatWillNotEncode(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	rec := Build(factsIn("ws-a"), []SessionUsage{sessA})
	rec.EstCostUSD = math.Inf(1)

	if err := Write(dir, rec, []string{"s-a"}); err == nil {
		t.Fatal("a record with an infinite cost: no error")
	}
	if _, err := os.Stat(recordPathFor(t, dir)); !os.IsNotExist(err) {
		t.Errorf("a record that did not encode left a file behind: %v", err)
	}
}

// A beads directory that cannot be made is an error. A dangling symlink is one: the record
// reads as absent through it, and the mkdir that follows fails on the link.
func TestWriteReturnsAnErrorWhenTheBeadsDirectoryCannotBeMade(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	beads := filepath.Join(dir, "beads")
	if err := os.Symlink(filepath.Join(dir, "nowhere"), beads); err != nil {
		t.Fatal(err)
	}

	if err := Write(dir, Build(factsIn("ws-a"), []SessionUsage{sessA}), []string{"s-a"}); err == nil {
		t.Fatal("write with no beads directory it can make: no error")
	}
	if _, err := os.Lstat(beads); err != nil {
		t.Errorf("the beads symlink was changed: %v", err)
	}
}

// A failed atomic replace returns its error and removes the temp file it made.
func TestWriteFileAtomicRemovesItsTempFileOnFailure(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// A non-empty directory at the target: the rename over it fails whoever writes.
	target := filepath.Join(dir, "gt-abc12.work.json")
	if err := os.MkdirAll(filepath.Join(target, "inside"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := (writer{create: os.CreateTemp}).writeFileAtomic(target, []byte("{}\n")); err == nil {
		t.Fatal("replacing a directory: no error")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "gt-abc12.work.json" {
		t.Errorf("failed replace left %d entries in the directory, want only the target", len(entries))
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func ptr(s string) *string { return &s }

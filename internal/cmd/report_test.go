package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/land"
)

// reportWindowEnd is the fixed clock the report fixtures are built around:
// 2026-10-02 13:52:05 CDT, so the window is 12:52:05–13:52:05 local.
var reportWindowEnd = at("2026-10-02T13:52:05-05:00")

// reportFakeStore is the bd stand-in: beadsfake for the client queries, plus
// the note-filtered rejection list. tests build one per store.
type reportFakeStore struct {
	*beadsfake.Fake
	rejected []*beads.Issue
}

func (f *reportFakeStore) RejectedSince(time.Time) ([]*beads.Issue, error) {
	return f.rejected, nil
}

// reportNoWriteStore fails the test if gt report calls any write method. The
// report is read-only: this is the assertion that keeps it so.
type reportNoWriteStore struct {
	reportStore
	t *testing.T
}

func (s reportNoWriteStore) fail(op string) {
	s.t.Errorf("gt report called the write method %s", op)
}

func (s reportNoWriteStore) Create(beads.CreateOptions) (*beads.Issue, error) {
	s.fail("Create")
	return nil, nil
}

func (s reportNoWriteStore) Update(string, beads.UpdateOptions) error {
	s.fail("Update")
	return nil
}

func (s reportNoWriteStore) Close(...string) error { s.fail("Close"); return nil }

func (s reportNoWriteStore) CloseWithReason(string, ...string) error {
	s.fail("CloseWithReason")
	return nil
}

func (s reportNoWriteStore) ForceCloseWithReason(string, ...string) error {
	s.fail("ForceCloseWithReason")
	return nil
}

func (s reportNoWriteStore) DeleteIssues(...string) error { s.fail("DeleteIssues"); return nil }

func (s reportNoWriteStore) Release(string) error { s.fail("Release"); return nil }

func (s reportNoWriteStore) ReleaseWithReason(string, string) error {
	s.fail("ReleaseWithReason")
	return nil
}

func (s reportNoWriteStore) AddComment(string, string) error { s.fail("AddComment"); return nil }

func (s reportNoWriteStore) AddCommentAs(string, string, string) error {
	s.fail("AddCommentAs")
	return nil
}

func (s reportNoWriteStore) AddDependency(string, string) error {
	s.fail("AddDependency")
	return nil
}

func (s reportNoWriteStore) AddTypedDependency(string, string, string) error {
	s.fail("AddTypedDependency")
	return nil
}

func (s reportNoWriteStore) RemoveDependency(string, string) error {
	s.fail("RemoveDependency")
	return nil
}

func (s reportNoWriteStore) AppendNotes(string, string) error {
	s.fail("AppendNotes")
	return nil
}

func (s reportNoWriteStore) ReleaseIfAssignee(string, string) (bool, error) {
	s.fail("ReleaseIfAssignee")
	return false, nil
}

func (s reportNoWriteStore) TransferIfAssignee(string, string, string, string) (bool, error) {
	s.fail("TransferIfAssignee")
	return false, nil
}

// reportReadOnlyClientMethods are the beads.Client methods gt report may
// call: the query surface. Everything else on Client is a write, and
// reportNoWriteStore has to override it.
var reportReadOnlyClientMethods = map[string]bool{
	"Show": true, "ShowMultiple": true, "List": true, "ListByAssignee": true,
	"GetAssignedIssue": true, "ListIssueStatuses": true,
	"ListAssignedIssueStatuses": true, "Ready": true, "ReadyAll": true,
	"Children": true, "ChildrenOf": true, "Comments": true, "DepList": true,
}

// TestReportNoWriteGuardCoversClient pins that the no-write guard still
// observes every write on beads.Client. reportNoWriteStore embeds
// reportStore, so a Client method it does not override is satisfied by the
// embedded store and a write through the guard goes unseen — which is how
// DeleteIssues slipped past it when the method landed (gt-7iwy0.4.2). The
// guard is read from this file's source rather than its Go method set, since
// reflection reports the embedded store's methods as the guard's own.
func TestReportNoWriteGuardCoversClient(t *testing.T) {
	t.Parallel()
	guarded := map[string]bool{}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "report_test.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	ast.Inspect(file, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Recv == nil || len(fn.Recv.List) != 1 {
			return true
		}
		if recv, ok := fn.Recv.List[0].Type.(*ast.Ident); ok && recv.Name == "reportNoWriteStore" {
			guarded[fn.Name.Name] = true
		}
		return true
	})
	if len(guarded) == 0 {
		t.Fatal("parsed no reportNoWriteStore methods: the guard moved or was renamed")
	}
	client := reflect.TypeOf((*beads.Client)(nil)).Elem()
	var uncovered []string
	for i := 0; i < client.NumMethod(); i++ {
		name := client.Method(i).Name
		if !guarded[name] && !reportReadOnlyClientMethods[name] {
			uncovered = append(uncovered, name)
		}
	}
	if len(uncovered) > 0 {
		t.Errorf("beads.Client methods the report no-write guard neither overrides nor lists read-only: %v\n"+
			"override each write in report_test.go, or add it to reportReadOnlyClientMethods if gt report may call it", uncovered)
	}
	for name := range guarded {
		if reportReadOnlyClientMethods[name] {
			t.Errorf("%s is both overridden and listed read-only; drop it from reportReadOnlyClientMethods", name)
		}
	}
}

// reportTown writes a town from relative path → contents and returns its root.
func reportTown(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, body := range files {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// reportRunFor builds a run over a fixture town with the fixed clock, a
// fake store per store name, and a fixed mains reading.
func reportRunFor(t *testing.T, townRoot string, store func(string) (reportStore, error)) (reportRun, *bytes.Buffer) {
	t.Helper()
	out := &bytes.Buffer{}
	return reportRun{
		townRoot: townRoot,
		loc:      tailTestLoc,
		until:    reportWindowEnd,
		out:      out,
		version:  "1.2.1-dev",
		commit:   "5e6f7a8b5e6f7a8b5e6f7a8b5e6f7a8b5e6f7a8b",
		rigs:     func() ([]string, error) { return []string{"gastown"}, nil },
		store:    store,
		mainTip: func(string) (reportMains, error) {
			return reportMains{SHA: "138368fd7571c0ffee00", CommitsBehind: 3}, nil
		},
	}, out
}

// reportGoldenTown is the fixture town the golden test reads: one rig with a
// daemon log, a landings file, a steward ledger, an attention state and a
// stale spend reading.
func reportGoldenTown(t *testing.T) string {
	t.Helper()
	return reportTown(t, map[string]string{
		"daemon/daemon.log": strings.Join([]string{
			"2026/10/02 12:52:04 landing_worker: [land] gt-before: merged 0000aaaa onto origin/main (1111bbbb) as 2222cccc; gating the merged tree, then om review",
			"2026/10/02 12:52:05 landing_worker: [land] gt-abc: merged 1a2b3c4d onto origin/main (5e6f7a8b) as 9a8b7c6d; gating the merged tree, then om review",
			"2026/10/02 12:53:00 landing_worker: [land] gt-abc: stages: lint 18s, gate 92s, om 2m31s",
			"2026/10/02 12:58:40 landing_worker: [land] gt-abc: landed 9a8b7c6d on origin/main (patch-id 11223344)",
			"2026/10/02 13:00:00 hm witness started",
			"2026/10/02 13:05:00 landing_worker: [land] gt-def: merged aaaa1111 onto origin/main (bbbb2222) as cccc3333; gating the merged tree, then om review",
			"2026/10/02 13:06:00 landing_worker: [land] gt-def: rejected (lint): docs-lint finding unaddressed",
			"2026/10/02 13:10:00 landing_worker: [land] gt-ghi: merged 11112222 onto origin/main (33334444) as 55556666; gating the merged tree, then om review",
			"2026/10/02 13:11:00 landing_worker: [land] gt-ghi: rejected (tests): gate red",
			"2026/10/02 13:12:00 landing_worker: [land] gt-ghi: merged 12121212 onto origin/main (34343434) as 56565656; gating the merged tree, then om review",
			"2026/10/02 13:20:00 landing_worker: [land] gt-ghi: rejected (tests): gate red again",
			"2026/10/02 13:30:00 landing_worker: [land] gt-jkl: already landed as ffff0000; finishing its record",
			"2026/10/02 13:52:06 landing_worker: [land] gt-after: merged 77778888 onto origin/main (99990000) as aaaabbbb; gating the merged tree, then om review",
			"",
		}, "\n"),
		".runtime/landings/gastown.jsonl": strings.Join([]string{
			`{"bead":"gt-abc","rig":"gastown","branch":"polecat/emerald/gt-abc+m1","head":"9a8b7c6d","target":"main","base":"5e6f7a8b","landed_commit":"9a8b7c6d","patch_id":"11223344","gate_result":"pass","om_verdict":"pass","om_score":0.92,"route":"normal","landed_at":"2026-10-02T17:58:40Z"}`,
			`{"bead":"gt-jkl","rig":"gastown","branch":"polecat/agate/gt-jkl+m2","head":"ffff0000","target":"main","base":"aaaa1111","landed_commit":"ffff0000","patch_id":"55667788","gate_result":"pass","om_verdict":"pass","om_score":0.75,"route":"normal","landed_at":"2026-10-02T18:30:00Z"}`,
			`{"bead":"gt-outside","rig":"gastown","branch":"polecat/opal/gt-outside+m3","head":"0f0f0f0f","target":"main","base":"1f1f1f1f","landed_commit":"0f0f0f0f","patch_id":"2f2f2f2f","gate_result":"pass","om_verdict":"pass","om_score":0.5,"route":"normal","landed_at":"2026-10-02T10:00:00Z"}`,
			"",
		}, "\n"),
		".runtime/steward/jobs.jsonl": strings.Join([]string{
			`{"id":"j1","event":"end","bead":"gt-abc","rig":"gastown","model":"deepseek-flash","started":"2026-10-02T17:50:00Z","ended":"2026-10-02T18:00:00Z","outcome":"pass"}`,
			`{"id":"j2","event":"end","bead":"gt-def","rig":"gastown","model":"deepseek-flash","started":"2026-10-02T18:10:00Z","ended":"2026-10-02T18:30:00Z","outcome":"fail"}`,
			`{"id":"j3","event":"end","bead":"gt-old","rig":"gastown","model":"deepseek-flash","started":"2026-10-02T16:00:00Z","ended":"2026-10-02T17:00:00Z","outcome":"pass"}`,
			"",
		}, "\n"),
		".runtime/attention/state.json": `{"updated":"2026-10-02T18:52:00Z","items":[` +
			`{"key":"queue-stuck:gastown","kind":"queue-stuck","severity":"high","rig":"gastown","summary":"landing queue silent 12m","first_seen":"2026-10-02T18:40:00Z","last_seen":"2026-10-02T18:52:00Z"},` +
			`{"key":"risk:gt-abc:6cf8b456","kind":"risk-path","severity":"low","bead":"gt-abc","summary":"risk path touched","first_seen":"2026-10-02T18:45:00Z","last_seen":"2026-10-02T18:52:00Z","acked_at":"2026-10-02T18:50:00Z"}]}`,
		// Stale: three hours older than the window's end, so the spend
		// section is left out rather than reported as a live reading.
		".runtime/watch/spend.json": `{"ts":"2026-10-02T10:00:00-05:00","per_hour":2.41,"balance":18.20}`,
		".runtime/seat-refill.json": fmt.Sprintf(
			`{"version":1,"episodes":{"gastown/opal":{"empty_since":%d,"last_nudge":%d}}}`,
			at("2026-10-02T18:11:05Z").Unix(), at("2026-10-02T18:41:05Z").Unix()),
		".runtime/agents/gastown/refinery.json":     `{"version":1,"desired":"run","updated_at":"2026-10-02T17:00:00Z"}`,
		".runtime/agents/gastown/polecat.opal.json": `{"version":1,"desired":"park","updated_at":"2026-10-02T17:00:00Z"}`,
	})
}

// reportGoldenStore seeds the beads the fixture town's sections read.
func reportGoldenStore(t *testing.T) reportStore {
	t.Helper()
	f := beadsfake.New()
	f.Seed(
		beads.Issue{ID: "gt-q1", Status: "open", Labels: []string{land.LabelReadyToLand}},
		beads.Issue{ID: "gt-q2", Status: "open", Labels: []string{land.LabelReadyToLand}},
		beads.Issue{ID: "gt-q-closed", Status: "closed", Labels: []string{land.LabelReadyToLand}},
		beads.Issue{ID: "gt-q-deferred", Status: "deferred", Labels: []string{land.LabelReadyToLand}},
		beads.Issue{ID: "gt-esc-open", Status: "open",
			Labels: []string{"gt:escalation", "severity:high"}, CreatedAt: "2026-10-02T18:10:00Z"},
		beads.Issue{ID: "gt-esc-closed", Status: "closed",
			Labels: []string{"gt:escalation", "severity:high"}, CreatedAt: "2026-10-02T17:00:00Z",
			ClosedAt: "2026-10-02T18:20:00Z"},
	)
	return &reportFakeStore{Fake: f, rejected: []*beads.Issue{
		{Notes: land.FormatRejectionNote(land.RejectionNote{Attempt: 1, Kind: "lint", Reason: "docs-lint finding unaddressed"})},
		{Notes: land.FormatRejectionNote(land.RejectionNote{Attempt: 1, Kind: "tests", Reason: "gate red"})},
	}}
}

// reportGoldenStores serves the fixture beads for the rig store and nothing
// for hq: the town store holds no escalations in this town.
func reportGoldenStores(t *testing.T) func(string) (reportStore, error) {
	t.Helper()
	rig := reportGoldenStore(t)
	empty := &reportFakeStore{Fake: beadsfake.New()}
	return func(store string) (reportStore, error) {
		if store == "hq" {
			return empty, nil
		}
		return rig, nil
	}
}

func TestReportHourMatchesGolden(t *testing.T) {
	t.Parallel()
	townRoot := reportGoldenTown(t)
	r, out := reportRunFor(t, townRoot, reportGoldenStores(t))
	r.render(r.collect())

	goldenPath := filepath.Join("testdata", "report_hour.txt")
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("reading %s: %v", goldenPath, err)
	}
	if got := out.String(); got != string(want) {
		t.Fatalf("gt report --hour:\n--- got ---\n%s\n--- want (%s) ---\n%s", got, goldenPath, want)
	}
}

// TestReportHourIsReadOnly injects a bd whose write methods fail the test: a
// report that writes anywhere — a note, a label, an event bead — is a bug.
func TestReportHourIsReadOnly(t *testing.T) {
	t.Parallel()
	townRoot := reportGoldenTown(t)
	stores := reportGoldenStores(t)
	guard := func(store string) (reportStore, error) {
		s, err := stores(store)
		if err != nil {
			return nil, err
		}
		return reportNoWriteStore{reportStore: s, t: t}, nil
	}
	r, _ := reportRunFor(t, townRoot, guard)
	r.render(r.collect())
}

// TestReportHourWindowIsTimezoneAware pins the window's two ends in both
// zones the sources use: daemon.log and the landings file are local time, bd
// JSON is UTC. Each source contributes a line just inside and just outside
// the same instant.
func TestReportHourWindowIsTimezoneAware(t *testing.T) {
	t.Parallel()
	townRoot := reportTown(t, map[string]string{
		"daemon/daemon.log": strings.Join([]string{
			"2026/10/02 12:52:04 landing_worker: [land] gt-too-early: merged 0000aaaa onto origin/main (1111bbbb) as 2222cccc; gating the merged tree, then om review",
			"2026/10/02 12:52:05 landing_worker: [land] gt-at-start: merged 1a2b3c4d onto origin/main (5e6f7a8b) as 9a8b7c6d; gating the merged tree, then om review",
			"2026/10/02 13:52:05 landing_worker: [land] gt-at-end: merged 3a3b3c3d onto origin/main (4e4f4a4b) as 5a5b5c5d; gating the merged tree, then om review",
			"2026/10/02 13:52:06 landing_worker: [land] gt-too-late: merged 6a6b6c6d onto origin/main (7e7f7a7b) as 8a8b8c8d; gating the merged tree, then om review",
			"",
		}, "\n"),
	})
	f := beadsfake.New()
	f.Seed(
		// 17:52:05Z is 12:52:05 CDT: the window's start. One second earlier is out.
		beads.Issue{ID: "gt-esc-in", Status: "open",
			Labels: []string{"gt:escalation"}, CreatedAt: "2026-10-02T17:52:05Z"},
		beads.Issue{ID: "gt-esc-before", Status: "open",
			Labels: []string{"gt:escalation"}, CreatedAt: "2026-10-02T17:52:04Z"},
		beads.Issue{ID: "gt-esc-closed-edge", Status: "closed",
			Labels: []string{"gt:escalation"}, CreatedAt: "2026-10-02T17:00:00Z",
			ClosedAt: "2026-10-02T18:52:06Z"},
		beads.Issue{ID: "gt-esc-closed-in", Status: "closed",
			Labels: []string{"gt:escalation"}, CreatedAt: "2026-10-02T17:00:00Z",
			ClosedAt: "2026-10-02T18:52:05Z"},
	)
	// The town store holds none of it: everything lives in the rig store.
	rig, empty := &reportFakeStore{Fake: f}, &reportFakeStore{Fake: beadsfake.New()}
	stores := func(store string) (reportStore, error) {
		if store == "hq" {
			return empty, nil
		}
		return rig, nil
	}
	r, _ := reportRunFor(t, townRoot, stores)
	doc := r.collect()

	var beads []string
	for _, l := range doc.Landings {
		beads = append(beads, l.Bead)
	}
	if want := []string{"gt-at-start", "gt-at-end"}; strings.Join(beads, ",") != strings.Join(want, ",") {
		t.Fatalf("landings = %v, want %v (the ends are inclusive; a second outside either is out)", beads, want)
	}
	if doc.Escalations.Opened != 1 || doc.Escalations.Closed != 1 {
		t.Fatalf("escalations = %+v, want one opened and one closed: only the rows inside [start, end] count", doc.Escalations)
	}
}

// TestReportHourSpendNeedsAFreshReading: the spend section appears only while
// the file is under reportSpendFreshness old, and a missing file is not an
// error.
func TestReportHourSpendNeedsAFreshReading(t *testing.T) {
	t.Parallel()
	stores := func(string) (reportStore, error) { return &reportFakeStore{Fake: beadsfake.New()}, nil }
	cases := []struct {
		name     string
		spend    string
		absent   bool
		stampMod bool
		shown    bool
	}{
		{name: "fresh", spend: `{"ts":"2026-10-02T13:45:05-05:00","per_hour":3.10,"balance":12.00}`, shown: true},
		{name: "at the bound", spend: `{"ts":"2026-10-02T13:37:05-05:00","per_hour":3.10,"balance":12.00}`},
		{name: "stale", spend: `{"ts":"2026-10-02T13:37:04-05:00","per_hour":3.10,"balance":12.00}`},
		{name: "absent", absent: true},
		{name: "no timestamp falls back to mtime", spend: `{"per_hour":3.10,"balance":12.00}`, stampMod: true, shown: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			files := map[string]string{}
			if !tc.absent {
				files[".runtime/watch/spend.json"] = tc.spend
			}
			townRoot := reportTown(t, files)
			if tc.stampMod {
				// The mtime fallback is read from the file system, so pin it
				// to the fixture clock rather than the test's wall clock.
				at := reportWindowEnd.Add(-time.Minute)
				if err := os.Chtimes(reportSpendPath(townRoot), at, at); err != nil {
					t.Fatal(err)
				}
			}
			r, out := reportRunFor(t, townRoot, stores)
			r.render(r.collect())
			has := strings.Contains(out.String(), "DeepSeek")
			if has != tc.shown {
				t.Fatalf("spend shown = %v, want %v:\n%s", has, tc.shown, out)
			}
			if !tc.shown && strings.Contains(out.String(), "spend\n  unavailable") {
				t.Fatalf("a stale spend file must be omitted, not reported unavailable:\n%s", out)
			}
		})
	}
}

// TestReportHourUnavailableSections: a source that cannot be read names its
// reason and the command still renders every other section. The attention
// queue is one of the two whose writer lands later (gt-vsct7.2): with no
// state.json there is nothing to show, and that reads as unavailable, not as
// an empty queue.
func TestReportHourUnavailableSections(t *testing.T) {
	t.Parallel()
	r, out := reportRunFor(t, reportTown(t, map[string]string{}),
		func(string) (reportStore, error) { return nil, errors.New("bd is down") })
	doc := r.collect()
	r.render(doc)

	text := out.String()
	for _, want := range []string{
		"mains\n  origin/main",
		"landings\n  (none)",
		"rejections by kind\n  unavailable (bd is down)",
		"queue depth\n  gastown  unavailable (bd is down)",
		"attention\n  unavailable (",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("report is missing %q:\n%s", want, text)
		}
	}
	if _, ok := doc.Unavailable["spend"]; ok {
		t.Errorf("an absent spend file must be omitted, not unavailable: %v", doc.Unavailable)
	}
}

// TestReportHourMainsNamesTheRefItCountedAgainst: a count made against a
// clone's own origin/main, because the remote tip was never fetched, names
// that ref rather than passing itself off as the count against the tip.
func TestReportHourMainsNamesTheRefItCountedAgainst(t *testing.T) {
	t.Parallel()
	stores := func(string) (reportStore, error) { return &reportFakeStore{Fake: beadsfake.New()}, nil }
	r, out := reportRunFor(t, reportTown(t, map[string]string{}), stores)
	r.mainTip = func(string) (reportMains, error) {
		return reportMains{SHA: "aa0a73eefad7", CommitsBehind: 12, BehindRef: "138368fd7571"}, nil
	}
	r.render(r.collect())
	want := "origin/main aa0a73ee  installed gt 1.2.1-dev (5e6f7a8b)  12 commits behind (counted against origin/main at 138368fd: the remote tip is not fetched)"
	if !strings.Contains(out.String(), want) {
		t.Fatalf("mains row:\n%s\nwant %q", out, want)
	}
}

// TestReportBehind: the tip's count when the tip is local; the clone's own
// origin/main ref, named, when it is not; unknown only when neither works.
func TestReportBehind(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		installed  string
		tipIsLocal bool
		local      string
		resolved   bool
		want       int
		wantRef    string
		wantOK     bool
	}{
		{name: "the tip is local", installed: "inst", tipIsLocal: true, want: 9, wantOK: true},
		{name: "the tip is not fetched", installed: "inst", local: "local", resolved: true, want: 7, wantRef: "local", wantOK: true},
		{name: "the local ref is the tip after all", installed: "inst", local: "tip", resolved: true},
		{name: "no local ref", installed: "inst"},
		{name: "no build commit", local: "local", resolved: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			behind, ref, ok := reportBehind(tc.installed, "tip",
				func(_, to string) (int, bool) {
					switch to {
					case "tip":
						return 9, tc.tipIsLocal
					case "local":
						return 7, true
					}
					return 0, false
				},
				func() (string, bool) { return tc.local, tc.resolved })
			if ok != tc.wantOK || behind != tc.want || ref != tc.wantRef {
				t.Fatalf("reportBehind = (%d, %q, %v), want (%d, %q, %v)", behind, ref, ok, tc.want, tc.wantRef, tc.wantOK)
			}
		})
	}
}

// TestReportHourJSON checks the --json shape: the same sections, typed.
func TestReportHourJSON(t *testing.T) {
	t.Parallel()
	townRoot := reportGoldenTown(t)
	r, out := reportRunFor(t, townRoot, reportGoldenStores(t))
	doc := r.collect()
	if err := json.NewEncoder(out).Encode(doc); err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("--json output is not JSON: %v\n%s", err, out)
	}
	for _, key := range []string{"since", "until", "mains", "landings", "rejections", "queue_depth", "seats", "sweep", "escalations", "steward", "attention"} {
		if _, ok := got[key]; !ok {
			t.Errorf("--json is missing %q: %v", key, got)
		}
	}
	if _, ok := got["spend"]; ok {
		t.Errorf("--json must omit the spend key for a stale reading: %v", got["spend"])
	}
}

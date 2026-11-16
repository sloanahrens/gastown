package beads

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/dashboard"
	"github.com/steveyegge/gastown/internal/usagerecord"
)

// everyFieldUsageRow carries every field of a UsageRecordRow at once: the codec
// writes whatever the record holds, so one row proves each field's trip.
func everyFieldUsageRow() UsageRecordRow {
	seat := "furiosa"
	return UsageRecordRow{
		BuiltAt: time.Date(2026, 11, 15, 17, 5, 0, 0, time.FixedZone("CST", -6*3600)),
		Record: usagerecord.Record{
			Bead:       "gt-usage-1",
			Kind:       dashboard.UsageKindWork,
			Class:      dashboard.UsageClassPolecat,
			Rig:        "gastown",
			Workspace:  "ws-town",
			Seat:       &seat,
			Complete:   true,
			Start:      time.Date(2026, 11, 15, 16, 0, 0, 0, time.FixedZone("CST", -6*3600)),
			End:        time.Date(2026, 11, 15, 17, 0, 0, 0, time.FixedZone("CST", -6*3600)),
			WallS:      3600,
			EstCostUSD: 1.25,
			Models: map[string]usagerecord.Model{
				"opus":   {Calls: 2, Input: 10, Output: 20, CacheCreation: 1, CacheRead: 3, EstCostUSD: 0.75},
				"sonnet": {Calls: 1, Input: 5, Output: 6, CacheCreation: 0, CacheRead: 1, EstCostUSD: 0.5},
			},
			Totals:    usagerecord.Totals{Calls: 3, Input: 15, Output: 26, CacheCreation: 1, CacheRead: 4, Uncached: 42},
			Subagents: usagerecord.Subagents{Calls: 1, Share: 0.5, Uncached: 7},
			Activity:  usagerecord.Activity{ToolCalls: 9, Edits: 3, TestRuns: 2, FailedTestRuns: 1},
		},
		Sessions: []string{"sess-1", "sess-2"},
	}
}

// TestUsageRecordArgvAndRoundTrip: the write runs bd sample add with the kind,
// the UTC built at and the payload on stdin; the read runs bd sample list with
// the kind, the UTC since and the limit; and every field of the row the write
// carried comes back from the row the read lists.
func TestUsageRecordArgvAndRoundTrip(t *testing.T) {
	t.Parallel()
	var listed []string
	r := newRecorder(func(args []string) reply {
		argv := strings.Join(args, " ")
		switch {
		case argv == "--help":
			return reply{stdout: bdHelpWithSampleVerb}
		case strings.HasPrefix(argv, "sample add "):
			return reply{}
		case strings.HasPrefix(argv, "sample list "):
			return reply{stdout: strings.Join(listed, "\n")}
		}
		return reply{err: fmt.Errorf("unexpected bd call %v", args)}
	})
	b := newRecordedBeads(t.TempDir(), r)
	ctx := context.Background()
	want := everyFieldUsageRow()

	if err := b.WriteUsageRecord(ctx, want); err != nil {
		t.Fatalf("WriteUsageRecord: %v", err)
	}
	calls := r.calls()
	if len(calls) != 2 {
		t.Fatalf("bd calls = %v, want --help then one sample add", r.argvs())
	}
	if argv := strings.Join(calls[1].args, " "); argv != "sample add --kind usage_record --at 2026-11-15T23:05:00Z --json -" {
		t.Errorf("sample add argv = %q, want the kind, the UTC built at and --json -", argv)
	}
	var payload map[string]any
	if err := json.Unmarshal(calls[1].stdin, &payload); err != nil {
		t.Fatalf("sample add stdin = %q, want a JSON object: %v", calls[1].stdin, err)
	}
	for _, field := range []string{"bead", "kind", "class", "rig", "workspace", "seat", "complete", "start", "end", "wall_s", "est_cost_usd", "models", "totals", "subagents", "activity", "sessions"} {
		if _, ok := payload[field]; !ok {
			t.Errorf("payload %s lacks %q", calls[1].stdin, field)
		}
	}

	row, err := json.Marshal(map[string]any{
		"id":         "gt-row-1",
		"kind":       "usage_record",
		"at":         "2026-11-15T23:05:00Z",
		"payload":    json.RawMessage(calls[1].stdin),
		"created_at": "2026-11-15T23:05:01Z",
	})
	if err != nil {
		t.Fatalf("build row: %v", err)
	}
	listed = []string{"[" + string(row) + "]"}

	got, err := b.ReadUsageRecords(ctx, time.Date(2026, 11, 15, 20, 0, 0, 0, time.UTC), 2)
	if err != nil {
		t.Fatalf("ReadUsageRecords: %v", err)
	}
	calls = r.calls()
	if argv := strings.Join(calls[len(calls)-1].args, " "); argv != "sample list --kind usage_record --since 2026-11-15T20:00:00Z --limit 2 --json" {
		t.Errorf("sample list argv = %q, want the kind, the UTC since, the limit and --json", argv)
	}
	if len(got) != 1 {
		t.Fatalf("ReadUsageRecords = %v, want the one listed row", got)
	}
	if !got[0].BuiltAt.Equal(want.BuiltAt) || got[0].BuiltAt.Location() != time.UTC {
		t.Errorf("BuiltAt = %v, want %v in UTC", got[0].BuiltAt, want.BuiltAt)
	}
	assertUsageRecord(t, want.Record, got[0].Record)
	if len(got[0].Sessions) != 2 || got[0].Sessions[0] != "sess-1" || got[0].Sessions[1] != "sess-2" {
		t.Errorf("Sessions = %v, want the two written", got[0].Sessions)
	}
}

// assertUsageRecord compares a round-tripped record field by field, with Start
// and End required in UTC.
func assertUsageRecord(t *testing.T, want, got usagerecord.Record) {
	t.Helper()
	if got.Bead != want.Bead || got.Kind != want.Kind || got.Class != want.Class || got.Rig != want.Rig || got.Workspace != want.Workspace {
		t.Errorf("record = %+v, want bead %q kind %q class %q rig %q workspace %q", got, want.Bead, want.Kind, want.Class, want.Rig, want.Workspace)
	}
	if (got.Seat == nil) != (want.Seat == nil) || (got.Seat != nil && *got.Seat != *want.Seat) {
		t.Errorf("Seat = %v, want %v", got.Seat, want.Seat)
	}
	if got.Complete != want.Complete || got.WallS != want.WallS || got.EstCostUSD != want.EstCostUSD {
		t.Errorf("complete/wall_s/est_cost_usd = %v/%d/%v, want %v/%d/%v", got.Complete, got.WallS, got.EstCostUSD, want.Complete, want.WallS, want.EstCostUSD)
	}
	if !got.Start.Equal(want.Start) || got.Start.Location() != time.UTC {
		t.Errorf("Start = %v, want %v in UTC", got.Start, want.Start)
	}
	if !got.End.Equal(want.End) || got.End.Location() != time.UTC {
		t.Errorf("End = %v, want %v in UTC", got.End, want.End)
	}
	if len(got.Models) != len(want.Models) {
		t.Errorf("Models = %v, want %v", got.Models, want.Models)
	}
	for name, model := range want.Models {
		if got.Models[name] != model {
			t.Errorf("Models[%q] = %+v, want %+v", name, got.Models[name], model)
		}
	}
	if got.Totals != want.Totals {
		t.Errorf("Totals = %+v, want %+v", got.Totals, want.Totals)
	}
	if got.Subagents != want.Subagents {
		t.Errorf("Subagents = %+v, want %+v", got.Subagents, want.Subagents)
	}
	if got.Activity != want.Activity {
		t.Errorf("Activity = %+v, want %+v", got.Activity, want.Activity)
	}
}

// TestUsageRecordReadArgvStepsTheFilters: the list argv carries --since and
// --limit only when the caller set them, and always asks for JSON.
func TestUsageRecordReadArgvStepsTheFilters(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		since time.Time
		limit int
		want  string
	}{
		{"neither", time.Time{}, 0, "sample list --kind usage_record --json"},
		{"since only", time.Date(2026, 11, 15, 20, 0, 0, 0, time.UTC), 0, "sample list --kind usage_record --since 2026-11-15T20:00:00Z --json"},
		{"limit only", time.Time{}, 5, "sample list --kind usage_record --limit 5 --json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newRecorder(func(args []string) reply {
				if strings.HasPrefix(strings.Join(args, " "), "sample list ") {
					return reply{stdout: "[]"}
				}
				return reply{err: fmt.Errorf("unexpected bd call %v", args)}
			})
			b := newRecordedBeads(t.TempDir(), r)
			got, err := b.ReadUsageRecords(context.Background(), tc.since, tc.limit)
			if err != nil {
				t.Fatalf("ReadUsageRecords: %v", err)
			}
			if len(got) != 0 {
				t.Errorf("ReadUsageRecords = %v, want no rows", got)
			}
			if argv := strings.Join(r.calls()[0].args, " "); argv != tc.want {
				t.Errorf("sample list argv = %q, want %q", argv, tc.want)
			}
		})
	}
}

// TestUsageRecordWriteRefusesBdWithoutTheVerb: a bd whose help lists no sample
// command refuses the write, naming the verb, and never runs sample add.
func TestUsageRecordWriteRefusesBdWithoutTheVerb(t *testing.T) {
	t.Parallel()
	r := newRecorder(func(args []string) reply {
		argv := strings.Join(args, " ")
		if argv == "--help" {
			return reply{stdout: bdHelpWithoutSampleVerb}
		}
		// A bd that accepted the write would succeed, so an unwired probe
		// shows up as a nil error here rather than as a stray call.
		if strings.HasPrefix(argv, "sample add ") {
			return reply{}
		}
		return reply{err: fmt.Errorf("unexpected bd call %v", args)}
	})
	b := newRecordedBeads(t.TempDir(), r)

	err := b.WriteUsageRecord(context.Background(), everyFieldUsageRow())
	if err == nil {
		t.Fatal("WriteUsageRecord = nil, want a refusal naming the sample verb")
	}
	if !strings.Contains(err.Error(), "does not offer the sample verb") {
		t.Errorf("error = %q, want the refusal to name the missing sample verb", err)
	}
	if calls := r.argvs(); len(calls) != 1 || calls[0] != "--help" {
		t.Errorf("bd calls = %v, want only the --help probe", calls)
	}
}

// TestUsageRecordProbeFailureStopsTheWrite: a bd that does not answer --help is
// an error, never a silent write.
func TestUsageRecordProbeFailureStopsTheWrite(t *testing.T) {
	t.Parallel()
	r := newRecorder(func(args []string) reply {
		if argv := strings.Join(args, " "); argv == "--help" {
			return reply{err: exitError{code: 2}}
		}
		return reply{err: fmt.Errorf("unexpected bd call %v", args)}
	})
	b := newRecordedBeads(t.TempDir(), r)

	err := b.WriteUsageRecord(context.Background(), everyFieldUsageRow())
	if err == nil {
		t.Fatal("WriteUsageRecord = nil, want the failed probe returned")
	}
	if calls := r.argvs(); len(calls) != 1 || calls[0] != "--help" {
		t.Errorf("bd calls = %v, want only the --help probe", calls)
	}
}

// TestUsageRecordRefusesLocalInput: the built at, the bead and the costs are
// checked before bd runs, and each refusal leaves no bd call.
func TestUsageRecordRefusesLocalInput(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 15, 22, 0, 0, 0, time.UTC)
	nan, inf := math.NaN(), math.Inf(1)
	for _, tc := range []struct {
		name    string
		record  func(usagerecord.Record) usagerecord.Record
		builtAt time.Time
		bead    string
		limit   int
		read    bool
		wantSub string
	}{
		{name: "write no built at", builtAt: time.Time{}, bead: "gt-1", wantSub: "no sample time"},
		{name: "write no bead", builtAt: at, bead: "", wantSub: "no bead"},
		{name: "write NaN cost", builtAt: at, bead: "gt-1", record: func(r usagerecord.Record) usagerecord.Record {
			r.EstCostUSD = nan
			return r
		}, wantSub: "est_cost_usd"},
		{name: "write infinite share", builtAt: at, bead: "gt-1", record: func(r usagerecord.Record) usagerecord.Record {
			r.Subagents.Share = inf
			return r
		}, wantSub: "subagents.share"},
		{name: "write NaN model cost", builtAt: at, bead: "gt-1", record: func(r usagerecord.Record) usagerecord.Record {
			r.Models = map[string]usagerecord.Model{"opus": {EstCostUSD: nan}}
			return r
		}, wantSub: "models.opus.est_cost_usd"},
		{name: "write unencodable start", builtAt: at, bead: "gt-1", record: func(r usagerecord.Record) usagerecord.Record {
			r.Start = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
			return r
		}, wantSub: "encode usage_record payload"},
		{name: "read negative limit", read: true, limit: -1, wantSub: "-1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newRecorder(func(args []string) reply {
				return reply{err: fmt.Errorf("unexpected bd call %v", args)}
			})
			b := newRecordedBeads(t.TempDir(), r)
			var err error
			if tc.read {
				_, err = b.ReadUsageRecords(context.Background(), time.Time{}, tc.limit)
			} else {
				rec := usagerecord.Record{Bead: tc.bead}
				if tc.record != nil {
					rec = tc.record(rec)
				}
				err = b.WriteUsageRecord(context.Background(), UsageRecordRow{BuiltAt: tc.builtAt, Record: rec})
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("error = %v, want one naming %q", err, tc.wantSub)
			}
			if calls := r.argvs(); len(calls) != 0 {
				t.Errorf("bd calls = %v, want none", calls)
			}
		})
	}
}

// TestUsageRecordReadReturnsTheBdFailure: a bd sample list that fails is the
// error, with no rows beside it.
func TestUsageRecordReadReturnsTheBdFailure(t *testing.T) {
	t.Parallel()
	r := newRecorder(func(args []string) reply {
		if strings.HasPrefix(strings.Join(args, " "), "sample list ") {
			return reply{stderr: "Error: failed to open database: invalid connection", err: exitError{code: 1}}
		}
		return reply{err: fmt.Errorf("unexpected bd call %v", args)}
	})
	b := newRecordedBeads(t.TempDir(), r)

	rows, err := b.ReadUsageRecords(context.Background(), time.Time{}, 0)
	if err == nil {
		t.Fatal("ReadUsageRecords = nil, want the bd failure returned")
	}
	if !strings.Contains(err.Error(), "bd sample list") {
		t.Errorf("error = %q, want it to name the bd sample list call", err)
	}
	if rows != nil {
		t.Errorf("ReadUsageRecords answered %v beside its error, want no rows", rows)
	}
}

// TestUsageRecordReadRefusesUnreadableRows: a row under another kind, a payload
// that is not an object, a record with no bead, a timestamp that does not parse
// and output that is not a row array are errors, never rows or a short series.
func TestUsageRecordReadRefusesUnreadableRows(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		out  string
		want string
	}{
		{"another kind", `[{"kind":"claude_plan","at":"2026-11-15T22:00:00Z","payload":{}}]`, `kind "claude_plan", want "usage_record"`},
		{"payload string", `[{"kind":"usage_record","at":"2026-11-15T22:00:00Z","payload":"{}"}]`, "payload is not a JSON object"},
		{"payload without bead", `[{"kind":"usage_record","at":"2026-11-15T22:00:00Z","payload":{"kind":"work"}}]`, "no bead"},
		{"unparseable start", `[{"kind":"usage_record","at":"2026-11-15T22:00:00Z","payload":{"bead":"gt-1","start":"soon"}}]`, "decode payload"},
		{"row field of the wrong type", `[{"kind":1,"at":"2026-11-15T22:00:00Z","payload":{}}]`, "decode rows"},
		{"not an array", `{"rows":[]}`, "decode rows"},
		{"row without at", `[{"kind":"usage_record","payload":{"bead":"gt-1"}}]`, "has no at"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newRecorder(func(args []string) reply {
				if strings.HasPrefix(strings.Join(args, " "), "sample list ") {
					return reply{stdout: tc.out}
				}
				return reply{err: fmt.Errorf("unexpected bd call %v", args)}
			})
			b := newRecordedBeads(t.TempDir(), r)
			got, err := b.ReadUsageRecords(context.Background(), time.Time{}, 1)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ReadUsageRecords = (%v, %v), want an error naming %q", got, err, tc.want)
			}
		})
	}
}

// TestUsageRecordWriteIsNotRetried: a sample add is an append, so a failure the
// container retry loop would re-run for a read leaves one attempt: a second
// attempt after a connection lost mid-command can store the row twice.
func TestUsageRecordWriteIsNotRetried(t *testing.T) {
	t.Parallel()
	adds := 0
	r := newRecorder(func(args []string) reply {
		switch argv := strings.Join(args, " "); {
		case argv == "--help":
			return reply{stdout: bdHelpWithSampleVerb}
		case strings.HasPrefix(argv, "sample add "):
			adds++
			return reply{stderr: "Error: failed to open database: invalid connection", err: exitError{code: 1}}
		}
		return reply{err: fmt.Errorf("unexpected bd call %v", args)}
	})
	// A test-container wrapper is one the retry loop retries.
	b := newBeads(beadsFields{workDir: t.TempDir(), isolated: true, serverPort: 3306, noRoute: true, exec: r.exec})

	err := b.WriteUsageRecord(context.Background(), everyFieldUsageRow())
	if err == nil {
		t.Fatal("WriteUsageRecord = nil, want the sample add failure returned")
	}
	if adds != 1 {
		t.Errorf("sample add attempts = %d, want 1: a retried append can store the row twice", adds)
	}
}

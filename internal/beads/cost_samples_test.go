package beads

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
)

const bdHelpWithoutSampleVerb = `Issues chained together like beads.

Usage:
  bd [flags]
  bd [command]

Available Commands:
  show             Show issue details
  update           Update one or more issues
`

const bdHelpWithSampleVerb = `Issues chained together like beads.

Usage:
  bd [flags]
  bd [command]

Available Commands:
  sample           Cost and usage samples
  show             Show issue details
`

// everyFieldSample carries every Sample field at once: the codec decides what
// the payload holds, not the kind, so one sample proves each field's trip.
func everyFieldSample() Sample {
	total, granted, toppedUp := 18.25, 5.0, 13.25
	fivePct, sevenPct := 41.0, 23.5
	fiveResets := time.Date(2026, 11, 15, 22, 0, 0, 0, time.UTC)
	sevenResets := time.Date(2026, 11, 20, 9, 30, 0, 0, time.FixedZone("CST", -6*3600))
	return Sample{
		Kind:             SampleKindClaudePlan,
		At:               time.Date(2026, 11, 15, 17, 5, 0, 0, time.FixedZone("CST", -6*3600)),
		Total:            &total,
		Granted:          &granted,
		ToppedUp:         &toppedUp,
		FiveHourPct:      &fivePct,
		FiveHourResetsAt: &fiveResets,
		SevenDayPct:      &sevenPct,
		SevenDayResetsAt: &sevenResets,
		Note:             "five-hour window is close to its cap",
	}
}

// TestCostSampleArgvAndRoundTrip: the write runs bd sample add with the kind,
// the UTC second-precision time and the payload on stdin; the read runs bd
// sample list with the kind, the UTC since and the limit; and every field of
// the sample the write carried comes back from the row the read lists.
func TestCostSampleArgvAndRoundTrip(t *testing.T) {
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
	want := everyFieldSample()

	if err := b.WriteCostSample(ctx, want); err != nil {
		t.Fatalf("WriteCostSample: %v", err)
	}
	calls := r.calls()
	if len(calls) != 2 {
		t.Fatalf("bd calls = %v, want --help then one sample add", r.argvs())
	}
	if argv := strings.Join(calls[1].args, " "); argv != "sample add --kind claude_plan --at 2026-11-15T23:05:00Z --json -" {
		t.Errorf("sample add argv = %q, want the kind, the UTC at and --json -", argv)
	}
	var payload map[string]any
	if err := json.Unmarshal(calls[1].stdin, &payload); err != nil {
		t.Fatalf("sample add stdin = %q, want a JSON object: %v", calls[1].stdin, err)
	}
	for _, field := range []string{"total", "granted", "topped_up", "five_hour_pct", "seven_day_pct", "note"} {
		if _, ok := payload[field]; !ok {
			t.Errorf("payload %s lacks %q", calls[1].stdin, field)
		}
	}

	row, err := json.Marshal(map[string]any{
		"id":         "gt-row-1",
		"kind":       "claude_plan",
		"at":         "2026-11-15T23:05:00Z",
		"payload":    json.RawMessage(calls[1].stdin),
		"created_at": "2026-11-15T23:05:01Z",
	})
	if err != nil {
		t.Fatalf("build row: %v", err)
	}
	listed = []string{"[" + string(row) + "]"}

	got, err := b.ReadCostSamples(ctx, SampleKindClaudePlan, time.Date(2026, 11, 15, 20, 0, 0, 0, time.UTC), 2)
	if err != nil {
		t.Fatalf("ReadCostSamples: %v", err)
	}
	calls = r.calls()
	if argv := strings.Join(calls[len(calls)-1].args, " "); argv != "sample list --kind claude_plan --since 2026-11-15T20:00:00Z --limit 2 --json" {
		t.Errorf("sample list argv = %q, want the kind, the UTC since, the limit and --json", argv)
	}
	if len(got) != 1 {
		t.Fatalf("ReadCostSamples = %v, want the one listed row", got)
	}
	assertSampleFields(t, want, got[0])
}

// assertSampleFields compares a round-tripped sample field by field, with At
// required in UTC at second precision.
func assertSampleFields(t *testing.T, want, got Sample) {
	t.Helper()
	if got.Kind != want.Kind {
		t.Errorf("Kind = %q, want %q", got.Kind, want.Kind)
	}
	if !got.At.Equal(want.At) {
		t.Errorf("At = %v, want %v", got.At, want.At)
	}
	if got.At.Location() != time.UTC {
		t.Errorf("At location = %v, want UTC", got.At.Location())
	}
	if got.At.Nanosecond() != 0 {
		t.Errorf("At nanosecond = %d, want second precision", got.At.Nanosecond())
	}
	assertFloat(t, "Total", got.Total, want.Total)
	assertFloat(t, "Granted", got.Granted, want.Granted)
	assertFloat(t, "ToppedUp", got.ToppedUp, want.ToppedUp)
	assertFloat(t, "FiveHourPct", got.FiveHourPct, want.FiveHourPct)
	assertFloat(t, "SevenDayPct", got.SevenDayPct, want.SevenDayPct)
	assertTime(t, "FiveHourResetsAt", got.FiveHourResetsAt, want.FiveHourResetsAt)
	assertTime(t, "SevenDayResetsAt", got.SevenDayResetsAt, want.SevenDayResetsAt)
	if got.Note != want.Note {
		t.Errorf("Note = %q, want %q", got.Note, want.Note)
	}
}

func assertFloat(t *testing.T, field string, got, want *float64) {
	t.Helper()
	if (got == nil) != (want == nil) {
		t.Errorf("%s = %v, want %v", field, got, want)
		return
	}
	if got != nil && *got != *want {
		t.Errorf("%s = %v, want %v", field, *got, *want)
	}
}

func assertTime(t *testing.T, field string, got, want *time.Time) {
	t.Helper()
	if (got == nil) != (want == nil) {
		t.Errorf("%s = %v, want %v", field, got, want)
		return
	}
	if got != nil && !got.Equal(*want) {
		t.Errorf("%s = %v, want %v", field, *got, *want)
	}
}

// TestCostSampleReadArgvStepsTheFilters: the list argv carries --since and
// --limit only when the caller set them, and always asks for JSON.
func TestCostSampleReadArgvStepsTheFilters(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		since time.Time
		limit int
		want  string
	}{
		{"neither", time.Time{}, 0, "sample list --kind deepseek_balance --json"},
		{"since only", time.Date(2026, 11, 15, 20, 0, 0, 0, time.UTC), 0, "sample list --kind deepseek_balance --since 2026-11-15T20:00:00Z --json"},
		{"limit only", time.Time{}, 5, "sample list --kind deepseek_balance --limit 5 --json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newRecorder(func(args []string) reply {
				argv := strings.Join(args, " ")
				if strings.HasPrefix(argv, "sample list ") {
					return reply{stdout: "[]"}
				}
				return reply{err: fmt.Errorf("unexpected bd call %v", args)}
			})
			b := newRecordedBeads(t.TempDir(), r)
			got, err := b.ReadCostSamples(context.Background(), SampleKindDeepSeekBalance, tc.since, tc.limit)
			if err != nil {
				t.Fatalf("ReadCostSamples: %v", err)
			}
			if len(got) != 0 {
				t.Errorf("ReadCostSamples = %v, want no rows", got)
			}
			if argv := strings.Join(r.calls()[0].args, " "); argv != tc.want {
				t.Errorf("sample list argv = %q, want %q", argv, tc.want)
			}
		})
	}
}

// TestCostSampleWriteRefusesBdWithoutTheVerb: a bd whose help lists no sample
// command refuses the write, naming the verb, and never runs sample add.
func TestCostSampleWriteRefusesBdWithoutTheVerb(t *testing.T) {
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

	err := b.WriteCostSample(context.Background(), Sample{Kind: SampleKindClaudePlan, At: time.Date(2026, 11, 15, 22, 0, 0, 0, time.UTC)})
	if err == nil {
		t.Fatal("WriteCostSample = nil, want a refusal naming the sample verb")
	}
	if !strings.Contains(err.Error(), "does not offer the sample verb") {
		t.Errorf("error = %q, want the refusal to name the missing sample verb", err)
	}
	if calls := r.argvs(); len(calls) != 1 || calls[0] != "--help" {
		t.Errorf("bd calls = %v, want only the --help probe", calls)
	}
}

// TestCostSampleProbeFailureStopsTheWrite: a bd that does not answer --help is
// an error, never a silent write.
func TestCostSampleProbeFailureStopsTheWrite(t *testing.T) {
	t.Parallel()
	r := newRecorder(func(args []string) reply {
		if argv := strings.Join(args, " "); argv == "--help" {
			return reply{err: exitError{code: 2}}
		}
		return reply{err: fmt.Errorf("unexpected bd call %v", args)}
	})
	b := newRecordedBeads(t.TempDir(), r)

	err := b.WriteCostSample(context.Background(), Sample{Kind: SampleKindClaudePlan, At: time.Date(2026, 11, 15, 22, 0, 0, 0, time.UTC)})
	if err == nil {
		t.Fatal("WriteCostSample = nil, want the failed probe returned")
	}
	if calls := r.argvs(); len(calls) != 1 || calls[0] != "--help" {
		t.Errorf("bd calls = %v, want only the --help probe", calls)
	}
}

// TestCostSampleRefusesLocalInput: the kind, the time, the limit and the
// payload are checked before bd runs, and each refusal leaves no bd call.
func TestCostSampleRefusesLocalInput(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 15, 22, 0, 0, 0, time.UTC)
	nan := math.NaN()
	for _, tc := range []struct {
		name    string
		write   Sample
		read    bool
		kind    string
		limit   int
		wantSub string
	}{
		{name: "write unknown kind", write: Sample{Kind: "usage_record", At: at}, wantSub: "usage_record"},
		{name: "write no time", write: Sample{Kind: SampleKindClaudePlan}, wantSub: "no sample time"},
		{name: "write NaN reading", write: Sample{Kind: SampleKindClaudePlan, At: at, FiveHourPct: &nan}, wantSub: "encode claude_plan payload"},
		{name: "read unknown kind", read: true, kind: "usage_record", limit: 1, wantSub: "usage_record"},
		{name: "read negative limit", read: true, kind: SampleKindClaudePlan, limit: -1, wantSub: "-1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newRecorder(func(args []string) reply {
				return reply{err: fmt.Errorf("unexpected bd call %v", args)}
			})
			b := newRecordedBeads(t.TempDir(), r)
			var err error
			if tc.read {
				_, err = b.ReadCostSamples(context.Background(), tc.kind, time.Time{}, tc.limit)
			} else {
				err = b.WriteCostSample(context.Background(), tc.write)
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

// TestCostSampleCallsBoundByTheirContext: the ctx each method takes reaches
// the subprocess seam of both kinds of wrapper, so a canceled caller ends its
// bd call.
func TestCostSampleCallsBoundByTheirContext(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		plain bool
		call  func(*Beads, context.Context) error
	}{
		{"write", false, func(b *Beads, ctx context.Context) error {
			return b.WriteCostSample(ctx, Sample{Kind: SampleKindClaudePlan, At: time.Date(2026, 11, 15, 22, 0, 0, 0, time.UTC)})
		}},
		{"read", false, func(b *Beads, ctx context.Context) error {
			_, err := b.ReadCostSamples(ctx, SampleKindClaudePlan, time.Time{}, 1)
			return err
		}},
		{"write (plain)", true, func(b *Beads, ctx context.Context) error {
			return b.WriteCostSample(ctx, Sample{Kind: SampleKindClaudePlan, At: time.Date(2026, 11, 15, 22, 0, 0, 0, time.UTC)})
		}},
		{"read (plain)", true, func(b *Beads, ctx context.Context) error {
			_, err := b.ReadCostSamples(ctx, SampleKindClaudePlan, time.Time{}, 1)
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var sawCanceled bool
			run := func(ctx context.Context, args []string) ([]byte, []byte, error) {
				switch argv := strings.Join(args, " "); argv {
				case "--allow-stale version":
					return nil, []byte("Error: unknown flag: --allow-stale\n"), nil
				case "--help":
					return []byte(bdHelpWithSampleVerb), nil, nil
				}
				sawCanceled = ctx.Err() != nil
				return nil, nil, ctx.Err()
			}
			var b *Beads
			if tc.plain {
				b = NewPlain(t.TempDir(), nil, WithRunner(run))
			} else {
				b = newBeads(beadsFields{
					workDir:  t.TempDir(),
					isolated: true,
					noRoute:  true,
					exec: func(ctx context.Context, c bdCall) ([]byte, []byte, error) {
						return run(ctx, c.args)
					},
				})
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if err := tc.call(b, ctx); !errors.Is(err, context.Canceled) {
				t.Fatalf("error = %v, want the caller's context.Canceled", err)
			}
			if !sawCanceled {
				t.Error("the bd call's context was not canceled with the caller's")
			}
		})
	}
}

// TestCostSampleReadRefusesUnreadableRows: a row under another kind, a payload
// that is not an object, a timestamp that does not parse and output that is
// not a row array are errors, never rows or an empty series.
func TestCostSampleReadRefusesUnreadableRows(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		out  string
		want string
	}{
		{"another kind", `[{"kind":"deepseek_balance","at":"2026-11-15T22:00:00Z","payload":{}}]`, `kind "deepseek_balance", want "claude_plan"`},
		{"payload string", `[{"kind":"claude_plan","at":"2026-11-15T22:00:00Z","payload":"{}"}]`, "payload is not a JSON object"},
		{"payload array", `[{"kind":"claude_plan","at":"2026-11-15T22:00:00Z","payload":[]}]`, "payload is not a JSON object"},
		{"unparseable resets at", `[{"kind":"claude_plan","at":"2026-11-15T22:00:00Z","payload":{"five_hour_resets_at":"soon"}}]`, "five_hour_resets_at"},
		{"unparseable seven-day resets at", `[{"kind":"claude_plan","at":"2026-11-15T22:00:00Z","payload":{"seven_day_resets_at":"soon"}}]`, "seven_day_resets_at"},
		{"row field of the wrong type", `[{"kind":1,"at":"2026-11-15T22:00:00Z","payload":{}}]`, "decode rows"},
		{"payload field of the wrong type", `[{"kind":"claude_plan","at":"2026-11-15T22:00:00Z","payload":{"total":"abc"}}]`, "decode payload"},
		{"not an array", `{"samples":[]}`, "decode rows"},
		{"null output", `null`, "decode rows"},
		{"empty output", ``, "decode rows"},
		{"row without at", `[{"kind":"claude_plan","payload":{}}]`, "has no at"},
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
			got, err := b.ReadCostSamples(context.Background(), SampleKindClaudePlan, time.Time{}, 1)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ReadCostSamples = (%v, %v), want an error naming %q", got, err, tc.want)
			}
		})
	}
}

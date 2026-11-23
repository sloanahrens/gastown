package townhealth

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// staticSweep is a tier sweep record the test sets; a nil source is no source.
type staticSweep struct{ rec SweepRecord }

func (s staticSweep) Sweep() (SweepRecord, error) { return s.rec, nil }

// errorSweep is a sweep source whose read fails.
type errorSweep struct{ err error }

func (s errorSweep) Sweep() (SweepRecord, error) { return SweepRecord{}, s.err }

// The sweep field is the operator's answer to "are the tier sweeps still
// green": RED after two red cycles in a row or three hours with no fully green
// sweep, GREEN otherwise. A record that could not be read, one the daemon has
// not filled, and one with no green sweep time are UNKNOWN, never GREEN: a
// sweep nobody can see is exactly the failure the field exists to catch
// (gt-82qa3x).
func TestHealthSweep(t *testing.T) {
	t.Parallel()
	recent := ago(time.Hour)
	boom := errors.New("tier-sweep: record unreadable")
	for _, tc := range []struct {
		name    string
		src     Sweep
		tag     Tag
		verdict Verdict
		value   string
		detail  string
	}{
		{"two red cycles in a row", staticSweep{SweepRecord{Known: true, RedCycles: 2, LastGreenAt: recent}}, Recorded, Red, "red", "2 red cycles in a row"},
		{"one red cycle", staticSweep{SweepRecord{Known: true, RedCycles: 1, LastGreenAt: recent}}, Recorded, Green, "ok", ""},
		{"three hours with no fully green sweep", staticSweep{SweepRecord{Known: true, LastGreenAt: ago(DefaultSweepGreenStale)}}, Recorded, Red, "red", "no green sweep in 3h"},
		{"a green sweep a minute inside the stale limit", staticSweep{SweepRecord{Known: true, LastGreenAt: ago(DefaultSweepGreenStale - time.Minute)}}, Recorded, Green, "ok", ""},
		{"one red cycle does not excuse a stale green sweep", staticSweep{SweepRecord{Known: true, RedCycles: 1, LastGreenAt: ago(4 * time.Hour)}}, Recorded, Red, "red", "no green sweep in 4h"},
		{"an unknown record is never green, even when it counts red cycles", staticSweep{SweepRecord{Known: false, RedCycles: 5, LastGreenAt: recent}}, Unknown, VerdictUnknown, "?", "the sweep record is unknown"},
		{"a known record with no green sweep time", staticSweep{SweepRecord{Known: true, RedCycles: 1}}, Unknown, VerdictUnknown, "?", "no green sweep time recorded"},
		{"a failed read carries the error", errorSweep{boom}, Unknown, VerdictUnknown, "?", boom.Error()},
		{"no source wired adds no field", nil, "", "", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			in := inputs(healthy())
			in.Sweep = tc.src
			r := Compute(context.Background(), in)
			if tc.src == nil {
				for _, f := range r.Fields {
					if f.Name == FieldSweep {
						t.Fatalf("an unwired sweep source still claimed a %s field: %+v", FieldSweep, f)
					}
				}
				if r.Verdict != Green {
					t.Errorf("verdict = %s, want green: an unwired sweep source changed the town's verdict", r.Verdict)
				}
				return
			}
			got := field(t, r, FieldSweep)
			if got.Verdict != tc.verdict || got.Value != tc.value || got.Tag != tc.tag {
				t.Errorf("sweep = %+v, want %s %q %s", got, tc.verdict, tc.value, tc.tag)
			}
			if !strings.Contains(got.Detail, tc.detail) {
				t.Errorf("sweep detail = %q, want it to carry %q", got.Detail, tc.detail)
			}
		})
	}
}

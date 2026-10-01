package townhealth

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type fakeSteward struct {
	snap StewardSnapshot
	err  error
}

func (f fakeSteward) Steward(context.Context) (StewardSnapshot, error) { return f.snap, f.err }

func stewardTown(snap StewardSnapshot, err error) Report {
	in := inputs(healthy())
	in.Steward = fakeSteward{snap, err}
	return Compute(context.Background(), in)
}

func counters(running, stuck, jobs, attempted, broke int) StewardSnapshot {
	return StewardSnapshot{Enabled: true, Counters: StewardCounters{Window: "1h", Running: running, Stuck: stuck, Jobs: jobs, Finished: attempted, Attempted: attempted, Broke: broke, Pro: 1}}
}

func TestStewardFieldJudgesStuckAndErrorRate(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		snap    StewardSnapshot
		want    Verdict
		value   string
		counted bool
	}{
		{"idle", counters(0, 0, 0, 0, 0), Green, "0 running, 0 jobs/1h", true},
		{"healthy", counters(1, 0, 5, 4, 1), Green, "1 running, 5 jobs/1h", true},
		{"rate over threshold", counters(0, 0, 4, 4, 3), Degraded, "3/4 failed", true},
		{"rate at threshold is not over", counters(0, 0, 4, 4, 2), Green, "0 running, 4 jobs/1h", true},
		{"too few attempts to judge", counters(0, 0, 2, 2, 2), Green, "0 running, 2 jobs/1h", true},
		{"stuck outranks the rate", counters(1, 1, 4, 3, 3), Red, "1 stuck", true},
		{"steward off adds nothing", StewardSnapshot{}, Green, "", false},
	} {
		r := stewardTown(tc.snap, nil)
		if !tc.counted {
			for _, f := range r.Fields {
				if f.Name == FieldSteward {
					t.Errorf("%s: got a steward field %+v for a town without one", tc.name, f)
				}
			}
			if r.Steward != nil {
				t.Errorf("%s: counters = %+v, want none", tc.name, r.Steward)
			}
			continue
		}
		f := field(t, r, "steward")
		if f.Verdict != tc.want || f.Value != tc.value || f.Tag != Recorded {
			t.Errorf("%s: field = %+v, want %s %q RECORDED", tc.name, f, tc.want, tc.value)
		}
		if r.Steward == nil || r.Steward.Jobs != tc.snap.Counters.Jobs {
			t.Errorf("%s: report counters = %+v", tc.name, r.Steward)
		}
		if r.Verdict != tc.want {
			t.Errorf("%s: town verdict = %s, want %s", tc.name, r.Verdict, tc.want)
		}
	}
}

func TestStewardFieldUnknownWhenLedgerUnreadable(t *testing.T) {
	t.Parallel()
	r := stewardTown(StewardSnapshot{}, errors.New("jobs.jsonl: permission denied"))
	f := field(t, r, "steward")
	if f.Tag != Unknown || f.Verdict != VerdictUnknown || !strings.Contains(f.Detail, "permission denied") {
		t.Errorf("field = %+v, want UNKNOWN carrying the error", f)
	}
	if r.Steward != nil {
		t.Errorf("counters = %+v, want none: an unread ledger is not zero jobs", r.Steward)
	}
}

func TestStewardStuckSurfacesInTheLine(t *testing.T) {
	t.Parallel()
	r := stewardTown(counters(1, 1, 3, 2, 0), nil)
	line := Line(r, now, DefaultStaleAfter)
	if !strings.Contains(line, "steward=1_stuck") {
		t.Errorf("line = %q, want steward=1_stuck", line)
	}
}

func TestStewardSettings(t *testing.T) {
	t.Parallel()
	th, _, err := (&Settings{StewardErrorRate: 0.25, StewardMinJobs: 5}).Resolve()
	if err != nil || th.StewardErrorRate != 0.25 || th.StewardMinJobs != 5 {
		t.Fatalf("resolved %+v %v", th, err)
	}
	for _, bad := range []Settings{{StewardErrorRate: 1.5}, {StewardErrorRate: -0.1}, {StewardMinJobs: -1}} {
		if _, _, err := bad.Resolve(); err == nil {
			t.Errorf("Resolve(%+v) succeeded, want an error", bad)
		}
	}
}

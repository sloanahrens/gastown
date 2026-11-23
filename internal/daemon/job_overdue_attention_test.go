package daemon

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/attention"
)

// jobOverdueNow is the tick every collection in these tests judges against, so
// no test reads the wall clock.
var jobOverdueNow = time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)

// jobOverdueItems runs the job-overdue collector once over readings.
func jobOverdueItems(t *testing.T, f *attentionFixture, readings []JobReading) []attention.Item {
	t.Helper()
	f.src.jobList = func(time.Time) ([]JobReading, error) { return readings, nil }
	c := collectorFor(t, f.src, attention.KindJobOverdue)
	items, err := c.collect(context.Background())
	if err != nil {
		t.Fatalf("collector: %v", err)
	}
	return items
}

// An overdue job raises one item keyed by its name and naming its last run age
// and its cadence; the same job on the next tick, run again, raises none.
func TestJobOverdueAttention(t *testing.T) {
	t.Parallel()
	f := newAttentionFixture(t, jobOverdueNow)

	overdue := JobReading{Name: "cache_trim", State: "overdue", LastRun: jobOverdueNow.Add(-3 * time.Hour), Cadence: time.Hour}
	items := jobOverdueItems(t, f, []JobReading{
		overdue,
		{Name: "dolt_backup", State: "ok", LastRun: jobOverdueNow.Add(-time.Hour), Cadence: 24 * time.Hour},
	})
	if len(items) != 1 {
		t.Fatalf("items = %+v, want one for the overdue job only", items)
	}
	it := items[0]
	if it.Key != "job-overdue:cache_trim" || it.Kind != attention.KindJobOverdue || it.Severity != attention.SeverityLow {
		t.Errorf("item = %+v, want a low job-overdue item keyed job-overdue:cache_trim", it)
	}
	for _, want := range []string{"cache_trim", "3h0m", "1h0m0s"} {
		if !strings.Contains(it.Summary, want) {
			t.Errorf("summary %q does not name %q", it.Summary, want)
		}
	}

	ranAgain := overdue
	ranAgain.State = "ok"
	ranAgain.LastRun = jobOverdueNow.Add(-time.Minute)
	if items := jobOverdueItems(t, f, []JobReading{ranAgain}); len(items) != 0 {
		t.Errorf("a job that ran again still raises %+v, want the item cleared", items)
	}
}

// The wake key is raised only for the jobs of jobOverdueWakeJobs, and only
// once now is more than twice their cadence past their last run: one test row
// per entry of that table, each resolving through attention.RuleFor to
// job.overdue_wake. Every other key resolves to the info rule (gt-obe5qp).
func TestJobOverdueWake(t *testing.T) {
	t.Parallel()

	// The table is pinned whole: a job added to or removed from it needs this
	// list and its row changed together, or the job silently loses the wake
	// key and the overseer is never woken (gt-eajich).
	if got, want := slices.Sorted(maps.Keys(jobOverdueWakeJobs)), []string{"dolt_backup", "dolt_gc"}; !slices.Equal(got, want) {
		t.Fatalf("jobOverdueWakeJobs names %q, want %q: a job added to or removed from the table needs this list and its row changed together", got, want)
	}

	// The row per entry of the wake table, at the cadence each job runs at:
	// dolt_gc is the weekly maintenance gc and dolt_backup the nightly one.
	wakeRows := map[string]struct {
		cadence time.Duration
		age     time.Duration
	}{
		"dolt_gc":     {cadence: MaintenanceGCCadence, age: 3 * MaintenanceGCCadence},
		"dolt_backup": {cadence: 24 * time.Hour, age: 3 * 24 * time.Hour},
	}
	for name := range jobOverdueWakeJobs {
		row, ok := wakeRows[name]
		if !ok {
			t.Fatalf("jobOverdueWakeJobs names %q with no test row", name)
		}
		t.Run("wake/"+name, func(t *testing.T) {
			t.Parallel()
			f := newAttentionFixture(t, jobOverdueNow)
			items := jobOverdueItems(t, f, []JobReading{{
				Name:    name,
				State:   jobStateOverdue,
				LastRun: jobOverdueNow.Add(-row.age),
				Cadence: row.cadence,
			}})
			if len(items) != 1 {
				t.Fatalf("items = %+v, want one for %s", items, name)
			}
			if got, want := items[0].Key, jobOverdueWakePrefix+name; got != want {
				t.Errorf("key = %q, want the wake key %q", got, want)
			}
			if rule := attention.RuleFor(items[0]); rule != attention.RuleJobOverdueWake {
				t.Errorf("RuleFor(%q) = %q, want %q", items[0].Key, rule, attention.RuleJobOverdueWake)
			}
		})
	}

	for _, tc := range []struct {
		name    string
		reading JobReading
	}{
		{
			// Overdue, but the last run is an hour short of two cadences.
			name:    "waking job before two cadences",
			reading: JobReading{Name: "dolt_gc", State: jobStateOverdue, LastRun: jobOverdueNow.Add(-(2*MaintenanceGCCadence - time.Hour)), Cadence: MaintenanceGCCadence},
		},
		{
			// Exactly two cadences is not "more than twice".
			name:    "waking job at exactly two cadences",
			reading: JobReading{Name: "dolt_backup", State: jobStateOverdue, LastRun: jobOverdueNow.Add(-48 * time.Hour), Cadence: 24 * time.Hour},
		},
		{
			// A cadence that says nothing proves no missed cadence: the key
			// stays the info one however old the last run.
			name:    "waking job with no cadence",
			reading: JobReading{Name: "dolt_gc", State: jobStateOverdue, LastRun: jobOverdueNow.Add(-100 * time.Hour)},
		},
		{
			// A job outside the wake table keeps the info key however late.
			name:    "other job past two cadences",
			reading: JobReading{Name: "cache_trim", State: jobStateOverdue, LastRun: jobOverdueNow.Add(-100 * time.Hour), Cadence: time.Hour},
		},
	} {
		t.Run("info/"+tc.name, func(t *testing.T) {
			t.Parallel()
			f := newAttentionFixture(t, jobOverdueNow)
			items := jobOverdueItems(t, f, []JobReading{tc.reading})
			if len(items) != 1 {
				t.Fatalf("items = %+v, want one for %s", items, tc.reading.Name)
			}
			if got, want := items[0].Key, "job-overdue:"+tc.reading.Name; got != want {
				t.Errorf("key = %q, want the info key %q", got, want)
			}
			if rule := attention.RuleFor(items[0]); rule != attention.RuleJobOverdue {
				t.Errorf("RuleFor(%q) = %q, want %q", items[0].Key, rule, attention.RuleJobOverdue)
			}
		})
	}
}

// A job whose newest outcome failed raises one item even when it is not
// overdue; a job both overdue and failed raises one item, not two; a later ok
// outcome clears it, and a row neither overdue nor failed raises nothing.
func TestJobOverdueAttentionFailed(t *testing.T) {
	t.Parallel()
	f := newAttentionFixture(t, jobOverdueNow)
	failed := JobReading{Name: "cache_trim", State: "ok", Outcome: "failed", LastRun: jobOverdueNow.Add(-2 * time.Hour), Cadence: 24 * time.Hour}
	for _, tc := range []struct {
		name    string
		reading JobReading
		want    int
	}{
		{"failed not overdue", failed, 1},
		{"overdue and failed", JobReading{Name: "cache_trim", State: jobStateOverdue, Outcome: "failed", LastRun: jobOverdueNow.Add(-3 * time.Hour), Cadence: 24 * time.Hour}, 1},
		{"ok outcome", JobReading{Name: "cache_trim", State: "ok", Outcome: "ok", LastRun: jobOverdueNow.Add(-time.Minute), Cadence: 24 * time.Hour}, 0},
		{"refused outcome", JobReading{Name: "cache_trim", State: "ok", Outcome: "refused", LastRun: jobOverdueNow.Add(-time.Minute), Cadence: 24 * time.Hour}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			items := jobOverdueItems(t, f, []JobReading{tc.reading})
			if len(items) != tc.want {
				t.Fatalf("items = %+v, want %d", items, tc.want)
			}
			if tc.want == 1 && (items[0].Key != "job-overdue:cache_trim" || items[0].Kind != attention.KindJobOverdue || items[0].Severity != attention.SeverityLow) {
				t.Errorf("item = %+v, want a low job-overdue item keyed job-overdue:cache_trim", items[0])
			}
		})
	}

	// The failed text says the job failed and when it ran; a failed row with no
	// run time says so rather than naming a bogus age.
	for _, tc := range []struct {
		reading JobReading
		want    string
	}{
		{failed, "2h0m"},
		{JobReading{Name: "cache_trim", State: "unknown", Outcome: jobOutcomeFailed}, "no run time recorded"},
	} {
		items := jobOverdueItems(t, f, []JobReading{tc.reading})
		if len(items) != 1 || !strings.Contains(items[0].Summary, "failed") || !strings.Contains(items[0].Summary, tc.want) {
			t.Errorf("items = %+v, want one summary naming failed and %q", items, tc.want)
		}
	}
}

// With no list source wired the collector raises nothing and no error: only
// the daemon start code's wiring (gt-poy10l) can supply the list.
func TestJobOverdueAttentionSource(t *testing.T) {
	t.Parallel()
	f := newAttentionFixture(t, jobOverdueNow)
	c := collectorFor(t, f.src, attention.KindJobOverdue)
	if items, err := c.collect(context.Background()); err != nil || len(items) != 0 {
		t.Errorf("no source: items %+v, err %v, want neither", items, err)
	}
}

// An unreadable list raises one item keyed job-overdue:list naming the read
// failure, never a list with no overdue jobs, and the item clears on the tick
// the list reads again. The states that raise nothing are named rows, each
// with the reason it is silent (gt-1ytq9f).
func TestJobOverdueAttentionRefusals(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		reading JobReading
	}{
		{"no_cadence", JobReading{Name: "cache_trim", State: jobStateNoCadence}},
		{"never_run on a disabled patrol", JobReading{Name: "cloud_check", State: jobStateNeverRun, Cadence: time.Hour}},
		{"unknown with an absent source", JobReading{Name: "dolt_backup", State: jobStateUnknown, Cadence: 24 * time.Hour}},
	} {
		t.Run("silent/"+tc.name, func(t *testing.T) {
			t.Parallel()
			f := newAttentionFixture(t, jobOverdueNow)
			if items := jobOverdueItems(t, f, []JobReading{tc.reading}); len(items) != 0 {
				t.Errorf("items = %+v, want none: %s raises nothing", items, tc.name)
			}
		})
	}

	t.Run("unreadable list", func(t *testing.T) {
		t.Parallel()
		boom := errors.New("patrol config: unexpected end of JSON")
		f := newAttentionFixture(t, jobOverdueNow)
		f.src.jobList = func(time.Time) ([]JobReading, error) { return nil, boom }
		c := collectorFor(t, f.src, attention.KindJobOverdue)
		items, err := c.collect(context.Background())
		if err != nil {
			t.Fatalf("collector: %v, a failed read is the item", err)
		}
		if len(items) != 1 {
			t.Fatalf("items = %+v, want one for the unreadable list", items)
		}
		it := items[0]
		if it.Key != jobOverdueListKey || it.Kind != attention.KindJobOverdue || it.Severity != attention.SeverityLow {
			t.Errorf("item = %+v, want a low job-overdue item keyed %q", it, jobOverdueListKey)
		}
		for _, want := range []string{"cannot be read", boom.Error()} {
			if !strings.Contains(it.Summary, want) {
				t.Errorf("summary %q does not name %q", it.Summary, want)
			}
		}

		// The next tick reads the list again, so the queue replaces the item.
		f.src.jobList = func(time.Time) ([]JobReading, error) { return nil, nil }
		next := collectAttention(context.Background(), attention.State{Items: items},
			[]attentionCollector{c}, func(string, ...any) {})
		if got, ok := attention.Find(attention.State{Items: next}, jobOverdueListKey); ok {
			t.Errorf("the list item survived a readable tick: %+v", got)
		}
	})
}

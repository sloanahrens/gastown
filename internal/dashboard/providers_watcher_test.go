package dashboard

import (
	"encoding/json"
	"slices"
	"testing"
	"time"
)

// watcherQuietReport is a spend report that is otherwise readable and whose live
// read succeeded, so the only warnings it can draw are the watcher's. An empty
// sample leaves last_sample out of the report.
func watcherQuietReport(t *testing.T, sample string, watcherPID any) json.RawMessage {
	t.Helper()
	report := map[string]any{
		"balance":     40.0,
		"live":        map[string]any{"ok": true, "available": true},
		"watcher_pid": watcherPID,
	}
	if sample != "" {
		report["last_sample"] = sample
	}
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// The DeepSeek row warns from the ledger's age, read at the clock it is given:
// the pod cannot see the watcher, so the watcher's pid never decides the line.
func TestDeepSeekWatcherQuietFromTheLedger(t *testing.T) {
	t.Parallel()

	central := time.FixedZone("CST", -6*60*60)
	for _, tc := range []struct {
		name   string
		now    time.Time
		sample string
		want   []string
	}{
		{"six minutes old is fresh", costProvidersNow, "2026-10-29T11:54:00", nil},
		{"forty minutes old is quiet", costProvidersNow, "2026-10-29T11:20:00", []string{"spend watcher quiet for 40 min"}},
		{"fifteen minutes old is not yet quiet", costProvidersNow, "2026-10-29T11:45:00", nil},
		{"sixteen minutes old is quiet", costProvidersNow, "2026-10-29T11:44:00", []string{"spend watcher quiet for 16 min"}},
		{"15 min 59 s old is quiet for fifteen whole minutes", costProvidersNow, "2026-10-29T11:44:01", []string{"spend watcher quiet for 15 min"}},
		{"seconds past the minute do not count", costProvidersNow, "2026-10-29T11:19:30", []string{"spend watcher quiet for 40 min"}},
		{"the minute-stamp the report writes", costProvidersNow, "2026-10-29T11:44", []string{"spend watcher quiet for 16 min"}},
		{"the stamp is read in now's own zone", costProvidersNow.In(central), "2026-10-29T05:54:00", nil},
		{"no sample", costProvidersNow, "", []string{"spend ledger has no sample"}},
		{"an unparsable sample", costProvidersNow, "yesterday at noon", []string{"spend ledger has no sample"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			for _, pid := range []any{nil, 4242} {
				p := deepSeekCostProvider(tc.now, watcherQuietReport(t, tc.sample, pid), nil, nil, nil)
				if !slices.Equal(p.Warnings, tc.want) {
					t.Errorf("sample %q at %v with watcher_pid %v gives warnings %q, want %q",
						tc.sample, tc.now, pid, p.Warnings, tc.want)
				}
			}
		})
	}
}

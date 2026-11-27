package attention

import "testing"

// TestUpdateMaintenanceItem pins the item the roll raises when the maintenance
// it runs first does not finish: none for an empty outcome or done, and one
// info item per other outcome, keyed by that outcome and naming it
// (gt-1mqpcb).
func TestUpdateMaintenanceItem(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		outcome string
		want    bool
		summary string
	}{
		{"empty outcome", "", false, ""},
		{"done outcome", "done", false, ""},
		{"timed out outcome", "timed out", true,
			"maintenance before the update roll did not finish: timed out"},
		{"deferred outcome", "deferred", true,
			"maintenance before the update roll did not finish: deferred"},
		{"failed outcome", "failed", true,
			"maintenance before the update roll did not finish: failed"},
		{"an outcome the builder does not know", "partial", true,
			"maintenance before the update roll did not finish: partial"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			item, ok := UpdateMaintenanceItem(tc.outcome)
			if ok != tc.want {
				t.Fatalf("UpdateMaintenanceItem(%q) reports %v, want %v", tc.outcome, ok, tc.want)
			}
			if !tc.want {
				if item != (Item{}) {
					t.Errorf("item = %+v, want the zero Item when no item is raised", item)
				}
				return
			}
			if want := "update-maintenance:" + tc.outcome; item.Key != want {
				t.Errorf("Key = %q, want %q", item.Key, want)
			}
			if item.Kind != KindUpdateMaintenance {
				t.Errorf("Kind = %q, want %q", item.Kind, KindUpdateMaintenance)
			}
			if f, found := Flags(RuleFor(item)); !found || f.Handled != HandledInfo || f.Wake {
				t.Errorf("Flags(RuleFor(item)) = %+v (found %v), want an info record that wakes no one", f, found)
			}
			if item.Severity != SeverityLow {
				t.Errorf("Severity = %q, want %q for an info record", item.Severity, SeverityLow)
			}
			if item.Summary != tc.summary {
				t.Errorf("Summary = %q, want %q", item.Summary, tc.summary)
			}
		})
	}
}

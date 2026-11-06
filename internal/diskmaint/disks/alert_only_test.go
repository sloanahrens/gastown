package disks

import (
	"errors"
	"strings"
	"testing"
)

// TestEvaluateAppendsConsumersToTheRaiseOnly: the biggest consumers ride on the
// usage raise, and no other alert carries them.
func TestEvaluateAppendsConsumersToTheRaiseOnly(t *testing.T) {
	t.Parallel()
	const consumers = "forgejo/forgejo-runner 61 GB"

	raise := oneAlert(t, Evaluate([]Reading{{Name: "control", UsedPct: 81, Detail: consumers}}, nil))
	if !strings.HasSuffix(raise.Detail, "; biggest consumers: "+consumers) {
		t.Fatalf("raise detail = %q, want it to end with the consumers", raise.Detail)
	}

	cleared := oneAlert(t, Evaluate([]Reading{{Name: "control", UsedPct: 65, Detail: consumers}}, activeKeys("diskmaint:control")))
	if !cleared.Clear || strings.Contains(cleared.Detail, consumers) {
		t.Fatalf("clear = %+v, want a clear without the consumers", cleared)
	}

	unreadable := oneAlert(t, Evaluate([]Reading{{Name: "control", Err: errors.New("no node"), Detail: consumers}}, nil))
	if strings.Contains(unreadable.Detail, consumers) {
		t.Fatalf("unreadable detail = %q, want no consumers", unreadable.Detail)
	}
}

// TestKeepForIgnoresAlertOnlyReadings: an alert-only reading that is unreadable or
// full changes neither the refusal nor the tightening; the same reading without
// the mark still does both.
func TestKeepForIgnoresAlertOnlyReadings(t *testing.T) {
	t.Parallel()
	healthy := Reading{Name: "colima", UsedPct: 40}
	cases := map[string]struct {
		control     Reading
		wantKeep    int
		wantRefusal bool
	}{
		"an unreadable control disk": {
			control:  Reading{Name: "control", Err: errors.New("no node"), AlertOnly: true},
			wantKeep: 9,
		},
		"a full control disk": {
			control:  Reading{Name: "control", UsedPct: 95, AlertOnly: true},
			wantKeep: 9,
		},
		"an unreadable sandbox disk refuses": {
			control:     Reading{Name: "control", Err: errors.New("no node")},
			wantRefusal: true,
		},
		"a full sandbox disk tightens": {
			control:  Reading{Name: "control", UsedPct: 95},
			wantKeep: tightenedKeep,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			keep, err := KeepFor([]Reading{healthy, tc.control}, 9).Count()
			if tc.wantRefusal {
				if err == nil {
					t.Fatalf("KeepFor = %d, want a refusal", keep)
				}
				return
			}
			if err != nil {
				t.Fatalf("KeepFor refused: %v", err)
			}
			if keep != tc.wantKeep {
				t.Fatalf("KeepFor = %d, want %d", keep, tc.wantKeep)
			}
		})
	}
}

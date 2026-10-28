package disks

import (
	"errors"
	"strings"
	"testing"
)

func TestParseDFPortableOutput(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		in   string
		want float64
	}{
		"the plan's sample": {in: "overlay 131787236 72250308 52809712 58% /\n", want: 58},
		"linux -P output": {in: "Filesystem 1024-blocks Used Available Capacity Mounted on\n" +
			"overlay 131787236 72250308 52809712 58% /\n", want: 58},
		"colima data disk": {in: "Filesystem 1024-blocks Used Available Capacity Mounted on\n" +
			"/dev/vdb 1000000 910000 90000 91% /mnt/lima-colima-gt-sbx\n", want: 91},
		// Without -P, macOS adds an iused/%iused pair: the capacity column is
		// still the first field carrying a percent, the iused share is not.
		"macOS without -P": {in: "Filesystem 512-blocks Used Available Capacity iused ifree %iused Mounted on\n" +
			"/dev/disk3s1 1000 850 150 85% 100 200 3% /\n", want: 85},
		"a full disk": {in: "overlay 1 1 0 100% /\n", want: 100},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := ParseDF(tc.in)
			if err != nil {
				t.Fatalf("ParseDF(%q) = error %v, want %v", tc.in, err, tc.want)
			}
			if got != tc.want {
				t.Fatalf("ParseDF(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestParseDFRejectsGarbage(t *testing.T) {
	t.Parallel()
	for name, in := range map[string]string{
		"empty output":         "",
		"blank lines":          "\n\n\n",
		"not df output":        "not df output at all\n",
		"header only":          "Filesystem 1024-blocks Used Available Capacity Mounted on\n",
		"unparseable share":    "overlay 1 2 3 lots% /\n",
		"share past a hundred": "overlay 1 2 3 140% /\n",
		"negative share":       "overlay 1 2 3 -5% /\n",
		"nan share":            "overlay 1 2 3 NaN% /\n",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got, err := ParseDF(in); err == nil {
				t.Fatalf("ParseDF(%q) = %v, want an error", in, got)
			}
		})
	}
}

// activeKeys builds the alert store the daemon hands Evaluate.
func activeKeys(keys ...string) map[string]bool {
	m := make(map[string]bool, len(keys))
	for _, k := range keys {
		m[k] = true
	}
	return m
}

func oneAlert(t *testing.T, alerts []Alert) Alert {
	t.Helper()
	if len(alerts) != 1 {
		t.Fatalf("Evaluate returned %d alerts (%+v), want exactly 1", len(alerts), alerts)
	}
	if alerts[0].Detail == "" {
		t.Fatalf("alert %q has no Detail", alerts[0].Key)
	}
	return alerts[0]
}

func TestEvaluateRaisesAtEighty(t *testing.T) {
	t.Parallel()
	a := oneAlert(t, Evaluate([]Reading{{Name: "mac", UsedPct: 80}}, nil))
	if a.Key != "diskmaint:mac" || !a.Raise || a.Clear {
		t.Fatalf("alert = %+v, want Key diskmaint:mac with Raise only", a)
	}
}

func TestEvaluateDoesNotRaiseBelowEighty(t *testing.T) {
	t.Parallel()
	for name, pct := range map[string]float64{
		"just below": 79.9,
		"half full":  50,
		"empty":      0,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if alerts := Evaluate([]Reading{{Name: "mac", UsedPct: pct}}, nil); len(alerts) != 0 {
				t.Fatalf("Evaluate at %v%% = %+v, want no alert", pct, alerts)
			}
		})
	}
}

func TestEvaluateDoesNotRaiseTwice(t *testing.T) {
	t.Parallel()
	for name, pct := range map[string]float64{
		"at the threshold": 80,
		"nearly full":      95,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			active := activeKeys("diskmaint:mac")
			if alerts := Evaluate([]Reading{{Name: "mac", UsedPct: pct}}, active); len(alerts) != 0 {
				t.Fatalf("Evaluate at %v%% with the alert active = %+v, want no alert", pct, alerts)
			}
		})
	}
}

func TestEvaluateClearsOnlyBelowSeventy(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		pct   float64
		clear bool
	}{
		"75 percent while active":   {pct: 75},
		"70 percent while active":   {pct: 70},
		"69.9 percent while active": {pct: 69.9, clear: true},
		"empty while active":        {pct: 0, clear: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			alerts := Evaluate([]Reading{{Name: "mac", UsedPct: tc.pct}}, activeKeys("diskmaint:mac"))
			if !tc.clear {
				if len(alerts) != 0 {
					t.Fatalf("Evaluate at %v%% with the alert active = %+v, want neither raise nor clear", tc.pct, alerts)
				}
				return
			}
			a := oneAlert(t, alerts)
			if a.Key != "diskmaint:mac" || !a.Clear || a.Raise {
				t.Fatalf("alert = %+v, want Key diskmaint:mac with Clear only", a)
			}
		})
	}
}

func TestEvaluateUnreadableDiskRaisesItsOwnAlert(t *testing.T) {
	t.Parallel()
	a := oneAlert(t, Evaluate([]Reading{{Name: "colima", Err: errors.New("ssh: i/o timeout")}}, nil))
	if a.Key != "diskmaint:colima:unreadable" || !a.Raise || a.Clear {
		t.Fatalf("alert = %+v, want Key diskmaint:colima:unreadable with Raise only", a)
	}
	if !strings.Contains(a.Detail, "ssh: i/o timeout") {
		t.Fatalf("Detail %q does not name the read failure", a.Detail)
	}
}

func TestEvaluateDoesNotRaiseUnreadableTwice(t *testing.T) {
	t.Parallel()
	active := activeKeys("diskmaint:colima:unreadable")
	if alerts := Evaluate([]Reading{{Name: "colima", Err: errors.New("ssh: i/o timeout")}}, active); len(alerts) != 0 {
		t.Fatalf("Evaluate with the unreadable alert active = %+v, want no alert", alerts)
	}
}

func TestEvaluateUnreadableAlertClearsWhenReadAgain(t *testing.T) {
	t.Parallel()
	a := oneAlert(t, Evaluate([]Reading{{Name: "colima", UsedPct: 50}}, activeKeys("diskmaint:colima:unreadable")))
	if a.Key != "diskmaint:colima:unreadable" || !a.Clear || a.Raise {
		t.Fatalf("alert = %+v, want Key diskmaint:colima:unreadable with Clear only", a)
	}
}

func TestEvaluateUnreadableDoesNotTouchTheUsageAlert(t *testing.T) {
	t.Parallel()
	a := oneAlert(t, Evaluate([]Reading{{Name: "mac", Err: errors.New("df: not found")}}, activeKeys("diskmaint:mac")))
	if a.Key != "diskmaint:mac:unreadable" || !a.Raise {
		t.Fatalf("alert = %+v, want only the unreadable raise; the active usage alert must stay active", a)
	}
}

func TestKeepForTightensOverEighty(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		readings []Reading
		base     int
		want     int
	}{
		"one disk nearly full":      {readings: []Reading{{Name: "mac", UsedPct: 85}}, base: 5, want: 3},
		"one disk at the threshold": {readings: []Reading{{Name: "mac", UsedPct: 80}}, base: 5, want: 3},
		"a second disk is full": {readings: []Reading{
			{Name: "mac", UsedPct: 40},
			{Name: "docker-desktop", UsedPct: 91},
		}, base: 10, want: 3},
		"a base already below the tightened keep stays": {readings: []Reading{{Name: "mac", UsedPct: 90}}, base: 2, want: 2},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := KeepFor(tc.readings, tc.base)
			if err != nil {
				t.Fatalf("KeepFor(%+v, %d) = error %v, want %d", tc.readings, tc.base, err, tc.want)
			}
			if got != tc.want {
				t.Fatalf("KeepFor(%+v, %d) = %d, want %d", tc.readings, tc.base, got, tc.want)
			}
		})
	}
}

func TestKeepForKeepsBaseBelowEighty(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		readings []Reading
		base     int
	}{
		"just below": {readings: []Reading{{Name: "mac", UsedPct: 79.9}}, base: 5},
		"roomy":      {readings: []Reading{{Name: "mac", UsedPct: 40}, {Name: "colima", UsedPct: 10}}, base: 5},
		"no disks":   {readings: nil, base: 7},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := KeepFor(tc.readings, tc.base)
			if err != nil {
				t.Fatalf("KeepFor(%+v, %d) = error %v, want the base %d", tc.readings, tc.base, err, tc.base)
			}
			if got != tc.base {
				t.Fatalf("KeepFor(%+v, %d) = %d, want the base %d", tc.readings, tc.base, got, tc.base)
			}
		})
	}
}

func TestKeepForRefusesUnreadableDisk(t *testing.T) {
	t.Parallel()
	cause := errors.New("ssh: i/o timeout")
	readings := []Reading{
		{Name: "mac", UsedPct: 20},
		{Name: "colima", Err: cause},
	}
	got, err := KeepFor(readings, 5)
	if err == nil {
		t.Fatalf("KeepFor with an unreadable disk = %d, nil; want a refusal", got)
	}
	if !strings.Contains(err.Error(), "colima") {
		t.Fatalf("refusal %q does not name the unreadable disk", err)
	}
	if !errors.Is(err, cause) {
		t.Fatalf("refusal %q does not wrap the read error %v", err, cause)
	}
	if got != 0 {
		t.Fatalf("KeepFor with an unreadable disk = %d, want 0 with the refusal, not a plausible keep", got)
	}
}

func TestKeepForRefusalNamesEveryUnreadableDisk(t *testing.T) {
	t.Parallel()
	readings := []Reading{
		{Name: "mac", Err: errors.New("df: not found")},
		{Name: "colima", Err: errors.New("ssh: i/o timeout")},
	}
	got, err := KeepFor(readings, 5)
	if err == nil {
		t.Fatalf("KeepFor with two unreadable disks = %d, nil; want a refusal", got)
	}
	for _, name := range []string{"mac", "colima"} {
		if !strings.Contains(err.Error(), name) {
			t.Fatalf("refusal %q does not name the unreadable disk %s", err, name)
		}
	}
}

func TestKeepForRefusesWhateverTheOtherDisksSay(t *testing.T) {
	t.Parallel()
	unreadable := Reading{Name: "colima", Err: errors.New("ssh: i/o timeout")}
	full := Reading{Name: "mac", UsedPct: 95}
	for name, readings := range map[string][]Reading{
		"unreadable disk before a full one": {unreadable, full},
		"unreadable disk after a full one":  {full, unreadable},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := KeepFor(readings, 5)
			if err == nil {
				t.Fatalf("KeepFor(%+v) = %d, nil; want a refusal", readings, got)
			}
			if got != 0 {
				t.Fatalf("KeepFor(%+v) = %d, want 0 with the refusal, not the tightened count", readings, got)
			}
		})
	}
}

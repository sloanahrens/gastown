package dashboard

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/diskmaint/growth"
)

// diskNow is the instant the fixtures count from. It sits well before the wall
// clock so a reader that fits against time.Now instead of the clock it is
// handed finds no samples in either window, rather than a stale answer that
// happens to look right.
var diskNow = time.Date(2025, 3, 4, 9, 0, 0, 0, time.UTC)

// diskRingPath is the ring the patrol writes for name under runtimeDir.
func diskRingPath(runtimeDir, name string) string {
	return filepath.Join(runtimeDir, "disk-growth", name+".jsonl")
}

// diskAppend writes one sample to name's ring, failing the test if it cannot.
func diskAppend(t *testing.T, runtimeDir, name string, s growth.Sample) {
	t.Helper()
	if err := growth.Append(diskRingPath(runtimeDir, name), s, 0); err != nil {
		t.Fatalf("appending a sample to the %s ring: %v", name, err)
	}
}

// ReadDisk reports the control node first and the sandbox second, each row
// carrying its ring's last sample rather than an earlier one, and the whole
// percent share rounded to the nearest number.
func TestDiskPanelReadsControlThenSandboxWithTheRoundedShare(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		used    int64
		wantPct int
	}{
		{name: "82.4 rounds down", used: 824, wantPct: 82},
		{name: "82.5 rounds up", used: 825, wantPct: 83},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			// An older sample the row must not report, then the last one.
			diskAppend(t, dir, "control", growth.Sample{At: diskNow.Add(-time.Hour), UsedBytes: 1, TotalBytes: 1000})
			diskAppend(t, dir, "control", growth.Sample{At: diskNow, UsedBytes: tc.used, TotalBytes: 1000})
			diskAppend(t, dir, "sandbox", growth.Sample{At: diskNow, UsedBytes: 500, TotalBytes: 1000})

			d := ReadDisk(dir, diskNow)
			if len(d.Rows) != 2 {
				t.Fatalf("ReadDisk returned %d rows, want two", len(d.Rows))
			}
			control, sandbox := d.Rows[0], d.Rows[1]
			if control.Name != "control" || sandbox.Name != "sandbox" {
				t.Fatalf("ReadDisk returned rows %q then %q, want control then sandbox", control.Name, sandbox.Name)
			}
			if control.UsedBytes != tc.used || control.TotalBytes != 1000 {
				t.Errorf("control row reads %d/%d bytes, want the last sample's %d/1000", control.UsedBytes, control.TotalBytes, tc.used)
			}
			if !control.SampledAt.Equal(diskNow) {
				t.Errorf("control row sampled_at is %v, want the last sample's %v", control.SampledAt, diskNow)
			}
			if control.UsedPct != tc.wantPct {
				t.Errorf("control row used_pct is %d, want %d", control.UsedPct, tc.wantPct)
			}
			if control.NoSamples || control.Err != "" {
				t.Errorf("control row carries no_samples=%t err=%q, want a reading", control.NoSamples, control.Err)
			}
			if sandbox.UsedPct != 50 {
				t.Errorf("sandbox row used_pct is %d, want 50", sandbox.UsedPct)
			}
		})
	}
}

// A ring that is not there, or holds no sample, is a row with NoSamples set; a
// ring the reader cannot read is a row with Err set; and a ring whose last
// sample has no total capacity is an error too rather than a 0% share.
func TestDiskPanelNoSamplesUnreadableAndNoCapacity(t *testing.T) {
	t.Parallel()

	t.Run("missing rings", func(t *testing.T) {
		t.Parallel()
		d := ReadDisk(t.TempDir(), diskNow)
		for _, row := range d.Rows {
			if !row.NoSamples || row.Err != "" {
				t.Errorf("%s row carries no_samples=%t err=%q, want no_samples with no error", row.Name, row.NoSamples, row.Err)
			}
		}
	})

	t.Run("empty ring", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Dir(diskRingPath(dir, "control")), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(diskRingPath(dir, "control"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if row := ReadDisk(dir, diskNow).Rows[0]; !row.NoSamples || row.Err != "" {
			t.Errorf("empty control ring reads no_samples=%t err=%q, want no_samples with no error", row.NoSamples, row.Err)
		}
	})

	t.Run("unreadable ring", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		if err := os.MkdirAll(diskRingPath(dir, "sandbox"), 0o755); err != nil {
			t.Fatal(err)
		}
		if row := ReadDisk(dir, diskNow).Rows[1]; row.Err == "" {
			t.Errorf("a directory in the sandbox ring's place reads no_samples=%t err=%q, want Err set", row.NoSamples, row.Err)
		}
	})

	t.Run("last sample has no capacity", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		diskAppend(t, dir, "control", growth.Sample{At: diskNow, UsedBytes: 0, TotalBytes: 0})
		row := ReadDisk(dir, diskNow).Rows[0]
		if row.Err == "" || row.NoSamples {
			t.Errorf("a zero-total sample reads no_samples=%t err=%q, want Err set", row.NoSamples, row.Err)
		}
	})
}

// The hub holds the rows its Disk reader read, and the state payload carries
// them under disk: the page is handed the reading the reader made.
func TestDiskPanelHubCarriesTheReading(t *testing.T) {
	t.Parallel()

	h := NewHub(Config{Now: func() time.Time { return diskNow }, Disk: func() *Disk {
		return &Disk{Rows: []DiskRow{{Name: "control", UsedBytes: 82, TotalBytes: 100, UsedPct: 82, SampledAt: diskNow}}}
	}})
	h.pollDisk()

	raw, err := json.Marshal(h.State())
	if err != nil {
		t.Fatal(err)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		t.Fatal(err)
	}
	if _, ok := keys["disk"]; !ok {
		t.Fatalf("the state payload has no disk key: %s", raw)
	}
	var st State
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatal(err)
	}
	if st.Disk == nil || len(st.Disk.Rows) != 1 {
		t.Fatalf("the state payload holds %+v, want the one row the reader read", st.Disk)
	}
	if row := st.Disk.Rows[0]; row.Name != "control" || row.UsedPct != 82 {
		t.Errorf("the row in the payload reads %+v, want the control node's 82%%", row)
	}
}

// A reader that hands back nothing changes nothing: the field stays out when
// there was no earlier reading, and an earlier reading survives a nil poll.
func TestDiskPanelHubNilReaderLeavesTheStateAlone(t *testing.T) {
	t.Parallel()

	t.Run("no earlier reading", func(t *testing.T) {
		t.Parallel()
		h := NewHub(Config{Now: func() time.Time { return diskNow }, Disk: func() *Disk { return nil }})
		h.pollDisk()

		raw, err := json.Marshal(h.State())
		if err != nil {
			t.Fatal(err)
		}
		var keys map[string]json.RawMessage
		if err := json.Unmarshal(raw, &keys); err != nil {
			t.Fatal(err)
		}
		if _, ok := keys["disk"]; ok {
			t.Errorf("a nil reader value put disk in the state payload: %s", raw)
		}
	})

	t.Run("earlier reading survives", func(t *testing.T) {
		t.Parallel()
		read := func() *Disk {
			return &Disk{Rows: []DiskRow{{Name: "control", UsedPct: 82}}}
		}
		h := NewHub(Config{Now: func() time.Time { return diskNow }, Disk: func() *Disk { return read() }})
		h.pollDisk()

		read = func() *Disk { return nil }
		h.pollDisk()

		var st State
		mustUnmarshalState(t, h, &st)
		if st.Disk == nil || len(st.Disk.Rows) != 1 || st.Disk.Rows[0].UsedPct != 82 {
			t.Errorf("the payload holds %+v after a nil poll, want the earlier reading kept", st.Disk)
		}
	})
}

// diskHourlyRing writes name's ring with one sample an hour for the week ending
// at diskNow, used given by used(k) for the kth hour, and returns the samples
// written.
func diskHourlyRing(t *testing.T, runtimeDir, name string, used func(k int) int64) []growth.Sample {
	t.Helper()
	const total = int64(500) << 30
	samples := make([]growth.Sample, 0, 169)
	for k := 0; k <= 168; k++ {
		s := growth.Sample{
			At:         diskNow.Add(-time.Duration(168-k) * time.Hour),
			UsedBytes:  used(k),
			TotalBytes: total,
		}
		diskAppend(t, runtimeDir, name, s)
		samples = append(samples, s)
	}
	return samples
}

// ReadDisk fits the two rates the line quotes over the same samples it read the
// last one from — the 24 hours and the week ending at now — and projects the
// days left at the week's pace. A half GiB an hour is 12 GiB a day in both
// windows, and 384 of 500 GiB used leaves 116 GiB, so about 9.7 days.
func TestDiskGrowthIsFittedOverTheDayAndTheWeek(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	const halfGiB = int64(1) << 29
	diskHourlyRing(t, dir, "control", func(k int) int64 { return int64(300)<<30 + int64(k)*halfGiB })

	row := ReadDisk(dir, diskNow).Rows[0]
	if !row.GrowthOK {
		t.Fatalf("control row carries growth_ok=%t on a week of hourly samples", row.GrowthOK)
	}
	for _, c := range []struct {
		name string
		got  float64
	}{
		{"gib_per_day_24h", row.GibPerDay24h},
		{"gib_per_day_7d", row.GibPerDay7d},
	} {
		if math.Abs(c.got-12) > 1e-6 {
			t.Errorf("control row %s is %v, want 12 GiB/day", c.name, c.got)
		}
	}
	if !row.DaysLeftOK {
		t.Fatalf("control row carries days_left_ok=%t at a positive rate", row.DaysLeftOK)
	}
	if want := 116.0 / 12; math.Abs(row.DaysLeft-want) > 1e-6 {
		t.Errorf("control row days_left is %v, want %v", row.DaysLeft, want)
	}
}

// The day's rate and the week's are different figures over a ring whose last
// day is busier than the week before it: a flat six days, then 2 GiB an hour
// for the last day. The day window fits that burst alone, 48 GiB a day, and the
// projection runs at the week's fit the reader put in gib_per_day_7d, not the
// day's.
func TestDiskGrowthQuotesTheDayAndProjectsAtTheWeek(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	samples := diskHourlyRing(t, dir, "control", func(k int) int64 {
		if k <= 144 {
			return int64(300) << 30
		}
		return int64(300)<<30 + int64(k-144)*(int64(2)<<30)
	})

	row := ReadDisk(dir, diskNow).Rows[0]
	if !row.GrowthOK {
		t.Fatalf("control row carries growth_ok=%t on a week of hourly samples", row.GrowthOK)
	}
	if math.Abs(row.GibPerDay24h-48) > 1e-6 {
		t.Errorf("control row gib_per_day_24h is %v, want the last day's 48 GiB/day", row.GibPerDay24h)
	}
	week, ok := growth.Rate(samples, diskNow, 7*24*time.Hour)
	if !ok {
		t.Fatal("the fixture's week does not fit")
	}
	if math.Abs(row.GibPerDay7d-week) > 1e-9 {
		t.Errorf("control row gib_per_day_7d is %v, want the week's fit %v", row.GibPerDay7d, week)
	}
	if math.Abs(week-48) < 1 {
		t.Fatalf("the fixture's week fits %v GiB/day, too close to the day's 48 to tell the two apart", week)
	}
	if !row.DaysLeftOK {
		t.Fatalf("control row carries days_left_ok=%t at a positive week rate", row.DaysLeftOK)
	}
	// 348 GiB used of 500 at the week's pace, never at the day's.
	if want := 152.0 / week; math.Abs(row.DaysLeft-want) > 1e-9 {
		t.Errorf("control row days_left is %v, want %v at the week's pace", row.DaysLeft, want)
	}
}

// A ring the fit cannot use is a row whose flags say so, in each way it
// happens: too few samples, a disk that is flat, one that is falling, a ring
// that is not there, and a ring no sample has reached in a day. None of them
// carries a days-left projection, and the rates are never invented for a ring
// the fit refused.
func TestDiskGrowthSaysWhenItCannotFit(t *testing.T) {
	t.Parallel()

	const gib = int64(1) << 30
	const total = int64(500) << 30

	t.Run("too few samples", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		diskAppend(t, dir, "control", growth.Sample{At: diskNow.Add(-time.Hour), UsedBytes: 400 * gib, TotalBytes: total})
		diskAppend(t, dir, "control", growth.Sample{At: diskNow, UsedBytes: 401 * gib, TotalBytes: total})

		row := ReadDisk(dir, diskNow).Rows[0]
		if row.GrowthOK || row.DaysLeftOK {
			t.Errorf("a two-sample ring reads growth_ok=%t days_left_ok=%t, want both false", row.GrowthOK, row.DaysLeftOK)
		}
		if row.GibPerDay24h != 0 || row.GibPerDay7d != 0 || row.DaysLeft != 0 {
			t.Errorf("a refused fit reads rates %v/%v and days left %v, want no figures", row.GibPerDay24h, row.GibPerDay7d, row.DaysLeft)
		}
	})

	t.Run("flat disk", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		diskHourlyRing(t, dir, "control", func(int) int64 { return 400 * gib })

		row := ReadDisk(dir, diskNow).Rows[0]
		if !row.GrowthOK {
			t.Fatalf("a flat week reads growth_ok=%t, want the zero rates reported", row.GrowthOK)
		}
		if row.GibPerDay24h != 0 || row.GibPerDay7d != 0 {
			t.Errorf("a flat week reads rates %v/%v, want 0", row.GibPerDay24h, row.GibPerDay7d)
		}
		if row.DaysLeftOK {
			t.Error("a flat disk carries a days-left projection, want none")
		}
	})

	t.Run("falling disk", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		diskHourlyRing(t, dir, "control", func(k int) int64 { return int64(500)<<30 - int64(k)*gib/2 })

		row := ReadDisk(dir, diskNow).Rows[0]
		if !row.GrowthOK {
			t.Fatalf("a falling week reads growth_ok=%t, want the negative rates reported", row.GrowthOK)
		}
		if !(row.GibPerDay7d < 0) {
			t.Errorf("a falling week reads a 7-day rate of %v, want a negative one", row.GibPerDay7d)
		}
		if row.DaysLeftOK {
			t.Error("a falling disk carries a days-left projection, want none")
		}
	})

	t.Run("missing ring", func(t *testing.T) {
		t.Parallel()
		row := ReadDisk(t.TempDir(), diskNow).Rows[0]
		if !row.NoSamples {
			t.Fatalf("a missing ring reads no_samples=%t, want the row to say so", row.NoSamples)
		}
		if row.GrowthOK || row.DaysLeftOK {
			t.Errorf("a missing ring reads growth_ok=%t days_left_ok=%t, want both false", row.GrowthOK, row.DaysLeftOK)
		}
	})

	// A stale ring is the shape a stalled patrol leaves: five days of samples,
	// none in the last day. The week fits and the day does not, so the row says
	// the fit is missing rather than quoting a week rate beside a day rate of
	// nothing.
	t.Run("stale ring", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		for k := 0; k <= 120; k++ {
			diskAppend(t, dir, "control", growth.Sample{
				At:         diskNow.Add(-time.Duration(168-k) * time.Hour),
				UsedBytes:  int64(300+k) << 30,
				TotalBytes: total,
			})
		}

		row := ReadDisk(dir, diskNow).Rows[0]
		if row.GrowthOK || row.DaysLeftOK {
			t.Errorf("a ring with no sample in the last day reads growth_ok=%t days_left_ok=%t, want both false", row.GrowthOK, row.DaysLeftOK)
		}
	})
}

// The page reads the growth under these keys, so the row's figures have to
// reach the payload under exactly these names; a renamed tag would leave the
// page drawing "not enough samples" over a fitted week.
func TestDiskGrowthFieldsAreOnTheWire(t *testing.T) {
	t.Parallel()

	raw, err := json.Marshal(&Disk{Rows: []DiskRow{{
		Name: "control", GibPerDay24h: 3, GibPerDay7d: 2.5, DaysLeft: 31.5, GrowthOK: true, DaysLeftOK: true,
	}}})
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Rows []map[string]json.RawMessage `json:"rows"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	if len(wire.Rows) != 1 {
		t.Fatalf("the payload carries %d rows, want one", len(wire.Rows))
	}
	for _, key := range []string{"gib_per_day_24h", "gib_per_day_7d", "days_left", "growth_ok", "days_left_ok"} {
		if _, ok := wire.Rows[0][key]; !ok {
			t.Errorf("the row's payload has no %q key: %s", key, raw)
		}
	}
}

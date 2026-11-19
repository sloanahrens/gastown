package dashboard

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/diskmaint/growth"
)

var diskNow = time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC)

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

			d := ReadDisk(dir)
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
		d := ReadDisk(t.TempDir())
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
		if row := ReadDisk(dir).Rows[0]; !row.NoSamples || row.Err != "" {
			t.Errorf("empty control ring reads no_samples=%t err=%q, want no_samples with no error", row.NoSamples, row.Err)
		}
	})

	t.Run("unreadable ring", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		if err := os.MkdirAll(diskRingPath(dir, "sandbox"), 0o755); err != nil {
			t.Fatal(err)
		}
		if row := ReadDisk(dir).Rows[1]; row.Err == "" {
			t.Errorf("a directory in the sandbox ring's place reads no_samples=%t err=%q, want Err set", row.NoSamples, row.Err)
		}
	})

	t.Run("last sample has no capacity", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		diskAppend(t, dir, "control", growth.Sample{At: diskNow, UsedBytes: 0, TotalBytes: 0})
		row := ReadDisk(dir).Rows[0]
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

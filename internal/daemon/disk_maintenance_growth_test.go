package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/diskmaint/disks"
	"github.com/steveyegge/gastown/internal/diskmaint/growth"
)

// diskMaintGrowthReadings is one pass's readings: the sandbox disk and the
// control disk, each with the bytes its share came from, and the Mac disk,
// which has no ring.
func diskMaintGrowthReadings() []disks.Reading {
	return []disks.Reading{
		{Name: diskMaintDiskColima, UsedPct: 50, UsedBytes: 50, TotalBytes: 100},
		{Name: diskMaintDiskControl, UsedPct: 25, UsedBytes: 25, TotalBytes: 100, AlertOnly: true},
		{Name: diskMaintDiskMac, UsedPct: 10},
	}
}

// diskMaintGrowthRing is the path of a disk's ring under the test town.
func diskMaintGrowthRing(d *Daemon, file string) string {
	return filepath.Join(d.config.TownRoot, ".runtime", "disk-growth", file+".jsonl")
}

// TestDiskMaintGrowthAppendsOneSamplePerDisk: a pass with two kept readings
// appends one sample to each disk's ring, carrying the used and total bytes
// the share came from; a disk with no ring writes nothing.
func TestDiskMaintGrowthAppendsOneSamplePerDisk(t *testing.T) {
	t.Parallel()
	d, _, f := diskMaintTestDaemon(t)
	f.readings = diskMaintGrowthReadings()

	d.readAndApplyDiskAlerts(context.Background(), diskMaintNow)

	for file, want := range map[string]growth.Sample{
		"sandbox": {At: diskMaintNow, UsedBytes: 50, TotalBytes: 100},
		"control": {At: diskMaintNow, UsedBytes: 25, TotalBytes: 100},
	} {
		got, err := growth.Read(diskMaintGrowthRing(d, file))
		if err != nil {
			t.Fatalf("reading the %s ring: %v", file, err)
		}
		if len(got) != 1 {
			t.Fatalf("%s ring holds %d samples, want one: %+v", file, len(got), got)
		}
		if !got[0].At.Equal(want.At) || got[0].UsedBytes != want.UsedBytes || got[0].TotalBytes != want.TotalBytes {
			t.Errorf("%s ring sample = %+v, want %+v", file, got[0], want)
		}
	}
	entries, err := os.ReadDir(filepath.Dir(diskMaintGrowthRing(d, "sandbox")))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Errorf("growth directory holds %d files, want the two kept rings", len(entries))
	}
}

// TestDiskMaintGrowthSpacesSamplesFiveMinutes: a pass inside five minutes of a
// disk's last sample appends nothing for it, and one at exactly five minutes
// appends again.
func TestDiskMaintGrowthSpacesSamplesFiveMinutes(t *testing.T) {
	t.Parallel()
	d, _, f := diskMaintTestDaemon(t)
	f.readings = diskMaintGrowthReadings()
	ctx := context.Background()

	d.readAndApplyDiskAlerts(ctx, diskMaintNow)
	d.readAndApplyDiskAlerts(ctx, diskMaintNow.Add(4*time.Minute))

	got, err := growth.Read(diskMaintGrowthRing(d, "sandbox"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("after a pass at +4m the sandbox ring holds %d samples, want the first one only", len(got))
	}

	d.readAndApplyDiskAlerts(ctx, diskMaintNow.Add(5*time.Minute))

	got, err = growth.Read(diskMaintGrowthRing(d, "sandbox"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("after a pass at +5m the sandbox ring holds %d samples, want two", len(got))
	}
}

// TestDiskMaintGrowthSkipsReadingsItCannotUse: a reading that carries an error,
// or names no byte count, appends nothing for its disk, and the other disk's
// reading still appends.
func TestDiskMaintGrowthSkipsReadingsItCannotUse(t *testing.T) {
	t.Parallel()
	cases := map[string]disks.Reading{
		"a reading with an error": {
			Name: diskMaintDiskColima, UsedBytes: 50, TotalBytes: 100,
			Err: errors.New("the connection to the server was refused"),
		},
		"a reading that names no byte count": {
			Name: diskMaintDiskColima, UsedPct: 50,
		},
	}
	for name, colima := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			d, _, f := diskMaintTestDaemon(t)
			f.readings = []disks.Reading{colima, {Name: diskMaintDiskControl, UsedBytes: 25, TotalBytes: 100}}

			d.readAndApplyDiskAlerts(context.Background(), diskMaintNow)

			if _, err := os.Stat(diskMaintGrowthRing(d, "sandbox")); !os.IsNotExist(err) {
				t.Fatalf("sandbox ring exists or cannot be checked (%v); want no sample for the unusable reading", err)
			}
			got, err := growth.Read(diskMaintGrowthRing(d, "control"))
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 1 {
				t.Fatalf("control ring holds %d samples, want its own one", len(got))
			}
		})
	}
}

// TestDiskMaintGrowthUnwritableDirectoryKeepsTheAlerts: a growth directory that
// cannot be made logs one line for the pass and raises the same alerts a
// writable one does.
func TestDiskMaintGrowthUnwritableDirectoryKeepsTheAlerts(t *testing.T) {
	t.Parallel()
	readings := []disks.Reading{
		{Name: diskMaintDiskColima, UsedPct: 85, UsedBytes: 85, TotalBytes: 100},
		{Name: diskMaintDiskControl, UsedPct: 85, UsedBytes: 85, TotalBytes: 100, AlertOnly: true},
	}

	blocked, blockedLog, blockedFakes := diskMaintTestDaemon(t)
	runtimeDir := filepath.Join(blocked.config.TownRoot, ".runtime")
	if err := os.MkdirAll(runtimeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// A regular file where the growth directory belongs: MkdirAll cannot make it.
	if err := os.WriteFile(filepath.Join(runtimeDir, "disk-growth"), []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	blockedFakes.readings = readings
	blocked.readAndApplyDiskAlerts(context.Background(), diskMaintNow)

	writable, _, writableFakes := diskMaintTestDaemon(t)
	writableFakes.readings = readings
	writable.readAndApplyDiskAlerts(context.Background(), diskMaintNow)

	if n := strings.Count(blockedLog.String(), "growth samples not recorded"); n != 1 {
		t.Fatalf("the unwritable pass logged %d growth lines, want exactly one:\n%s", n, blockedLog.String())
	}
	gotBlocked := diskMaintFingerprints(blockedFakes)
	gotWritable := diskMaintFingerprints(writableFakes)
	if !slices.Equal(gotBlocked, gotWritable) {
		t.Fatalf("alerts with an unwritable growth directory = %v, with a writable one = %v; want the same", gotBlocked, gotWritable)
	}
	want := []string{"diskmaint:colima", "diskmaint:control"}
	if !slices.Equal(gotWritable, want) {
		t.Fatalf("alerts = %v, want %v", gotWritable, want)
	}
}

// diskMaintFingerprints is the sorted fingerprints of the escalations a test's
// notifier received.
func diskMaintFingerprints(f *diskMaintFakes) []string {
	var got []string
	for _, e := range f.notify.Escalations() {
		got = append(got, e.Escalation.Fingerprint)
	}
	slices.Sort(got)
	return got
}

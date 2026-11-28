package hostprobe

import (
	"fmt"
	"testing"
	"time"
)

// TestForgejoBackupDecodesTheTimeAndErr: a forgejo_backup reading decodes into
// its last_good time and its err, whether or not the reading carries an err.
func TestForgejoBackupDecodesTheTimeAndErr(t *testing.T) {
	t.Parallel()
	want := time.Date(2026, 11, 27, 3, 15, 0, 0, time.UTC)
	for _, tc := range []struct {
		name, field, wantErr string
	}{
		{"a clean reading", `{"value":{"last_good":"2026-11-27T03:15:00Z"},"err":""}`, ""},
		{"a reading with an err", `{"value":{"last_good":"2026-11-27T03:15:00Z"},"err":"pg_dump: exit status 1"}`, "pg_dump: exit status 1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			now := testAt()
			path := writeProbes(t, fmt.Sprintf(`{"v":%d,"at":%q,"forgejo_backup":%s}`, SchemaVersion, now.Format(time.RFC3339), tc.field))
			got, err := Read(path, now, 3*time.Minute)
			if err != nil {
				t.Fatalf("Read: %v", err)
			}
			if got.ForgejoBackup == nil {
				t.Fatalf("ForgejoBackup = nil, want the reading the file carries")
			}
			if !got.ForgejoBackup.Value.LastGood.Equal(want) || got.ForgejoBackup.Err != tc.wantErr {
				t.Errorf("ForgejoBackup = %+v, want last_good %v with err %q", *got.ForgejoBackup, want, tc.wantErr)
			}
		})
	}
}

// TestForgejoBackupIsNilWhenTheFileHasNoKey: a file written before the field
// decodes with it nil, not a zero reading a caller could take for a dump.
func TestForgejoBackupIsNilWhenTheFileHasNoKey(t *testing.T) {
	t.Parallel()
	now := testAt()
	path := writeProbes(t, fmt.Sprintf(`{"v":%d,"at":%q,"mem_free_pct":{"value":94}}`, SchemaVersion, now.Format(time.RFC3339)))
	got, err := Read(path, now, 3*time.Minute)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got.ForgejoBackup != nil {
		t.Errorf("ForgejoBackup = %+v, want nil for a file without the key", *got.ForgejoBackup)
	}
}

// TestForgejoBackupWithoutLastGoodKeepsZeroTimeAndErr: a reading that carries
// an err but no last_good decodes to the zero time with that err, so a failed
// dump is reported rather than read as a dump at the epoch.
func TestForgejoBackupWithoutLastGoodKeepsZeroTimeAndErr(t *testing.T) {
	t.Parallel()
	now := testAt()
	path := writeProbes(t, fmt.Sprintf(`{"v":%d,"at":%q,"forgejo_backup":{"value":{},"err":"dump aborted"}}`, SchemaVersion, now.Format(time.RFC3339)))
	got, err := Read(path, now, 3*time.Minute)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got.ForgejoBackup == nil {
		t.Fatalf("ForgejoBackup = nil, want the reading the file carries")
	}
	if !got.ForgejoBackup.Value.LastGood.IsZero() || got.ForgejoBackup.Err != "dump aborted" {
		t.Errorf("ForgejoBackup = %+v, want the zero last_good with err %q", *got.ForgejoBackup, "dump aborted")
	}
}

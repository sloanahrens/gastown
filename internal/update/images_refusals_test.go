package update

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestFileImageLedgerRefusesADamagedRecord holds each way a record on disk
// cannot be read: the read fails, the bytes do not parse, or the version is not
// one this build writes. None is read as an empty record.
func TestFileImageLedgerRefusesADamagedRecord(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		prepare func(t *testing.T, path string)
		wantErr string
	}{
		{
			name: "a record that cannot be read is an error",
			prepare: func(t *testing.T, path string) {
				if err := os.Mkdir(path, 0o755); err != nil {
					t.Fatal(err)
				}
			},
			wantErr: "read the daemon image record",
		},
		{
			name: "a record that does not parse is an error",
			prepare: func(t *testing.T, path string) {
				if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			wantErr: "parse the daemon image record",
		},
		{
			name: "a record of another version is refused",
			prepare: func(t *testing.T, path string) {
				if err := os.WriteFile(path, []byte(`{"v":2}`), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			wantErr: "unsupported version 2",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			tt.prepare(t, filepath.Join(dir, imageRecordFileName))
			_, err := FileImageLedger{Dir: dir}.Read()
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Read error = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

// TestFileImageLedgerRefusesAnUnwritableRecord holds each way a record cannot be
// published: its directory cannot be made, the record cannot be encoded, or the
// path it publishes to is taken.
func TestFileImageLedgerRefusesAnUnwritableRecord(t *testing.T) {
	t.Parallel()
	t.Run("a state directory that cannot be created is refused", func(t *testing.T) {
		t.Parallel()
		blocker := filepath.Join(t.TempDir(), "blocker")
		if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		err := FileImageLedger{Dir: filepath.Join(blocker, "state")}.Write(ImageRecord{Current: newPair})
		if err == nil || !strings.Contains(err.Error(), "create update state directory") {
			t.Fatalf("Write error = %v, want the directory refusal", err)
		}
	})
	t.Run("a record that cannot be encoded is refused", func(t *testing.T) {
		t.Parallel()
		ledger := FileImageLedger{
			Dir: t.TempDir(),
			marshal: func(any, string, string) ([]byte, error) {
				return nil, errors.New("encoder refused")
			},
		}
		err := ledger.Write(ImageRecord{Current: newPair})
		if err == nil || !strings.Contains(err.Error(), "encode the daemon image record") {
			t.Fatalf("Write error = %v, want the encode refusal", err)
		}
	})
	t.Run("a record whose path is taken is refused", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		if err := os.Mkdir(filepath.Join(dir, imageRecordFileName), 0o755); err != nil {
			t.Fatal(err)
		}
		err := FileImageLedger{Dir: dir}.Write(ImageRecord{Current: newPair})
		if err == nil || !strings.Contains(err.Error(), "write the daemon image record") {
			t.Fatalf("Write error = %v, want the publish refusal", err)
		}
	})
}

// TestFileImageLedgerRoundTrips holds that a record written reads back as
// written, with the version the ledger stamps.
func TestFileImageLedgerRoundTrips(t *testing.T) {
	t.Parallel()
	ledger := FileImageLedger{Dir: t.TempDir()}
	want := ImageRecord{Current: newPair, Previous: oldPair}
	if err := ledger.Write(want); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := ledger.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got.Current != want.Current || got.Previous != want.Previous || got.V != imageRecordVersion {
		t.Errorf("Read = %+v, want the pairs %+v at version %d", got, want, imageRecordVersion)
	}
}

// TestImageDepsRefuseAnUnwiredEffect holds that a digest install and a rollback
// refuse, before they act, when any effect they reach is not wired.
func TestImageDepsRefuseAnUnwiredEffect(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		unwire func(d *ImageDeps)
	}{
		{name: "no ledger", unwire: func(d *ImageDeps) { d.Ledger = nil }},
		{name: "no swap", unwire: func(d *ImageDeps) { d.Swap = nil }},
		{name: "no version probe", unwire: func(d *ImageDeps) { d.Verify = nil }},
		{name: "no clock", unwire: func(d *ImageDeps) { d.Clock = nil }},
		{name: "no sleep", unwire: func(d *ImageDeps) { d.Sleep = nil }},
		{name: "no escalation sink", unwire: func(d *ImageDeps) { d.Notify = nil }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newImageFixture(t, ImageRecord{Current: oldPair})
			tt.unwire(&f.inst.Images)
			if _, err := f.inst.Install(context.Background(), newPair.Commit); err == nil {
				t.Fatal("Install with an unwired effect returned no error")
			}
			if f.host.calls != 0 || len(f.swap.applied) != 0 {
				t.Errorf("an install with an unwired effect ran the host install (%d) or swapped (%+v)", f.host.calls, f.swap.applied)
			}
			if _, err := Rollback(context.Background(), f.inst.Images, func(context.Context, ImagePair) error { return nil }); err == nil {
				t.Fatal("Rollback with an unwired effect returned no error")
			}
		})
	}
}

// TestDigestInstallRefusesAFailingStep holds the install's own failure paths:
// a record that cannot be read stops it before the host install, and a host
// install that errors ends it before the daemon is touched.
func TestDigestInstallRefusesAFailingStep(t *testing.T) {
	t.Parallel()

	t.Run("a record that cannot be read stops the install before the host install", func(t *testing.T) {
		t.Parallel()
		f := newImageFixture(t, ImageRecord{Current: oldPair})
		f.ledger.readErr = errors.New("disk gone")
		if _, err := f.inst.Install(context.Background(), newPair.Commit); err == nil || !strings.Contains(err.Error(), "reading the daemon image record") {
			t.Fatalf("Install error = %v, want the record read refusal", err)
		}
		if f.host.calls != 0 || len(f.swap.applied) != 0 {
			t.Errorf("the host install ran %d times and the daemon was swapped to %+v", f.host.calls, f.swap.applied)
		}
	})

	t.Run("a host install that errors is returned with the daemon untouched", func(t *testing.T) {
		t.Parallel()
		f := newImageFixture(t, ImageRecord{Current: oldPair})
		f.host.err = errors.New("make gt-installer failed")
		if _, err := f.inst.Install(context.Background(), newPair.Commit); err == nil {
			t.Fatal("Install with a failing host install returned no error")
		}
		if len(f.swap.applied) != 0 || len(f.ledger.writes) != 0 {
			t.Errorf("the daemon was swapped to %+v and the record written %d times", f.swap.applied, len(f.ledger.writes))
		}
	})
}

// TestRollbackRefusesAFailingStep holds each refusal the rollback makes, each
// with nothing changed unless the step itself failed after it began.
func TestRollbackRefusesAFailingStep(t *testing.T) {
	t.Parallel()
	passes := func(context.Context, ImagePair) error { return nil }
	refuses := func(context.Context, ImagePair) error { return errors.New("registry has no such digest") }

	t.Run("a record that cannot be read is refused", func(t *testing.T) {
		t.Parallel()
		f := newImageFixture(t, ImageRecord{Current: newPair, Previous: oldPair})
		f.ledger.readErr = errors.New("disk gone")
		if _, err := Rollback(context.Background(), f.inst.Images, passes); err == nil || !strings.Contains(err.Error(), "reading the daemon image record") {
			t.Fatalf("Rollback error = %v, want the record read refusal", err)
		}
		if len(f.swap.applied) != 0 {
			t.Errorf("the daemon was swapped to %+v", f.swap.applied)
		}
	})

	t.Run("nothing recorded is refused", func(t *testing.T) {
		t.Parallel()
		f := newImageFixture(t, ImageRecord{})
		if _, err := Rollback(context.Background(), f.inst.Images, passes); !errors.Is(err, ErrNothingRecorded) {
			t.Fatalf("Rollback error = %v, want ErrNothingRecorded", err)
		}
	})

	t.Run("a recorded previous the daemon runs is refused", func(t *testing.T) {
		t.Parallel()
		f := newImageFixture(t, ImageRecord{Current: oldPair, Previous: oldPair})
		if _, err := Rollback(context.Background(), f.inst.Images, passes); !errors.Is(err, ErrAlreadyRunning) {
			t.Fatalf("Rollback error = %v, want ErrAlreadyRunning", err)
		}
	})

	t.Run("no registry check is wired", func(t *testing.T) {
		t.Parallel()
		f := newImageFixture(t, ImageRecord{Current: newPair, Previous: oldPair})
		if _, err := Rollback(context.Background(), f.inst.Images, nil); err == nil || !strings.Contains(err.Error(), "no registry check is wired") {
			t.Fatalf("Rollback error = %v, want the missing check named", err)
		}
	})

	t.Run("a pair the registry refuses is refused with nothing changed", func(t *testing.T) {
		t.Parallel()
		f := newImageFixture(t, ImageRecord{Current: newPair, Previous: oldPair})
		_, err := Rollback(context.Background(), f.inst.Images, refuses)
		if err == nil || !strings.Contains(err.Error(), "refusing the recorded previous image") {
			t.Fatalf("Rollback error = %v, want the registry refusal", err)
		}
		if len(f.swap.applied) != 0 || len(f.ledger.writes) != 0 {
			t.Errorf("the daemon was swapped to %+v and the record written %d times", f.swap.applied, len(f.ledger.writes))
		}
	})

	t.Run("a restore that does not come back is refused and escalated", func(t *testing.T) {
		t.Parallel()
		f := newImageFixture(t, ImageRecord{Current: newPair, Previous: oldPair})
		f.swap.stuck = map[string]bool{oldPair.Daemon: true}
		if _, err := Rollback(context.Background(), f.inst.Images, passes); err == nil {
			t.Fatal("Rollback whose restore never comes up returned no error")
		}
		if n := len(f.notify.Escalations()); n != 1 {
			t.Errorf("escalations = %d, want the failed restore escalated once", n)
		}
	})

	t.Run("a restored pair that cannot be recorded is reported", func(t *testing.T) {
		t.Parallel()
		f := newImageFixture(t, ImageRecord{Current: newPair, Previous: oldPair})
		f.ledger.writeErr = errors.New("disk full")
		_, err := Rollback(context.Background(), f.inst.Images, passes)
		if err == nil || !strings.Contains(err.Error(), "recording that failed") {
			t.Fatalf("Rollback error = %v, want the unrecorded restore reported", err)
		}
	})
}

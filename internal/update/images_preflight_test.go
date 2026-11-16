package update

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPreRollGuardRefusesThePairBeforeTheSwap holds the pre-swap check to its
// refusal: nothing is installed on the host or swapped, the recorded pairs are
// left as they were, nothing is restored, and the pair is kept on the record as
// refused with the failure that named it.
func TestPreRollGuardRefusesThePairBeforeTheSwap(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		check    func() error
		wantName string
	}{
		{
			name:     "a failure that names no check is refused as the pair",
			check:    func() error { return errors.New("the registry has no digest for the polecat image") },
			wantName: CheckPair,
		},
		{
			name: "a failure that names its check is named by it",
			check: func() error {
				return PairCheckError{Check: CheckRegistryTag, Err: errors.New("the dev tag names another digest")}
			},
			wantName: CheckRegistryTag,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newImageFixture(t, ImageRecord{Current: oldPair})
			var checked []ImagePair
			f.inst.PreSwap = func(_ context.Context, pair ImagePair) error {
				checked = append(checked, pair)
				return tt.check()
			}

			res, err := f.inst.Install(context.Background(), newPair.Commit)
			if err != nil {
				t.Fatalf("Install: %v", err)
			}
			if ClassifyInstall(res) != ResultFailed || res.Event != "failed" || res.Exit != 1 {
				t.Fatalf("result = %+v, want a failed step exiting 1", res)
			}
			want := CheckPreflight + " " + tt.wantName + ": " + tt.check().Error()
			if res.Reason != want {
				t.Errorf("reason = %q, want %q", res.Reason, want)
			}
			if len(checked) != 1 || checked[0] != newPair {
				t.Errorf("checked = %+v, want the check run once on the target pair", checked)
			}
			if f.host.calls != 0 {
				t.Errorf("host install ran %d times, want none: a refused pair is never installed", f.host.calls)
			}
			if len(f.swap.applied) != 0 {
				t.Errorf("swapped to %+v, want no swap after a refusal", f.swap.applied)
			}
			wantRec := ImageRecord{Current: oldPair, Refused: newPair, RefusedReason: want}
			if f.ledger.rec != wantRec || len(f.ledger.writes) != 1 {
				t.Errorf("record = %+v after %d writes, want %+v recorded once", f.ledger.rec, len(f.ledger.writes), wantRec)
			}
			if res.Rollback != nil {
				t.Errorf("rollback = %+v, want none: a refused pair was never swapped in", res.Rollback)
			}
			if n := len(f.notify.Escalations()); n != 0 {
				t.Errorf("escalations = %d, want none", n)
			}
		})
	}

	t.Run("a refusal the record cannot keep is named in the failure", func(t *testing.T) {
		t.Parallel()
		f := newImageFixture(t, ImageRecord{Current: oldPair})
		f.ledger.writeErr = errors.New("the state directory is read-only")
		f.inst.PreSwap = func(context.Context, ImagePair) error { return errors.New("no digest for the daemon image") }

		res, err := f.inst.Install(context.Background(), newPair.Commit)
		if err != nil {
			t.Fatalf("Install: %v", err)
		}
		if ClassifyInstall(res) != ResultFailed {
			t.Fatalf("result = %+v, want a failed step", res)
		}
		if !strings.Contains(res.Reason, "preflight pair: no digest for the daemon image") || !strings.Contains(res.Reason, "recording the refused pair failed") {
			t.Errorf("reason = %q, want the refusal and the record failure named", res.Reason)
		}
		if f.host.calls != 0 {
			t.Errorf("host install ran %d times, want none: a refused pair is never installed", f.host.calls)
		}
		if len(f.swap.applied) != 0 {
			t.Errorf("swapped to %+v, want no swap after a refusal", f.swap.applied)
		}
	})

	t.Run("a canceled context is the cancel, not a refusal", func(t *testing.T) {
		t.Parallel()
		f := newImageFixture(t, ImageRecord{Current: oldPair})
		ctx, cancel := context.WithCancel(context.Background())
		f.inst.PreSwap = func(context.Context, ImagePair) error {
			cancel()
			return errors.New("the registry read did not finish")
		}

		res, err := f.inst.Install(ctx, newPair.Commit)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Install = %+v, %v; want the context cancel", res, err)
		}
		if len(f.ledger.writes) != 0 {
			t.Errorf("wrote %+v, want no refusal recorded for a cancel", f.ledger.writes)
		}
		if len(f.swap.applied) != 0 {
			t.Errorf("swapped to %+v, want no swap after a cancel", f.swap.applied)
		}
	})
}

// TestPreRollGuardKeepsTheInstallPath holds the check to what it leaves alone: a
// nil check is skipped, a passing check runs once before the host install, the
// record write and the swap and changes none of them, and an install that swaps
// clears an earlier refusal.
func TestPreRollGuardKeepsTheInstallPath(t *testing.T) {
	t.Parallel()

	t.Run("a nil check is skipped", func(t *testing.T) {
		t.Parallel()
		f := newImageFixture(t, ImageRecord{Current: oldPair})

		res, err := f.inst.Install(context.Background(), newPair.Commit)
		if err != nil {
			t.Fatalf("Install: %v", err)
		}
		if ClassifyInstall(res) != ResultOK {
			t.Fatalf("result = %+v, want an installed pair", res)
		}
		if got := f.swap.applied; len(got) != 1 || got[0] != newPair {
			t.Errorf("applied = %+v, want the target pair alone", got)
		}
		if want := (ImageRecord{Current: newPair, Previous: oldPair}); f.ledger.rec != want {
			t.Errorf("record = %+v, want %+v", f.ledger.rec, want)
		}
	})

	t.Run("a passing check runs once before the host install, the record write and the swap", func(t *testing.T) {
		t.Parallel()
		f := newImageFixture(t, ImageRecord{Current: oldPair})
		var checked []ImagePair
		var recordAtCheck ImageRecord
		var appliedAtCheck int
		var hostCallsAtCheck int
		f.inst.PreSwap = func(_ context.Context, pair ImagePair) error {
			checked = append(checked, pair)
			recordAtCheck = f.ledger.rec
			appliedAtCheck = len(f.swap.applied)
			hostCallsAtCheck = f.host.calls
			return nil
		}

		res, err := f.inst.Install(context.Background(), newPair.Commit)
		if err != nil {
			t.Fatalf("Install: %v", err)
		}
		if ClassifyInstall(res) != ResultOK {
			t.Fatalf("result = %+v, want an installed pair", res)
		}
		if len(checked) != 1 || checked[0] != newPair {
			t.Errorf("checked = %+v, want the check run once on the target pair", checked)
		}
		if want := (ImageRecord{Current: oldPair}); recordAtCheck != want || appliedAtCheck != 0 || hostCallsAtCheck != 0 {
			t.Errorf("the check saw record %+v after %d swaps and %d host installs, want %+v and none: it must run before the host install, the write and the swap", recordAtCheck, appliedAtCheck, hostCallsAtCheck, want)
		}
		if got := f.swap.applied; len(got) != 1 || got[0] != newPair {
			t.Errorf("applied = %+v, want the target pair alone", got)
		}
		if want := (ImageRecord{Current: newPair, Previous: oldPair}); f.ledger.rec != want {
			t.Errorf("record = %+v, want %+v", f.ledger.rec, want)
		}
	})

	t.Run("an install that swaps clears an earlier refusal", func(t *testing.T) {
		t.Parallel()
		f := newImageFixture(t, ImageRecord{
			Current:       oldPair,
			Refused:       newPair,
			RefusedReason: CheckPreflight + " " + CheckPair + ": no digest for the daemon image",
		})
		f.inst.PreSwap = func(context.Context, ImagePair) error { return nil }

		res, err := f.inst.Install(context.Background(), newPair.Commit)
		if err != nil {
			t.Fatalf("Install: %v", err)
		}
		if ClassifyInstall(res) != ResultOK {
			t.Fatalf("result = %+v, want an installed pair", res)
		}
		if want := (ImageRecord{Current: newPair, Previous: oldPair}); f.ledger.rec != want {
			t.Errorf("record = %+v, want %+v with the refusal cleared", f.ledger.rec, want)
		}
	})
}

// TestPreRollGuardClearsARefusalOnAnEarlySuccess holds a later successful
// install to the record: when the daemon already runs the target pair and the
// install leaves it alone, a refusal the record still names is cleared with the
// same record write the swap path makes, and a record that names no refusal is
// not written.
func TestPreRollGuardClearsARefusalOnAnEarlySuccess(t *testing.T) {
	t.Parallel()
	refusal := CheckPreflight + " " + CheckPair + ": no digest for the daemon image"

	tests := []struct {
		name       string
		recorded   ImageRecord
		wantRecord ImageRecord
		wantWrites int
	}{
		{
			name:       "a refusal the record still names is cleared",
			recorded:   ImageRecord{Current: newPair, Previous: oldPair, Refused: newPair, RefusedReason: refusal},
			wantRecord: ImageRecord{Current: newPair, Previous: oldPair},
			wantWrites: 1,
		},
		{
			name:       "a record that names no refusal is left as it was",
			recorded:   ImageRecord{Current: newPair, Previous: oldPair},
			wantRecord: ImageRecord{Current: newPair, Previous: oldPair},
			wantWrites: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newImageFixture(t, tt.recorded)
			f.inst.PreSwap = func(context.Context, ImagePair) error { return nil }

			res, err := f.inst.Install(context.Background(), newPair.Commit)
			if err != nil {
				t.Fatalf("Install: %v", err)
			}
			if ClassifyInstall(res) != ResultOK {
				t.Fatalf("result = %+v, want an installed pair", res)
			}
			if len(f.swap.applied) != 0 {
				t.Errorf("applied = %+v, want no swap when the daemon already runs the pair", f.swap.applied)
			}
			if f.ledger.rec != tt.wantRecord || len(f.ledger.writes) != tt.wantWrites {
				t.Errorf("record = %+v after %d writes, want %+v after %d", f.ledger.rec, len(f.ledger.writes), tt.wantRecord, tt.wantWrites)
			}
		})
	}

	t.Run("a refusal the record cannot clear fails the install", func(t *testing.T) {
		t.Parallel()
		f := newImageFixture(t, ImageRecord{Current: newPair, Previous: oldPair, Refused: newPair, RefusedReason: refusal})
		f.inst.PreSwap = func(context.Context, ImagePair) error { return nil }
		f.ledger.writeErr = errors.New("the state directory is read-only")

		res, err := f.inst.Install(context.Background(), newPair.Commit)
		if err == nil || !strings.Contains(err.Error(), "clearing the refused daemon image pair") {
			t.Fatalf("Install = %+v, %v; want the uncleared refusal named as an error", res, err)
		}
		if len(f.swap.applied) != 0 {
			t.Errorf("applied = %+v, want no swap", f.swap.applied)
		}
		if f.ledger.rec.Refused != newPair {
			t.Errorf("record = %+v, want the refusal still on it", f.ledger.rec)
		}
	})
}

// TestPreRollGuardLegacyLedgerReadsAndRewrites holds the record's new keys to
// backward compatibility: a file written before them reads with no refusal, and
// a rewrite keeps the recorded pairs and records no refusal. encoding/json
// cannot leave out a struct field tagged omitempty, so the zero pair is written
// as empty images; a reader of either shape takes it as nothing refused.
func TestPreRollGuardLegacyLedgerReadsAndRewrites(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	legacy := `{"v":1,` +
		`"current":{"daemon":"` + oldPair.Daemon + `","polecat":"` + oldPair.Polecat + `","commit":"` + oldPair.Commit + `"},` +
		`"previous":{"daemon":"` + newPair.Daemon + `","polecat":"` + newPair.Polecat + `","commit":"` + newPair.Commit + `"}}`
	path := filepath.Join(dir, imageRecordFileName)
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatalf("write the legacy record: %v", err)
	}
	ledger := FileImageLedger{Dir: dir}

	rec, err := ledger.Read()
	if err != nil {
		t.Fatalf("Read of a record written before the new keys: %v", err)
	}
	want := ImageRecord{V: imageRecordVersion, Current: oldPair, Previous: newPair}
	if rec != want {
		t.Errorf("read %+v, want %+v with nothing refused", rec, want)
	}
	if err := ledger.Write(rec); err != nil {
		t.Fatalf("Write: %v", err)
	}
	again, err := ledger.Read()
	if err != nil {
		t.Fatalf("Read after the rewrite: %v", err)
	}
	if again != want {
		t.Errorf("read back %+v, want %+v", again, want)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the rewritten record: %v", err)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(data, &keys); err != nil {
		t.Fatalf("the rewritten record does not parse: %v", err)
	}
	if _, ok := keys["refused_reason"]; ok {
		t.Errorf("the rewritten record names a refusal reason; it should record none: %s", data)
	}
}

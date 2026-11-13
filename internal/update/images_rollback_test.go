package update

import (
	"context"
	"errors"
	"testing"
)

// TestInstallByDigestNamesTheCheckThatFailed drives each check a digest install
// runs after the swap to a failure, and holds the rollback record to the check
// that failed and the pair restored. A check that fails restores the recorded
// previous pair, and the record names that check. A check that holds records no
// rollback.
func TestInstallByDigestNamesTheCheckThatFailed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// setup makes the fixture's swap or pair check fail.
		setup func(f *imageFixture)
		want  *RollbackRecord
	}{
		{
			name: "the swap errors: rollout",
			setup: func(f *imageFixture) {
				f.swap.fail = map[string]error{newPair.Daemon: errors.New("compose refused the image")}
			},
			want: &RollbackRecord{Check: CheckRollout, Restored: oldPair},
		},
		{
			name: "the daemon never reports the commit: version",
			setup: func(f *imageFixture) {
				f.swap.stuck = map[string]bool{newPair.Daemon: true}
			},
			want: &RollbackRecord{Check: CheckVersion, Restored: oldPair},
		},
		{
			name: "a daemon pod runs another digest: running-digests",
			setup: func(f *imageFixture) {
				f.inst.PairCheck = func(context.Context, ImagePair) error {
					return PairCheckError{Check: CheckRunningDigests, Err: errors.New("pod runs another digest")}
				}
			},
			want: &RollbackRecord{Check: CheckRunningDigests, Restored: oldPair},
		},
		{
			name: "the registry's dev tag is another digest: registry-tag",
			setup: func(f *imageFixture) {
				f.inst.PairCheck = func(context.Context, ImagePair) error {
					return PairCheckError{Check: CheckRegistryTag, Err: errors.New("dev tag names another digest")}
				}
			},
			want: &RollbackRecord{Check: CheckRegistryTag, Restored: oldPair},
		},
		{
			name: "a pair check that names no check: pair",
			setup: func(f *imageFixture) {
				f.inst.PairCheck = func(context.Context, ImagePair) error {
					return errors.New("the values file no longer holds the pair")
				}
			},
			want: &RollbackRecord{Check: CheckPair, Restored: oldPair},
		},
		{
			name:  "every check holds: no rollback",
			setup: func(*imageFixture) {},
			want:  nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newImageFixture(t, ImageRecord{Current: oldPair})
			tt.setup(f)
			res, err := f.inst.Install(context.Background(), newPair.Commit)
			if err != nil {
				t.Fatalf("Install: %v", err)
			}
			if tt.want == nil {
				if res.Rollback != nil || ClassifyInstall(res) != ResultOK {
					t.Fatalf("result = %+v, want a clean install with no rollback", res)
				}
				return
			}
			if ClassifyInstall(res) != ResultFailed || res.Exit == 0 {
				t.Fatalf("result = %+v, want a failed install with a nonzero exit", res)
			}
			if res.Rollback == nil || *res.Rollback != *tt.want {
				t.Fatalf("rollback = %+v, want %+v", res.Rollback, tt.want)
			}
			if got := f.swap.applied; len(got) < 2 || got[len(got)-1] != oldPair {
				t.Errorf("applied = %+v, want the last swap to restore the previous pair", got)
			}
			if n := len(f.notify.Escalations()); n != 0 {
				t.Errorf("a restore that holds raised %d escalations, want none", n)
			}
		})
	}
}

// TestInstallByDigestRecordsNoRollbackWhenTheRestoreFails holds a failed restore to
// no rollback record. The restore did not put the previous pair back, so the
// record must not say it did; the critical escalation is the record of that
// failure.
func TestInstallByDigestRecordsNoRollbackWhenTheRestoreFails(t *testing.T) {
	t.Parallel()
	f := newImageFixture(t, ImageRecord{Current: oldPair})
	f.swap.stuck = map[string]bool{newPair.Daemon: true, oldPair.Daemon: true}
	res, err := f.inst.Install(context.Background(), newPair.Commit)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if res.Rollback != nil {
		t.Fatalf("rollback = %+v, want none when the restore failed", res.Rollback)
	}
	escalations := f.notify.Escalations()
	if len(escalations) != 1 || escalations[0].Escalation.Severity != "critical" {
		t.Fatalf("escalations = %+v, want exactly one critical escalation", escalations)
	}
}

// TestPairCheckErrorKeepsItsMessageAndCheck holds the wrapper to what the pair
// check reports: its message is unchanged and the check it names is reachable.
func TestPairCheckErrorKeepsItsMessageAndCheck(t *testing.T) {
	t.Parallel()
	inner := errors.New("pod runs another digest")
	err := PairCheckError{Check: CheckRunningDigests, Err: inner}
	if err.Error() != inner.Error() {
		t.Errorf("Error() = %q, want %q", err.Error(), inner.Error())
	}
	if !errors.Is(err, inner) {
		t.Errorf("errors.Is(err, inner) = false, want true")
	}
	if got := pairCheckName(err); got != CheckRunningDigests {
		t.Errorf("pairCheckName = %q, want %q", got, CheckRunningDigests)
	}
	if got := pairCheckName(inner); got != CheckPair {
		t.Errorf("pairCheckName(untyped) = %q, want %q", got, CheckPair)
	}
}

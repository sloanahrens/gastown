package update

import (
	"context"
	"errors"
	"testing"
)

// podDeployment is the pod's read of the images its Deployment runs, with no
// commit. Before any swap it reads as before. After a swap it reads as the pair
// the last swap put in force, unless held names images the Deployment keeps
// whatever is applied, or err fails the read.
type podDeployment struct {
	swap   *fakeImageSwap
	before ImagePair
	held   ImagePair
	err    error
}

func (d *podDeployment) RunningPair(context.Context) (ImagePair, error) {
	if d.err != nil {
		return ImagePair{}, d.err
	}
	n := len(d.swap.applied)
	if n == 0 {
		return d.before, nil
	}
	if d.held != (ImagePair{}) {
		return d.held, nil
	}
	last := d.swap.applied[n-1]
	return ImagePair{Daemon: last.Daemon, Polecat: last.Polecat}, nil
}

// TestCommitlessRestoreIsConfirmedByTheImagesRunning holds a restore to a pair
// with no commit to what the Deployment runs after the swap. A pod's restore has
// no build to wait for, so the images are the only evidence the restore took.
// Each subtest's failure is a restore recorded as running when the Deployment
// does not run its images.
func TestCommitlessRestoreIsConfirmedByTheImagesRunning(t *testing.T) {
	t.Parallel()
	passes := func(context.Context, ImagePair) error { return nil }

	t.Run("the Deployment keeps the new images: the restore is escalated, not recorded as running", func(t *testing.T) {
		t.Parallel()
		f := newImageFixture(t, ImageRecord{})
		f.inst.Images.Running = &podDeployment{swap: f.swap, before: clusterPair, held: ImagePair{Daemon: newPair.Daemon, Polecat: newPair.Polecat}}
		f.inst.PairCheck = func(context.Context, ImagePair) error {
			return PairCheckError{Check: CheckRunningDigests, Err: errors.New("the pod runs another digest")}
		}
		res, err := f.inst.Install(context.Background(), newPair.Commit)
		if err != nil {
			t.Fatalf("Install: %v", err)
		}
		if ClassifyInstall(res) != ResultFailed || res.Rollback != nil {
			t.Fatalf("result = %+v, want a failed install with no rollback record, since the restore did not hold", res)
		}
		if n := len(f.notify.Escalations()); n != 1 {
			t.Errorf("escalations = %d, want 1 for the restore the Deployment did not take", n)
		}
		if want := (ImageRecord{Current: newPair, Previous: clusterPair}); f.ledger.rec != want {
			t.Errorf("record = %+v, want %+v: the cluster pair must not be recorded as running", f.ledger.rec, want)
		}
	})

	t.Run("the Deployment moves back to the restored images: the restore holds", func(t *testing.T) {
		t.Parallel()
		f := newImageFixture(t, ImageRecord{})
		f.inst.Images.Running = &podDeployment{swap: f.swap, before: clusterPair}
		f.inst.PairCheck = func(context.Context, ImagePair) error {
			return PairCheckError{Check: CheckRunningDigests, Err: errors.New("the pod runs another digest")}
		}
		res, err := f.inst.Install(context.Background(), newPair.Commit)
		if err != nil {
			t.Fatalf("Install: %v", err)
		}
		if want := (RollbackRecord{Check: CheckRunningDigests, Restored: clusterPair}); res.Rollback == nil || *res.Rollback != want {
			t.Errorf("rollback = %+v, want %+v", res.Rollback, want)
		}
		if n := len(f.notify.Escalations()); n != 0 {
			t.Errorf("a restore that holds raised %d escalations, want none", n)
		}
		if want := (ImageRecord{Current: clusterPair, Previous: clusterPair}); f.ledger.rec != want {
			t.Errorf("record = %+v, want %+v", f.ledger.rec, want)
		}
	})

	t.Run("no read of the running pair is wired: the rollback is refused before the swap", func(t *testing.T) {
		t.Parallel()
		f := newImageFixture(t, ImageRecord{Current: newPair, Previous: clusterPair})
		f.inst.Images.Running = nil
		if _, err := Rollback(context.Background(), f.inst.Images, passes); err == nil {
			t.Fatal("Rollback = nil error, want the restore to a pair with no commit refused")
		}
		if len(f.swap.applied) != 0 {
			t.Errorf("applied = %+v, want no swap when the restore cannot be confirmed", f.swap.applied)
		}
		if n := len(f.notify.Escalations()); n != 1 {
			t.Errorf("escalations = %d, want 1", n)
		}
	})

	t.Run("the running pair read fails: the rollback is escalated, not recorded as running", func(t *testing.T) {
		t.Parallel()
		f := newImageFixture(t, ImageRecord{Current: newPair, Previous: clusterPair})
		f.inst.Images.Running = &podDeployment{swap: f.swap, err: errors.New("kubectl for the running pair read exited 1")}
		if _, err := Rollback(context.Background(), f.inst.Images, passes); err == nil {
			t.Fatal("Rollback = nil error, want the unreadable Deployment escalated")
		}
		if want := (ImageRecord{Current: newPair, Previous: clusterPair}); f.ledger.rec != want {
			t.Errorf("record = %+v, want %+v untouched", f.ledger.rec, want)
		}
		if n := len(f.notify.Escalations()); n != 1 {
			t.Errorf("escalations = %d, want 1", n)
		}
	})

	t.Run("the Deployment moves back: the rollback holds and the pair is recorded as running", func(t *testing.T) {
		t.Parallel()
		f := newImageFixture(t, ImageRecord{Current: newPair, Previous: clusterPair})
		f.inst.Images.Running = &podDeployment{swap: f.swap, before: newPair}
		prev, err := Rollback(context.Background(), f.inst.Images, passes)
		if err != nil {
			t.Fatalf("Rollback: %v", err)
		}
		if prev != clusterPair {
			t.Errorf("rolled back to %+v, want %+v", prev, clusterPair)
		}
		if want := (ImageRecord{Current: clusterPair, Previous: clusterPair}); f.ledger.rec != want {
			t.Errorf("record = %+v, want %+v", f.ledger.rec, want)
		}
		if n := len(f.notify.Escalations()); n != 0 {
			t.Errorf("escalations = %d, want none", n)
		}
	})
}

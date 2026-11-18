package update

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/imagevalues"
)

// clusterBdPair is the pair a cluster with a bd gateway runs before a pod
// install: the gateway image a restore puts back, and no commit to name a build
// with.
var clusterBdPair = ImagePair{
	Daemon:  imgRef("gastown-daemon", "5"),
	Polecat: imgRef("gastown-polecat", "6"),
	Bd:      imgRef("bd-gateway", "4"),
}

// gatewayPair is the pair a pod install's values file names, with a gateway digest
// distinct from the one clusterBdPair runs.
var gatewayPair = ImagePair{
	Daemon:  newPair.Daemon,
	Polecat: newPair.Polecat,
	Bd:      imgRef("bd-gateway", "7"),
	Commit:  newPair.Commit,
}

// gatewayFixture is a digest install over a cluster that runs a bd gateway: the
// values file names gatewayPair, the cluster pair is read from the fake
// Deployment, and the fake swap tracks the gateway image it put in force.
func gatewayFixture(t *testing.T) *imageFixture {
	t.Helper()
	f := newImageFixture(t, ImageRecord{})
	if err := imagevalues.Write(f.path, imagevalues.Values{
		Daemon: gatewayPair.Daemon, Polecat: gatewayPair.Polecat, Bd: gatewayPair.Bd, Commit: gatewayPair.Commit,
	}); err != nil {
		t.Fatalf("write the values file: %v", err)
	}
	f.inst.Images.Running = &fakeRunningPair{pair: clusterBdPair}
	f.swap.bd = clusterBdPair.Bd
	return f
}

// TestDigestInstallerGatewayStepFailureRestoresAllThree: a gateway rollout that
// never reports ready fails the install at the gateway step, before the daemon
// moves. The run ends failed naming that step, the recorded previous pair goes
// back over all three images, and the rollback record says so.
func TestDigestInstallerGatewayStepFailureRestoresAllThree(t *testing.T) {
	t.Parallel()
	f := gatewayFixture(t)
	f.swap.gatewayFail = map[string]error{gatewayPair.Bd: errors.New("the bd gateway's rollout status exited 1")}

	res, err := f.inst.Install(context.Background(), gatewayPair.Commit)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if ClassifyInstall(res) != ResultFailed || res.Exit == 0 {
		t.Fatalf("result = %+v, want a failed install with a nonzero exit", res)
	}
	for _, want := range []string{"the bd gateway step failed", "rollout status exited 1"} {
		if !strings.Contains(res.Reason, want) {
			t.Errorf("reason = %q, want it to name the gateway step: %q missing", res.Reason, want)
		}
	}
	if want := (RollbackRecord{Check: CheckGateway, Restored: clusterBdPair}); res.Rollback == nil || *res.Rollback != want {
		t.Fatalf("rollback = %+v, want %+v with the gateway step named and all three images restored", res.Rollback, want)
	}
	if got := f.swap.applied; len(got) != 2 || got[0] != gatewayPair || got[1] != clusterBdPair {
		t.Errorf("applied = %+v, want the pair, then the cluster pair put back", got)
	}
	if got := f.swap.bd; got != clusterBdPair.Bd {
		t.Errorf("the cluster's gateway image = %q, want the previous %q put back", got, clusterBdPair.Bd)
	}
	if want := (ImageRecord{Current: clusterBdPair, Previous: clusterBdPair}); f.ledger.rec != want {
		t.Errorf("record after the restore = %+v, want %+v", f.ledger.rec, want)
	}
	if n := len(f.notify.Escalations()); n != 0 {
		t.Errorf("a restore that holds raised %d escalations, want none", n)
	}
}

// TestDigestInstallerGatewayDaemonFailureRestoresAllThree: a daemon step failure
// after the gateway step held restores all three images, the gateway image the
// swap had already put in force included, and names the daemon's rollout, not the
// gateway step that passed.
func TestDigestInstallerGatewayDaemonFailureRestoresAllThree(t *testing.T) {
	t.Parallel()
	f := gatewayFixture(t)
	f.swap.fail = map[string]error{gatewayPair.Daemon: errors.New("the daemon's patch exited 1")}

	res, err := f.inst.Install(context.Background(), gatewayPair.Commit)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if ClassifyInstall(res) != ResultFailed || res.Exit == 0 {
		t.Fatalf("result = %+v, want a failed install with a nonzero exit", res)
	}
	if strings.Contains(res.Reason, "bd gateway step") {
		t.Errorf("reason = %q, want the daemon's rollout named, since the gateway step held", res.Reason)
	}
	if want := (RollbackRecord{Check: CheckRollout, Restored: clusterBdPair}); res.Rollback == nil || *res.Rollback != want {
		t.Fatalf("rollback = %+v, want %+v with all three images restored", res.Rollback, want)
	}
	if got := f.swap.applied; len(got) != 2 || got[0] != gatewayPair || got[1] != clusterBdPair {
		t.Errorf("applied = %+v, want the pair, then the cluster pair put back", got)
	}
	if got := f.swap.bd; got != clusterBdPair.Bd {
		t.Errorf("the cluster's gateway image = %q, want the previous %q put back", got, clusterBdPair.Bd)
	}
	if want := (ImageRecord{Current: clusterBdPair, Previous: clusterBdPair}); f.ledger.rec != want {
		t.Errorf("record after the restore = %+v, want %+v", f.ledger.rec, want)
	}
	if n := len(f.notify.Escalations()); n != 0 {
		t.Errorf("a restore that holds raised %d escalations, want none", n)
	}
}

// TestGatewayStepErrorKeepsItsMessage: a gateway step's failure keeps the step's
// message and unwraps to the failure underneath, so an install can name the
// gateway step and still carry what failed.
func TestGatewayStepErrorKeepsItsMessage(t *testing.T) {
	t.Parallel()
	inner := errors.New("the bd gateway's rollout status exited 1")
	err := GatewayStepError{Err: inner}
	if err.Error() != inner.Error() {
		t.Errorf("Error() = %q, want %q", err.Error(), inner.Error())
	}
	if !errors.Is(err, inner) {
		t.Errorf("errors.Is(err, inner) = false, want true")
	}
}

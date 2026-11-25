package cmd

import (
	"errors"
	"iter"
	"reflect"
	"testing"

	"github.com/steveyegge/gastown/internal/polecat"
)

// seatLockFake is the idle-reuse fake with the seat-pick lock, recording the
// order of the lock, idle read, reuse, unlock and the post-loop Get.
type seatLockFake struct {
	idlePolecatReuseFake
	lockErr error
	events  []string
}

func (f *seatLockFake) LockSeatPick() (func(), error) {
	f.events = append(f.events, "lock")
	if f.lockErr != nil {
		return nil, f.lockErr
	}
	return func() { f.events = append(f.events, "unlock") }, nil
}

func (f *seatLockFake) IdlePolecats() (iter.Seq[*polecat.Polecat], error) {
	f.events = append(f.events, "idle")
	return f.idlePolecatReuseFake.IdlePolecats()
}

func (f *seatLockFake) ReuseIdlePolecat(name string, opts polecat.AddOptions) (*polecat.Polecat, error) {
	f.events = append(f.events, "reuse")
	return f.idlePolecatReuseFake.ReuseIdlePolecat(name, opts)
}

func (f *seatLockFake) Get(name string) (*polecat.Polecat, error) {
	f.events = append(f.events, "get")
	return f.idlePolecatReuseFake.Get(name)
}

// TestSeatLockOrder guards gt-qy0s27: the pool path takes the seat-pick lock
// before the idle read and releases it when the reuse loop ends, on every exit.
func TestSeatLockOrder(t *testing.T) {
	t.Parallel()
	seat := []*polecat.Polecat{{Name: "alpha"}}
	tests := []struct {
		name       string
		seats      []*polecat.Polecat
		lockErr    error
		wantEvents []string
		wantInfo   bool
		wantErr    bool
	}{
		{"reuse succeeds", seat, nil, []string{"lock", "idle", "reuse", "unlock", "get"}, true, false},
		{"no idle seat still unlocks", nil, nil, []string{"lock", "idle", "unlock"}, false, false},
		{"lock error refuses before idle read", seat, errors.New("flock busy"), []string{"lock"}, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fake := &seatLockFake{idlePolecatReuseFake: idlePolecatReuseFake{seats: tt.seats}, lockErr: tt.lockErr}

			info, err := reuseIdlePolecatForSlingWith(fake, fakeIdleReuseEnv(), "rig",
				SlingSpawnOptions{HookBead: "gt-next"}, func() polecat.RespawnMark { return polecat.RespawnMark{} })

			if !reflect.DeepEqual(fake.events, tt.wantEvents) {
				t.Errorf("events = %v, want %v", fake.events, tt.wantEvents)
			}
			if (info != nil) != tt.wantInfo || (err != nil) != tt.wantErr {
				t.Errorf("info = %+v, err = %v; want info=%v error=%v", info, err, tt.wantInfo, tt.wantErr)
			}
			if tt.lockErr != nil && !errors.Is(err, tt.lockErr) {
				t.Errorf("err = %v, want it to wrap %v", err, tt.lockErr)
			}
		})
	}
}

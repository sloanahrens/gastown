package cmd

import (
	"errors"
	"iter"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/dispatch"
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

// TestSeatLockManagerImplementsLocker guards the seam the pool path uses: the
// real polecat manager must offer LockSeatPick, or the optional assertion
// silently skips the lock in production.
func TestSeatLockManagerImplementsLocker(t *testing.T) {
	t.Parallel()
	var mgr idlePolecatReuse = &polecat.Manager{}
	if _, ok := mgr.(seatPickLocker); !ok {
		t.Fatal("*polecat.Manager does not implement seatPickLocker, so the pool pick would take no seat-pick lock")
	}
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
			if tt.lockErr != nil && (!errors.Is(err, tt.lockErr) || !strings.Contains(err.Error(), dispatch.SlingRefusalMarker)) {
				t.Errorf("err = %v, want it to wrap %v behind %s", err, tt.lockErr, dispatch.SlingRefusalMarker)
			}
		})
	}
}

// TestSeatLockSkipped guards gt-k222qq: a named seat and an available rework
// seat take no seat-pick lock, because the seat is chosen before the pool pick.
// A rework whose seat is unavailable falls to the pool, which takes the lock.
func TestSeatLockSkipped(t *testing.T) {
	t.Parallel()
	const alphaBranch = "polecat/alpha/gt-rw1+muu1"
	seat := &polecat.Polecat{Name: "alpha", Branch: alphaBranch, State: polecat.StateIdle}
	tests := []struct {
		name     string
		opts     SlingSpawnOptions
		seats    map[string]*reworkSeat
		wantLock bool
	}{
		{"named seat takes no lock", SlingSpawnOptions{Name: "alpha", HookBead: "gt-next"}, nil, false},
		{"rework preference takes no lock", SlingSpawnOptions{HookBead: "gt-rw1"},
			map[string]*reworkSeat{"gt-rw1": {PolecatName: "alpha", Branch: alphaBranch}}, false},
		{"unavailable rework seat falls to the pool and locks", SlingSpawnOptions{HookBead: "gt-rw1"},
			map[string]*reworkSeat{"gt-rw1": {PolecatName: "beta", Branch: "polecat/beta/gt-rw1+muu1"}}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fake := &seatLockFake{idlePolecatReuseFake: idlePolecatReuseFake{idle: seat, seats: []*polecat.Polecat{seat}}}

			info, err := reuseIdlePolecatForSlingWith(fake, reworkIdleReuseEnv(tt.seats), "rig",
				tt.opts, func() polecat.RespawnMark { return polecat.RespawnMark{} })

			if err != nil || info == nil {
				t.Fatalf("info = %+v, err = %v; want a reused seat", info, err)
			}
			if got := slices.Contains(fake.events, "lock"); got != tt.wantLock {
				t.Errorf("took the seat-pick lock = %v, want %v (events %v)", got, tt.wantLock, fake.events)
			}
		})
	}
}

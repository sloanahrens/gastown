//go:build !windows

package testutil

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestHostContainerSlotCaps pins the host-wide Dolt container cap's one
// load-bearing guarantee (gt-elvf4): the pool holds at most
// doltContainerConcurrency tokens, so no more than that many containers run at
// once. It asserts the bound on the pool's capacity and the buffer fill/drain
// cycle using plain blocking sends and receives on the package-level
// doltContainerSlots var (no select, no goroutine): those primitives this
// runtime executes correctly, while a blocked-waiter goroutine or a
// default-branch select on a full channel do not.
//
// The pool is an empty buffered channel of capacity doltContainerConcurrency:
// holders are accounted for by tokens sitting in the buffer. Sending one token
// per holder fills the buffer to exactly the cap (the bound); draining it
// frees every slot.
func TestHostContainerSlotCaps(t *testing.T) {
	want := doltContainerConcurrency()
	if got := cap(doltContainerSlots); got != want {
		t.Fatalf("doltContainerSlots cap = %d, want %d (the host-wide cap)", got, want)
	}
	// A fresh make(chan struct{}, n) is empty: no holders, nothing in the buffer.
	if got := len(doltContainerSlots); got != 0 {
		t.Fatalf("doltContainerSlots len at init = %d, want 0 (no holders yet)", got)
	}

	// Simulate want holders each taking a slot: the buffer fills to exactly the
	// cap. A (want+1)-th token would have to block on a full buffer, which is
	// the cap doing its job — we do not attempt it (it would hang the test).
	for i := 0; i < want; i++ {
		doltContainerSlots <- struct{}{}
	}
	if got := len(doltContainerSlots); got != want {
		t.Fatalf("after want holders, len = %d, want %d (buffer holds exactly the cap)", got, want)
	}

	// Release every slot in turn; the buffer empties and all slots are free.
	for i := 0; i < want; i++ {
		<-doltContainerSlots
	}
	if got := len(doltContainerSlots); got != 0 {
		t.Fatalf("after releasing all %d slots, len = %d, want 0", want, got)
	}
}

// TestRequireDockerSlotBounded_TimesOutRatherThanHangs pins the fix for the
// deadlock the first attempt at gt-elvf4 was rejected for: a pool with no
// free slot must not block its caller forever (see doltContainerSlotWait).
// The pool starts empty (no token to hand out, matching an exhausted cap)
// and doltContainerSlotWait is shrunk so the test proves the deadline fires
// without spending the real 5 minutes.
func TestRequireDockerSlotBounded_TimesOutRatherThanHangs(t *testing.T) {
	origSlots := doltContainerSlots
	origWait := doltContainerSlotWait
	t.Cleanup(func() {
		doltContainerSlots = origSlots
		doltContainerSlotWait = origWait
	})
	doltContainerSlots = make(chan struct{}, 1) // empty: no free slot
	doltContainerSlotWait = 20 * time.Millisecond

	_, err := requireDockerSlotBounded(context.Background())
	if err == nil {
		t.Fatal("requireDockerSlotBounded() on an exhausted pool returned nil error, want a bounded timeout")
	}
}

// TestDoltContainerConcurrency reads the dial: unset, invalid, and valid env
// values resolve to the right pool size.
func TestDoltContainerConcurrency(t *testing.T) {
	tests := []struct {
		name string
		env  string
		set  bool
		want int
	}{
		{name: "unset takes the default", want: doltContainerConcurrencyDefault},
		{name: "zero falls back to the default", set: true, env: "0", want: doltContainerConcurrencyDefault},
		{name: "negative falls back to the default", set: true, env: "-3", want: doltContainerConcurrencyDefault},
		{name: "non-numeric falls back to the default", set: true, env: "abc", want: doltContainerConcurrencyDefault},
		{name: "positive override wins", set: true, env: "2", want: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.set {
				t.Setenv(doltContainerConcurrencyEnv, tt.env)
			} else {
				_ = os.Unsetenv(doltContainerConcurrencyEnv)
			}
			if got := doltContainerConcurrency(); got != tt.want {
				t.Errorf("doltContainerConcurrency() = %d, want %d", got, tt.want)
			}
		})
	}
}

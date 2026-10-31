package dispatch

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/estop"
	"github.com/steveyegge/gastown/internal/ring"
	"github.com/steveyegge/gastown/internal/ring/ringtest"
)

// ringKey is the key the Ring is signed and checked under in these tests. It is
// handed to operatorHold directly, so no test reads the process environment.
var ringKey = []byte("dispatch-ring-key")

func testKey(string) ([]byte, error) { return ringKey, nil }

// A town with no ring section holds nothing. Absent = off lands with slice 9,
// after the proving step (slice 8): until then a town that was never forged
// keeps dispatching.
func TestRingHold_AbsentRingChangesNothing(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	if reason := operatorHold(townRoot, testKey, time.Now()); reason != "" {
		t.Fatalf("operatorHold on a town with no ring = %q, want \"\"", reason)
	}
}

func TestRingHold_WanedRingParksWithNoHoldFileOrEstop(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	ringtest.Waned(t, townRoot, ringKey, time.Now())
	reason := operatorHold(townRoot, testKey, time.Now())
	if !strings.Contains(reason, "the Ring waned at") {
		t.Fatalf("operatorHold under a waned Ring = %q, want the wane named", reason)
	}
	if got := ring.Hold(townRoot, testKey, time.Now()); got != reason {
		t.Fatalf("ring.Hold = %q, want the same reason operatorHold gives (%q)", got, reason)
	}
}

func TestRingHold_ValidRingDoesNotPark(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	ringtest.Valid(t, townRoot, ringKey, time.Now())
	if reason := operatorHold(townRoot, testKey, time.Now()); reason != "" {
		t.Fatalf("operatorHold under a valid Ring = %q, want \"\"", reason)
	}
}

func TestRingHold_ValidRingStillParksWithTheHoldFile(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	ringtest.Valid(t, townRoot, ringKey, time.Now())
	if err := os.WriteFile(HoldFilePath(townRoot), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if reason := operatorHold(townRoot, testKey, time.Now()); !strings.Contains(reason, "operator dispatch hold present") {
		t.Fatalf("operatorHold under a valid Ring and a hold file = %q, want the hold file", reason)
	}
}

func TestRingHold_ValidRingStillParksWithTheTownEstop(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	ringtest.Valid(t, townRoot, ringKey, time.Now())
	if err := estop.Activate(townRoot, estop.TriggerManual, "test"); err != nil {
		t.Fatal(err)
	}
	if reason := operatorHold(townRoot, testKey, time.Now()); !strings.Contains(reason, "town ESTOP active") {
		t.Fatalf("operatorHold under a valid Ring and an ESTOP = %q, want the ESTOP", reason)
	}
}

// A ring that cannot be verified holds with the reason (gt-ifijm): here the
// key the settings name does not resolve.
func TestRingHold_UnresolvableKeyHolds(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	ringtest.Valid(t, townRoot, ringKey, time.Now())
	unset := func(string) ([]byte, error) { return nil, errors.New("ring.key names GT_RING_KEY, which is unset") }
	if reason := operatorHold(townRoot, unset, time.Now()); !strings.Contains(reason, "cannot be verified") {
		t.Fatalf("operatorHold with an unresolvable ring key = %q, want it held", reason)
	}
}

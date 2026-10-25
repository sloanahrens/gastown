package townconfig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/config"
)

// A dispatch hold is written to the rig's registry entry and read back as the
// dispatcher sees it: the holder, the reason, and the command that lifts it.
// A second hold keeps the first record, the way a park does (gt-vty1v).
func TestDispatchHoldRegistryField(t *testing.T) {
	t.Parallel()
	root := copyLiveTown(t)
	since := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)

	kept, err := Hold(root, "gastown", config.RigDispatchHold{Since: since, By: "sloan", Reason: "drain the landing backlog"})
	if err != nil {
		t.Fatalf("Hold = %v", err)
	}
	if !kept.Since.Equal(since) || kept.By != "sloan" || kept.Reason != "drain the landing backlog" {
		t.Fatalf("Hold returned %+v", kept)
	}
	held, err := DispatchHoldState(root, "gastown")
	if err != nil || held == nil || !held.Since.Equal(since) || held.Reason != "drain the landing backlog" {
		t.Fatalf("DispatchHoldState(gastown) = %+v, %v", held, err)
	}
	if reason := DispatchHeldReason(root, "gastown"); !strings.Contains(reason, "drain the landing backlog") ||
		!strings.Contains(reason, "gt rig release gastown") {
		t.Errorf("DispatchHeldReason(gastown) = %q, want the reason and the release command", reason)
	}
	// The record lives under the documented key, so a hand-written file and the
	// command's file are the same shape.
	data, err := os.ReadFile(filepath.Join(root, FileRigs))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"dispatch_hold"`) {
		t.Errorf("%s carries no dispatch_hold key:\n%s", FileRigs, data)
	}

	again, err := Hold(root, "gastown", config.RigDispatchHold{Since: since.Add(time.Hour), By: "mayor", Reason: "other"})
	if err != nil || again.By != "sloan" {
		t.Errorf("second Hold = %+v, %v; want the first record kept", again, err)
	}

	if reason := DispatchHeldReason(root, "om"); reason != "" {
		t.Errorf("DispatchHeldReason(om) = %q; another rig's hold must not hold it", reason)
	}

	was, err := Release(root, "gastown")
	if err != nil || !was {
		t.Fatalf("Release = %v, %v", was, err)
	}
	if held, err := DispatchHoldState(root, "gastown"); held != nil || err != nil {
		t.Errorf("DispatchHoldState after Release = %+v, %v; want not held", held, err)
	}
	if was, err := Release(root, "gastown"); err != nil || was {
		t.Errorf("second Release = %v, %v; want not held", was, err)
	}
}

// The dispatch hold and the park record are independent: holding a parked rig
// leaves it parked with its original record, releasing the hold leaves it
// parked, and holding an unparked rig does not park it (gt-vty1v).
func TestDispatchHoldLeavesParkStateAlone(t *testing.T) {
	t.Parallel()
	root := copyLiveTown(t)
	parked, err := ParkState(root, "mango")
	if err != nil || parked == nil {
		t.Fatalf("ParkState(mango) = %+v, %v; the fixture rig must be parked", parked, err)
	}
	if _, err := Hold(root, "mango", config.RigDispatchHold{Since: time.Now().UTC(), By: "sloan", Reason: "landing backlog"}); err != nil {
		t.Fatalf("Hold(mango) = %v", err)
	}
	after, err := ParkState(root, "mango")
	if err != nil || after == nil || !after.Since.Equal(parked.Since) || after.Reason != parked.Reason {
		t.Errorf("ParkState(mango) after Hold = %+v, %v; want the fixture park record kept", after, err)
	}
	if _, err := Release(root, "mango"); err != nil {
		t.Fatalf("Release(mango) = %v", err)
	}
	if after, err := ParkState(root, "mango"); err != nil || after == nil {
		t.Errorf("ParkState(mango) after Release = %+v, %v; the rig must still be parked", after, err)
	}

	if _, err := Hold(root, "gastown", config.RigDispatchHold{Since: time.Now().UTC(), By: "sloan"}); err != nil {
		t.Fatalf("Hold(gastown) = %v", err)
	}
	if p, err := ParkState(root, "gastown"); p != nil || err != nil {
		t.Errorf("ParkState(gastown) after Hold = %+v, %v; a hold must not park the rig", p, err)
	}
	// A park written while the rig is held keeps the hold: both records are the
	// rig's, and each command owns one.
	if _, err := Park(root, "gastown", config.RigParked{Since: time.Now().UTC(), By: "sloan"}); err != nil {
		t.Fatalf("Park(gastown) = %v", err)
	}
	if held, err := DispatchHoldState(root, "gastown"); held == nil || err != nil {
		t.Errorf("DispatchHoldState(gastown) after Park = %+v, %v; the hold must survive a park", held, err)
	}
}

// A town with no rig registry has no hold records, so dispatch is not held by
// this record; a registry that is present and does not load cannot rule the
// hold out, so it reads as held (gt-vty1v).
func TestDispatchHoldState_RegistryReadFailsClosed(t *testing.T) {
	t.Parallel()
	bare := t.TempDir()
	if held, err := DispatchHoldState(bare, "gastown"); held != nil || err != nil {
		t.Errorf("DispatchHoldState with no registry = %+v, %v; want not held", held, err)
	}
	if reason := DispatchHeldReason(bare, "gastown"); reason != "" {
		t.Errorf("DispatchHeldReason with no registry = %q, want no hold", reason)
	}

	root := copyLiveTown(t)
	if err := os.WriteFile(filepath.Join(root, FileRigs), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := DispatchHoldState(root, "gastown"); err == nil {
		t.Fatal("DispatchHoldState read a broken registry as no hold")
	}
	if reason := DispatchHeldReason(root, "gastown"); !strings.Contains(reason, "treating as held") {
		t.Errorf("DispatchHeldReason on a broken registry = %q, want a fail-closed hold", reason)
	}
}

// A rig the registry does not carry cannot be held: gt rig hold says so
// instead of writing a record nothing reads.
func TestHold_UnknownRig(t *testing.T) {
	t.Parallel()
	root := copyLiveTown(t)
	if _, err := Hold(root, "nosuchrig", config.RigDispatchHold{Since: time.Now().UTC()}); err == nil {
		t.Fatal("Hold(nosuchrig) wrote a record for an unregistered rig")
	}
	if was, err := Release(root, "nosuchrig"); err == nil || was {
		t.Errorf("Release(nosuchrig) = %v, %v; want an error and no release", was, err)
	}
}

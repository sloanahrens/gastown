package cmd

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/townconfig"
)

// A hold lands on exactly the named rig, names the reason, and is what the
// dispatcher reads; releasing it lifts that rig's hold alone (gt-vty1v).
func TestHoldAndReleaseRigs(t *testing.T) {
	t.Parallel()
	town := parkTown(t)
	var out bytes.Buffer
	rec := config.RigDispatchHold{Since: time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC), By: "sloan", Reason: "drain the landing backlog"}
	if err := holdRigs(&out, town, []string{"testrig"}, rec); err != nil {
		t.Fatalf("holdRigs = %v\n%s", err, out.String())
	}
	held, err := townconfig.DispatchHoldState(town, "testrig")
	if err != nil || held == nil || held.Reason != "drain the landing backlog" {
		t.Fatalf("DispatchHoldState(testrig) = %+v, %v", held, err)
	}
	if reason := townconfig.DispatchHeldReason(town, "otherrig"); reason != "" {
		t.Errorf("DispatchHeldReason(otherrig) = %q; the hold must cover testrig alone", reason)
	}
	if !strings.Contains(out.String(), "drain the landing backlog") || !strings.Contains(out.String(), "sloan") {
		t.Errorf("hold output = %q, want the record it wrote", out.String())
	}

	out.Reset()
	if err := releaseRigs(&out, town, []string{"testrig", "otherrig"}); err != nil {
		t.Fatalf("releaseRigs = %v\n%s", err, out.String())
	}
	if held, err := townconfig.DispatchHoldState(town, "testrig"); held != nil || err != nil {
		t.Errorf("DispatchHoldState(testrig) after release = %+v, %v; want not held", held, err)
	}
	if !strings.Contains(out.String(), "released") || !strings.Contains(out.String(), "was not held") {
		t.Errorf("release output = %q, want one release and one not-held line", out.String())
	}
}

// Holding one unknown rig among known ones reports it and still holds the
// known rig, the way park handles a partial failure.
func TestHoldRigs_UnknownRig(t *testing.T) {
	t.Parallel()
	town := parkTown(t)
	var out bytes.Buffer
	err := holdRigs(&out, town, []string{"nosuchrig", "testrig"}, config.RigDispatchHold{Since: time.Now().UTC(), By: "sloan"})
	if err == nil {
		t.Fatalf("holdRigs with an unknown rig = nil error\n%s", out.String())
	}
	if held, _ := townconfig.DispatchHoldState(town, "testrig"); held == nil {
		t.Error("the known rig was not held because the unknown one failed")
	}
	if !strings.Contains(out.String(), "nosuchrig") || !strings.Contains(err.Error(), "failed to hold 1 rig(s)") {
		t.Errorf("output = %q, err = %v; want the unknown rig named and the failure counted", out.String(), err)
	}
}

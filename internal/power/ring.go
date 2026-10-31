package power

import (
	"errors"
	"fmt"
	"time"

	"github.com/steveyegge/gastown/internal/ring"
)

// The Ring's part of the power states (gt-6mlik.7). A town with no ring section
// is untouched: no state writes, forges or removes a Ring, and each reports
// that sentinels are all it has. Forging stays the operator's act (gt ring
// forge), so no state creates a Ring that is not already there.

// noRingLine is what a state prints for a town with no ring section.
const noRingLine = "no Ring forged; sentinels only"

// ringKey resolves the Ring's signing key through the injected reader. The key
// is handed to ring.Sign and never printed.
func (e *engine) ringKey() ([]byte, error) {
	if e.o.RingKey == nil {
		return nil, errors.New("no ring key reader is configured")
	}
	return e.o.RingKey(e.o.TownRoot)
}

// checkRingRenewable is full's refusal, run before any state changes. A ring
// section that exists but cannot be renewed would leave a holding Ring behind a
// full that reports success, so full refuses instead. A town with no section
// passes.
func (e *engine) checkRingRenewable() error {
	present, err := ring.Present(e.o.TownRoot)
	if err != nil {
		return exitError(ExitUsage, "full refuses: the Ring cannot be read (%v); nothing was changed", err)
	}
	if !present {
		return nil
	}
	key, err := e.ringKey()
	if err != nil {
		return exitError(ExitUsage, "full refuses: the Ring is set but its key is unavailable (%v); nothing was changed", err)
	}
	st, err := ring.Read(e.o.TownRoot, key, e.o.Now())
	if err != nil {
		return exitError(ExitUsage, "full refuses: the Ring cannot be read (%v); nothing was changed", err)
	}
	if st.Kind == ring.Absent {
		return exitError(ExitUsage, "full refuses: the Ring cannot be verified (%s); nothing was changed. Replace it with 'gt ring forge' or drop it with 'gt ring remove'", st.Reason)
	}
	return nil
}

// renewRing is full's step, run once the infra is up: a Ring the town keeps
// gets a fresh lease, valid for ring.Lease from now.
func (e *engine) renewRing() error {
	present, err := ring.Present(e.o.TownRoot)
	if err != nil {
		return fmt.Errorf("reading the Ring: %w", err)
	}
	if !present {
		e.say("%s", noRingLine)
		return nil
	}
	if e.o.DryRun {
		fmt.Fprintf(e.o.Out, "  [dry-run] renew the Ring: valid for %s from now\n", ring.Lease)
		return nil
	}
	expires := e.o.Now().Add(ring.Lease)
	if err := e.leaseRing(expires); err != nil {
		return fmt.Errorf("renewing the Ring; hold left in place: %w", err)
	}
	e.say("Ring renewed: valid until %s", expires.UTC().Format(time.RFC3339))
	return nil
}

// waneRing is standby's step, run once the ESTOP is set: the Ring's lease ends
// now, so the signed record reads Waned. A failure is returned for the caller
// to report, since standby proceeds regardless: stopping is safe.
func (e *engine) waneRing() error {
	present, err := ring.Present(e.o.TownRoot)
	if err != nil {
		return fmt.Errorf("reading the Ring: %w", err)
	}
	if !present {
		e.say("%s", noRingLine)
		return nil
	}
	if e.o.DryRun {
		fmt.Fprintf(e.o.Out, "  [dry-run] wane the Ring: its lease ends now\n")
		return nil
	}
	if err := e.leaseRing(e.o.Now()); err != nil {
		return err
	}
	e.say("Ring waned: its lease ended at %s", e.o.Now().UTC().Format(time.RFC3339))
	return nil
}

// removeRing is off's step, run after the drain and before the daemon is
// booted out: the Ring's section is deleted, so the town reads Absent. It needs
// the key as gt ring remove does, and leaves the Ring in place without it. A
// failure is returned for the caller to report, since off proceeds regardless.
func (e *engine) removeRing() error {
	present, err := ring.Present(e.o.TownRoot)
	if err != nil {
		return fmt.Errorf("reading the Ring: %w", err)
	}
	if !present {
		e.say("%s", noRingLine)
		return nil
	}
	if e.o.DryRun {
		fmt.Fprintf(e.o.Out, "  [dry-run] remove the Ring from mayor/town.json\n")
		return nil
	}
	if _, err := e.ringKey(); err != nil {
		return fmt.Errorf("the Ring cannot be removed: %w", err)
	}
	if _, err := ring.Remove(e.o.TownRoot); err != nil {
		return fmt.Errorf("removing the Ring: %w", err)
	}
	e.say("Ring removed")
	return nil
}

// leaseRing signs a record that expires at expires, forged now by the session
// actor, and stores it as the Ring.
func (e *engine) leaseRing(expires time.Time) error {
	key, err := e.ringKey()
	if err != nil {
		return fmt.Errorf("the Ring cannot be signed: %w", err)
	}
	rec, err := ring.Sign(ring.Record{ForgedAt: e.o.Now(), ExpiresAt: expires, ForgedBy: e.o.Actor}, key)
	if err != nil {
		return fmt.Errorf("the Ring cannot be signed: %w", err)
	}
	return ring.Write(e.o.TownRoot, rec)
}

// reportRingLeft ends the output with one loud line when standby's wane or
// off's removal failed, so a stop that stands is not read as a Ring that moved
// with it.
func (e *engine) reportRingLeft(err error) {
	if err == nil {
		return
	}
	e.warn("WARNING: the Ring was not changed: %v. The stop stands; fix the cause and re-run to change the Ring.", err)
}

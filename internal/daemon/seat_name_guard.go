package daemon

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/sandbox"
	"github.com/steveyegge/gastown/internal/session"
)

// A sling's seat name is its sandbox's name (session.PolecatSessionName), and
// the gateway keeps a sandbox in a terminal failure phase after the seat behind
// it is gone, so the freed name dies again at every session start until the
// sandbox is deleted (gt-zapy97; the release path's own end deletes it,
// gt-wgim24). The guard reads one gateway listing, keeps those names out of the
// dispatcher's draw, and raises the item that names the sandbox to delete.

const (
	// seatSandboxStaleKeyPrefix is the attention item's key prefix; the
	// sandbox's name follows it.
	seatSandboxStaleKeyPrefix = "seat-sandbox-stale:"

	// seatSandboxListTimeout bounds the one gateway call the attention
	// collector makes for the listing, so a gateway that hangs costs its read
	// and not the collectors behind it.
	seatSandboxListTimeout = 30 * time.Second
)

// StaleSeatSandbox is one gateway sandbox in a terminal failure phase.
type StaleSeatSandbox struct {
	// Sandbox is the sandbox's name, which is also its seat's session name:
	// the rig's prefix and the polecat's name.
	Sandbox string
	// Phase is the phase the gateway reports.
	Phase string
	// Since is when the guard first read the sandbox in this phase. It is
	// memory, not state on disk: a daemon restart starts the clock again.
	Since time.Time
}

// Line is the dispatcher's log line for the sandbox: the seat name it keeps out
// of the draw, the phase, and the command that frees the name. The dispatcher
// logs it once per tick, so a name drawn for three beads does not name the same
// sandbox three times.
func (f StaleSeatSandbox) Line() string {
	return fmt.Sprintf("seat sandbox %s is in phase %s: its seat name is not slung to until the sandbox is deleted (openshell sandbox delete %s)",
		f.Sandbox, f.Phase, f.Sandbox)
}

// SeatNameGuard is the dispatcher's answer to which seat names a sling may not
// draw, read from a gateway sandbox listing. A sandbox leaves only by being
// deleted, so a name the guard keeps out stays out until a listing no longer
// has the sandbox.
type SeatNameGuard struct {
	// stale is keyed by sandbox name, so a listing that names a sandbox twice
	// answers once.
	stale map[string]StaleSeatSandbox
}

// NewSeatNameGuard returns a guard holding the failing sandboxes of one
// listing, each first seen at now.
func NewSeatNameGuard(entries []sandbox.Entry, now time.Time) *SeatNameGuard {
	g := &SeatNameGuard{}
	g.Observe(entries, now)
	return g
}

// Observe replaces the guard's picture with entries. A sandbox that has left
// the listing is forgotten, so its name can be drawn again once the gateway no
// longer holds it; a sandbox still failing keeps the time it was first seen in
// its current phase.
func (g *SeatNameGuard) Observe(entries []sandbox.Entry, now time.Time) {
	next := make(map[string]StaleSeatSandbox, len(entries))
	for _, e := range entries {
		name := strings.TrimSpace(e.Name)
		phase := strings.TrimSpace(e.Phase)
		if name == "" || !session.SandboxPhaseFailed(phase) {
			continue
		}
		since := now
		if prev, ok := g.stale[name]; ok && strings.EqualFold(prev.Phase, phase) && !prev.Since.After(now) {
			since = prev.Since
		}
		next[name] = StaleSeatSandbox{Sandbox: name, Phase: phase, Since: since}
	}
	g.stale = next
}

// Findings returns the stale sandboxes, sorted by name.
func (g *SeatNameGuard) Findings() []StaleSeatSandbox {
	out := make([]StaleSeatSandbox, 0, len(g.stale))
	for _, f := range g.stale {
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Sandbox < out[j].Sandbox })
	return out
}

// Stale reports the finding for a sandbox by name.
func (g *SeatNameGuard) Stale(sandboxName string) (StaleSeatSandbox, bool) {
	f, ok := g.stale[strings.TrimSpace(sandboxName)]
	return f, ok
}

// Polecats returns the polecat names the stale sandboxes name in the rig with
// rigPrefix, sorted. A sandbox is named for its seat's session
// (session.PolecatSessionName), so the name after the prefix and a hyphen is
// the polecat the pool must not draw. A sandbox named outside the rig's prefix
// names no seat of it and is left alone.
func (g *SeatNameGuard) Polecats(rigPrefix string) []string {
	if rigPrefix == "" {
		return nil
	}
	head := rigPrefix + "-"
	var out []string
	for _, f := range g.Findings() {
		name, ok := strings.CutPrefix(f.Sandbox, head)
		if !ok || name == "" || strings.Contains(name, "/") {
			continue
		}
		out = append(out, name)
	}
	return out
}

// Items is the queue's view: one high item per stale sandbox, keyed
// seat-sandbox-stale:<sandbox>, naming the phase and how long it has been in
// it. The item clears itself when the sandbox leaves the listing, so deleting
// the sandbox needs no code path here.
func (g *SeatNameGuard) Items(now time.Time) []attention.Item {
	findings := g.Findings()
	out := make([]attention.Item, 0, len(findings))
	for _, f := range findings {
		age := now.Sub(f.Since)
		if age < 0 {
			age = 0
		}
		out = append(out, attention.Item{
			Key:      seatSandboxStaleKeyPrefix + f.Sandbox,
			Kind:     attention.KindSeatSandboxStale,
			Severity: attention.SeverityHigh,
			Summary: fmt.Sprintf("seat sandbox %s has been in phase %s for %s; the spec dispatcher slings to no seat name whose sandbox is stale, so the name stays unusable until the sandbox is deleted (openshell sandbox delete %s)",
				f.Sandbox, f.Phase, parkedAge(age), f.Sandbox),
		})
	}
	return out
}

// seatSandboxList reads the gateway's sandbox listing; nil is the openshell CLI
// on its active gateway.
type seatSandboxList func(ctx context.Context) ([]sandbox.Entry, error)

// gatewaySeatSandboxes is the production listing, bounded by
// seatSandboxListTimeout.
func gatewaySeatSandboxes(ctx context.Context) ([]sandbox.Entry, error) {
	ctx, cancel := context.WithTimeout(ctx, seatSandboxListTimeout)
	defer cancel()
	return (&sandbox.CLI{}).List(ctx)
}

// collectStaleSeatSandboxes raises the item of every gateway sandbox in a
// terminal failure phase, in a town that names a sandbox profile: a town with
// none runs no gateway and is asked nothing. The guard is the daemon's, so a
// sandbox that stays stale keeps the time it was first read so; a profile or
// listing read that fails returns the error and no items, so the previous
// tick's items are kept rather than cleared by an unanswered read.
func (s *attentionSources) collectStaleSeatSandboxes(ctx context.Context) ([]attention.Item, error) {
	profiles, err := s.d.diskMaintenanceSeams().sandboxes()
	if err != nil {
		return nil, fmt.Errorf("reading the town's sandbox profiles: %w", err)
	}
	if len(profiles) == 0 {
		// A town with no sandbox profile runs no gateway, so no seat's
		// sandbox can be stale and the gateway is not asked.
		return nil, nil
	}
	list := s.sandboxList
	if list == nil {
		list = gatewaySeatSandboxes
	}
	entries, err := list(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing gateway sandboxes: %w", err)
	}
	if s.d.seatSandboxGuard == nil {
		s.d.seatSandboxGuard = &SeatNameGuard{}
	}
	s.d.seatSandboxGuard.Observe(entries, s.now)
	return s.d.seatSandboxGuard.Items(s.now), nil
}

// seatSandboxStaleItem picks the stale-seat items out of the kind's previous
// items, so a failed collector keeps only its own (collectAttention).
func seatSandboxStaleItem(it attention.Item) bool {
	return it.Kind == attention.KindSeatSandboxStale && strings.HasPrefix(it.Key, seatSandboxStaleKeyPrefix)
}

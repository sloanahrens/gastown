package stale

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/doltserver"
	"github.com/steveyegge/gastown/internal/jev"
	"github.com/steveyegge/gastown/internal/jev/battery"
)

// DefaultMaxUSDPerDay is the daily cap when the config names none. What the
// cap counts is battery.Cap's to say.
const DefaultMaxUSDPerDay = 1.0

// closeLanded is the close reason the molecule close writes on a bead whose
// change landed.
const closeLanded = "landed"

// closeLandedOn starts the close reason the landing worker writes on a bead
// whose change landed: "Landed on <target>", then the landing record.
const closeLandedOn = "Landed on"

// closeLandedCommit starts the line of the landing record that names the
// commit the change landed as. A reason whose first line is that line is the
// record with its "Landed on" line dropped, so it still says landed.
const closeLandedCommit = "landed_commit:"

// closeSuperseded and closeDuplicate start the reasons that say a bead lost
// to another.
const (
	closeSuperseded = "superseded"
	closeDuplicate  = "duplicate"
)

// closeLandedOnLower is closeLandedOn folded once, so ClosingVerdict does not
// fold it on every close of every bead the pass reads.
var closeLandedOnLower = strings.ToLower(closeLandedOn)

// Verdict is what a closed bead's close reason says about the bead. The stale
// pass, the scorecard's stale and dispatch batteries and the attention
// battery's mechanism labeling all read a reason through ClosingVerdict, so
// they cannot disagree about the same bead.
type Verdict int

const (
	// VerdictNone is a reason that reports neither: a wontfix, a plain close,
	// an empty reason.
	VerdictNone Verdict = iota
	// VerdictLanded is a landing's reason: the bare word the molecule close
	// writes, the landing worker's "Landed on <target>" record, or that
	// record's landed_commit line standing as the whole reason.
	VerdictLanded
	// VerdictSuperseded is a reason that says the bead was superseded or is a
	// duplicate of another.
	VerdictSuperseded
)

// ClosingVerdict reads a close reason.
//
// Only the reason's first line decides, because that line is the close verb a
// person or a producer writes. The lines under it are detail, and a detail
// line can quote a landing record — a superseding close that says what the
// landing it replaces landed as — so reading the whole reason would let that
// quote call the bead landed. A person writes "Landed on main" as readily as
// the landing worker does, and "Superseded by ..." as "superseded by ...", so
// every match ignores case.
func ClosingVerdict(reason string) Verdict {
	verb, _, _ := strings.Cut(strings.TrimSpace(reason), "\n")
	verb = strings.ToLower(strings.TrimSpace(verb))
	switch {
	case verb == closeLanded,
		strings.HasPrefix(verb, closeLandedOnLower),
		strings.HasPrefix(verb, closeLandedCommit):
		return VerdictLanded
	case strings.HasPrefix(verb, closeSuperseded),
		strings.HasPrefix(verb, closeDuplicate):
		return VerdictSuperseded
	}
	return VerdictNone
}

// IsLandedReason reports whether a close reason is a landing's. It is
// ClosingVerdict's landed case, for callers that ask only that.
func IsLandedReason(reason string) bool {
	return ClosingVerdict(reason) == VerdictLanded
}

// hmSearchLimit is how many hits one hm query asks for.
const hmSearchLimit = 3 * MaxLanded

// Settings is the operator's configuration for the pass, read from
// patrols.jev_stale.
type Settings struct {
	// MaxUSDPerDay is the daily cap. Zero takes DefaultMaxUSDPerDay.
	MaxUSDPerDay float64
	// USDPerBillionTokens prices the ledger's tokens. It is required: the
	// ledger stores tokens, never dollars.
	USDPerBillionTokens float64

	// Scrub scrubs the state before it is sent: the patrol's scrub key
	// sets it (gt-aqczg).
	Scrub bool
}

// Open wires the production pass for a town: the Jev client writing to the
// usage ledger under the Caller name, the hm search when hm is installed, the
// beads of each rig, and the spend read from the same ledger. warnings
// receives the ledger's own warnings and may be nil.
func Open(townRoot string, rigs []string, s Settings, warnings io.Writer) (*Pass, error) {
	price := s.USDPerBillionTokens
	if !(price > 0 && !math.IsInf(price, 0)) {
		return nil, fmt.Errorf("jev_stale.usd_per_billion_tokens is required and must be above zero, not %v", price)
	}
	capUSD := s.MaxUSDPerDay
	if capUSD == 0 {
		capUSD = DefaultMaxUSDPerDay
	}
	if !(capUSD > 0 && !math.IsInf(capUSD, 0)) {
		return nil, fmt.Errorf("jev_stale.max_usd_per_day must be above zero, not %v", capUSD)
	}

	client, err := jev.NewClient(jev.BatteryOptions(townRoot, Caller, warnings, s.Scrub)...)
	if err != nil {
		return nil, fmt.Errorf("opening the Jev client: %w", err)
	}

	town := battery.Cap{Ledger: jev.UsageLedgerPath(townRoot), MaxUSDPerDay: capUSD, USDPerBillionTokens: price}

	var searcher Searcher
	if _, err := exec.LookPath("hm"); err == nil {
		searcher = hmSearcher{}
	}

	return &Pass{
		Asker:    client,
		Searcher: searcher,
		Beads:    townBeads{townRoot: townRoot},
		Rigs:     rigs,
		LogPath:  LogPath(townRoot),
		Allows:   func() (bool, error) { return town.Allows(time.Now) },
	}, nil
}

// hmSearcher runs hm query for a rig.
type hmSearcher struct {
	// run executes hm with args and returns its stdout. Nil runs the hm on the
	// PATH.
	run func(ctx context.Context, args ...string) ([]byte, error)
}

// Search runs `hm query --json` and returns the bead IDs among its hits, best
// first. The text follows a -- so a title that begins with a dash is not read
// as a flag.
func (h hmSearcher) Search(ctx context.Context, rig, text string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, hmTimeout)
	defer cancel()
	run := h.run
	if run == nil {
		run = runHM
	}
	out, err := run(ctx, "query", "--json",
		"--rig", rig, "--limit", strconv.Itoa(hmSearchLimit), "--", text)
	if err != nil {
		return nil, fmt.Errorf("hm query: %w", err)
	}
	return parseHitIDs(out)
}

func runHM(ctx context.Context, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, "hm", args...).Output()
}

// parseHitIDs decodes hm query --json output and keeps the bead hits, in the
// order hm ranked them.
func parseHitIDs(out []byte) ([]string, error) {
	var raw []struct {
		Kind string `json:"kind"`
		ID   string `json:"id"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("hm query: decoding hits: %w", err)
	}
	ids := make([]string, 0, len(raw))
	for _, r := range raw {
		if r.Kind == "bead" && r.ID != "" {
			ids = append(ids, r.ID)
		}
	}
	return ids, nil
}

// townBeads reads each rig's beads through the beads client. It never writes.
type townBeads struct {
	townRoot string
}

// Open lists a rig's open beads. A rig with no beads directory is an error for
// that rig alone.
func (t townBeads) Open(rig string) ([]*beads.Issue, error) {
	b, err := t.rig(rig)
	if err != nil {
		return nil, err
	}
	return b.List(beads.ListOptions{Status: "open"})
}

// Landed lists a rig's beads closed as landed at or after since.
func (t townBeads) Landed(rig string, since time.Time) ([]*beads.Issue, error) {
	b, err := t.rig(rig)
	if err != nil {
		return nil, err
	}
	closed, err := b.List(beads.ListOptions{Status: "closed", ClosedAfter: since})
	if err != nil {
		return nil, err
	}
	return landedAmong(closed), nil
}

// landedAmong keeps the closed beads whose close reason is a landing's.
func landedAmong(closed []*beads.Issue) []*beads.Issue {
	var out []*beads.Issue
	for _, c := range closed {
		if IsLandedReason(c.CloseReason) {
			out = append(out, c)
		}
	}
	return out
}

func (t townBeads) rig(rig string) (*beads.Beads, error) {
	dir := doltserver.FindRigBeadsDir(t.townRoot, rig)
	if dir == "" {
		return nil, fmt.Errorf("no beads directory for rig %s", rig)
	}
	return beads.New(dir), nil
}

// Ensure the production dependencies satisfy the pass's seams.
var (
	_ Asker    = (*jev.Client)(nil)
	_ Searcher = hmSearcher{}
	_ Beads    = townBeads{}
)

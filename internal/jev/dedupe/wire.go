package dedupe

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os/exec"
	"strconv"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/doltserver"
	"github.com/steveyegge/gastown/internal/jev"
	"github.com/steveyegge/gastown/internal/jev/battery"
)

// DefaultMaxUSDPerDay is the daily cap when the config names none. What the
// cap counts is battery.Cap's to say.
const DefaultMaxUSDPerDay = 1.0

// hmSearchLimit is how many hits one hm query asks for before the candidate
// filter runs.
const hmSearchLimit = 20

// hmTimeout bounds one hm query.
const hmTimeout = time.Minute

// Settings is the operator's configuration for the pass, read from
// patrols.jev_dedupe.
type Settings struct {
	// MaxUSDPerDay is the daily cap. Zero takes DefaultMaxUSDPerDay.
	MaxUSDPerDay float64
	// USDPerBillionTokens prices the ledger's tokens. It is required: the
	// ledger stores tokens, never dollars.
	USDPerBillionTokens float64

	// Scrub scrubs the state before it is sent: the patrol's scrub key
	// sets it (gt-aqczg).
	Scrub bool

	// ScrubCheck runs the shadow scrub check on a sample of requests. The
	// scrub_check key sets it (gt-jb20i).
	ScrubCheck bool
}

// Open wires the production pass for a town: the Jev client writing to the
// usage ledger under the Caller name, the hm search, the rig's beads, and the
// spend read from the same ledger. warnings receives the ledger's own
// warnings and may be nil.
func Open(townRoot string, s Settings, warnings io.Writer) (*Pass, error) {
	return open(townRoot, s, warnings, jev.NewClient)
}

// open is Open with the Jev client's constructor passed in, so a test can
// supply a key without one in the environment.
func open(townRoot string, s Settings, warnings io.Writer, newClient func(...jev.Option) (*jev.Client, error)) (*Pass, error) {
	price := s.USDPerBillionTokens
	if !(price > 0 && !math.IsInf(price, 0)) {
		return nil, fmt.Errorf("jev_dedupe.usd_per_billion_tokens is required and must be above zero, not %v", price)
	}
	capUSD := s.MaxUSDPerDay
	if capUSD == 0 {
		capUSD = DefaultMaxUSDPerDay
	}
	if !(capUSD > 0 && !math.IsInf(capUSD, 0)) {
		return nil, fmt.Errorf("jev_dedupe.max_usd_per_day must be above zero, not %v", capUSD)
	}

	town := battery.Cap{Ledger: jev.UsageLedgerPath(townRoot), MaxUSDPerDay: capUSD, USDPerBillionTokens: price}
	client, err := newClient(jev.BatteryOptions(townRoot, Caller, warnings, jev.BatteryScrub{
		Scrub:      s.Scrub,
		ScrubCheck: s.ScrubCheck,
		Allows:     town.AllowsNow,
	})...)
	if err != nil {
		return nil, fmt.Errorf("opening the Jev client: %w", err)
	}

	beadsDir := doltserver.FindRigBeadsDir(townRoot, RigName)
	if beadsDir == "" {
		return nil, fmt.Errorf("no beads directory for rig %s", RigName)
	}

	return &Pass{
		Asker:      client,
		Searcher:   hmSearcher{rig: RigName},
		Beads:      rigBeads{b: beads.New(beadsDir)},
		LogPath:    LogPath(townRoot),
		CursorPath: CursorPath(townRoot),
		Allows:     town.AllowsNow,
	}, nil
}

// hmSearcher runs hm query for a rig.
type hmSearcher struct {
	rig string
	// run executes hm with args and returns its stdout. Nil runs the hm on the
	// PATH.
	run func(ctx context.Context, args ...string) ([]byte, error)
}

// Search runs `hm query --json` and keeps the fields a pass reads. The text
// follows a -- so a title that begins with a dash is not read as a flag.
func (h hmSearcher) Search(ctx context.Context, text string) ([]Hit, error) {
	ctx, cancel := context.WithTimeout(ctx, hmTimeout)
	defer cancel()
	run := h.run
	if run == nil {
		run = runHM
	}
	out, err := run(ctx, "query", "--json",
		"--rig", h.rig, "--limit", strconv.Itoa(hmSearchLimit), "--", text)
	if err != nil {
		return nil, fmt.Errorf("hm query: %w", err)
	}
	return parseHits(out)
}

func runHM(ctx context.Context, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, "hm", args...).Output()
}

// parseHits decodes hm query --json output. A hit that names no matched chunk
// (a commit, say, or a bead hm gave no provenance for) keeps an empty Field,
// and the candidate filter drops it: a pass offers a bead only when it knows
// the match was on the title or the description.
func parseHits(out []byte) ([]Hit, error) {
	var raw []struct {
		Kind            string `json:"kind"`
		ID              string `json:"id"`
		ChildProvenance *struct {
			Field string `json:"field"`
		} `json:"child_provenance"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("hm query: decoding hits: %w", err)
	}
	hits := make([]Hit, 0, len(raw))
	for _, r := range raw {
		h := Hit{Kind: r.Kind, ID: r.ID}
		if r.ChildProvenance != nil {
			h.Field = r.ChildProvenance.Field
		}
		hits = append(hits, h)
	}
	return hits, nil
}

// rigBeads reads a rig's beads through the beads client. It never writes.
type rigBeads struct {
	b *beads.Beads
}

// Since lists every bead created at or after since, in every status.
func (r rigBeads) Since(since time.Time) ([]*beads.Issue, error) {
	return r.b.List(beads.ListOptions{Status: "all", CreatedAfter: since})
}

// Show returns one bead. A bead that does not exist is an error.
func (r rigBeads) Show(id string) (*beads.Issue, error) {
	return r.b.Show(id)
}

// Ensure the production dependencies satisfy the pass's seams.
var (
	_ Asker    = (*jev.Client)(nil)
	_ Searcher = hmSearcher{}
	_ Beads    = rigBeads{}
)

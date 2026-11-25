package main

import (
	"encoding/json"
	"regexp"
	"slices"
	"strconv"
	"time"

	"github.com/steveyegge/gastown/internal/tiersweep"
)

// Progress is one sweep's tiers as far as the summary lines it has printed
// show them. The runner rewrites it after each tier so the dashboard can show
// the tier running now beside the finished ones.
type Progress struct {
	Rig     string         `json:"rig"`
	SHA     string         `json:"sha"`
	Tiers   []ProgressTier `json:"tiers"`
	Running string         `json:"running"`

	// order is the cycle's tiers in the order the sweep runs them.
	order []string
}

// ProgressTier is one finished tier, read from its summary line.
type ProgressTier struct {
	Tier    string  `json:"tier"`
	Verdict string  `json:"verdict"`
	Passed  int     `json:"passed"`
	Failed  int     `json:"failed"`
	Skipped int     `json:"skipped"`
	Secs    float64 `json:"secs"`
}

// summaryRE matches the line tiersweep.Summary prints. progress_test.go feeds
// Summary's own output, so a change to that format fails there, not here.
var summaryRE = regexp.MustCompile(`^` + regexp.QuoteMeta(tiersweep.Name) +
	`: (\S+) (GREEN|RED) passed=(\d+) failed=(\d+) skipped=(\d+)(?: failed: .+?)? \(logs .+\) in (\S+)$`)

// NewProgress starts the record of a sweep of sha in rig that runs tiers in
// the given order. Running is the first tier and no tier has finished yet.
func NewProgress(rig, sha string, tiers []string) *Progress {
	p := &Progress{Rig: rig, SHA: sha, Tiers: []ProgressTier{}, order: slices.Clone(tiers)}
	p.advance()
	return p
}

// Feed reads one line of the sweep's output. A summary line for a tier of the
// cycle that has not finished appends that tier and reports true. Any other
// line, a summary for a tier outside the cycle, or a second summary for a tier
// already finished changes nothing and reports false. Running then stays on the
// first tier until a summary arrives, so a sweep whose lines do not match the
// format shows no finished tiers. Feed is not safe for concurrent use.
func (p *Progress) Feed(line string) (changed bool) {
	m := summaryRE.FindStringSubmatch(line)
	if m == nil || !slices.Contains(p.order, m[1]) || p.finished(m[1]) {
		return false
	}
	t, ok := parseTier(m)
	if !ok {
		return false
	}
	p.Tiers = append(p.Tiers, t)
	p.advance()
	return true
}

// JSON returns the record as the bytes the progress file is written with.
func (p *Progress) JSON() ([]byte, error) {
	return json.Marshal(p)
}

// advance sets Running to the first cycle tier not yet finished, or to empty
// once every tier has finished.
func (p *Progress) advance() {
	p.Running = ""
	for _, tier := range p.order {
		if !p.finished(tier) {
			p.Running = tier
			return
		}
	}
}

func (p *Progress) finished(tier string) bool {
	for _, t := range p.Tiers {
		if t.Tier == tier {
			return true
		}
	}
	return false
}

// parseTier reads the fields of a summary line matched by summaryRE. It reports
// false when a count or the duration does not parse, so a bad line is refused
// rather than recorded as zero.
func parseTier(m []string) (ProgressTier, bool) {
	passed, errP := strconv.Atoi(m[3])
	failed, errF := strconv.Atoi(m[4])
	skipped, errS := strconv.Atoi(m[5])
	dur, errD := time.ParseDuration(m[6])
	if errP != nil || errF != nil || errS != nil || errD != nil {
		return ProgressTier{}, false
	}
	return ProgressTier{
		Tier:    m[1],
		Verdict: m[2],
		Passed:  passed,
		Failed:  failed,
		Skipped: skipped,
		Secs:    dur.Seconds(),
	}, true
}

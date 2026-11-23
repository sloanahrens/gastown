package townhealth

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/atomicfile"
	"github.com/steveyegge/gastown/internal/constants"
)

// FileName is the health file under the town runtime directory.
const FileName = "townhealth.json"

// Path is <town>/.runtime/townhealth.json, the one file the daemon writes
// every tick and gt status reads.
func Path(townRoot string) string {
	return filepath.Join(constants.TownRuntimePath(townRoot), FileName)
}

// Write replaces the health file with r, atomically.
func Write(townRoot string, r Report) error {
	return atomicfile.EnsureDirAndWriteJSON(Path(townRoot), r)
}

// Read loads the health file.
func Read(townRoot string) (Report, error) {
	var r Report
	data, err := os.ReadFile(Path(townRoot))
	if err != nil {
		return r, err
	}
	// The landing_passes key decodes beside the report, shadowing the report's
	// own tag: a report from a daemon before the field has no key, and reads as
	// passes unknown, not zero (gt-u98suh).
	var f struct {
		Report
		LandingPasses *int `json:"landing_passes"`
	}
	if err := json.Unmarshal(data, &f); err != nil {
		return r, fmt.Errorf("parse %s: %w", FileName, err)
	}
	r = f.Report
	if f.LandingPasses != nil {
		r.LandingPasses = *f.LandingPasses
	}
	r.PassesKnown = f.LandingPasses != nil
	if r.At.IsZero() {
		return r, fmt.Errorf("%s has no tick time", FileName)
	}
	return r, nil
}

// LineMax is the length the one-line signal is held to.
const LineMax = 120

// Effective is the verdict a reader acts on: the report's, or unknown when
// the report is staleAfter old or older (the daemon stopped writing it).
func Effective(r Report, now time.Time, staleAfter time.Duration) Verdict {
	if now.Sub(r.At) >= staleAfter {
		return VerdictUnknown
	}
	return r.Verdict
}

// Line renders the one-line signal: the verdict, the tick age, the landing
// pause when one stands (it is the reason no landing starts, so it is named
// whatever the verdict), and then
//
//   - green: the day's landing count;
//   - stale: the verdict the stale report carried;
//   - otherwise: every non-green field, worst first, as key=value, with
//     [R] after a RECORDED field and [?] after an UNKNOWN one (whose value
//     is "?" and is left out), and "+N" for fields that do not fit in
//     LineMax. The sweep field's cause replaces its value, which both red
//     reasons write as "red" (gt-6k21z3).
//
// Keys are the field name, /rig on per-rig fields and :subject on per-tick
// and per-seat ones.
func Line(r Report, now time.Time, staleAfter time.Duration) string {
	v := Effective(r, now, staleAfter)
	head := fmt.Sprintf("%s tick %s ago", strings.ToUpper(string(v)), Short(now.Sub(r.At)))
	if r.LandingPaused != "" {
		head += fmt.Sprintf(" (landings paused: %s)", r.LandingPaused)
	}
	if v != r.Verdict {
		return fmt.Sprintf("%s (stale; last %s)", head, r.Verdict)
	}
	if v == Green {
		landed := "?"
		if r.Landed != nil {
			landed = fmt.Sprint(*r.Landed)
		}
		return fmt.Sprintf("%s, %s landed/24h", head, landed)
	}
	var bad []Field
	for _, f := range r.Fields {
		if f.Verdict != Green {
			bad = append(bad, f)
		}
	}
	sort.SliceStable(bad, func(i, j int) bool { return bad[i].Verdict.rank() > bad[j].Verdict.rank() })
	line := head + ":"
	for i, f := range bad {
		tok := " " + token(f)
		rest := len(bad) - i - 1
		more := ""
		if rest > 0 {
			more = fmt.Sprintf(" +%d", rest)
		}
		if len(line)+len(tok)+len(more) > LineMax && i > 0 {
			return line + fmt.Sprintf(" +%d", len(bad)-i)
		}
		line += tok
	}
	return line
}

// token renders one non-green field for the line.
func token(f Field) string {
	if f.Name == FieldSweep {
		return sweepToken(f)
	}
	// The red hold's kinds are a list to scan, so they keep their spacing
	// where every other value is compacted (gt-xq3s9u).
	if f.Name == FieldDispatch && strings.HasPrefix(f.Value, redHoldPrefix) {
		return f.Key() + "=" + f.Value + "[R]"
	}
	switch f.Tag {
	case Unknown:
		return f.Key() + "[?]"
	case Recorded:
		return f.Key() + "=" + compact(f.Value) + "[R]"
	default:
		return f.Key() + "=" + compact(f.Value)
	}
}

// sweepToken renders the sweep field's cause, which its value does not carry:
// both red reasons write "red" and put the reason in the detail (gt-6k21z3).
func sweepToken(f Field) string {
	if f.Tag == Unknown {
		return f.Key() + "=UNKNOWN"
	}
	tok := f.Key() + "=" + sweepCause(f.Detail)
	if f.Tag == Recorded {
		tok += "[R]"
	}
	return tok
}

// sweepCause turns the sweep evaluation's detail into the line's cause: "2 red
// cycles in a row" reads "RED 2 cycles" and "no green sweep in 3h" reads
// "no green 3h". Any other detail is shown as it stands (gt-6k21z3).
func sweepCause(detail string) string {
	if n, ok := strings.CutSuffix(detail, " red cycles in a row"); ok {
		return "RED " + n + " cycles"
	}
	if age, ok := strings.CutPrefix(detail, "no green sweep in "); ok {
		return "no green " + sweepAgeHours(age)
	}
	return detail
}

// sweepAgeHours states a stale sweep's age in whole hours: the report writes it
// in its largest unit ("3h", "2d"), so a day converts and a token it cannot
// read is left as it stands.
func sweepAgeHours(age string) string {
	days, ok := strings.CutSuffix(age, "d")
	if !ok {
		return age
	}
	n, err := strconv.Atoi(days)
	if err != nil {
		return age
	}
	return strconv.Itoa(n*24) + "h"
}

// compact joins a value's words so the line splits only between fields.
func compact(s string) string { return strings.Join(strings.Fields(s), "_") }

// Lines renders every field, one per line, for gt status.
func Lines(r Report, now time.Time, staleAfter time.Duration) []string {
	out := []string{Line(r, now, staleAfter)}
	for _, f := range r.Fields {
		l := fmt.Sprintf("  %-8s %-8s %-32s %s", f.Verdict, f.Tag, f.Key(), f.Value)
		if f.Detail != "" {
			l += " — " + f.Detail
		}
		out = append(out, strings.TrimRight(l, " "))
	}
	return out
}

package townhealth

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
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
	if err := json.Unmarshal(data, &r); err != nil {
		return r, fmt.Errorf("parse %s: %w", FileName, err)
	}
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

// Line renders the one-line signal: the verdict, the tick age, and then
//
//   - green: the day's landing count;
//   - stale: the verdict the stale report carried;
//   - otherwise: every non-green field, worst first, as key=value, with
//     [R] after a RECORDED field and [?] after an UNKNOWN one (whose value
//     is "?" and is left out), and "+N" for fields that do not fit in
//     LineMax.
//
// Keys are the field name, /rig on per-rig fields and :subject on per-tick
// and per-seat ones.
func Line(r Report, now time.Time, staleAfter time.Duration) string {
	v := Effective(r, now, staleAfter)
	head := fmt.Sprintf("%s tick %s ago", strings.ToUpper(string(v)), Short(now.Sub(r.At)))
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
	switch f.Tag {
	case Unknown:
		return f.Key() + "[?]"
	case Recorded:
		return f.Key() + "=" + compact(f.Value) + "[R]"
	default:
		return f.Key() + "=" + compact(f.Value)
	}
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

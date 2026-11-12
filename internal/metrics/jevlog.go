package metrics

import (
	"errors"
	"fmt"
	"io/fs"

	"github.com/steveyegge/gastown/internal/jev/dispatch"
)

// jevLogSource names the Jev dispatch log in the footer's gaps.
const jevLogSource = "jev:dispatch.jsonl"

// LoadJevFirst reads the Jev dispatch log at path and keeps the earliest line
// per bead by its logged time, the first answer Jev gave for the bead.
//
// A missing log is not an error: the landings' Jev columns stay empty and
// dropped names the gap once. A log that cannot be read stops the extract,
// so an unreadable log never passes for one with no answers. A line that does
// not parse is skipped and named as a gap.
func LoadJevFirst(path string, dropped *Dropped) (map[string]dispatch.Line, error) {
	lines, malformed, err := dispatch.ReadAll(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		dropped.Add(jevLogSource, "no dispatch log: jev_fits20 and jev_size are empty")
		return map[string]dispatch.Line{}, nil
	case err != nil:
		return nil, fmt.Errorf("jev dispatch log: %w", err)
	}
	if malformed > 0 {
		dropped.Add(jevLogSource, fmt.Sprintf("%d lines did not parse and were skipped", malformed))
	}
	return dispatch.FirstPerBead(lines), nil
}

// jevCells are a landing's Jev columns: the first line's probability that the
// bead fits in twenty minutes and its size score. Both are empty for a bead the
// log holds no line for, so a missing answer never reads as a zero.
func jevCells(first map[string]dispatch.Line, bead string) (fits, size string) {
	l, ok := first[bead]
	if !ok {
		return "", ""
	}
	return FormatFloat(l.Answers.FitsTwentyMinutes), FormatFloat(l.Answers.Size.Score)
}

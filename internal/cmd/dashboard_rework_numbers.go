package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/dashboard"
)

// The rework program's document (docs/design/rework-program.md, lever 6). The
// daemon's weekly step writes it to <town>/.runtime/rework-numbers/latest.json,
// and this reader hands its bytes to the feed unchanged
// (dashboard.State.ReworkNumbers). No panel draws it yet: the key exists so a
// panel can read the program's numbers without a second reader of the ledgers.

const (
	// reworkNumbersDocument is the step's file name in its directory.
	reworkNumbersDocument = "latest.json"

	// reworkNumbersMaxBytes bounds the document a page's state frame carries.
	// The frame is built for every open page, so a run that wrote something
	// pathological cannot bloat them all; a larger document is refused.
	reworkNumbersMaxBytes = 1 << 20
)

// dashReworkNumbersReader reads the rework program's document for the feed. The
// failures it reports are reported through why, once per change.
type dashReworkNumbersReader struct {
	path string
	why  *dashWhyLog
}

// newDashReworkNumbersReader reads town's rework program document.
func newDashReworkNumbersReader(townRoot string) *dashReworkNumbersReader {
	return &dashReworkNumbersReader{
		path: filepath.Join(constants.TownRuntimePath(townRoot), "rework-numbers", reworkNumbersDocument),
	}
}

// withWhy reports a failed read through why, once per change.
func (r *dashReworkNumbersReader) withWhy(why *dashWhyLog) *dashReworkNumbersReader {
	r.why = why
	return r
}

// read returns the document verbatim, or nil when no run has written one or the
// document cannot be passed on. A missing file is a step that has not run yet,
// which is not a failure. A document that is not JSON would make the whole state
// frame unparsable, and one over the cap would bloat it, so both are refused and
// the feed keeps its last good document.
func (r *dashReworkNumbersReader) read() json.RawMessage {
	fh, err := os.Open(r.path)
	if errors.Is(err, fs.ErrNotExist) {
		r.why.note(dashboard.SourceReworkNumbers, "")
		return nil
	}
	if err != nil {
		r.why.note(dashboard.SourceReworkNumbers, dashWhy(err))
		return nil
	}
	defer func() { _ = fh.Close() }()
	// One byte past the cap, so a document exactly at the cap passes and a
	// larger one is known to be larger.
	data, err := io.ReadAll(io.LimitReader(fh, reworkNumbersMaxBytes+1))
	if err != nil {
		r.why.note(dashboard.SourceReworkNumbers, dashWhy(err))
		return nil
	}
	if len(data) > reworkNumbersMaxBytes {
		r.why.note(dashboard.SourceReworkNumbers, fmt.Sprintf("the document is larger than %d bytes", reworkNumbersMaxBytes))
		return nil
	}
	if !json.Valid(data) {
		r.why.note(dashboard.SourceReworkNumbers, "the document is not JSON")
		return nil
	}
	r.why.note(dashboard.SourceReworkNumbers, "")
	return json.RawMessage(data)
}

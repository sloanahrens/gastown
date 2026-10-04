// Package landings reads a rig's append-only landings file: one JSON object
// per landing, written by the D2 landing worker (gt-v4ssj.9) after the
// target's tip is read back.
//
// This package only reads. The writer owns the file's permissions and the
// record's meaning; Record mirrors the writer's JSON keys, and a test pins
// them to a line the writer produces.
package landings

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/constants"
)

// Record is one landing as the landing worker serializes land.LandingRecord.
type Record struct {
	Bead         string    `json:"bead"`
	Rig          string    `json:"rig"`
	Branch       string    `json:"branch"`
	Head         string    `json:"head"`
	Target       string    `json:"target"`
	Base         string    `json:"base"`
	LandedCommit string    `json:"landed_commit"`
	PatchID      string    `json:"patch_id"`
	GateResult   string    `json:"gate_result"`
	OMVerdict    string    `json:"om_verdict"`
	OMScore      float64   `json:"om_score"`
	Route        string    `json:"route"`
	LandedAt     time.Time `json:"landed_at"`
	// The CI fields are the Forgejo gate's verdict on the landing's candidate,
	// written when the rig ran in shadow mode (slice 8) with GateResult
	// holding the local gate that decided instead. Empty on a landing no
	// candidate gate saw.
	CIVerdict   string `json:"ci_verdict,omitempty"`
	CIContext   string `json:"ci_context,omitempty"`
	CICandidate string `json:"ci_candidate,omitempty"`
	CIBranch    string `json:"ci_branch,omitempty"`
	CIRunStatus string `json:"ci_run_status,omitempty"`
}

// Path is <town>/.runtime/landings/<rig>.jsonl, the file the landing worker
// appends to. A rig name that could leave the directory is refused.
func Path(townRoot, rig string) (string, error) {
	if rig == "" || strings.ContainsAny(rig, `/\`) || rig == "." || rig == ".." {
		return "", fmt.Errorf("invalid rig name %q for a landings file", rig)
	}
	return filepath.Join(constants.TownRuntimePath(townRoot), "landings", rig+".jsonl"), nil
}

// Reader tails one landings file by byte offset. Each ReadNew returns the
// complete lines appended since the previous call; a trailing line with no
// newline yet is left for the next call. A file that shrank or was replaced
// is read again from the start. The zero offset reads the whole file.
type Reader struct {
	Path string

	offset int64
	info   os.FileInfo
}

// ReadNew returns the records appended since the last call, and the text of
// any line that is not a record (reported, skipped, never fatal). A missing
// file has no landings yet and is not an error.
func (r *Reader) ReadNew() (recs []Record, bad []string, err error) {
	f, err := os.Open(r.Path)
	if errors.Is(err, os.ErrNotExist) {
		r.offset, r.info = 0, nil
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("opening landings file: %w", err)
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		return nil, nil, fmt.Errorf("checking landings file: %w", err)
	}
	if r.info != nil && (!os.SameFile(r.info, info) || info.Size() < r.offset) {
		r.offset = 0
	}
	r.info = info
	if info.Size() == r.offset {
		return nil, nil, nil
	}
	if _, err := f.Seek(r.offset, io.SeekStart); err != nil {
		return nil, nil, fmt.Errorf("seeking landings file: %w", err)
	}
	data, err := io.ReadAll(io.LimitReader(f, info.Size()-r.offset))
	if err != nil {
		return nil, nil, fmt.Errorf("reading landings file: %w", err)
	}
	end := bytes.LastIndexByte(data, '\n')
	if end < 0 {
		return nil, nil, nil
	}
	r.offset += int64(end + 1)
	for _, line := range bytes.Split(data[:end], []byte{'\n'}) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var rec Record
		if err := json.Unmarshal(line, &rec); err != nil {
			bad = append(bad, string(line))
			continue
		}
		recs = append(recs, rec)
	}
	return recs, bad, nil
}

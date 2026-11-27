// Package rehearsal records one load rehearsal run.
package rehearsal

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/steveyegge/gastown/internal/atomicfile"
)

// Sling outcomes and gate verdicts.
const (
	SlingOK, SlingFailed, SlingKilled   = "ok", "failed", "killed"
	GatePass, GateFail, GateUnavailable = "pass", "fail", "unavailable"
)

type Sling struct {
	Bead, Outcome string
	Seconds       float64
}
type Gate struct {
	Bead, Verdict string
	Seconds       float64
}
type Landing struct {
	Bead             string
	Landed, CIFailed bool
}
type SideRun struct {
	Seconds  float64
	ExitCode int
}
type Load struct {
	Samples      int
	Max1, P90One float64
}
type Record struct {
	RunID, Tag, ChangeSet string
	Start, End            time.Time
	Stopped               bool
	StopReason            string
	CacheBytes            int64
	Interventions         []string
	Slings                []Sling
	Gates                 []Gate
	Landings              []Landing
	FailedCalls, Stranded []string
	SideRuns              []SideRun
	Load                  Load
	SweepRan              bool
}

// Save atomically writes rec to path as indented JSON, leaving an earlier file on failure.
func Save(path string, rec Record) error {
	return atomicfile.WriteJSON(path, rec)
}

// Read decodes the record at path. It refuses a file it cannot open or decode,
// an unknown field, and any content after the first JSON value, and returns no partial record.
func Read(path string) (Record, error) {
	f, err := os.Open(path)
	if err != nil {
		return Record{}, fmt.Errorf("read rehearsal record: %w", err)
	}
	defer f.Close()
	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()
	var rec Record
	if err := dec.Decode(&rec); err != nil {
		return Record{}, fmt.Errorf("read rehearsal record %s: %w", path, err)
	}
	switch _, err := dec.Token(); {
	case err == io.EOF:
	case err != nil:
		return Record{}, fmt.Errorf("read rehearsal record %s: after the first JSON value: %w", path, err)
	default:
		return Record{}, fmt.Errorf("read rehearsal record %s: content after the first JSON value", path)
	}
	return rec, nil
}

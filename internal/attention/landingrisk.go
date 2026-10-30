package attention

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"sync"

	"github.com/steveyegge/gastown/internal/constants"
)

// LandingRiskPath is the log Jev's landing battery writes one line per landing
// to, {ts, bead, rig, head, landed_at, answers:{risk:{score,...}}}. It is the
// same file the battery appends to, so the path and the fields read from it
// live beside the attention readers rather than in the battery's own package
// (gt-b9j3o.2).
func LandingRiskPath(townRoot string) string {
	return filepath.Join(townRoot, constants.DirRuntime, "jev", "landing.jsonl")
}

// LandingRisks is the landing battery's risk answer per risk-path item, keyed
// by the item's key (risk:<bead>:<head[:12]>): the risk level Jev gave that
// landing, higher being riskier. An item with no entry has no answer, which is
// not the same as a risk of zero.
type LandingRisks map[string]float64

// landingLine is the part of one landing log line the risk answers come from.
// The battery's line carries more (the rig, the answers to its other
// questions); a reader that needs those reads the battery's own type.
type landingLine struct {
	Bead    string `json:"bead"`
	Head    string `json:"head"`
	Answers *struct {
		Risk *struct {
			Score *float64 `json:"score"`
		} `json:"risk"`
	} `json:"answers"`
}

// LandingRiskReader returns the newest risk answer for each risk-path item from
// one landing log, keyed by the item's key. It reads only the lines appended
// since its last call, so a reader that runs on every dashboard publish costs
// one stat while the log is quiet, and it starts over when the log shrank or
// was replaced, as ScoreReader does.
//
// It is safe for concurrent use, and each call returns its own map.
type LandingRiskReader struct {
	mu    sync.Mutex
	tail  consumedTail
	risks LandingRisks
}

// NewLandingRiskReader reads the risk answers from the landing log at path.
func NewLandingRiskReader(path string) *LandingRiskReader {
	return &LandingRiskReader{tail: consumedTail{path: path}}
}

// Risks returns the risk answer per risk-path item key as the log stands, and
// the error of a log that exists but could not be read. A missing log is no
// answers and no error: the battery has not run, so no landing has an answer
// yet. An unreadable one returns the answers of the last read beside the error,
// so a caller can name the failed read rather than draw the items as
// unanswered.
func (r *LandingRiskReader) Risks() (LandingRisks, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	got, err := r.tail.next()
	if err != nil {
		return r.copy(), err
	}
	if got.restart {
		r.risks = nil
	}
	if len(got.data) == 0 {
		return r.copy(), nil
	}
	if r.risks == nil {
		r.risks = LandingRisks{}
	}
	for _, raw := range bytes.Split(got.data, []byte("\n")) {
		var l landingLine
		if err := json.Unmarshal(raw, &l); err != nil || l.Bead == "" || l.Head == "" ||
			l.Answers == nil || l.Answers.Risk == nil || l.Answers.Risk.Score == nil {
			continue // a failed call has no answer to rank a landing with
		}
		// The newest line wins: the log is append-only, so a later line for the
		// same landing is the later answer to it.
		r.risks[landingRiskKey(l.Bead, l.Head)] = *l.Answers.Risk.Score
	}
	return r.copy(), nil
}

// copy returns the reader's answers as a map of its own, so a caller holding the
// last result does not see it change under the next read.
func (r *LandingRiskReader) copy() LandingRisks {
	if len(r.risks) == 0 {
		return nil
	}
	out := make(LandingRisks, len(r.risks))
	for key, score := range r.risks {
		out[key] = score
	}
	return out
}

// landingRiskKey is the attention item a landing's answer belongs to: the
// risk-path collector's key, risk:<bead>:<first 12 characters of the head>.
func landingRiskKey(bead, head string) string {
	if len(head) > 12 {
		head = head[:12]
	}
	return "risk:" + bead + ":" + head
}

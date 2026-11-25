package land

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/steveyegge/gastown/internal/atomicfile"
	"github.com/steveyegge/gastown/internal/constants"
)

// The review-ahead store keeps the verdict of a review made before its landing
// asks for one, so the landing can reuse that verdict when nothing it depends
// on has changed (gt-ofzu2e). It keeps the verdict only: om's raw replies go to
// the verdict store (verdictstore.go), which a reader asking "why" needs.

// aheadRecordVersion is the v every record carries, so a reader can tell the
// layout it was written in.
const aheadRecordVersion = 1

// The outcomes a stored review may carry. Put refuses any other spelling, so a
// reader switching on these two never meets a record it cannot trust.
const (
	aheadOutcomeVerdict  = "verdict"
	aheadOutcomeConflict = "conflict"
)

// AheadRecord is one review made ahead of a landing, as the store keeps it.
type AheadRecord struct {
	V int `json:"v"`
	// Bead and Rig name the work the review judged; Put stamps Rig from the store.
	Bead string `json:"bead"`
	Rig  string `json:"rig"`
	// Head is the commit reviewed. Put keeps one record per bead, so a reader
	// compares Head with the head in front of it and never assumes a match.
	Head         string `json:"head"`
	PatchID      string `json:"patch_id"`
	OMVersion    string `json:"om_version"`
	OMConfigHash string `json:"om_config_hash"`
	InputDigest  string `json:"input_digest"`
	Base         string `json:"base"`
	// StartedAt and ReviewedAt bracket the review; ReviewSecs is their span.
	StartedAt  time.Time `json:"started_at"`
	ReviewedAt time.Time `json:"reviewed_at"`
	ReviewSecs float64   `json:"review_secs"`
	// Outcome is "verdict" or "conflict"; Source is "pool" or "inline".
	Outcome string  `json:"outcome"`
	Source  string  `json:"source"`
	Verdict Verdict `json:"verdict"`
	// Rescored says a borderline rejection was re-scored. The rescore detail is
	// not stored: a reader needs only whether the verdict was re-scored.
	Rescored bool `json:"rescored"`
}

// AheadStore keeps one review-ahead record per bead under
// <town>/.runtime/landings/review-ahead/<rig>/<bead>.json.
type AheadStore struct {
	// TownRoot is the town the records belong to; the store lives in its runtime tree.
	TownRoot string
	// Rig is the rig whose landings the store keeps, and the rig every record carries.
	Rig string
	// Now is the store's clock; nil means time.Now. Nothing in this file reads
	// it yet: the reuse age check in a later bead does.
	Now func() time.Time

	// writeFile is the test seam for the write Put makes; nil means atomicfile.WriteFile.
	writeFile func(path string, data []byte, perm os.FileMode) error
}

// TownAheadStore is the review-ahead store for one rig's landings.
func TownAheadStore(townRoot, rig string) *AheadStore {
	return &AheadStore{TownRoot: townRoot, Rig: rig}
}

// Put stores rec as its bead's record, replacing whatever record the bead had
// before, whatever head that record judged. A failed write is returned as an
// error; the atomic write (atomicfile.WriteFile) is what leaves no partial file
// behind.
func (s AheadStore) Put(rec AheadRecord) error {
	if err := rec.refuseToStore(); err != nil {
		return err
	}
	path, err := s.path(rec.Bead)
	if err != nil {
		return err
	}
	rec.V, rec.Rig = aheadRecordVersion, s.Rig
	data, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("encoding review-ahead record: %w", err)
	}
	if err := ensureLandingsDir(filepath.Dir(path)); err != nil {
		return err
	}
	write := s.writeFile
	if write == nil {
		write = atomicfile.WriteFile
	}
	if err := write(path, data, 0o600); err != nil {
		return fmt.Errorf("writing review-ahead record: %w", err)
	}
	return nil
}

// refuseToStore reports whether the store may keep rec: nil when it may, and
// the error naming why not otherwise.
func (rec AheadRecord) refuseToStore() error {
	if !validComponent(rec.Bead) {
		return fmt.Errorf("review-ahead store: refusing bead %q: not one path component", rec.Bead)
	}
	if !isHexSHA(rec.Head) {
		return fmt.Errorf("review-ahead store: refusing head %q: not a lowercase hex sha", rec.Head)
	}
	// Only a verdict om itself reached may be reused. A skip, an error, a
	// waiver, an empty or unknown outcome, or a case-variant name is refused
	// rather than kept by default, so a later reader cannot reuse a review
	// that never decided the work.
	switch rec.Outcome {
	case aheadOutcomeVerdict:
		if rec.Verdict.Verdict == VerdictApprove || rec.Verdict.Verdict == VerdictRequestChanges {
			return nil
		}
	case aheadOutcomeConflict:
		if rec.Verdict.Verdict == "" {
			return nil
		}
	}
	return fmt.Errorf("review-ahead store: refusing outcome %q with verdict %q", rec.Outcome, rec.Verdict.Verdict)
}

// path is the bead's record file. The rig and the bead each stand for one path
// component, so an id that climbs out of the store is refused, not written elsewhere.
func (s AheadStore) path(bead string) (string, error) {
	if s.TownRoot == "" {
		return "", errors.New("review-ahead store: no town root configured")
	}
	if !validComponent(s.Rig) {
		return "", fmt.Errorf("review-ahead store: invalid rig %q", s.Rig)
	}
	if !validComponent(bead) {
		return "", fmt.Errorf("review-ahead store: invalid bead %q", bead)
	}
	return filepath.Join(constants.TownRuntimePath(s.TownRoot), "landings", "review-ahead", s.Rig, bead+".json"), nil
}

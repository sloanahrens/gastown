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
	// readFile is the test seam for the reads Put and Get make; nil means
	// os.ReadFile.
	readFile func(path string) ([]byte, error)
}

// TownAheadStore is the review-ahead store for one rig's landings.
func TownAheadStore(townRoot, rig string) *AheadStore {
	return &AheadStore{TownRoot: townRoot, Rig: rig}
}

// Put stores rec as its bead's record unless the record already stored began
// its review later: a slow review of an old head finishes after a new head's
// review, and only the start can tell them apart. A stored file that does not
// decode is replaced. A failed write is returned as an error; the atomic write
// (atomicfile.WriteFile) is what leaves no partial file behind.
func (s AheadStore) Put(rec AheadRecord) error {
	if err := rec.refuseToStore(); err != nil {
		return err
	}
	path, err := s.path(rec.Bead)
	if err != nil {
		return err
	}
	// The store's tree is checked before the record is read, so a landings dir
	// that is not private is refused as that, not as a record that will not read.
	if err := ensureLandingsDir(filepath.Dir(path)); err != nil {
		return err
	}
	stored, err := s.recordAt(path)
	if err != nil {
		return err
	}
	if stored != nil && stored.StartedAt.After(rec.StartedAt) {
		return nil
	}
	rec.V, rec.Rig = aheadRecordVersion, s.Rig
	data, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("encoding review-ahead record: %w", err)
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

// Get returns the record the store keeps for bead. A bead with no record is
// (zero, false, nil), so a caller with nothing to reuse runs its own review. A
// file the store could not have written, or one that names another bead, is an
// error and no record: a read that cannot be trusted must run a fresh review
// rather than read as a miss.
func (s AheadStore) Get(bead string) (AheadRecord, bool, error) {
	path, err := s.path(bead)
	if err != nil {
		return AheadRecord{}, false, err
	}
	read := s.readFile
	if read == nil {
		read = os.ReadFile
	}
	data, err := read(path)
	// Absence is the checked answer "nothing to reuse", and it is read before
	// the failure branch so it is not mistaken for a read that failed.
	if errors.Is(err, os.ErrNotExist) {
		return AheadRecord{}, false, nil
	}
	if err != nil {
		return AheadRecord{}, false, fmt.Errorf("reading review-ahead record: %w", err)
	}
	var rec AheadRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return AheadRecord{}, false, fmt.Errorf("decoding review-ahead record for %q: %w", bead, err)
	}
	if rec.V != aheadRecordVersion {
		return AheadRecord{}, false, fmt.Errorf("review-ahead store: record for %q has version %d, want %d", bead, rec.V, aheadRecordVersion)
	}
	if rec.Bead != bead {
		return AheadRecord{}, false, fmt.Errorf("review-ahead store: record for %q names bead %q", bead, rec.Bead)
	}
	// The refusals Put applies are the store's own answer to whether it would
	// have written this record, so the reader refuses exactly what Put refuses:
	// a head that is not a sha, or an outcome and verdict om never reached.
	if err := rec.refuseToStore(); err != nil {
		return AheadRecord{}, false, fmt.Errorf("review-ahead store: unusable record for %q: %w", bead, err)
	}
	return rec, true, nil
}

// recordAt returns the record stored at path. A nil record with a nil error
// means there is no record to keep: no file, a path that is not a regular
// file, or a file that does not decode as a record. A regular file that cannot
// be read is an error: it may be the newer record Put exists to keep, so Put
// must not write over it.
func (s AheadStore) recordAt(path string) (*AheadRecord, error) {
	read := s.readFile
	if read == nil {
		read = os.ReadFile
	}
	data, err := read(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		// Only this store writes a record to that path, and it writes a file.
		if info, statErr := os.Stat(path); statErr == nil && !info.Mode().IsRegular() {
			return nil, nil
		}
		return nil, fmt.Errorf("reading review-ahead record: %w", err)
	}
	var rec AheadRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, nil
	}
	return &rec, nil
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

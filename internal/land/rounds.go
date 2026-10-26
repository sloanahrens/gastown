package land

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/steveyegge/gastown/internal/beads"
)

// DefaultRoundsCap bounds Render's output in bytes when the caller names no
// cap: the 16 KiB a landing's rounds file is designed to stay under, small
// enough for om to read whole (gt-yphcl).
const DefaultRoundsCap = 16 << 10

// ErrRoundsCorrupt reports a ledger file that does not parse: a caller treats
// it as a landing with no history and leaves the file for a human (gt-yphcl).
var ErrRoundsCorrupt = errors.New("rounds ledger is unreadable")

// Outcome is what became of one finding a review raised.
type Outcome string

// The outcomes a finding takes. A finding nobody settles stays open, so the
// count of open findings is the rework the landing still owes.
const (
	OutcomeOpen     Outcome = "open"
	OutcomeFixed    Outcome = "fixed"
	OutcomeAccepted Outcome = "accepted"
	OutcomeRebutted Outcome = "rebutted"
)

// valid reports whether o is one of the outcomes the ledger writes.
func (o Outcome) valid() bool {
	switch o {
	case OutcomeOpen, OutcomeFixed, OutcomeAccepted, OutcomeRebutted:
		return true
	}
	return false
}

// Classification is what a later round says about a finding an earlier round
// raised, as om's --prior-findings classification reports it.
type Classification string

const (
	// ClassificationResolved is a finding the later round classified resolved.
	ClassificationResolved Classification = "resolved"
	// ClassificationUnresolved is a finding the later round still sees, or one
	// it did not mention: omission is never a fix.
	ClassificationUnresolved Classification = "unresolved"
	// ClassificationRegressed is a finding that had been fixed and is back.
	ClassificationRegressed Classification = "regressed"
)

// FindingKey identifies a finding across rounds by its id and the file it is
// in: om's ids are known unique only within one review, so the pair is the
// identity and a comment naming a bare id settles every finding that carries
// it (gt-yphcl).
type FindingKey struct {
	File string
	ID   string
}

// String renders the key as a reader names it: file:id, or the bare id for a
// finding om gave no file.
func (k FindingKey) String() string {
	if k.File == "" {
		return k.ID
	}
	return k.File + ":" + k.ID
}

// Key is the finding's identity in the rounds ledger.
func (f Finding) Key() FindingKey { return FindingKey{File: f.Path, ID: f.ID} }

// rebuttalMarker opens a bead comment that records a finding the later round
// should re-judge. It settles like an accepted risk does, so it takes the same
// allowlist: the decision is the overseer's or Sloan's, never the author's.
const rebuttalMarker = "REBUTTAL:"

// RoundsLedger is one landing's record of what each om round raised and what
// became of it, stored at <OutDir>/rounds-<bead>.json, where a round is the
// landing's rework attempt, the number the reviews log records as
// ReviewRecord.Attempt (gt-yphcl). The landing path appends; gt-3l5qy settles
// the history and hands it to om.
type RoundsLedger struct {
	// OutDir is the directory the ledger file lives in.
	OutDir string
	// Bead is the work bead whose rounds the ledger keeps.
	Bead string
	// FS, when set, is the filesystem the ledger reads and writes through.
	// Nil means the host's, so a test drives a write failure with a fake.
	FS LedgerFS
	// Now, when set, is the clock a new round's time is stamped from.
	// Nil means time.Now.
	Now func() time.Time
}

// LedgerFS is the filesystem a rounds ledger needs: read, write, atomic
// replace and a directory that may not exist yet.
type LedgerFS interface {
	ReadFile(name string) ([]byte, error)
	WriteFile(name string, data []byte, perm os.FileMode) error
	Rename(oldpath, newpath string) error
	Remove(name string) error
	MkdirAll(path string, perm os.FileMode) error
}

// osLedgerFS is the host filesystem.
type osLedgerFS struct{}

func (osLedgerFS) ReadFile(name string) ([]byte, error) { return os.ReadFile(name) }
func (osLedgerFS) WriteFile(name string, data []byte, perm os.FileMode) error {
	return os.WriteFile(name, data, perm)
}
func (osLedgerFS) Rename(oldpath, newpath string) error { return os.Rename(oldpath, newpath) }
func (osLedgerFS) Remove(name string) error             { return os.Remove(name) }
func (osLedgerFS) MkdirAll(path string, perm os.FileMode) error {
	return os.MkdirAll(path, perm)
}

// ledgerDoc is the ledger file: every round the landing reached, oldest first.
type ledgerDoc struct {
	Bead   string        `json:"bead"`
	Rounds []ledgerRound `json:"rounds"`
}

// ledgerRound is one om round as stored: the attempt it belongs to, the work
// branch head it was submitted at, and its findings with what became of each.
type ledgerRound struct {
	Round    int             `json:"round"`
	Head     string          `json:"head"`
	At       time.Time       `json:"at"`
	Findings []ledgerFinding `json:"findings"`
}

// ledgerFinding is one finding as stored.
type ledgerFinding struct {
	ID      string  `json:"id"`
	File    string  `json:"file,omitempty"`
	Text    string  `json:"text"`
	Outcome Outcome `json:"outcome"`
}

// key is the finding's identity across rounds.
func (f ledgerFinding) key() FindingKey { return FindingKey{File: f.File, ID: f.ID} }

// renderedRound is one round as Render emits it: the shape gt-3l5qy hands om
// through --rounds-file.
type renderedRound struct {
	Round    int               `json:"round"`
	Head     string            `json:"head"`
	Findings []renderedFinding `json:"findings"`
	// OpenAtLand is how many of the round's findings were still open when the
	// ledger was rendered, the count a landing's unfinished rework shows.
	OpenAtLand int `json:"open_at_land"`
}

// renderedFinding is one finding as Render emits it.
type renderedFinding struct {
	ID      string  `json:"id"`
	File    string  `json:"file,omitempty"`
	Text    string  `json:"text"`
	Outcome Outcome `json:"outcome"`
}

// Append records one round of findings for head, the work branch head the
// round was submitted at, stamped with the injected clock. A round already in
// the ledger is replaced, keeping the outcome every finding it does not
// re-raise still carries: a retry of the same attempt re-reviews the same
// findings, and a settled outcome is the bead's, not the retry's.
func (l *RoundsLedger) Append(round int, head string, findings []Finding) error {
	if round < 1 {
		return fmt.Errorf("rounds ledger: round %d is not a landing attempt", round)
	}
	doc, err := l.load()
	if err != nil {
		return err
	}
	next := ledgerRound{Round: round, Head: head, At: l.now().UTC(), Findings: recordFindings(findings)}
	for i, have := range doc.Rounds {
		if have.Round != round {
			continue
		}
		for j := range next.Findings {
			if out, ok := have.outcome(next.Findings[j].key()); ok {
				next.Findings[j].Outcome = out
			}
		}
		next.At = have.At
		doc.Rounds[i] = next
		return l.save(doc)
	}
	doc.Rounds = append(doc.Rounds, next)
	sort.SliceStable(doc.Rounds, func(i, j int) bool { return doc.Rounds[i].Round < doc.Rounds[j].Round })
	return l.save(doc)
}

// Settle records what became of one finding, updating every round that
// carries it. A key with no file settles every finding carrying its id, the
// way a bare id in a comment does. It reports an error when no round records
// the finding or the key names none, so a caller learns its key was wrong
// rather than settling nothing.
func (l *RoundsLedger) Settle(key FindingKey, outcome Outcome) error {
	if key.ID == "" {
		return errors.New("rounds ledger: a finding without an id cannot be settled")
	}
	if !outcome.valid() {
		return fmt.Errorf("rounds ledger: %q is not an outcome", outcome)
	}
	doc, err := l.load()
	if err != nil {
		return err
	}
	settled := 0
	for i := range doc.Rounds {
		for j := range doc.Rounds[i].Findings {
			have := doc.Rounds[i].Findings[j].key()
			if have.ID != key.ID || (key.File != "" && have.File != key.File) {
				continue
			}
			doc.Rounds[i].Findings[j].Outcome = outcome
			settled++
		}
	}
	if settled == 0 {
		return fmt.Errorf("rounds ledger: no round records finding %s", key)
	}
	return l.save(doc)
}

// Render returns the ledger as the JSON array gt-3l5qy writes for om, newest
// rounds first kept: an output over maxBytes loses its oldest rounds whole,
// and the newest round stays even alone over the cap. Zero or negative
// maxBytes means DefaultRoundsCap. A ledger that does not parse is
// ErrRoundsCorrupt, which the caller treats as no history.
func (l *RoundsLedger) Render(maxBytes int) ([]byte, error) {
	if maxBytes <= 0 {
		maxBytes = DefaultRoundsCap
	}
	doc, err := l.load()
	if err != nil {
		return nil, err
	}
	rounds := doc.Rounds
	for n := len(rounds); n > 0; n-- {
		data, err := json.Marshal(renderRounds(rounds[len(rounds)-n:]))
		if err != nil {
			return nil, fmt.Errorf("rounds ledger: rendering: %w", err)
		}
		if len(data) <= maxBytes || n == 1 {
			return data, nil
		}
	}
	return []byte("[]"), nil
}

// FindingOutcome is what became of key, the finding an earlier round raised:
// the mapping the ledger settles by (gt-yphcl). An allowlisted ACCEPTED-RISK
// comment naming the finding accepts it and a REBUTTAL: comment naming it
// rebuts it, whatever the round said. Otherwise the later round's
// classification decides: resolved is fixed, regressed reopens what had been
// fixed, and unresolved — including a round that does not mention the finding
// — leaves the previous outcome standing, so a fixed finding stays fixed and
// a finding nobody settled stays open.
func FindingOutcome(prev Outcome, class Classification, comments []beads.Comment, key FindingKey) Outcome {
	if acceptedFinding(comments, key) {
		return OutcomeAccepted
	}
	if rebuttedFinding(comments, key) {
		return OutcomeRebutted
	}
	switch class {
	case ClassificationResolved:
		return OutcomeFixed
	case ClassificationRegressed:
		return OutcomeOpen
	}
	switch prev {
	case OutcomeFixed, OutcomeAccepted, OutcomeRebutted:
		return prev
	}
	return OutcomeOpen
}

// acceptedFinding reports whether an allowlisted accepted-risk comment names
// key.
func acceptedFinding(comments []beads.Comment, key FindingKey) bool {
	return anyCommentNames(comments, acceptedRiskMarker, key)
}

// rebuttedFinding reports whether an allowlisted REBUTTAL: comment names key.
func rebuttedFinding(comments []beads.Comment, key FindingKey) bool {
	return anyCommentNames(comments, rebuttalMarker, key)
}

// anyCommentNames reports whether a comment that opens with marker and whose
// author may write it names key.
func anyCommentNames(comments []beads.Comment, marker string, key FindingKey) bool {
	for _, c := range comments {
		if text, ok := decisionComment(c, marker); ok && commentNamesFinding(text, key) {
			return true
		}
	}
	return false
}

// commentNamesFinding reports whether text names key as one of its words: the
// finding's id, its file and id joined by a colon, or its base name and id.
// om's ids repeat across files, so a bare id settles every finding carrying
// it, while a file:id names one.
func commentNamesFinding(text string, key FindingKey) bool {
	if key.ID == "" {
		return false
	}
	// A comment that omits the space after the marker — "ACCEPTED-RISK:F2" —
	// names the finding the same as one that keeps it.
	text = strings.TrimPrefix(strings.TrimPrefix(text, acceptedRiskMarker), rebuttalMarker)
	names := []string{key.ID}
	if key.File != "" {
		names = append(names, key.File+":"+key.ID, path.Base(key.File)+":"+key.ID)
	}
	for _, word := range findingWords(text) {
		for _, name := range names {
			if strings.EqualFold(word, name) {
				return true
			}
		}
	}
	return false
}

// findingWords splits text into the words an id can be named by, trimming the
// punctuation around each so "F1:", "(F1)" and "F1." all read as F1.
func findingWords(text string) []string {
	words := strings.Fields(text)
	for i, word := range words {
		words[i] = strings.TrimFunc(word, func(r rune) bool {
			return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '/' && r != '\\'
		})
	}
	return words
}

// findingText is the ledger's text for a finding: om's title, or its detail
// for a finding om gave no title.
func findingText(f Finding) string {
	if title := strings.TrimSpace(f.Title); title != "" {
		return title
	}
	return strings.TrimSpace(f.Detail)
}

// recordFindings is findings as the ledger stores them: every one open until
// a round or a comment settles it.
func recordFindings(findings []Finding) []ledgerFinding {
	out := make([]ledgerFinding, 0, len(findings))
	for _, f := range findings {
		out = append(out, ledgerFinding{ID: f.ID, File: f.Path, Text: findingText(f), Outcome: OutcomeOpen})
	}
	return out
}

// outcome is what this round recorded for key, reported only when exactly one
// finding carries it: om's findings need not have ids, and id-less ones share
// a key, so a neighbor's outcome is not theirs to inherit.
func (r ledgerRound) outcome(key FindingKey) (Outcome, bool) {
	if key.ID == "" {
		return "", false
	}
	found, out := 0, Outcome("")
	for _, f := range r.Findings {
		if f.key() != key {
			continue
		}
		found, out = found+1, f.Outcome
	}
	return out, found == 1
}

// renderRounds is rounds as Render emits them, oldest first.
func renderRounds(rounds []ledgerRound) []renderedRound {
	out := make([]renderedRound, 0, len(rounds))
	for _, r := range rounds {
		rendered := renderedRound{Round: r.Round, Head: r.Head, Findings: make([]renderedFinding, 0, len(r.Findings))}
		for _, f := range r.Findings {
			rendered.Findings = append(rendered.Findings, renderedFinding{ID: f.ID, File: f.File, Text: f.Text, Outcome: f.Outcome})
			if f.Outcome == OutcomeOpen {
				rendered.OpenAtLand++
			}
		}
		out = append(out, rendered)
	}
	return out
}

// load reads the ledger, and reports a missing file as a landing with no
// rounds yet.
func (l *RoundsLedger) load() (ledgerDoc, error) {
	name, err := l.file()
	if err != nil {
		return ledgerDoc{}, err
	}
	data, err := l.fs().ReadFile(name)
	if errors.Is(err, os.ErrNotExist) {
		return ledgerDoc{Bead: l.Bead}, nil
	}
	if err != nil {
		return ledgerDoc{}, fmt.Errorf("rounds ledger: reading %s: %w", name, err)
	}
	var doc ledgerDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		return ledgerDoc{}, fmt.Errorf("%w: %s: %v", ErrRoundsCorrupt, name, err)
	}
	if err := doc.check(name, l.Bead); err != nil {
		return ledgerDoc{}, err
	}
	sort.SliceStable(doc.Rounds, func(i, j int) bool { return doc.Rounds[i].Round < doc.Rounds[j].Round })
	return doc, nil
}

// check reports a document that cannot be this landing's ledger: another
// bead's rounds, a round number a landing never has, two entries for one
// round, or an outcome the ledger never writes.
func (d ledgerDoc) check(name, bead string) error {
	if d.Bead != "" && d.Bead != bead {
		return fmt.Errorf("%w: %s records bead %q, not %q", ErrRoundsCorrupt, name, d.Bead, bead)
	}
	seen := make(map[int]bool, len(d.Rounds))
	for _, r := range d.Rounds {
		if r.Round < 1 {
			return fmt.Errorf("%w: %s records round %d", ErrRoundsCorrupt, name, r.Round)
		}
		if seen[r.Round] {
			return fmt.Errorf("%w: %s records round %d twice", ErrRoundsCorrupt, name, r.Round)
		}
		seen[r.Round] = true
		for _, f := range r.Findings {
			if !f.Outcome.valid() {
				return fmt.Errorf("%w: %s records finding %s with outcome %q", ErrRoundsCorrupt, name, f.key(), f.Outcome)
			}
		}
	}
	return nil
}

// save replaces the ledger file: a reader sees either the previous round or
// this one, never a half-written file.
func (l *RoundsLedger) save(doc ledgerDoc) error {
	name, err := l.file()
	if err != nil {
		return err
	}
	doc.Bead = l.Bead
	if doc.Rounds == nil {
		doc.Rounds = []ledgerRound{}
	}
	if err := l.fs().MkdirAll(l.OutDir, 0o700); err != nil {
		return fmt.Errorf("rounds ledger: creating %s: %w", l.OutDir, err)
	}
	body, err := json.Marshal(doc)
	if err != nil {
		return fmt.Errorf("rounds ledger: encoding %s: %w", name, err)
	}
	body = append(body, '\n')
	tmp := name + ".tmp"
	if err := l.fs().WriteFile(tmp, body, 0o600); err != nil {
		return fmt.Errorf("rounds ledger: writing %s: %w", tmp, err)
	}
	if err := l.fs().Rename(tmp, name); err != nil {
		_ = l.fs().Remove(tmp)
		return fmt.Errorf("rounds ledger: replacing %s: %w", name, err)
	}
	return nil
}

// file is the ledger's path, refusing a bead id that would leave OutDir.
func (l *RoundsLedger) file() (string, error) {
	if l.OutDir == "" {
		return "", errors.New("rounds ledger: no directory configured")
	}
	if l.Bead == "" || l.Bead != filepath.Base(l.Bead) || strings.ContainsAny(l.Bead, `/\`) {
		return "", fmt.Errorf("rounds ledger: invalid bead id %q", l.Bead)
	}
	return filepath.Join(l.OutDir, "rounds-"+l.Bead+".json"), nil
}

// fs is the filesystem the ledger uses.
func (l *RoundsLedger) fs() LedgerFS {
	if l.FS != nil {
		return l.FS
	}
	return osLedgerFS{}
}

// now is the clock a new round is stamped from.
func (l *RoundsLedger) now() time.Time {
	if l.Now != nil {
		return l.Now()
	}
	return time.Now()
}

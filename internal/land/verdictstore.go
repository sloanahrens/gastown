package land

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/atomicfile"
	"github.com/steveyegge/gastown/internal/constants"
)

// The verdict store keeps om's answer whole, because a landing's bead notes
// keep only each finding's id, severity, path and title and reviews.jsonl keeps
// a count: the summary and the detail under each finding are what an overseer
// asking "why" would otherwise pay a second om run for (gt-uxqb9).
//
// A record carries om's JSON as received, written 0600 under the private
// landings tree (ensureLandingsDir), so the only readers are the account that
// wrote it. Redacting it would defeat the point of keeping it: a reader who
// cannot see what om saw cannot answer the question the store exists for.
const (
	// verdictRetainNewest is how many verdict files one bead directory keeps
	// whatever their age, so a bead's recent review history is readable.
	verdictRetainNewest = 10
	// verdictRetainAge is how long a verdict file is kept whatever its rank: a
	// month of verdicts outlives the landing that wrote them, so a late
	// question still finds its answer.
	verdictRetainAge = 30 * 24 * time.Hour
	// headPrefixLen is how much of the reviewed head names its verdict file,
	// at git's short-sha length.
	headPrefixLen = 12
)

// VerdictRecord is one review as the store keeps it: which bead and head it
// judged, when, by which om and flags, and om's JSON verbatim.
type VerdictRecord struct {
	Bead      string          `json:"bead"`
	Rig       string          `json:"rig"`
	Head      string          `json:"head"`
	At        time.Time       `json:"at"`
	OMVersion string          `json:"om_version"`
	Flags     []string        `json:"flags"`
	Verdict   json.RawMessage `json:"verdict"`
}

// VerdictStore keeps every om verdict under
// <town>/.runtime/landings/verdicts/<rig>/<bead>/<head12>.json.
type VerdictStore struct {
	// TownRoot is the town the verdicts belong to; the store lives in its
	// runtime tree, beside the review log (TownVerdicts).
	TownRoot string
	// Rig is the rig whose landings the store keeps, and the wrapper field
	// every record it writes carries.
	Rig string
	// Now is the clock the record's At and the retention window read; nil
	// means time.Now.
	Now func() time.Time

	// The fields below are test seams for the filesystem calls Write and prune
	// make; nil means the os function of the same shape.
	writeFile func(path string, data []byte, perm os.FileMode) error
	readDir   func(dir string) ([]os.DirEntry, error)
	remove    func(path string) error
}

// TownVerdicts is the verdict store for one rig's landings, beside the town's
// review log (TownReviewsFile).
func TownVerdicts(townRoot, rig string) *VerdictStore {
	return &VerdictStore{TownRoot: townRoot, Rig: rig}
}

// VerdictsDir is <town>/.runtime/landings/verdicts.
func VerdictsDir(townRoot string) string {
	return filepath.Join(constants.TownRuntimePath(townRoot), "landings", "verdicts")
}

// Write stores rec at its bead/head path and then prunes that bead's directory
// to the retention bound. Write fills the record's rig and time. A prune
// failure goes to logf and is not returned: the verdict is stored and only the
// cleanup failed. A nil logf drops the line.
func (s VerdictStore) Write(rec VerdictRecord, logf func(format string, args ...any)) error {
	path, err := s.path(rec.Bead, rec.Head)
	if err != nil {
		return err
	}
	rec.Rig, rec.At = s.Rig, s.now().UTC()
	data, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("encoding verdict record: %w", err)
	}
	dir := filepath.Dir(path)
	if err := ensureLandingsDir(dir); err != nil {
		return err
	}
	// The head names the file, so a second review of one head replaces the
	// first. The newest verdict for a head is the one a reader wants, and the
	// line keeps the earlier record from vanishing without a trace (gt-uxqb9).
	switch _, err := os.Stat(path); {
	case err == nil:
		reportf(logf, "verdict store: replacing the record for %s@%s", rec.Bead, headPrefix(rec.Head))
	case !errors.Is(err, os.ErrNotExist):
		reportf(logf, "verdict store: checking %s: %v", path, err)
	}
	write := s.writeFile
	if write == nil {
		write = atomicfile.WriteFile
	}
	if err := write(path, data, 0o600); err != nil {
		return fmt.Errorf("writing verdict file: %w", err)
	}
	s.prune(dir, path, logf)
	return nil
}

// path is the record's file, one directory level per component so a bead id or
// sha outside the store's alphabet is refused rather than named somewhere else.
func (s VerdictStore) path(bead, head string) (string, error) {
	if s.TownRoot == "" {
		return "", errors.New("verdict store: no town root configured")
	}
	if !validComponent(s.Rig) {
		return "", fmt.Errorf("verdict store: invalid rig %q", s.Rig)
	}
	if !validComponent(bead) {
		return "", fmt.Errorf("verdict store: invalid bead %q", bead)
	}
	if !isHexSHA(head) {
		return "", fmt.Errorf("verdict store: head %q is not a sha", head)
	}
	return filepath.Join(VerdictsDir(s.TownRoot), s.Rig, bead, headPrefix(head)+".json"), nil
}

// validComponent reports whether s is exactly one path component: an id that
// climbed out with a separator or ".." would name a directory outside the store.
func validComponent(s string) bool {
	return s != "" && s != "." && s != ".." &&
		filepath.Base(s) == s && !strings.ContainsAny(s, `/\`) && !strings.ContainsRune(s, 0)
}

// headPrefix is the part of head that names its verdict file.
func headPrefix(head string) string {
	if len(head) > headPrefixLen {
		return head[:headPrefixLen]
	}
	return head
}

// isHexSHA reports whether s is a git sha: one or more lowercase hex digits.
func isHexSHA(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !strings.ContainsRune("0123456789abcdef", r) {
			return false
		}
	}
	return true
}

// verdictFile is one of the store's files as retention sees it.
type verdictFile struct {
	path string
	mod  time.Time
}

// prune deletes the verdict files in beadDir that are outside both the newest
// verdictRetainNewest and the verdictRetainAge window. keep, the file just
// written, stays whatever its rank, and a name the store did not write is not
// its to delete.
func (s VerdictStore) prune(beadDir, keep string, logf func(format string, args ...any)) {
	readDir := s.readDir
	if readDir == nil {
		readDir = os.ReadDir
	}
	entries, err := readDir(beadDir)
	if err != nil {
		reportf(logf, "verdict store: reading %s for retention: %v", beadDir, err)
		return
	}
	now := s.now()
	var files []verdictFile
	for _, e := range entries {
		if e.IsDir() || !isVerdictFile(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			reportf(logf, "verdict store: reading %s: %v", filepath.Join(beadDir, e.Name()), err)
			continue
		}
		files = append(files, verdictFile{path: filepath.Join(beadDir, e.Name()), mod: info.ModTime()})
	}
	sort.SliceStable(files, func(i, j int) bool { return files[i].mod.After(files[j].mod) })
	remove := s.remove
	if remove == nil {
		remove = os.Remove
	}
	for i, f := range files {
		if f.path == keep || i < verdictRetainNewest || now.Sub(f.mod) < verdictRetainAge {
			continue
		}
		if err := remove(f.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			reportf(logf, "verdict store: removing %s: %v", f.path, err)
		}
	}
}

// isVerdictFile reports whether name is one the store writes: a head prefix and
// .json.
func isVerdictFile(name string) bool {
	stem, ok := strings.CutSuffix(name, ".json")
	return ok && len(stem) <= headPrefixLen && isHexSHA(stem)
}

// reportf writes one warning to a caller's log sink; nil drops it.
func reportf(logf func(format string, args ...any), format string, args ...any) {
	if logf != nil {
		logf(format, args...)
	}
}

// now is the store's clock.
func (s VerdictStore) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

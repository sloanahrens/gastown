package land

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"

	"github.com/steveyegge/gastown/internal/config"
)

// ScrubResult is what one scrub of the review log did, or would do.
type ScrubResult struct {
	// Rows counts the log's lines that decode as a record.
	Rows int
	// RowsChanged counts the rows whose finding text held a token-shaped word.
	RowsChanged int
	// Words counts the token-shaped words replaced, never their text.
	Words int
	// Beads lists the bead of each changed row, never a value.
	Beads []string
	// Oversize counts the lines past the scrub's byte bound, copied through
	// unexamined.
	Oversize int
	// Skipped and SkippedBeads report the rows the scrub cannot rewrite, by
	// bead id: zero and empty here, filled by the bead that judges them
	// (gt-btleae).
	Skipped      int
	SkippedBeads []string
}

// ScrubPlan reports what a scrub of the log would rewrite, without writing
// (gt-uxee5k, part 1): it runs scrubClaim's walk over each row's finding_text
// claim and counts each flagged word in the raw line. A line that is not a JSON
// object is passed over, and one past the scrub's byte bound is left unexamined
// and counted in Oversize. The result carries each changed row's bead id and
// never a claim's text, and every error names the file only. The log is opened
// for reading alone.
func (f *ReviewsFile) ScrubPlan() (ScrubResult, error) { return f.scrub(false) }

// Scrub is ScrubPlan that, with apply true and at least one changed row, writes
// the rewritten log through the replace seam in the file's own mode (gt-9h1asy,
// part 2): each flagged word in a changed row's raw line becomes redactedWord, so
// every other byte of the line stays, and a line that is not a JSON object or is
// past the bound is copied through. The write is atomic, so a replace error
// leaves the original log, and a second run finds nothing. With apply false
// nothing is written.
func (f *ReviewsFile) Scrub(apply bool) (ScrubResult, error) { return f.scrub(apply) }

func (f *ReviewsFile) scrub(apply bool) (ScrubResult, error) {
	reviewsMu.Lock()
	defer reviewsMu.Unlock()
	fh, err := os.OpenFile(f.Path, os.O_RDONLY|oNoFollow, 0)
	if errors.Is(err, os.ErrNotExist) {
		return ScrubResult{}, nil
	}
	if err != nil {
		return ScrubResult{}, fmt.Errorf("opening reviews file: %w", err)
	}
	limit := f.lineLimit
	if limit <= 0 {
		limit = maxCompactLineBytes
	}
	var res ScrubResult
	var kept [][]byte
	err = scanLogLines(fh, func(line []byte) {
		if len(bytes.TrimSpace(line)) > limit {
			res.Oversize++
			kept = append(kept, line)
			return
		}
		var rec ReviewRecord
		if !isJSONObject(line) || json.Unmarshal(line, &rec) != nil {
			kept = append(kept, line)
			return
		}
		res.Rows++
		scrubbed, words := scrubRawLine(line, rec)
		if words == 0 {
			kept = append(kept, line)
			return
		}
		res.RowsChanged++
		res.Words += words
		res.Beads = append(res.Beads, safeBeadID(rec.BeadID))
		kept = append(kept, scrubbed)
	})
	// The mode is the open file's, read once the rows are, so the rewrite keeps
	// the file it read and a failed stat is a failed read.
	var info os.FileInfo
	if err == nil {
		info, err = fh.Stat()
	}
	if cerr := fh.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return ScrubResult{}, fmt.Errorf("reading reviews file %s: %w", f.Path, err)
	}
	if !apply || res.RowsChanged == 0 {
		return res, nil
	}
	var buf bytes.Buffer
	for _, line := range kept {
		buf.Write(line)
		buf.WriteByte('\n')
	}
	replace := f.replace
	if replace == nil {
		replace = config.ReplaceFile
	}
	if err := replace(f.Path, buf.Bytes(), info.Mode().Perm()); err != nil {
		return ScrubResult{}, fmt.Errorf("replacing reviews file: %w", err)
	}
	return res, nil
}

// safeBeadID is the only way a bead id enters a ScrubResult: the log's bead
// field is whatever a row held, so a token-shaped id is reported as the marker
// like a claim's token is. Beads, SkippedBeads (gt-btleae) and the apply's
// report (gt-9h1asy) call it, so no id is printed raw (gt-6r1l6k).
func safeBeadID(id string) string { return scrubClaim(id) }

// isJSONObject reports whether line opens as a JSON object: a record is an
// object, and everything else is copied through rather than judged. Syntax is
// json.Unmarshal's call.
func isJSONObject(line []byte) bool {
	t := bytes.TrimSpace(line)
	return len(t) > 0 && t[0] == '{'
}

// scrubRawLine replaces in one raw log line each word scrubClaim flags in the
// row's claims, and reports how many words it replaced. A flagged word holds
// only characters JSON never escapes, so the raw line holds it as it is; a word
// the line does not hold (a claim JSON escaped a byte of it) is left for the
// bead that reports the rows this scrub cannot rewrite (gt-btleae).
func scrubRawLine(line []byte, rec ReviewRecord) ([]byte, int) {
	seen := make(map[string]bool)
	var words []string
	for _, ft := range rec.FindingText {
		for _, w := range flaggedWords(ft.Claim) {
			if !seen[w] {
				seen[w] = true
				words = append(words, w)
			}
		}
	}
	// A longer word first, so one flagged word that is another's prefix cannot
	// leave the tail of the longer one in the line.
	sort.Slice(words, func(i, j int) bool { return len(words[i]) > len(words[j]) })
	out, n := line, 0
	for _, w := range words {
		if c := bytes.Count(out, []byte(w)); c > 0 {
			out = bytes.ReplaceAll(out, []byte(w), []byte(redactedWord))
			n += c
		}
	}
	return out, n
}

// flaggedWords is scrubClaim's own walk: the token-shaped runs it replaces, in
// order. Both call tokenEnd, so they cannot disagree about a token's extent, and
// the scrub can find each flagged word in the raw line.
func flaggedWords(s string) []string {
	s = cutAtRune(s, scrubInputBound)
	var words []string
	for i := 0; i < len(s); {
		if end := tokenEnd(s, i); end > i {
			words = append(words, s[i:end])
			i = end
			continue
		}
		i++
	}
	return words
}

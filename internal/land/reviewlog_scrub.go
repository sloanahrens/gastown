package land

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
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
func (f *ReviewsFile) ScrubPlan() (ScrubResult, error) {
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
	err = scanLogLines(fh, func(line []byte) {
		if len(bytes.TrimSpace(line)) > limit {
			res.Oversize++
			return
		}
		var rec ReviewRecord
		if !isJSONObject(line) || json.Unmarshal(line, &rec) != nil {
			return
		}
		res.Rows++
		if _, words := scrubRawLine(line, rec); words > 0 {
			res.RowsChanged++
			res.Words += words
			res.Beads = append(res.Beads, rec.BeadID)
		}
	})
	if cerr := fh.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return ScrubResult{}, fmt.Errorf("reading reviews file %s: %w", f.Path, err)
	}
	return res, nil
}

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

package land

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	// Skipped and SkippedBeads report the decodable rows the scrub cannot
	// rewrite, because a flagged word of a claim is not in the raw line (a
	// claim JSON escaped a byte of): each is kept byte for byte and listed by
	// safeBeadID of its bead, or "line N" when it has none (gt-btleae).
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
	// A line the scrub cannot decode, or one past the bound, is scanned over its
	// raw bytes: a token run in it is replaced too (gt-9h1asy), and only that run
	// changes a byte.
	keepRaw := func(line, term []byte) {
		out, words := scrubRawBytes(line)
		if words > 0 {
			res.RowsChanged++
			res.Words += words
		}
		kept = append(kept, append(out[:len(out):len(out)], term...))
	}
	lineNo := 0
	err = scanScrubLines(fh, func(line, term []byte) {
		lineNo++
		if len(bytes.TrimSpace(line)) > limit {
			res.Oversize++
			keepRaw(line, term)
			return
		}
		var rec ReviewRecord
		if !isJSONObject(line) || json.Unmarshal(line, &rec) != nil {
			keepRaw(line, term)
			return
		}
		res.Rows++
		scrubbed, words, missing := scrubRawLine(line, rec)
		if missing > 0 {
			label := fmt.Sprintf("line %d", lineNo)
			if rec.BeadID != "" {
				label = safeBeadID(rec.BeadID)
			}
			res.Skipped++
			res.SkippedBeads = append(res.SkippedBeads, label)
			kept = append(kept, append(line[:len(line):len(line)], term...))
			return
		}
		if words == 0 {
			kept = append(kept, append(line[:len(line):len(line)], term...))
			return
		}
		res.RowsChanged++
		res.Words += words
		res.Beads = append(res.Beads, safeBeadID(rec.BeadID))
		kept = append(kept, append(scrubbed[:len(scrubbed):len(scrubbed)], term...))
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

// scanScrubLines is scanLogLines that also hands fn each line's own terminator
// (\n, \r\n, or none for an unterminated last line), so a rewrite writes every
// line back as it read it and only a token run changes a byte.
func scanScrubLines(r io.Reader, fn func(line, term []byte)) error {
	br := bufio.NewReader(r)
	for {
		raw, err := br.ReadBytes('\n')
		if len(raw) > 0 {
			body := bytes.TrimRight(raw, "\r\n")
			fn(body, raw[len(body):])
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// scrubRawBytes replaces each token-shaped run in the raw bytes of a line with
// redactedWord, keeping every other byte, and reports how many it replaced. It
// is scrubClaim's offset scan over the whole line, with no input cut: tokenEnd
// tries a run of at most maxTokenRun bytes only where a prefix starts, so the
// scan stays linear in the line (gt-9h1asy).
func scrubRawBytes(line []byte) ([]byte, int) {
	s := string(line)
	var out []byte
	words, last := 0, 0
	for i := 0; i < len(s); {
		if end := tokenEnd(s, i); end > i {
			out = append(out, s[last:i]...)
			out = append(out, redactedWord...)
			last, i = end, end
			words++
			continue
		}
		i++
	}
	if words == 0 {
		return line, 0
	}
	return append(out, s[last:]...), words
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
// row's claims, and reports how many words it replaced and how many the line
// did not hold when their turn came (a claim JSON escaped a byte of it, or its
// only plain copy sat inside a longer word already replaced). The caller skips a
// row with a missing word (gt-btleae).
func scrubRawLine(line []byte, rec ReviewRecord) ([]byte, int, int) {
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
	out, n, missing := line, 0, 0
	for _, w := range words {
		c := bytes.Count(out, []byte(w))
		if c == 0 {
			missing++
			continue
		}
		out = bytes.ReplaceAll(out, []byte(w), []byte(redactedWord))
		n += c
	}
	return out, n, missing
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

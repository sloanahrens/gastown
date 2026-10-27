package land

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// The verdict view reads what the verdict store keeps and prints it for an
// operator asking why a landing was decided (gt-lh0sc). It only reads: the
// store comes in as an io/fs.FS, which has no write, and the bead's notes come
// in as a string.

const (
	// verdictDetailMax caps one finding's detail as printed. A longer detail is
	// cut at a rune boundary and says how many bytes were left out, so one
	// runaway detail cannot flood the terminal. The stored file keeps it whole.
	verdictDetailMax = 4 << 10
	// verdictLineMax caps a one-line field (summary, title, path, reason) as
	// printed, so a long om string cannot stand in for the bounded detail.
	verdictLineMax = 1 << 10
	// verdictFindingsMax caps how many findings print for one verdict; the count
	// of the rest follows.
	verdictFindingsMax = 50
	// verdictHeadPrefixMin is the shortest --head prefix that names a head: the
	// length HeadsEqual accepts as an abbreviated sha.
	verdictHeadPrefixMin = 7
)

var (
	// ErrNoVerdictForHead means a head prefix matches no head the store or the
	// notes hold.
	ErrNoVerdictForHead = errors.New("no verdict for that head")
	// ErrAmbiguousHead means a head prefix matches more than one head.
	ErrAmbiguousHead = errors.New("head prefix is ambiguous")
	// ErrNoStoredVerdict means the store holds no verdict for the bead at all.
	ErrNoStoredVerdict = errors.New("no stored verdict for this bead")
)

// StoredVerdict is one verdict the store holds: the file's bytes as written,
// the record they carry and om's verdict decoded from that record.
type StoredVerdict struct {
	Raw     []byte
	Record  VerdictRecord
	Verdict Verdict
}

// ReadStoredVerdict reads the verdict the store holds for bead in rig: the one
// whose head head prefixes when head is set, else the newest. A bead with no
// stored verdict at all returns ErrNoStoredVerdict, the caller's cue to fall
// back to the bead's notes; every other error stops the read. A bead that has
// stored verdicts is not that case, so a head none of them matches is
// ErrNoVerdictForHead.
//
// With head set, only the files whose names can hold that head are read, so a
// corrupt file for another head does not block this one. The newest read has no
// such filter and fails on any corrupt file, since the newest is unknown then.
func ReadStoredVerdict(store fs.FS, rig, bead, head string) (StoredVerdict, error) {
	if !validComponent(rig) {
		return StoredVerdict{}, fmt.Errorf("invalid rig %q", rig)
	}
	if !validComponent(bead) {
		return StoredVerdict{}, fmt.Errorf("invalid bead %q", bead)
	}
	var prefix string
	if head != "" {
		p, err := verdictHeadPrefix(head)
		if err != nil {
			return StoredVerdict{}, err
		}
		prefix = p
	}
	dir := path.Join(rig, bead)
	entries, err := fs.ReadDir(store, dir)
	if errors.Is(err, fs.ErrNotExist) {
		return StoredVerdict{}, ErrNoStoredVerdict
	}
	if err != nil {
		return StoredVerdict{}, fmt.Errorf("reading the verdict store for %s: %w", bead, err)
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && isVerdictFile(e.Name()) {
			names = append(names, e.Name())
		}
	}
	if len(names) == 0 {
		return StoredVerdict{}, ErrNoStoredVerdict
	}
	var all []StoredVerdict
	for _, name := range names {
		if prefix != "" && !fileMayHoldHead(name, prefix) {
			continue
		}
		raw, err := fs.ReadFile(store, path.Join(dir, name))
		if err != nil {
			return StoredVerdict{}, fmt.Errorf("reading %s: %w", name, err)
		}
		v, err := decodeStoredVerdict(raw)
		if err != nil {
			return StoredVerdict{}, fmt.Errorf("%s: %w", name, err)
		}
		all = append(all, v)
	}
	if head == "" {
		return newestVerdict(all), nil
	}
	heads := make([]string, len(all))
	for i, v := range all {
		heads[i] = v.Record.Head
	}
	i, err := matchHead(heads, prefix)
	if err != nil {
		return StoredVerdict{}, err
	}
	return all[i], nil
}

// decodeStoredVerdict reads one stored file. A file that is not a record, or
// whose om verdict is not JSON, is an error: the store is not guessed at.
func decodeStoredVerdict(raw []byte) (StoredVerdict, error) {
	var rec VerdictRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return StoredVerdict{}, fmt.Errorf("not a verdict record: %w", err)
	}
	var v Verdict
	if err := json.Unmarshal(rec.Verdict, &v); err != nil {
		return StoredVerdict{}, fmt.Errorf("the record's om verdict is not JSON: %w", err)
	}
	return StoredVerdict{Raw: raw, Record: rec, Verdict: v}, nil
}

// newestVerdict is the verdict recorded last. vs must not be empty.
func newestVerdict(vs []StoredVerdict) StoredVerdict {
	best := vs[0]
	for _, v := range vs[1:] {
		if v.Record.At.After(best.Record.At) {
			best = v
		}
	}
	return best
}

// verdictHeadPrefix lower-cases and checks a --head argument: a hex prefix of
// at least verdictHeadPrefixMin digits, so a short word cannot match by luck.
func verdictHeadPrefix(head string) (string, error) {
	p := strings.ToLower(strings.TrimSpace(head))
	if len(p) < verdictHeadPrefixMin || !isHexSHA(p) {
		return "", fmt.Errorf("%q is not a sha prefix of at least %d hex digits", head, verdictHeadPrefixMin)
	}
	return p, nil
}

// fileMayHoldHead reports whether the verdict file name can hold a head that
// prefix names. The file name carries the head's first headPrefixLen digits, so
// the two must agree on the shorter of the two.
func fileMayHoldHead(name, prefix string) bool {
	stem := strings.TrimSuffix(name, ".json")
	return strings.HasPrefix(stem, prefix) || strings.HasPrefix(prefix, stem)
}

// matchHead is the index of the first entry of heads whose head prefix names.
// Entries that share one head are one head, so only distinct heads count toward
// ambiguity.
func matchHead(heads []string, prefix string) (int, error) {
	var found []string
	first := -1
	for i, h := range heads {
		lh := strings.ToLower(strings.TrimSpace(h))
		if !strings.HasPrefix(lh, prefix) {
			continue
		}
		if first < 0 {
			first = i
		}
		if !slices.Contains(found, lh) {
			found = append(found, lh)
		}
	}
	switch len(found) {
	case 0:
		return -1, fmt.Errorf("%w: %s", ErrNoVerdictForHead, prefix)
	case 1:
		return first, nil
	}
	shorts := make([]string, len(found))
	for i, h := range found {
		shorts[i] = headPrefix(h)
	}
	return -1, fmt.Errorf("%w: %s matches %s", ErrAmbiguousHead, prefix, strings.Join(shorts, ", "))
}

// NotesRejections reads the MERGE REJECTION blocks in a bead's notes, in the
// order they were written. head, when set, keeps the blocks that rejected the
// head it prefixes, and fails with ErrNoVerdictForHead when none did.
func NotesRejections(notes, head string) ([]RejectionNote, error) {
	blocks := rejectionBlocks(notes)
	if head == "" {
		return blocks, nil
	}
	prefix, err := verdictHeadPrefix(head)
	if err != nil {
		return nil, err
	}
	heads := make([]string, len(blocks))
	for i, b := range blocks {
		heads[i] = b.Head
	}
	i, err := matchHead(heads, prefix)
	if err != nil {
		return nil, err
	}
	var out []RejectionNote
	for _, b := range blocks {
		if strings.EqualFold(b.Head, blocks[i].Head) {
			out = append(out, b)
		}
	}
	return out, nil
}

// rejectionBlocks parses every MERGE REJECTION block in notes. Each block runs
// from its header to the next header, so ParseRejectionNote reads exactly one
// block of each slice.
func rejectionBlocks(notes string) []RejectionNote {
	starts := rejectionBlockRE.FindAllStringIndex(notes, -1)
	var out []RejectionNote
	for i, s := range starts {
		end := len(notes)
		if i+1 < len(starts) {
			end = starts[i+1][0]
		}
		if n, ok := ParseRejectionNote(notes[s[0]:end]); ok {
			out = append(out, n)
		}
	}
	return out
}

// RenderStoredVerdict writes a stored verdict for an operator: om's verdict,
// score and summary, then each finding with its detail, within the caps.
func RenderStoredVerdict(w io.Writer, bead string, sv StoredVerdict) error {
	r := sv.Record
	var b strings.Builder
	fmt.Fprintf(&b, "Verdict for %s @ %s (%s)\n", bead, oneLine(r.Head), oneLine(r.Rig))
	fmt.Fprintf(&b, "  verdict: %s, score %.4f\n", oneLine(sv.Verdict.Verdict), sv.Verdict.Score)
	fmt.Fprintf(&b, "  judged:  %s by om %s\n", r.At.UTC().Format(time.RFC3339), orNone(oneLine(r.OMVersion)))
	fmt.Fprintf(&b, "  flags:   %s\n", orNone(oneLine(strings.Join(r.Flags, " "))))
	if sv.Verdict.Summary != "" {
		fmt.Fprintf(&b, "  summary: %s\n", oneLine(sv.Verdict.Summary))
	}
	writeFindings(&b, sv.Verdict.Findings)
	_, err := io.WriteString(w, b.String())
	return err
}

// RenderRejectionNotes writes the MERGE REJECTION blocks of a bead that has no
// stored verdict. The notes keep no finding detail, and the output says so
// rather than leaving the reader to wonder where it went.
func RenderRejectionNotes(w io.Writer, bead string, blocks []RejectionNote) error {
	var b strings.Builder
	fmt.Fprintf(&b, "No stored verdict for %s; showing its %s notes.\n", bead, MergeRejectionNoteMarker)
	b.WriteString("The notes keep each finding's id, severity, path, line and title, not its detail, so no detail is shown.\n")
	for _, n := range blocks {
		header := fmt.Sprintf("\n%s (attempt %d)", MergeRejectionNoteMarker, n.Attempt)
		if n.Kind != "" {
			header += " " + oneLine(n.Kind)
		}
		if n.Reason != "" {
			header += ": " + oneLine(n.Reason)
		}
		b.WriteString(header + "\n")
		if n.Head != "" {
			fmt.Fprintf(&b, "  Head:    %s\n", oneLine(n.Head))
		}
		if n.Receipt != nil {
			fmt.Fprintf(&b, "  Score:   %.4f\n", n.Receipt.Score)
		}
		writeFindings(&b, n.Findings)
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// writeFindings writes the finding count, then each finding up to the cap.
func writeFindings(b *strings.Builder, findings []Finding) {
	fmt.Fprintf(b, "  Findings: %d\n", len(findings))
	for i, f := range findings {
		if i == verdictFindingsMax {
			fmt.Fprintf(b, "  ... and %d more not shown\n", len(findings)-verdictFindingsMax)
			break
		}
		writeFinding(b, f)
	}
}

// writeFinding writes one finding's line, then its detail indented beneath it.
func writeFinding(b *strings.Builder, f Finding) {
	fmt.Fprintf(b, "    %s  %s  %s — %s\n", orNone(oneLine(f.ID)), orNone(oneLine(f.Severity)), findingLocation(f), orNone(oneLine(f.Title)))
	if f.Detail == "" {
		return
	}
	// The detail keeps its own lines, so each is made terminal-safe on its own.
	for _, line := range strings.Split(capDetail(f.Detail), "\n") {
		fmt.Fprintf(b, "        %s\n", terminalText(line))
	}
}

// findingLocation is the path, and the line when om named one.
func findingLocation(f Finding) string {
	if f.Line > 0 {
		return fmt.Sprintf("%s:%d", orNone(oneLine(f.Path)), f.Line)
	}
	return orNone(oneLine(f.Path))
}

// capDetail cuts a detail that runs past verdictDetailMax at a rune boundary,
// and names how many bytes it left out. A detail within the cap is returned as
// it is.
func capDetail(detail string) string {
	kept, dropped := cutAt(detail, verdictDetailMax)
	if dropped == 0 {
		return detail
	}
	return fmt.Sprintf("%s\n[detail truncated: %d bytes not shown]", kept, dropped)
}

// oneLine is a one-line field as printed: capped at verdictLineMax, then made
// terminal-safe. A cut line names how many bytes it left out.
func oneLine(s string) string {
	kept, dropped := cutAt(s, verdictLineMax)
	if dropped > 0 {
		kept += fmt.Sprintf(" [truncated: %d bytes not shown]", dropped)
	}
	return terminalText(kept)
}

// cutAt keeps the first limit bytes of s, backing up to a rune boundary, and
// reports how many bytes it left out. A string within the limit comes back whole.
func cutAt(s string, limit int) (string, int) {
	if len(s) <= limit {
		return s, 0
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut], len(s) - cut
}

// terminalText writes s with every rune that is not printable as its Go escape,
// so text om wrote cannot move the cursor, rewrite a line or hide a finding with
// an escape sequence. Newlines escape too, so a one-line field stays one line.
func terminalText(s string) string {
	var b strings.Builder
	for _, r := range s {
		if unicode.IsPrint(r) {
			b.WriteRune(r)
			continue
		}
		// QuoteRune wraps the escape in single quotes; the escape is what is between.
		q := strconv.QuoteRune(r)
		b.WriteString(q[1 : len(q)-1])
	}
	return b.String()
}

// orNone is s, or "(none)" when s is empty, so a missing field reads as absent
// rather than as a blank that runs into the next label. s is already printable.
func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

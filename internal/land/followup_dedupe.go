package land

import (
	"crypto/sha256"
	"encoding/hex"
	"path"
	"strings"
	"unicode"
)

// The building blocks behind de-duplicating om follow-up beads. The landing
// worker will file one bead per approved finding; om re-raises the same finding
// across re-reviews (3,179 findings collapse to 1,994 distinct ids), and 5-10%
// of the existing Found: beads are paraphrases of each other. Everything here
// is pure: no I/O, no bd calls, so the decision is testable offline and the
// next bead wires it to the beads store (design claude-cl7.27).

// FingerprintPrefix opens every finding fingerprint. Beads that already carry
// a finding are labeled with it, which is how a later run recognizes the
// finding without re-reading the bead bodies.
const FingerprintPrefix = "om-fp:"

// DefaultFollowupSimilarity is the same-path title similarity at or above which
// a finding counts as a paraphrase of an existing bead rather than a new one. It
// sits below the 0.57 the known paraphrase pair scores and far above unrelated
// findings, which share almost nothing. Callers pass their own threshold; a
// non-positive one takes this default, since a zero threshold would make every
// same-path bead a match.
const DefaultFollowupSimilarity = 0.5

// followupStopWords are the function words the similarity tokenizer drops. The
// list is deliberately small: it holds words that appear in so many finding
// titles that they say nothing about which finding is meant.
var followupStopWords = map[string]bool{
	"and": true, "any": true, "are": true, "all": true, "but": true,
	"for": true, "from": true, "has": true, "have": true, "into": true,
	"its": true, "not": true, "our": true, "that": true, "the": true,
	"them": true, "they": true, "this": true, "too": true, "was": true,
	"were": true, "when": true, "with": true, "you": true, "your": true,
}

// Fingerprint is the stable identity of one finding: its file path plus its
// title, both normalized, hashed to 12 hex characters. Two runs over the same
// finding produce the same fingerprint, and a different path or a rewritten
// title produces a different one, so the fingerprint can be stored as a bead
// label and matched on the next run. An empty path is allowed: it still
// fingerprints, and it never equals the fingerprint of a named path.
//
// The path is kept as given (only path.Clean, so "./x.go" and "x.go" agree);
// lowercasing applies to the title, because a path is case-sensitive on the
// filesystems this runs on.
func Fingerprint(fpath, title string) string {
	sum := sha256.Sum256([]byte(normalizePath(fpath) + "\n" + normalizeTitle(title)))
	return FingerprintPrefix + hex.EncodeToString(sum[:])[:12]
}

// TitleSimilarity scores two finding titles as the Jaccard index of their word
// sets: the shared words over all words either title uses. 1 means the same
// words, 0 means no shared word. Titles that normalize to the same string score
// 1, and a title with no word of three or more letters scores 0 against
// anything, including another such title — there is nothing to compare.
//
// The known paraphrase pair "guards judge every segment against the hook cwd"
// / "the idle gate judges every segment against the hook cwd" scores 0.57:
// enough to clear the default threshold, and it separates that pair from
// unrelated findings, which score well under 0.3.
func TitleSimilarity(a, b string) float64 {
	at, bt := titleTokens(a), titleTokens(b)
	if len(at) == 0 || len(bt) == 0 {
		return 0
	}
	if normalizeTitle(a) == normalizeTitle(b) {
		return 1
	}
	shared := 0
	for tok := range at {
		if bt[tok] {
			shared++
		}
	}
	return float64(shared) / float64(len(at)+len(bt)-shared)
}

// ExistingFollowup is the part of an open or closed Found: bead the decision
// reads. The body is scanned for the candidate's path, and the labels for its
// fingerprint; Status is carried but not consulted.
type ExistingFollowup struct {
	ID          string
	Title       string
	Description string
	Status      string
	Labels      []string
}

// FollowupCandidate is one finding awaiting a decision: an om finding, or a bead
// the landing worker is about to file for it. Detail is the finding rendered as
// a bead body lists it and Claim is om's own account of it, so a finding the
// backpressure bound queues can be filed later from what it carries; the
// decision itself needs only Path and Title. Candidate is already the gate
// interface in this package (candidate.go), hence the longer name.
type FollowupCandidate struct {
	ID       string
	Path     string
	Title    string
	Detail   string
	Claim    string
	Severity string
	Line     int
}

// DedupeAction is what the landing worker does with a finding.
type DedupeAction string

const (
	// DedupeFile files a new bead for the finding.
	DedupeFile DedupeAction = "file"
	// DedupeComment records the finding again on an existing bead, as a
	// comment, and files nothing.
	DedupeComment DedupeAction = "comment"
)

// Decision is the answer for one finding. Target is the existing bead's id for
// DedupeComment and empty for DedupeFile; Reason is "fingerprint" or
// "same-path similarity" and says which rule matched.
type Decision struct {
	Action DedupeAction
	Target string
	Reason string
}

// Decide says whether a finding is new or a repeat of one already filed.
//
// A bead carrying the finding's fingerprint label is the same finding by
// construction. Otherwise a bead that names the same path in its title or
// description and whose title is at least sameSimilarity similar is a
// paraphrase. Anything else is new. A closed match still counts: refiling a
// finding that was fixed would hide that it came back, so the comment says the
// finding was seen again on a closed bead instead.
//
// Nil and empty inputs are safe: with nothing to compare against, and with no
// path to compare on, the answer is DedupeFile.
func Decide(c FollowupCandidate, existing []ExistingFollowup, sameSimilarity float64) Decision {
	if sameSimilarity <= 0 {
		sameSimilarity = DefaultFollowupSimilarity
	}
	fingerprint := Fingerprint(c.Path, c.Title)
	for _, e := range existing {
		if carriesLabel(e.Labels, fingerprint) {
			return Decision{Action: DedupeComment, Target: e.ID, Reason: "fingerprint"}
		}
	}
	if c.Path == "" {
		return Decision{Action: DedupeFile}
	}
	for _, e := range existing {
		if !namesPath(e, c.Path) {
			continue
		}
		if TitleSimilarity(c.Title, e.Title) >= sameSimilarity {
			return Decision{Action: DedupeComment, Target: e.ID, Reason: "same-path similarity"}
		}
	}
	return Decision{Action: DedupeFile}
}

// normalizeTitle reduces a finding title to the words that identify it:
// lowercase, every run of punctuation and quotes to one space, and a leading
// article dropped. "Found: The nil-map panic!" and "found:  nil map panic"
// become the same string, so case, spacing and punctuation cannot change a
// fingerprint on their own.
func normalizeTitle(title string) string {
	var b strings.Builder
	b.Grow(len(title))
	for _, r := range strings.ToLower(title) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		} else {
			b.WriteByte(' ')
		}
	}
	s := strings.Join(strings.Fields(b.String()), " ")
	for {
		trimmed, ok := strings.CutPrefix(s, "the ")
		if !ok {
			trimmed, ok = strings.CutPrefix(s, "a ")
		}
		if !ok {
			return s
		}
		s = trimmed
	}
}

// normalizePath cleans a candidate path so two spellings of one file agree.
// path.Clean also drops a leading "./", so the caller's spelling is otherwise
// kept. An empty path stays empty rather than becoming ".".
func normalizePath(fpath string) string {
	if fpath == "" {
		return ""
	}
	return path.Clean(fpath)
}

// titleTokens is the word set TitleSimilarity compares: lowercase words of
// three or more letters, less the stop words.
func titleTokens(title string) map[string]bool {
	tokens := make(map[string]bool)
	for _, field := range strings.FieldsFunc(strings.ToLower(title), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		if len([]rune(field)) < 3 || followupStopWords[field] {
			continue
		}
		tokens[field] = true
	}
	return tokens
}

// carriesLabel reports whether a bead carries the fingerprint label verbatim.
func carriesLabel(labels []string, want string) bool {
	for _, label := range labels {
		if strings.TrimSpace(label) == want {
			return true
		}
	}
	return false
}

// namesPath reports whether a bead's title or body names the candidate's path.
// The path is compared as written; a bead that says "internal/land/note.go"
// does not cover "internal/land/notes.go".
func namesPath(e ExistingFollowup, fpath string) bool {
	return strings.Contains(e.Title, fpath) || strings.Contains(e.Description, fpath)
}

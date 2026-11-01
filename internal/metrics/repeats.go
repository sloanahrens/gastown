package metrics

import (
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/land"
)

// RepeatSimilarityThreshold is the Jaccard similarity of two findings' title
// and claim words over which they count as one request restated.
//
// The threshold is a policy choice, not a measured cut. A finding a later round
// restates keeps most of its distinctive words, while two different findings on
// one file share little beyond the file's own vocabulary, so the value sits
// between the two: high enough that a shared file and generic review wording
// alone do not match, low enough that a reworded restatement still does. The
// report's share is compared from run to run, so changing the value changes
// what every earlier report meant.
const RepeatSimilarityThreshold = 0.6

// RepeatTopBeads caps the beads-with-most-repeats table: the operator reads
// the worst offenders, not the long tail.
const RepeatTopBeads = 10

// RepeatReport is the repeated findings among a set of review records: how many
// findings the records name, how many restate an earlier round of their own
// bead, and which beads repeat most.
type RepeatReport struct {
	Findings    int     `json:"findings"`
	Repeated    int     `json:"repeated"`
	RepeatShare float64 `json:"repeat_share"`
	// NoText counts the records whose findings count exceeds the finding text
	// they record: the text is what this report can match, so their unrecorded
	// findings are in neither Findings nor Repeated.
	NoText int `json:"no_text"`
	// Skipped counts the lines the reader could not use as a review record.
	Skipped int `json:"skipped"`
	// NoLogAt is the path of a review log that does not exist, set by the
	// caller that looked for it: a missing log reads as an empty report, and
	// this says it was missing, so a wrong town does not look like a quiet one.
	NoLogAt string       `json:"no_log_at,omitempty"`
	Beads   []RepeatBead `json:"beads"`
}

// RepeatBead is one bead's repeat count, for the beads repeating most.
type RepeatBead struct {
	Bead     string  `json:"bead"`
	Rig      string  `json:"rig,omitempty"`
	Findings int     `json:"findings"`
	Repeated int     `json:"repeated"`
	Share    float64 `json:"share"`
}

// RepeatedFindings reports the findings that restate a finding of an earlier
// round of their own bead, matched by file and by the words of title and claim
// rather than by the finding id, which the review log re-hashes every round
// (gt-657q2).
//
// A finding repeats when a record of the same bead with a lower Attempt names a
// finding on the same file whose title and claim share at least
// RepeatSimilarityThreshold of their words. Records of one attempt are never
// compared with each other, so no finding is its own repeat, and a record that
// carries no attempt has no earlier round to compare with.
func RepeatedFindings(records []land.ReviewRecord) RepeatReport {
	report := RepeatReport{Beads: []RepeatBead{}}
	byBead := map[string][]land.ReviewRecord{}
	for _, rec := range records {
		byBead[rec.BeadID] = append(byBead[rec.BeadID], rec)
		if rec.Findings > len(rec.FindingText) {
			report.NoText++
		}
	}
	for bead, recs := range byBead {
		findings, repeated, rig := repeatBeadCounts(recs)
		report.Findings += findings
		report.Repeated += repeated
		if repeated == 0 {
			continue
		}
		report.Beads = append(report.Beads, RepeatBead{
			Bead:     bead,
			Rig:      rig,
			Findings: findings,
			Repeated: repeated,
			Share:    repeatShare(repeated, findings),
		})
	}
	report.RepeatShare = repeatShare(report.Repeated, report.Findings)
	sort.Slice(report.Beads, func(i, j int) bool {
		if report.Beads[i].Repeated != report.Beads[j].Repeated {
			return report.Beads[i].Repeated > report.Beads[j].Repeated
		}
		if report.Beads[i].Findings != report.Beads[j].Findings {
			return report.Beads[i].Findings > report.Beads[j].Findings
		}
		return report.Beads[i].Bead < report.Beads[j].Bead
	})
	if len(report.Beads) > RepeatTopBeads {
		report.Beads = report.Beads[:RepeatTopBeads]
	}
	return report
}

// repeatBeadCounts counts one bead's findings and repeats, and returns the rig
// its last record named.
//
// The records are walked in attempt order with one attempt at a time, so a
// finding is compared with every finding of the bead's earlier attempts and
// with none from its own round.
func repeatBeadCounts(records []land.ReviewRecord) (findings, repeated int, rig string) {
	recs := append([]land.ReviewRecord(nil), records...)
	sort.SliceStable(recs, func(i, j int) bool {
		if recs[i].Attempt != recs[j].Attempt {
			return recs[i].Attempt < recs[j].Attempt
		}
		return recs[i].At.Before(recs[j].At)
	})
	var earlier []repeatFinding
	for i := 0; i < len(recs); {
		roundEnd := i
		for roundEnd < len(recs) && recs[roundEnd].Attempt == recs[i].Attempt {
			roundEnd++
		}
		var round []repeatFinding
		for _, rec := range recs[i:roundEnd] {
			if rec.Rig != "" {
				rig = rec.Rig
			}
			for _, f := range rec.FindingText {
				got := newRepeatFinding(f)
				findings++
				if repeatMatch(got, earlier) {
					repeated++
				}
				round = append(round, got)
			}
		}
		earlier = append(earlier, round...)
		i = roundEnd
	}
	return findings, repeated, rig
}

// repeatFinding is one finding as matching reads it: its file and the set of
// lowercase words of its title and claim.
type repeatFinding struct {
	file  string
	words map[string]struct{}
}

func newRepeatFinding(f land.ReviewFindingText) repeatFinding {
	return repeatFinding{file: f.File, words: repeatWords(f.Title + " " + f.Claim)}
}

// repeatWords is the set of letter and digit runs in text, lowercased:
// punctuation and case say nothing about whether two findings ask for the same
// thing.
func repeatWords(text string) map[string]struct{} {
	found := repeatWordRe.FindAllString(strings.ToLower(text), -1)
	if len(found) == 0 {
		return nil
	}
	words := make(map[string]struct{}, len(found))
	for _, w := range found {
		words[w] = struct{}{}
	}
	return words
}

var repeatWordRe = regexp.MustCompile(`[\p{L}\p{N}]+`)

// repeatMatch reports whether f restates any finding of the earlier rounds.
func repeatMatch(f repeatFinding, earlier []repeatFinding) bool {
	for _, prev := range earlier {
		if prev.file == f.file && repeatJaccard(f.words, prev.words) >= RepeatSimilarityThreshold {
			return true
		}
	}
	return false
}

// repeatJaccard is the share of the two word sets' union they have in common, 0
// when either is empty: no words are no evidence of a restatement.
func repeatJaccard(a, b map[string]struct{}) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	common := 0
	for w := range a {
		if _, ok := b[w]; ok {
			common++
		}
	}
	return float64(common) / float64(len(a)+len(b)-common)
}

func repeatShare(repeated, findings int) float64 {
	if findings == 0 {
		return 0
	}
	return float64(repeated) / float64(findings)
}

// ReadRepeatReport reads the review-record lines of r, keeps those at or after
// since (the zero time for all of them), and reports the repeated findings
// among them.
//
// A line that is not a review record counts in Skipped, so a log the reader
// cannot read is never an empty report. A follow-up queue line is the feed's
// bookkeeping rather than a verdict and is skipped uncounted (gt-3arxj); a
// line with no bead names no review a report could place.
func ReadRepeatReport(r io.Reader, since time.Time) (RepeatReport, error) {
	records, skipped, err := readRepeatRecords(r)
	if err != nil {
		return RepeatReport{}, err
	}
	if !since.IsZero() {
		kept := records[:0]
		for _, rec := range records {
			if !rec.At.Before(since) {
				kept = append(kept, rec)
			}
		}
		records = kept
	}
	report := RepeatedFindings(records)
	report.Skipped = skipped
	return report, nil
}

// readRepeatRecords decodes each line of r as a review record and counts the
// lines it could not use. The lines are read as the review log's own readers
// read them (land.ScanReviewLines): a line past the length cap is counted and
// skipped, not a failed read.
func readRepeatRecords(r io.Reader) ([]land.ReviewRecord, int, error) {
	var records []land.ReviewRecord
	skipped := 0
	tooLong, err := land.ScanReviewLines(r, func(raw []byte) {
		// A queue line carries a bead and an instant like a verdict, so it is
		// refused before it can pass as one (gt-3arxj).
		if len(raw) == 0 || land.IsQueueLine(raw) {
			return
		}
		if rec, ok := parseRepeatRecord(raw); ok {
			records = append(records, rec)
		} else {
			skipped++
		}
	})
	skipped += tooLong
	if err != nil {
		return nil, skipped, fmt.Errorf("reading review records: %w", err)
	}
	return records, skipped, nil
}

// parseRepeatRecord decodes raw as one review record. A line that does not
// decode, names no bead or carries no instant is not one: without a bead there
// is no earlier round to compare its findings with, and without an instant
// --since cannot place it.
func parseRepeatRecord(raw []byte) (land.ReviewRecord, bool) {
	var rec land.ReviewRecord
	if json.Unmarshal(raw, &rec) != nil || rec.BeadID == "" || rec.At.IsZero() {
		return land.ReviewRecord{}, false
	}
	return rec, true
}

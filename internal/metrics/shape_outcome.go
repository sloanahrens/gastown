package metrics

// The shape-outcome join answers one question from the tracker's 15-minute git
// export: does the shape of a bead as filed predict how many review rounds it
// needs and whether its first review approves? Everything here is a pure
// function over decoded rows. The command in internal/cmd reads the archive
// through an injected runner and prints the buckets.

import (
	"bytes"
	"encoding/json"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/steveyegge/gastown/internal/land"
)

// ThinBucketN is the n below which a bucket's numbers are thin: fewer than
// twenty landed beads is a sample an operator must not read as a rate.
const ThinBucketN = 20

// Feature names, in the order the table prints them.
const (
	FeatureAcceptanceBoxes   = "acceptance_boxes"
	FeatureConstraintWords   = "constraint_words"
	FeatureSizeOneWorker     = "size_one_worker"
	FeatureFilesNamed        = "files_named"
	FeatureDescriptionLength = "description_length"
)

// ArchiveIssueRow is the part of one archive issues.jsonl row the join reads:
// the first-filed text that the shape features count and the stamps that date
// it. CreatedAt is parsed at decode, so a row that cannot be dated is refused
// there rather than dated as the zero time here.
type ArchiveIssueRow struct {
	ID                 string
	Description        string
	AcceptanceCriteria string
	CreatedAt          time.Time
	// UpdatedAt is the row's last-write stamp as the commit carries it. It
	// equals CreatedAt while the tracker shows no write since the bead was
	// created, and is zero when the row does not date its last write (which is
	// never equal to the required non-zero CreatedAt).
	UpdatedAt time.Time
}

// archiveIssueJSON is the part of one issues.jsonl line the decode reads.
type archiveIssueJSON struct {
	ID                 string `json:"id"`
	Description        string `json:"description"`
	AcceptanceCriteria string `json:"acceptance_criteria"`
	CreatedAt          string `json:"created_at"`
	UpdatedAt          string `json:"updated_at"`
}

// DecodeArchiveIssues parses one archive commit's issues.jsonl. A line that is
// not JSON, names no id, or carries no parseable created_at is recorded in
// dropped and left out: it is never read as a bead with an empty text and a
// zero date, which would land in the first bucket and skew it.
func DecodeArchiveIssues(data []byte, source string, dropped *Dropped) []ArchiveIssueRow {
	if dropped == nil {
		dropped = &Dropped{}
	}
	var rows []ArchiveIssueRow
	for _, line := range bytes.Split(data, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var raw archiveIssueJSON
		if err := json.Unmarshal(line, &raw); err != nil {
			dropped.Add(source, "unparseable json")
			continue
		}
		if raw.ID == "" {
			dropped.Add(source, "missing id")
			continue
		}
		created, err := ParseTS(raw.CreatedAt)
		if err != nil {
			dropped.Add(source, "missing or unparseable created_at")
			continue
		}
		// A row the decode cannot date at its last write keeps its text and its
		// created_at — the shape query reads neither UpdatedAt nor AsFiled — but
		// UpdatedAt stays zero, so the import never reads it as as-filed.
		updated, err := ParseTS(raw.UpdatedAt)
		if err != nil {
			updated = time.Time{}
		}
		rows = append(rows, ArchiveIssueRow{
			ID:                 raw.ID,
			Description:        raw.Description,
			AcceptanceCriteria: raw.AcceptanceCriteria,
			CreatedAt:          created,
			UpdatedAt:          updated,
		})
	}
	return rows
}

// ArchiveCommit is the issues rows one sampled archive commit holds, with the
// time the commit was made in its own zone.
type ArchiveCommit struct {
	At   time.Time
	Rows []ArchiveIssueRow
}

// FirstFiled is one bead's first-filed row: the row the earliest sampled
// commit that carries the bead holds.
//
// EditedSameDay says the bead's created_at falls on the sampled commit's own
// calendar day, creation converted into the commit's zone so an instant near
// midnight is not compared as two different days. One commit is sampled per
// day, so a bead filed and edited within that day reads as its edited version
// here; the flag counts it rather than letting it pass as the text as filed.
type FirstFiled struct {
	Row           ArchiveIssueRow
	EditedSameDay bool
	// AsFiled says the row's UpdatedAt equals its CreatedAt: the archive shows
	// no write since the bead was created, so the row's description and
	// acceptance criteria are the text as filed. The import records a bead only
	// when this holds, because it cannot correct a text stored at created_at.
	// A row with no updated_at is not as filed.
	AsFiled bool
}

// FirstFiledVersions walks the sampled commits oldest first and keeps, per
// bead id, the row the first commit carrying it holds. A later edit never
// replaces the first row, and the result is in first-seen order so the table
// is the same from run to run.
func FirstFiledVersions(commits []ArchiveCommit) []FirstFiled {
	seen := make(map[string]bool)
	var out []FirstFiled
	for _, c := range commits {
		day := c.At.Format(time.DateOnly)
		for _, row := range c.Rows {
			if seen[row.ID] {
				continue
			}
			seen[row.ID] = true
			out = append(out, FirstFiled{
				Row:           row,
				EditedSameDay: row.CreatedAt.In(c.At.Location()).Format(time.DateOnly) == day,
				AsFiled:       !row.UpdatedAt.IsZero() && row.UpdatedAt.Equal(row.CreatedAt),
			})
		}
	}
	return out
}

// Shape is the countable shape of one bead's first-filed text.
type Shape struct {
	Bead              string
	AcceptanceBoxes   int
	ConstraintWords   int
	SizeOneWorker     bool
	FilesNamed        int
	DescriptionLength int
}

// ShapeFeatures counts row's shape:
//
//   - AcceptanceBoxes is the number of checkbox items in the acceptance
//     criteria, checked or not.
//   - ConstraintWords is the word count of the "Constraints" section of the
//     description, 0 when the filed text has no such section. A word is a
//     whitespace-separated token holding a letter or a digit, so a list
//     marker such as "-" is not counted.
//   - SizeOneWorker is whether the "Size" section says "one worker".
//   - FilesNamed is the number of distinct file paths named in the description
//     and the acceptance criteria.
//   - DescriptionLength is the description's length in runes.
func ShapeFeatures(row ArchiveIssueRow) Shape {
	constraints := shapeSection(row.Description, "Constraints")
	size := shapeSection(row.Description, "Size")
	return Shape{
		Bead:              row.ID,
		AcceptanceBoxes:   len(shapeBoxRE.FindAllStringIndex(row.AcceptanceCriteria, -1)),
		ConstraintWords:   shapeWordCount(constraints),
		SizeOneWorker:     strings.Contains(strings.ToLower(size), "one worker"),
		FilesNamed:        shapeFilesNamed(row.Description + "\n" + row.AcceptanceCriteria),
		DescriptionLength: utf8.RuneCountInString(row.Description),
	}
}

// shapeSection returns the text under the first heading named name (matched
// case-insensitively) in body, up to the next heading of any level. A body
// with no such heading has no section.
func shapeSection(body, name string) string {
	var out []string
	in := false
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if heading, ok := strings.CutPrefix(trimmed, "#"); ok {
			heading = strings.TrimSpace(strings.TrimLeft(heading, "#"))
			in = strings.EqualFold(heading, name)
			continue
		}
		if in {
			out = append(out, line)
		}
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

// shapeWordCount counts the words in text: the whitespace-separated tokens that
// hold a letter or a digit. A Markdown list marker is punctuation standing
// alone, so the bullets of a Constraints section do not inflate its word count.
func shapeWordCount(text string) int {
	n := 0
	for _, tok := range strings.Fields(text) {
		if strings.ContainsFunc(tok, func(r rune) bool {
			return unicode.IsLetter(r) || unicode.IsDigit(r)
		}) {
			n++
		}
	}
	return n
}

// shapeBoxRE matches one acceptance checkbox: "- [ ]", "* [x]" and "+ [X]" are
// the forms a filed bead uses.
var shapeBoxRE = regexp.MustCompile(`(?m)^\s*[-*+]\s+\[[ xX]\]`)

// shapeFileRE matches a token that ends in a source or data extension.
var shapeFileRE = regexp.MustCompile(`^[\w./-]+\.(go|md|jsonl|json|sh|yaml|yml|toml|txt)$`)

// shapeFilesNamed counts the distinct file names in text. Tokens are cut at
// everything that is not a path character, so a backtick, a quote, a comma or
// a sentence's full stop does not hide a name; a URL is skipped.
func shapeFilesNamed(text string) int {
	seen := make(map[string]bool)
	for _, tok := range strings.FieldsFunc(text, func(r rune) bool {
		return !(r == '/' || r == '.' || r == '-' || r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r))
	}) {
		if strings.Contains(tok, "//") {
			continue
		}
		if shapeFileRE.MatchString(tok) {
			seen[tok] = true
		}
	}
	return len(seen)
}

// ShapeReview is one reviews.jsonl verdict the join reads.
type ShapeReview struct {
	Bead     string
	Attempt  int
	Score    float64
	Verdict  string
	Rejected bool
	At       time.Time
}

// DecodeShapeReviews parses the review log's lines. A queue line is the feed's
// bookkeeping rather than a verdict and is skipped uncounted, as the other
// review reader does. Any other line that is not a verdict is recorded in
// dropped, never read as a zero-score review.
func DecodeShapeReviews(lines []string, source string, dropped *Dropped) []ShapeReview {
	if dropped == nil {
		dropped = &Dropped{}
	}
	var rows []ShapeReview
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		raw := []byte(line)
		if !json.Valid(raw) {
			dropped.Add(source, "unparseable json")
			continue
		}
		if land.IsQueueLine(raw) {
			continue
		}
		var rec struct {
			Bead     string   `json:"bead"`
			Attempt  *int     `json:"attempt"`
			Score    *float64 `json:"score"`
			Verdict  string   `json:"verdict"`
			Rejected bool     `json:"rejected"`
			At       string   `json:"at"`
		}
		if err := json.Unmarshal(raw, &rec); err != nil {
			dropped.Add(source, "unparseable json")
			continue
		}
		at, err := ParseTS(rec.At)
		if err != nil || rec.Bead == "" || rec.Attempt == nil || *rec.Attempt < 1 ||
			rec.Score == nil || rec.Verdict == "" {
			dropped.Add(source, "missing or unparseable field")
			continue
		}
		rows = append(rows, ShapeReview{
			Bead:     rec.Bead,
			Attempt:  *rec.Attempt,
			Score:    *rec.Score,
			Verdict:  rec.Verdict,
			Rejected: rec.Rejected,
			At:       at,
		})
	}
	return rows
}

// ShapeOutcome is one landed bead's outcome: the rounds it took, its last
// review's score, whether its first review approved, and how long it took from
// creation to closing when the archive's events date both ends.
type ShapeOutcome struct {
	Rounds        int
	Score         float64
	ApprovedFirst bool
	HoursToClose  float64
	HasHours      bool
}

// ShapeOutcomes groups the review rows into one outcome per landed bead.
//
// A bead is landed when its last review is an approval the landing did not
// refuse; a bead whose last review requests changes never reached the target,
// and its rounds are not a cost the target paid, so it is left out (the
// caller counts it). Rounds is the highest attempt, which is the landing's own
// round number. The first review is the lowest attempt: an approval there
// means the bead landed on its first round. Score is the highest attempt's
// score, and a tie is decided by the later verdict.
//
// hours fills HoursToClose where the archive's events carry both stamps; a
// bead they do not date has none rather than zero.
func ShapeOutcomes(reviews []ShapeReview, hours map[string]time.Duration) map[string]ShapeOutcome {
	type bead struct {
		first, last ShapeReview
		rounds      int
	}
	byBead := make(map[string]*bead)
	for _, r := range reviews {
		b := byBead[r.Bead]
		if b == nil {
			byBead[r.Bead] = &bead{first: r, last: r, rounds: r.Attempt}
			continue
		}
		if r.Attempt < b.first.Attempt {
			b.first = r
		}
		if r.Attempt > b.last.Attempt || (r.Attempt == b.last.Attempt && r.At.After(b.last.At)) {
			b.last = r
		}
		if r.Attempt > b.rounds {
			b.rounds = r.Attempt
		}
	}
	out := make(map[string]ShapeOutcome, len(byBead))
	for id, b := range byBead {
		if b.last.Verdict != land.VerdictApprove || b.last.Rejected {
			continue
		}
		o := ShapeOutcome{
			Rounds:        b.rounds,
			Score:         b.last.Score,
			ApprovedFirst: b.first.Verdict == land.VerdictApprove && !b.first.Rejected,
		}
		if d, ok := hours[id]; ok {
			o.HoursToClose = d.Hours()
			o.HasHours = true
		}
		out[id] = o
	}
	return out
}

// ArchiveCloseHours returns each bead's created-to-closed time from the
// archive's event lines: the earliest "created" event and the latest
// "closed" one. A bead missing either stamp is absent rather than zero, and a
// line that cannot be read or dated is recorded in dropped.
func ArchiveCloseHours(events []byte, source string, dropped *Dropped) map[string]time.Duration {
	if dropped == nil {
		dropped = &Dropped{}
	}
	created := make(map[string]time.Time)
	closed := make(map[string]time.Time)
	for _, line := range bytes.Split(events, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var e struct {
			Issue string `json:"issue_id"`
			Type  string `json:"event_type"`
			At    string `json:"created_at"`
		}
		if err := json.Unmarshal(line, &e); err != nil {
			dropped.Add(source, "unparseable json")
			continue
		}
		if e.Type != "created" && e.Type != "closed" {
			continue
		}
		if e.Issue == "" {
			dropped.Add(source, "created or closed event without an issue")
			continue
		}
		at, err := archiveEventTime(e.At)
		if err != nil {
			dropped.Add(source, "unparseable created_at")
			continue
		}
		if e.Type == "created" {
			if cur, ok := created[e.Issue]; !ok || at.Before(cur) {
				created[e.Issue] = at
			}
			continue
		}
		if cur, ok := closed[e.Issue]; !ok || at.After(cur) {
			closed[e.Issue] = at
		}
	}
	out := make(map[string]time.Duration)
	for id, c := range created {
		if cl, ok := closed[id]; ok && !cl.Before(c) {
			out[id] = cl.Sub(c)
		}
	}
	return out
}

// archiveEventTime parses an event timestamp: the export's own
// "2006-01-02 15:04:05" UTC form, or an ISO timestamp with a zone.
func archiveEventTime(s string) (time.Time, error) {
	if t, err := time.ParseInLocation("2006-01-02 15:04:05", s, time.UTC); err == nil {
		return t, nil
	}
	return ParseTS(s)
}

// BucketOutcome is one bucket of one shape feature and the outcomes of the
// landed beads whose first-filed text fell in it. A bucket with no landed bead
// still appears, with N 0, so an empty band is visible rather than hidden.
type BucketOutcome struct {
	Feature      string
	Label        string
	N            int
	MedianRounds float64
	ApproveShare float64
	MedianScore  float64
	MedianHours  float64
	// HoursN is how many bucket beads have a time to close; MedianHours is
	// over those, so it carries its own n.
	HoursN int
	Thin   bool
}

// shapeBand is one bucket of a numeric feature: label names it, and hi below
// zero means no upper bound.
type shapeBand struct {
	label  string
	lo, hi int
}

// shapeFeature is one feature and the buckets the table prints for it.
type shapeFeature struct {
	name  string
	value func(Shape) int
	bands []shapeBand
}

// shapeFeatures is every feature the table carries, in print order. The
// buckets are the bands the question is asked in; every feature's bands are
// contiguous over the values a filed bead takes.
var shapeFeatures = []shapeFeature{
	{FeatureAcceptanceBoxes, func(s Shape) int { return s.AcceptanceBoxes }, []shapeBand{
		{"0", 0, 0}, {"1", 1, 1}, {"2-3", 2, 3}, {"4+", 4, -1},
	}},
	{FeatureConstraintWords, func(s Shape) int { return s.ConstraintWords }, []shapeBand{
		{"0", 0, 0}, {"1-50", 1, 50}, {"51-150", 51, 150}, {"151+", 151, -1},
	}},
	{FeatureSizeOneWorker, func(s Shape) int {
		if s.SizeOneWorker {
			return 1
		}
		return 0
	}, []shapeBand{{"yes", 1, 1}, {"no", 0, 0}}},
	{FeatureFilesNamed, func(s Shape) int { return s.FilesNamed }, []shapeBand{
		{"0", 0, 0}, {"1-2", 1, 2}, {"3-5", 3, 5}, {"6+", 6, -1},
	}},
	{FeatureDescriptionLength, func(s Shape) int { return s.DescriptionLength }, []shapeBand{
		{"0-500", 0, 500}, {"501-1500", 501, 1500}, {"1501-4000", 1501, 4000}, {"4001+", 4001, -1},
	}},
}

// BucketOutcomes buckets each shape by every feature and joins the outcome of
// the bead's landing. A bead with no outcome (it did not land, or the review
// log does not reach it) is not counted in any bucket; the caller accounts for
// those. Thin marks a bucket under ThinBucketN.
func BucketOutcomes(features []Shape, outcomes map[string]ShapeOutcome) []BucketOutcome {
	type acc struct {
		rounds, scores, hours []float64
		approved              int
	}
	// accs[feature][band]
	accs := make([][]acc, len(shapeFeatures))
	for i, f := range shapeFeatures {
		accs[i] = make([]acc, len(f.bands))
	}
	for _, s := range features {
		o, ok := outcomes[s.Bead]
		if !ok {
			continue
		}
		for i, f := range shapeFeatures {
			v := f.value(s)
			for j, b := range f.bands {
				if v < b.lo || (b.hi >= 0 && v > b.hi) {
					continue
				}
				a := &accs[i][j]
				a.rounds = append(a.rounds, float64(o.Rounds))
				a.scores = append(a.scores, o.Score)
				if o.ApprovedFirst {
					a.approved++
				}
				if o.HasHours {
					a.hours = append(a.hours, o.HoursToClose)
				}
			}
		}
	}
	var out []BucketOutcome
	for i, f := range shapeFeatures {
		for j, b := range f.bands {
			a := accs[i][j]
			row := BucketOutcome{Feature: f.name, Label: b.label, N: len(a.rounds)}
			row.Thin = row.N < ThinBucketN
			if row.N > 0 {
				row.MedianRounds = shapeMedian(a.rounds)
				row.MedianScore = shapeMedian(a.scores)
				row.ApproveShare = float64(a.approved) / float64(row.N)
			}
			row.HoursN = len(a.hours)
			if row.HoursN > 0 {
				row.MedianHours = shapeMedian(a.hours)
			}
			out = append(out, row)
		}
	}
	return out
}

// shapeMedian returns the median of xs, or 0 when xs is empty (a bucket with
// no landed bead has no middle).
func shapeMedian(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	sorted := append([]float64(nil), xs...)
	sort.Float64s(sorted)
	return *median(sorted)
}

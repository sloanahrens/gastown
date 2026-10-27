package metrics

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/steveyegge/gastown/internal/land"
)

// The digest's rejection-rate section: how often landings were refused, split
// by the om checklist pattern each refusal matched and by the agent seat that
// authored the work, week by week. It joins the landing worker's review log to
// the MERGE REJECTION notes the refusals left on the beads and reads nothing
// else; gt land om-stats owns the town-wide verdict counts (gt-b9j3o.10).

// RejectionPatternUnclassified is the row a rejection counts in when its note
// matched no checklist rule.
const RejectionPatternUnclassified = "unclassified"

// RejectionPatternNoNote is the row a rejection counts in when its bead
// carries no rejection note for that attempt: the note is the only place a
// pattern is recorded, so a refusal whose note is missing is unknown rather
// than unclassified.
const RejectionPatternNoNote = "no note"

// rejectionPattern is one rule of the om checklist as the section measures it.
// The checklist is the polecat directive's "What om rejects" list, and each
// pattern is the phrase om's finding for that rule carries, so a rule is
// counted by the wording that names it.
type rejectionPattern struct {
	// Name is the row label the section prints.
	Name string
	// Phrases are lowercase substrings of a rejection note that name the
	// pattern. A note matching none of them is unclassified.
	Phrases []string
}

// rejectionPatterns is the pattern table, in classification order: the first
// pattern holding one of its phrases names the rejection. The order is the
// checklist's, so a note that names two rules counts as the earlier one and a
// week's pattern rows add up to its rejections.
//
// The kind word a note's header opens with (conflict, gate, review) is a
// different axis: ClassifyRejection reads that one, and this table reads why
// om refused, from the finding's wording.
var rejectionPatterns = []rejectionPattern{
	{Name: "fail_open", Phrases: []string{"fail-open", "fail open"}},
	{Name: "criterion_untested", Phrases: []string{"has no test", "no test for the criterion"}},
	{Name: "refusal_untested", Phrases: []string{
		"refusal branch", "failing branch", "branch has no assertion", "branches lack tests",
	}},
	{Name: "forged_authority", Phrases: []string{"prefix alone", "can forge", "authority is granted by"}},
	{Name: "self_review", Phrases: []string{"self-rebut", "rebut its own", "review its own"}},
	{Name: "unshown_verification", Phrases: []string{"no shown output", "without the run's output", "verification claim"}},
	{Name: "hermeticity", Phrases: []string{"not hermetic", "hermetic", "$shell", "$home", "live town tree"}},
	{Name: "script", Phrases: []string{"shell script", "python script", "new script", "new .sh"}},
	{Name: "out_of_scope", Phrases: []string{
		"outside the stated scope", "unexplained transitive dependency", "out of scope",
	}},
	{Name: "call_site_untested", Phrases: []string{"helper directly", "call site", "logging call deleted"}},
	{Name: "helper_fail_open", Phrases: []string{"does not apply", "helper error", "fail closed"}},
	{Name: "secrets_in_errors", Phrases: []string{"captured output", "captured command output", "secret in the error"}},
}

// classifyRejectionPattern names the pattern note matches: the first rule in
// rejectionPatterns holding one of its phrases, else
// RejectionPatternUnclassified. Case is ignored, because a finding's wording
// reaches a note through free text.
func classifyRejectionPattern(note string) string {
	text := strings.ToLower(note)
	for _, pattern := range rejectionPatterns {
		for _, phrase := range pattern.Phrases {
			if strings.Contains(text, phrase) {
				return pattern.Name
			}
		}
	}
	return RejectionPatternUnclassified
}

// rejectionBlockRE finds a rejection block's header, anchored the way
// land.ParseRejectionNote anchors it: a marker quoted inside a finding is
// content, not a block. Only the boundary is read here; the fields inside a
// block belong to land (gt-9bioi.1).
var rejectionBlockRE = regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(land.MergeRejectionNoteMarker) + ` \(attempt ([0-9]+)\): `)

// rejectionBlock is one MERGE REJECTION block of a bead's notes: the attempt
// it refused and the block's text.
type rejectionBlock struct {
	Attempt int
	Text    string
}

// rejectionBlocks splits notes at the anchors of its rejection blocks, in
// order. A note with no block yields none.
func rejectionBlocks(notes string) []rejectionBlock {
	headers := rejectionBlockRE.FindAllStringSubmatchIndex(notes, -1)
	blocks := make([]rejectionBlock, 0, len(headers))
	for i, header := range headers {
		attempt, err := strconv.Atoi(notes[header[2]:header[3]])
		if err != nil {
			// The header regex admits digits only, so this cannot happen.
			continue
		}
		end := len(notes)
		if i+1 < len(headers) {
			end = headers[i+1][0]
		}
		blocks = append(blocks, rejectionBlock{Attempt: attempt, Text: notes[header[0]:end]})
	}
	return blocks
}

// rejectionPatternFor is the pattern of record's refusal: the block its
// attempt wrote, classified, or RejectionPatternNoNote when the bead has no
// block for that attempt.
func rejectionPatternFor(rec land.ReviewRecord, notes map[string]string) string {
	for _, block := range rejectionBlocks(notes[rec.BeadID]) {
		if block.Attempt == rec.Attempt {
			return classifyRejectionPattern(block.Text)
		}
	}
	return RejectionPatternNoNote
}

// RejectionWeekGroup is one week's verdicts for one pattern or one seat.
// Verdicts is the row's denominator: the week's recorded verdicts for a
// pattern, the seat's own for a seat.
type RejectionWeekGroup struct {
	Week       string
	Name       string
	Verdicts   int
	Rejections int
	Rate       float64
}

// RejectionRates joins the review records to the beads' rejection notes and
// returns the week tables, per pattern and per seat, each ordered by week and
// then by rejections, most first.
//
// A record is one verdict; a rejection is a verdict that refused the work,
// whether om asked for changes or the worker turned an approval away on its
// findings. Each rejection is counted once, under the first checklist rule its
// note matches, so a week's pattern rejections add up to its rejections. A
// record that names no seat counts as land.OmStatsUnnamed.
func RejectionRates(records []land.ReviewRecord, notes map[string]string) (byPattern, bySeat []RejectionWeekGroup, err error) {
	weekVerdicts := map[string]int{}
	seatVerdicts := map[[2]string]int{}
	patternRejections := map[[2]string]int{}
	seatRejections := map[[2]string]int{}
	for _, rec := range records {
		if rec.At.IsZero() {
			return nil, nil, fmt.Errorf("review of %s carries no instant", rec.BeadID)
		}
		week := weekOf(rec.At)
		seat := rec.Agent
		if seat == "" {
			seat = land.OmStatsUnnamed
		}
		key := [2]string{week, seat}
		weekVerdicts[week]++
		seatVerdicts[key]++
		if !rec.IsRejection() {
			continue
		}
		pattern := rejectionPatternFor(rec, notes)
		patternRejections[[2]string{week, pattern}]++
		seatRejections[key]++
	}
	// A pattern is only known from a refusal's note, so a week's pattern rows
	// are the patterns it saw, each over the week's whole verdict count; a
	// seat is known from every verdict it cast, so a seat that refused none
	// reads 0% over its own.
	patternVerdicts := make(map[[2]string]int, len(patternRejections))
	for key := range patternRejections {
		patternVerdicts[key] = weekVerdicts[key[0]]
	}
	byPattern = rejectionGroups(patternVerdicts, patternRejections)
	bySeat = rejectionGroups(seatVerdicts, seatRejections)
	return byPattern, bySeat, nil
}

// rejectionGroups builds the table rows from one entry per (week, name), the
// entries being the rows' denominators: verdicts holds each key's verdicts and
// rejections its refusals, absent when the row refused none.
func rejectionGroups(verdicts, rejections map[[2]string]int) []RejectionWeekGroup {
	groups := make([]RejectionWeekGroup, 0, len(verdicts))
	for key, total := range verdicts {
		n := rejections[key]
		groups = append(groups, RejectionWeekGroup{
			Week:       key[0],
			Name:       key[1],
			Verdicts:   total,
			Rejections: n,
			Rate:       fraction(n, total),
		})
	}
	sort.Slice(groups, func(i, j int) bool {
		if groups[i].Week != groups[j].Week {
			return groups[i].Week < groups[j].Week
		}
		if groups[i].Rejections != groups[j].Rejections {
			return groups[i].Rejections > groups[j].Rejections
		}
		return groups[i].Name < groups[j].Name
	})
	return groups
}

// MangoRejectionsSection renders the digest's rejection tables, reading the
// town's review log and the notes of the beads it refused through env. A town
// with no review log yet has no verdicts and says so; an unreadable log or a
// bead listing that fails is an error, never an empty table.
func MangoRejectionsSection(ctx context.Context, env Env) (string, error) {
	switch {
	case env.ReadFile == nil:
		return "", errors.New("rejections section: Env.ReadFile is unset")
	case env.Run == nil:
		return "", errors.New("rejections section: Env.Run is unset")
	}
	path := land.TownReviewsFile(env.Town).Path
	data, err := env.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "### Rejections by pattern\n\nNo review records yet: " + path + " does not exist.", nil
	}
	if err != nil {
		return "", fmt.Errorf("reviews.jsonl: %w", err)
	}
	records, skipped := parseReviewRecords(data)
	dirs, err := rejectionNoteDirs(env.Town, records)
	if err != nil {
		return "", err
	}
	notes, err := loadRejectionNotes(ctx, env, dirs)
	if err != nil {
		return "", err
	}
	byPattern, bySeat, err := RejectionRates(records, notes)
	if err != nil {
		return "", err
	}
	return rejectionSectionText(byPattern, bySeat, skipped), nil
}

// parseReviewRecords reads the log's lines into records. A line that is not a
// record, names no bead or no verdict is skipped and counted, so the section
// can say how many lines it could not attribute; a record with no instant is
// kept, so a log that lost one fails the section rather than a week.
func parseReviewRecords(data []byte) ([]land.ReviewRecord, int) {
	var records []land.ReviewRecord
	skipped := 0
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var rec land.ReviewRecord
		if json.Unmarshal([]byte(line), &rec) != nil || rec.BeadID == "" || rec.Verdict == "" {
			skipped++
			continue
		}
		records = append(records, rec)
	}
	return records, skipped
}

// rejectionNoteDirs is the bead stores the join reads: one per rig a refused
// record names, and the town's own store for a record that names none. Only
// refusals need notes, so an approval alone costs no bd call.
//
// A rig that is not one path element is an error: the record comes from a
// file, and a rig name reaching filepath.Join unchecked could point bd at a
// directory the town never named.
func rejectionNoteDirs(town string, records []land.ReviewRecord) ([]string, error) {
	seen := map[string]bool{}
	var dirs []string
	for _, rec := range records {
		if !rec.IsRejection() {
			continue
		}
		dir := town
		if rec.Rig != "" {
			if rec.Rig != filepath.Base(rec.Rig) || rec.Rig == "." || rec.Rig == ".." {
				return nil, fmt.Errorf("review of %s names rig %q, which is not one path element", rec.BeadID, rec.Rig)
			}
			dir = filepath.Join(town, rec.Rig)
		}
		if !seen[dir] {
			seen[dir] = true
			dirs = append(dirs, dir)
		}
	}
	sort.Strings(dirs)
	return dirs, nil
}

// loadRejectionNotes reads each store's bead list through env.Run and returns
// the beads' notes by id. A listing that fails, or that is not a bead list,
// fails the section: a join missing one rig's notes would report its refusals
// as note-less rather than say the read broke.
func loadRejectionNotes(ctx context.Context, env Env, dirs []string) (map[string]string, error) {
	notes := map[string]string{}
	for _, dir := range dirs {
		out, err := env.Run(ctx, []string{"bd", "-C", dir, "list", "--all", "--limit", "0", "--json"})
		if err != nil {
			return nil, fmt.Errorf("bd list in %s: %w", dir, err)
		}
		var rows []bdListBead
		if err := json.Unmarshal(out, &rows); err != nil {
			return nil, fmt.Errorf("bd list in %s: the output is not a bead list", dir)
		}
		for _, row := range rows {
			if row.ID != "" {
				notes[row.ID] = row.Notes
			}
		}
	}
	return notes, nil
}

// rejectionSectionText renders the two tables and the count of lines the log's
// reader could not attribute.
func rejectionSectionText(byPattern, bySeat []RejectionWeekGroup, skipped int) string {
	if len(bySeat) == 0 {
		text := "### Rejections by pattern\n\nNo review records yet: no verdict to count."
		if skipped > 0 {
			text += rejectionSkippedLines(skipped)
		}
		return text
	}
	var b strings.Builder
	b.WriteString("### Rejections by pattern\n\n")
	b.WriteString("A rejection is one verdict that refused a landing, counted under the first om " +
		"checklist rule its bead's MERGE REJECTION note matches. Rate is the pattern's rejections " +
		"over every recorded verdict that week, so a week's rows add up to its rejection rate. " +
		"A refusal whose note matched no rule is " + RejectionPatternUnclassified + "; one whose " +
		"bead carries no note for that attempt is " + RejectionPatternNoNote + ".\n\n")
	b.WriteString(rejectionTable([]string{"week", "pattern", "rejections", "verdicts", "rate"}, byPattern))
	b.WriteString("\n\n### Rejections by agent seat\n\n")
	b.WriteString("Rate is the seat's rejections over that seat's own recorded verdicts that week. " +
		"A record written before the log carried a seat counts as " + land.OmStatsUnnamed + ".\n\n")
	b.WriteString(rejectionTable([]string{"week", "seat", "rejections", "verdicts", "rate"}, bySeat))
	if skipped > 0 {
		b.WriteString(rejectionSkippedLines(skipped))
	}
	return b.String()
}

// rejectionSkippedLines names the log's unreadable lines, so a section built
// from the lines around them says how much of the log it could not read.
func rejectionSkippedLines(skipped int) string {
	return fmt.Sprintf("\n\n%d review-log line(s) were not readable as records and count in no row.", skipped)
}

// rejectionTable is the rows as the report's Markdown table.
func rejectionTable(header []string, groups []RejectionWeekGroup) string {
	lines := []string{
		"| " + strings.Join(header, " | ") + " |",
		"|" + strings.Repeat("---|", len(header)),
	}
	for _, g := range groups {
		lines = append(lines, "| "+strings.Join([]string{
			g.Week, g.Name, strconv.Itoa(g.Rejections), strconv.Itoa(g.Verdicts), rejectionPercent(g.Rate),
		}, " | ")+" |")
	}
	return strings.Join(lines, "\n")
}

// rejectionPercent is a rate as the table writes it.
func rejectionPercent(rate float64) string {
	return strconv.FormatFloat(100*rate, 'f', 1, 64) + "%"
}

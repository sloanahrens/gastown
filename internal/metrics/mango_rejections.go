package metrics

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
// The checklist is the directive's "What om rejects" list; a pattern's phrases
// are fragments of the finding titles om writes when that rule is what it
// refuses, never a bullet of the checklist itself, so the rule is counted by
// the wording a rejection actually carries. A rule the directive rewords is
// re-read from the findings it files, not translated from the new bullet.
type rejectionPattern struct {
	// Name is the row label the section prints.
	Name string
	// Rule is the rule's number in the directive's list, which the phrases are
	// read from so a reworded rule is re-read rather than translated.
	Rule int
	// Phrases are lowercase substrings of a rejection's finding titles that
	// name the pattern. A rejection matching none of them is unclassified.
	Phrases []string
}

// rejectionPatterns is the pattern table, in classification order: the first
// pattern holding one of its phrases names the rejection. The order is the
// checklist's, so a refusal whose findings name two rules counts as the
// earlier one.
//
// Before adding or changing a phrase, read the rule's own wording in
// directives/polecat.md's "What om rejects" section: each phrase below is a
// fragment of what that file quotes om filing for the same rule, and
// TestEveryRejectionPhraseIsInTheDirective holds it there, so the table cannot
// drift into matching the checklist's own prose.
//
// The kind word a note's header opens with (conflict, gate, review) is a
// different axis: this table reads why om refused, from the finding's wording.
var rejectionPatterns = []rejectionPattern{
	{Name: "fail_open", Rule: 1, Phrases: []string{"fail-open", "does not apply"}},
	{Name: "criterion_untested", Rule: 2, Phrases: []string{"has no test"}},
	{Name: "refusal_untested", Rule: 3, Phrases: []string{"failing branch", "branch has no assertion"}},
	{Name: "forged_authority", Rule: 4, Phrases: []string{"prefix alone", "can forge", "authority is granted by"}},
	{Name: "self_review", Rule: 5, Phrases: []string{"self-rebut", "rebut its own"}},
	{Name: "unshown_verification", Rule: 6, Phrases: []string{"no shown output", "verification claim"}},
	{Name: "hermeticity", Rule: 7, Phrases: []string{"not hermetic", "$shell", "live town tree"}},
	{Name: "script", Rule: 8, Phrases: []string{"shell script", "python script"}},
	{Name: "out_of_scope", Rule: 9, Phrases: []string{"outside the bead's constraints", "unexplained transitive dependency"}},
	{Name: "call_site_untested", Rule: 10, Phrases: []string{"helper directly", "call site"}},
	{Name: "secrets_in_errors", Rule: 11, Phrases: []string{"captured output", "captured command output"}},
}

// classifyRejectionPattern names the pattern findings match: the first rule in
// rejectionPatterns holding one of its phrases, else
// RejectionPatternUnclassified. Case is ignored, because a finding's wording
// reaches a note through free text.
func classifyRejectionPattern(findings []string) string {
	text := strings.ToLower(strings.Join(findings, "\n"))
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
var rejectionBlockRE = regexp.MustCompile(`^` + regexp.QuoteMeta(land.MergeRejectionNoteMarker) + ` \(attempt ([0-9]+)\): `)

// rejectionFindingRE reads a rejection block's finding line, the
// "- id:<id> sev:<severity> <path>:<line> — <title>" FormatRejectionNote
// writes. The title is what the pattern table reads.
var rejectionFindingRE = regexp.MustCompile(`^- id:\S+ sev:\S+ .*:[0-9]+ — (.*)$`)

// rejectionFieldRE matches a line a rejection block carries besides its
// findings: the fields FormatRejectionNote writes and the gate tail's quoted
// lines.
var rejectionFieldRE = regexp.MustCompile(`^(Branch|Target|MR|Score|Unresolved|Head|Conflicting): |^Gate tail:$|^  \| `)

// rejectionBlock is one MERGE REJECTION block of a bead's notes: the attempt
// it refused and the titles of the findings it filed.
type rejectionBlock struct {
	Attempt int
	// Findings are the finding titles. A block's other lines are not read: a
	// pattern is a finding om filed, and the gate tail can quote any output.
	Findings []string
}

// rejectionBlocks splits notes at the headers of its rejection blocks, in
// order. A block is its header and the lines that continue it: the fields and
// findings FormatRejectionNote writes, and the gate tail. The notes carry
// later notes after a block, and their prose is not part of the refusal, so a
// block ends at its first line the block's grammar does not admit
// (gt-b9j3o.10). A note with no block yields none.
func rejectionBlocks(notes string) []rejectionBlock {
	lines := strings.Split(notes, "\n")
	var blocks []rejectionBlock
	for i := 0; i < len(lines); i++ {
		header := rejectionBlockRE.FindStringSubmatch(lines[i])
		if header == nil {
			continue
		}
		attempt, err := strconv.Atoi(header[1])
		if err != nil {
			// The header admits any run of digits, and a run too long for an
			// int overflows here, so this is not a header land's parser accepts
			// either. No review names such an attempt, so the block is skipped
			// rather than read as another attempt's note.
			continue
		}
		var findings []string
		end := i + 1
		for ; end < len(lines); end++ {
			line := lines[end]
			if m := rejectionFindingRE.FindStringSubmatch(line); m != nil {
				findings = append(findings, m[1])
				continue
			}
			if line == "" || !rejectionFieldRE.MatchString(line) {
				break
			}
		}
		blocks = append(blocks, rejectionBlock{Attempt: attempt, Findings: findings})
		i = end - 1
	}
	return blocks
}

// RejectionNoteKey names one bead's notes in the join: the rig whose store
// lists it ("" is the town's own store) and its id. A bead id alone is not
// unique across a town's stores, so a join keyed by id would read one store's
// notes for another store's bead.
type RejectionNoteKey struct {
	Rig  string
	Bead string
}

// rejectionPatternFor is the pattern of rec's refusal: the findings of the
// block its attempt wrote, classified, or RejectionPatternNoNote when the bead
// has no block for that attempt.
func rejectionPatternFor(rec land.ReviewRecord, notes map[RejectionNoteKey]string) string {
	key := RejectionNoteKey{Rig: rec.Rig, Bead: rec.BeadID}
	for _, block := range rejectionBlocks(notes[key]) {
		if block.Attempt == rec.Attempt {
			return classifyRejectionPattern(block.Findings)
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
// note's findings match, so a week's pattern rejections add up to its
// rejections. A record that names no seat counts as land.OmStatsUnnamed.
func RejectionRates(records []land.ReviewRecord, notes map[RejectionNoteKey]string) (byPattern, bySeat []RejectionWeekGroup, err error) {
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
// town's review log and the notes of the beads it refused through env: one
// review-log read, then one store read per rig a refusal names, asking the
// store for the refused beads alone.
//
// A town with a log but no verdicts yet says so; a missing or unreadable log
// is an error naming its path, and so is a bead whose notes the store does not
// return, because a join that read them as absent would report a refusal as
// note-less instead of saying the read broke.
func MangoRejectionsSection(ctx context.Context, env Env) (string, error) {
	switch {
	case env.Town == "":
		return "", errors.New("rejections section: Env.Town is unset")
	case env.ReadFile == nil:
		return "", errors.New("rejections section: Env.ReadFile is unset")
	case env.Beads == nil:
		return "", errors.New("rejections section: Env.Beads is unset")
	}
	records, skipped, err := townReviewRecords(env)
	if err != nil {
		return "", err
	}
	return mangoRejectionsSection(ctx, env, records, skipped)
}

// townReviewRecords reads the town's review log: the records the digest's
// tables join, and the lines that are not records. The digest reads it once
// and hands the same lines to the refusal tables and the first-pass table
// (gt-b9j3o.15).
func townReviewRecords(env Env) ([]land.ReviewRecord, int, error) {
	path := land.TownReviewsFile(env.Town).Path
	data, err := env.ReadFile(path)
	if err != nil {
		return nil, 0, fmt.Errorf("reviews.jsonl: %s: %w", path, err)
	}
	records, skipped := parseReviewRecords(data)
	return records, skipped, nil
}

// mangoRejectionsSection renders the tables from records the caller already
// read.
func mangoRejectionsSection(ctx context.Context, env Env, records []land.ReviewRecord, skipped int) (string, error) {
	stores, err := rejectionStores(env.Town, records)
	if err != nil {
		return "", err
	}
	notes, err := loadRejectionNotes(ctx, env, stores)
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
// can say how many lines it could not attribute; a follow-up queue line names
// no verdict on purpose and is skipped uncounted (gt-3arxj); a record with no
// instant is kept, so a log that lost one fails the section rather than a week.
func parseReviewRecords(data []byte) ([]land.ReviewRecord, int) {
	var records []land.ReviewRecord
	skipped := 0
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var rec land.ReviewRecord
		if json.Unmarshal([]byte(line), &rec) != nil || rec.BeadID == "" {
			skipped++
			continue
		}
		// A follow-up queue line is the feed's bookkeeping rather than a
		// verdict, and land.IsQueueLine is the one decision on what one is
		// (gt-3arxj).
		if land.IsQueueLine([]byte(line)) {
			continue
		}
		if rec.Verdict == "" {
			skipped++
			continue
		}
		records = append(records, rec)
	}
	return records, skipped
}

// rejectionStore is one bead store the join reads: the rig the review log
// names ("" is the town's own store), the directory bd reads it in, and the
// refused beads whose notes the section asks for.
type rejectionStore struct {
	Rig   string
	Dir   string
	Beads []string
}

// rejectionStores is the bead stores the join reads, with the beads each one
// holds: one store per rig a refused record names, and the town's own store
// for a record that names none. Only refusals need notes, so an approval alone
// costs no store read.
//
// A rig that is not one path element is an error: the record comes from a
// file, and a rig name reaching filepath.Join unchecked could point the store
// at a directory the town never named.
func rejectionStores(town string, records []land.ReviewRecord) ([]rejectionStore, error) {
	beads := map[string]map[string]bool{}
	for _, rec := range records {
		if !rec.IsRejection() {
			continue
		}
		if rec.Rig != "" && (rec.Rig != filepath.Base(rec.Rig) || rec.Rig == "." || rec.Rig == "..") {
			return nil, fmt.Errorf("review of %s names rig %q, which is not one path element", rec.BeadID, rec.Rig)
		}
		// A store reads an id as an argument, and bd reads an argument beginning
		// with a dash as a flag, so an id the log names that is not a bead id
		// refuses the log rather than reaching a store. The empty id is one too:
		// no answer a store could give for it would be a bead.
		if !isBeadID(rec.BeadID) {
			return nil, fmt.Errorf("review names bead id %q, which is not a bead id", rec.BeadID)
		}
		if beads[rec.Rig] == nil {
			beads[rec.Rig] = map[string]bool{}
		}
		beads[rec.Rig][rec.BeadID] = true
	}
	stores := make([]rejectionStore, 0, len(beads))
	for rig, ids := range beads {
		store := rejectionStore{Rig: rig, Dir: town}
		if rig != "" {
			store.Dir = filepath.Join(town, rig)
		}
		store.Beads = make([]string, 0, len(ids))
		for id := range ids {
			store.Beads = append(store.Beads, id)
		}
		sort.Strings(store.Beads)
		stores = append(stores, store)
	}
	sort.Slice(stores, func(i, j int) bool { return stores[i].Dir < stores[j].Dir })
	return stores, nil
}

// loadRejectionNotes reads the refused beads' notes, one store read per rig,
// keyed by the store and the bead. The read goes through the bead store seam
// (showBeads), so the section runs no bd of its own and an id reaches the store
// as its own argument rather than as an --id= flag. A read that fails, or one
// that does not return every bead asked for, fails the section: a join missing
// one bead's notes would report its refusals as note-less rather than say the
// read broke. The error keeps "bd show in <dir>", the wording the pre-seam
// reader's failure carried.
func loadRejectionNotes(ctx context.Context, env Env, stores []rejectionStore) (map[RejectionNoteKey]string, error) {
	notes := map[RejectionNoteKey]string{}
	for _, store := range stores {
		found, err := showBeads(ctx, env, store.Dir, store.Beads)
		if err != nil {
			return nil, fmt.Errorf("bd show in %s: %w", store.Dir, err)
		}
		for bead, issue := range found {
			if issue == nil || issue.ID == "" {
				continue
			}
			notes[RejectionNoteKey{Rig: store.Rig, Bead: bead}] = issue.Notes
		}
		for _, bead := range store.Beads {
			if issue := found[bead]; issue == nil || issue.ID == "" {
				return nil, fmt.Errorf("bd show in %s: %s is not in the store the review log names", store.Dir, bead)
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
		"checklist rule its bead's MERGE REJECTION findings match. Rate is the pattern's rejections " +
		"over every recorded verdict that week, so a week's rows add up to its rejection rate. " +
		"A refusal whose findings matched no rule is " + RejectionPatternUnclassified + "; one whose " +
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
			rejectionCell(g.Week), rejectionCell(g.Name),
			strconv.Itoa(g.Rejections), strconv.Itoa(g.Verdicts), rejectionPercent(g.Rate),
		}, " | ")+" |")
	}
	return strings.Join(lines, "\n")
}

// rejectionCell is one table cell with the characters that would end it
// escaped: a pipe splits a cell and a line break ends the row. A seat name
// comes from the review log, so it can hold either; the counts and the rate
// are written here.
func rejectionCell(cell string) string {
	return strings.NewReplacer(`\`, `\\`, "|", `\|`, "\r", " ", "\n", " ").Replace(cell)
}

// rejectionPercent is a rate as the table writes it.
func rejectionPercent(rate float64) string {
	return strconv.FormatFloat(100*rate, 'f', 1, 64) + "%"
}

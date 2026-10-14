// Package cmd — "Memories for this bead", the per-bead memory section a
// polecat's prime can carry (gt-jhf24).
//
// A polecat starts every bead without the town's cross-issue context, and its
// prime payload has no room to carry a whole memory corpus: the crew memory
// index is the first section the hook budget evicts. This section buys the
// small end of that: the handful of rig memories the memrecall package scores
// against THIS bead, capped at memrecall's 2048-byte budget and rendered after
// the hooked work.
//
// It is off until a town sets prime.memory.polecat, and every failure — no
// hooked bead, an unreadable kv, no memory scoring at all — renders nothing,
// leaving prime's payload exactly as it was before this section existed.
// `gt prime --explain-memory <bead>` prints the same scoring read-only, so the
// selection can be judged before the flag is turned on.
//
// Every rendered section also appends one line to the town's recall log, the
// only record of which memories a polecat actually received (appendRecall).

package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/memrecall"
)

// beadMemoryHeading heads the section and is the marker tests and readers grep
// for. The crew index's "# Agent Memories" is a different section for a
// different reader; this one names the bead it was selected for.
const beadMemoryHeading = "## Memories for this bead"

// beadMemoryFooterLookups bounds the footer's named keys. See
// beadMemoryFooter.
const beadMemoryFooterLookups = 8

// polecatMemoriesEnabled reports whether the town turns on the polecat per-bead
// memory section (settings/config.json's prime.memory.polecat, default false).
// An unreadable settings file leaves it off: a memory section is never worth
// failing a prime over.
func polecatMemoriesEnabled(townRoot string) bool {
	settings, err := config.LoadOrCreateTownSettings(config.TownSettingsPath(townRoot))
	if err != nil {
		return false
	}
	return settings.PrimeMemoryPolecat()
}

// polecatBeadMemories renders the per-bead memory section for a polecat prime
// and reports whether this role and flag own the section at all.
//
// Ownership is the role, the hooked bead and the flag together: a false return
// leaves the caller's existing "skipped for role" explanation in place, so a
// polecat whose town has not turned the section on says exactly what it said
// before the flag existed.
func (p primeTools) polecatBeadMemories(ctx RoleContext, cwd string, hookedBead *beads.Issue) bool {
	if ctx.Role != RolePolecat || hookedBead == nil || hookedBead.ID == "" {
		return false
	}
	if !polecatMemoriesEnabled(ctx.TownRoot) {
		return false
	}

	kvs, err := p.kvList(cwd)
	if err != nil {
		// A failed read is a quiet prime, never a failed one.
		explainTo(p.w(), primeExplain, true, "memory injection: rig kv unreadable, no bead memories")
		return true
	}

	rows := beadMemoryRows(memoryCorpus(kvs), hookedBead, p.clk().Now())
	section := renderBeadMemorySection(rows, hookedBead.ID)
	if section == "" {
		return true
	}
	_, _ = fmt.Fprint(p.w(), section)
	p.appendRecall(ctx, hookedBead.ID, rows, len(section))
	return true
}

// recallRecord is one line of the recall log: which memories prime injected for
// which bead, and what the section cost.
//
// Keys is the memories the section carried, not the candidates it scored. The
// log's reader is the memory hygiene audit, which needs the recalled set to
// tell a memory polecats keep getting from one that has never been worth
// selecting (gt-k7t18).
type recallRecord struct {
	TS    string   `json:"ts"`
	Bead  string   `json:"bead"`
	Rig   string   `json:"rig"`
	Keys  []string `json:"keys"`
	Bytes int      `json:"bytes"`
}

// recallLogPath is <townRoot>/.runtime/memory-recall.jsonl, the town's
// append-only recall log.
func recallLogPath(townRoot string) string {
	return filepath.Join(constants.TownRuntimePath(townRoot), "memory-recall.jsonl")
}

// appendRecall records one injection. It is called only when the section
// rendered: a prime that injected no memories logs no line, so the file holds
// injections rather than primes.
//
// Best-effort by contract — the caller is prime, which must not fail, wait or
// grow a dependency on an audit log, so a write error is one stderr line and
// nothing else.
func (p primeTools) appendRecall(ctx RoleContext, beadID string, rows []beadMemory, sectionBytes int) {
	if ctx.TownRoot == "" {
		return
	}
	rec := recallRecord{
		TS:    p.clk().Now().UTC().Format(time.RFC3339),
		Bead:  beadID,
		Rig:   ctx.Rig,
		Keys:  recalledKeys(rows),
		Bytes: sectionBytes,
	}
	if err := appendRecallRecord(recallLogPath(ctx.TownRoot), rec); err != nil {
		fmt.Fprintf(p.errW(), "gt prime: memory recall log: %v\n", err)
	}
}

// recalledKeys returns the keys the section carries, in the order it carries
// them — the ranked order beadMemoryRows produced.
func recalledKeys(rows []beadMemory) []string {
	keys := make([]string, 0, len(rows))
	for _, row := range rows {
		if row.selected {
			keys = append(keys, row.mem.Key)
		}
	}
	return keys
}

// appendRecallRecord appends rec to path as one JSON line: a single O_APPEND
// write, mode 0600, the directory created when absent.
//
// One write and no fsync or read-back is the point (gt-k7t18): the line is an
// audit record, and prime pays for it one syscall at a time. A crash between
// the write and the next line leaves the record, which is all the log claims.
func appendRecallRecord(path string, rec recallRecord) error {
	line, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	line = append(line, '\n')

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	fh, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = fh.Close() }()

	_, err = fh.Write(line)
	return err
}

// memoryCorpus returns the rig kv's memories in key order, so the same corpus
// always selects the same section. It reads both memory namespaces
// (isMemoryKey), the same corpus the crew index renders, and skips a bare
// namespace key, which names no memory.
//
// The rig kv is the polecat's whole readable corpus: a town-scope memory is
// stored in the town database, which this command cannot read, so no scope
// filter is needed here.
func memoryCorpus(kvs map[string]string) []memrecall.Memory {
	keys := make([]string, 0, len(kvs))
	for k := range kvs {
		if _, short := parseMemoryKey(k); isMemoryKey(k) && short != "" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)

	mems := make([]memrecall.Memory, 0, len(keys))
	for _, k := range keys {
		mems = append(mems, memrecall.Parse(k, kvs[k]))
	}
	return mems
}

// beadMemoryRows scores every candidate memory in corpus against bead's query
// and returns them ranked by final score, then key.
//
// The section itself renders memrecall.Select's order; this ranking is the
// diagnostic one, shared by the footer and --explain-memory.
func beadMemoryRows(corpus []memrecall.Memory, bead *beads.Issue, now time.Time) []beadMemory {
	q, paths := beadMemoryQuery(bead)
	selected := memrecall.Select(q, paths, corpus, now, memrecall.Budget)
	texts := make(map[string]string, len(selected))
	for _, s := range selected {
		texts[s.Key] = s.Text
	}

	rows := make([]beadMemory, 0, len(corpus))
	for _, m := range corpus {
		raw, score := memrecall.Score(q, paths, m, now)
		if !beadMemoryCandidate(m, raw) {
			continue
		}
		text, kept := texts[m.Key]
		rows = append(rows, beadMemory{mem: m, raw: raw, score: score, text: text, selected: kept})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].score != rows[j].score {
			return rows[i].score > rows[j].score
		}
		return rows[i].mem.Key < rows[j].mem.Key
	})
	return rows
}

// beadMemory is one candidate memory with the scores both readers report.
type beadMemory struct {
	mem   memrecall.Memory
	raw   float64
	score float64
	// text is the memory's rendered contribution, empty when Select had no
	// room for it.
	text string
	// selected reports whether the section carries the memory.
	selected bool
}

// beadMemoryCandidate reports whether memrecall counts a memory as a candidate
// at all: an always-and-durable memory is pinned whatever it scores, and any
// other memory has to clear CandidateFloor on its raw score. It restates
// Select's own switch (memrecall.go), which the footer and --explain-memory
// need but the package does not export.
func beadMemoryCandidate(m memrecall.Memory, raw float64) bool {
	return m.Always && m.Durable || raw >= memrecall.CandidateFloor
}

// beadMemoryQuery builds the bead side of memrecall's comparison: its title,
// the Goal and Constraints text of its description, its labels, and the paths
// it names.
func beadMemoryQuery(bead *beads.Issue) (map[string]struct{}, []string) {
	paths := beadMemoryPaths(bead)
	return memrecall.Query(bead.Title, beadGoalAndConstraints(bead.Description), bead.Labels, paths), paths
}

// beadGoalAndConstraints returns the body of the description's Goal and
// Constraints sections — the two a shaped bead states its work in.
//
// The rest of the description is dispatch metadata (the attached-molecule
// header, Out of scope, Gate, Size): its words describe the machinery around
// the work, and they would score memories about the dispatch pipeline against
// every bead.
func beadGoalAndConstraints(description string) string {
	var kept []string
	want := false
	for _, line := range strings.Split(description, "\n") {
		if name, ok := markdownHeading(line); ok {
			want = strings.EqualFold(name, "Goal") || strings.EqualFold(name, "Constraints")
			continue
		}
		if want {
			kept = append(kept, line)
		}
	}
	return strings.Join(kept, "\n")
}

// markdownHeading returns a heading line's first word, so a section named by
// title ("## Goal", "# Constraints", "## Goal:") is recognized however it is
// spelled.
func markdownHeading(line string) (string, bool) {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "#") {
		return "", false
	}
	text := strings.TrimSpace(strings.TrimLeft(trimmed, "#"))
	if text == "" {
		return "", false
	}
	name, _, _ := strings.Cut(text, " ")
	return strings.TrimSuffix(name, ":"), true
}

// beadMemoryPaths returns the file paths a bead names: the file references
// extractContentRefs finds (the same extraction the polecat duplicate check
// uses), plus the source-root tokens it drops because they name a directory
// ("internal/cmd/") rather than a file — still an area worth matching a memory
// on.
func beadMemoryPaths(bead *beads.Issue) []string {
	paths := extractContentRefs(bead.Title, bead.Description).Files

	seen := make(map[string]bool, len(paths))
	for _, p := range paths {
		seen[p] = true
	}
	for _, part := range []string{bead.Title, bead.Description} {
		for _, tok := range pathTokenRe.FindAllString(part, -1) {
			// The prefix is tested before the trailing "./-" a sentence adds
			// is trimmed: it is the slash that makes "cmd/" a source root.
			if !hasSourceRootPrefix(tok) {
				continue
			}
			tok = strings.TrimRight(tok, "./-")
			if tok == "" || seen[tok] {
				continue
			}
			seen[tok] = true
			paths = append(paths, tok)
		}
	}
	return paths
}

// hasSourceRootPrefix reports whether a token names a path under one of the
// repository's source roots.
func hasSourceRootPrefix(tok string) bool {
	for _, root := range []string{"internal/", "cmd/", "scripts/"} {
		if strings.HasPrefix(tok, root) {
			return true
		}
	}
	return false
}

// renderBeadMemorySection renders the section: memrecall's selected texts in
// Select's order, then a line naming the candidates that scored but did not
// fit, so the agent knows what matched its bead beyond what it carries.
// beadID is the bead the section was built for; the footer points at it.
//
// It returns "" when nothing is a candidate, which is what omits the section,
// and a header over the footer alone when candidates exist but none fit.
func renderBeadMemorySection(rows []beadMemory, beadID string) string {
	if len(rows) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\n%s\n\n", beadMemoryHeading)

	var dropped []beadMemory
	selectedBytes := 0
	for _, row := range rows {
		if !row.selected {
			dropped = append(dropped, row)
			continue
		}
		b.WriteString(row.text)
		b.WriteString("\n")
		selectedBytes += len(row.text)
	}
	if footer := beadMemoryFooter(dropped, beadID, memrecall.Budget-selectedBytes); footer != "" {
		b.WriteString("\n")
		b.WriteString(footer)
	}
	return b.String()
}

// beadMemoryFooter counts the candidates that scored but did not fit and points
// at the command that prints them: `gt prime --explain-memory <beadID>`.
//
// The key list is a courtesy after that command. It names the
// highest-scoring beadMemoryFooterLookups of the dropped candidates, and only
// when the line fits in room, the bytes memrecall's budget has left after the
// selected texts. Select fills the budget with text first, so on a crowded bead
// the keys are what gives way and the count and the command stay.
//
// Prime's output is agent prose, which may name only the bd commands in
// cmdtree.AgentBdAllowed (TestAgentProseBdAllowlist, gt-7iwy0.6), and the bd
// key-value reads are not among them; the lookup is a gt command so the footer
// can name it.
// --explain-memory prints each candidate's key and scores, not its text.
//
// The count is the whole truth. The rig corpus is 241 memories and the query is
// a bead's whole Goal and Constraints text, so most of them score above
// memrecall's floor: naming them all would cost ~15 KB of the 9000-byte hook
// budget that this section is meant to leave to the hooked work (measured with
// `gt prime --explain-memory gt-jhf24`, 2026-10-14).
func beadMemoryFooter(dropped []beadMemory, beadID string, room int) string {
	if len(dropped) == 0 {
		return ""
	}
	line := fmt.Sprintf("%d more matched: gt prime --explain-memory %s", len(dropped), beadID)

	named := dropped
	if len(named) > beadMemoryFooterLookups {
		named = named[:beadMemoryFooterLookups]
	}
	keys := make([]string, 0, len(named))
	for _, d := range named {
		keys = append(keys, d.mem.Key)
	}
	list := strings.Join(keys, ", ")
	if rest := len(dropped) - len(named); rest > 0 {
		list += fmt.Sprintf(", and %d more", rest)
	}
	withKeys := line + " (" + list + ")\n"
	if len(withKeys) <= room {
		return withKeys
	}
	return line + "\n"
}

// runPrimeExplainMemory prints memrecall's view of one bead's selection: the
// rig memories that are candidates, each with its raw score, its final score
// and whether the section would carry it. It is the read-only half of the
// feature — no section output, no session side effects — so an operator can
// judge what prime.memory.polecat would inject before turning it on.
//
// Unlike the section it explains, a failure here is the operator's to see: a
// bead or a kv that cannot be read is reported, not swallowed.
func runPrimeExplainMemory(out io.Writer, ctx RoleContext, beadID string) error {
	bead, err := beads.New(rigBeadsRoot(ctx)).Show(beadID)
	if err != nil {
		return fmt.Errorf("--explain-memory: reading bead %s: %w", beadID, err)
	}

	kvs, err := primeTools{}.kvList(ctx.WorkDir)
	if err != nil {
		return fmt.Errorf("--explain-memory: reading the rig kv: %w", err)
	}

	_, err = io.WriteString(out, explainMemoryReport(bead, memoryCorpus(kvs), time.Now()))
	return err
}

// explainMemoryReport is the text --explain-memory prints.
func explainMemoryReport(bead *beads.Issue, corpus []memrecall.Memory, now time.Time) string {
	rows := beadMemoryRows(corpus, bead, now)

	kept := 0
	for _, row := range rows {
		if row.selected {
			kept++
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "explain-memory %s: %d rig memories, %d candidates, %d selected (budget %d bytes, floor %.1f)\n",
		bead.ID, len(corpus), len(rows), kept, memrecall.Budget, memrecall.CandidateFloor)
	for _, row := range rows {
		state := "not-selected"
		if row.selected {
			state = "selected    "
		}
		fmt.Fprintf(&b, "  %s  raw %.1f  final %.1f  %s\n", state, row.raw, row.score, row.mem.Key)
	}
	if len(rows) == 0 {
		fmt.Fprintf(&b, "  no candidate: no rig memory is always-and-durable, and none scored raw %.1f or more\n",
			memrecall.CandidateFloor)
	}
	return b.String()
}

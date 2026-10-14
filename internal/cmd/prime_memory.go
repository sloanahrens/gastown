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
func (p primeTools) appendRecall(ctx RoleContext, beadID string, rows []memrecall.Ranked, sectionBytes int) {
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
// them.
func recalledKeys(rows []memrecall.Ranked) []string {
	keys := make([]string, 0, len(rows))
	for _, row := range selectedRows(rows) {
		keys = append(keys, row.Memory.Key)
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
// always selects the same section, and always the same statistics memrecall's
// keyword weights are measured against.
//
// The rig kv is the polecat's whole readable corpus: a town-scope memory is
// stored in the town database, which this command cannot read, so no scope
// filter is needed here.
func memoryCorpus(kvs map[string]string) []memrecall.Memory {
	keys := make([]string, 0, len(kvs))
	for k := range kvs {
		if liveMemoryKey(k) {
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

// liveMemoryKey reports whether a kv key names a memory the section may select.
// It reads the key alone, through the parsing the crew memory index uses
// (isMemoryKey, parseMemoryKey):
//
//   - a key under the operator's archive namespace (archive.…) is a retired
//     memory, and the rig kv holds hundreds of them: counted as candidates
//     they would be most of the pool, and every weight would be measured
//     against memories the operator has already taken out of service;
//   - a key in neither memory namespace is not a memory at all, and the rig kv
//     also holds schema and dispatch keys;
//   - a key that names no slug (gt., memory., gt.feedback.) names no memory.
func liveMemoryKey(key string) bool {
	if strings.HasPrefix(key, archiveMemoryPrefix) {
		return false
	}
	if !isMemoryKey(key) {
		return false
	}
	_, short := parseMemoryKey(key)
	return short != ""
}

// archiveMemoryPrefix is the namespace the operator archives a retired memory
// into.
const archiveMemoryPrefix = "archive."

// beadMemoryRows scores every memory in corpus against bead's query and returns
// the pool ranked by score, then newer verification, then key.
//
// Every pool memory comes back, not only the candidates: --explain-memory
// reports each one with the reason it was dropped. The section's own readers
// are the two filters below, and a row carries the section's order as Order.
func beadMemoryRows(corpus []memrecall.Memory, bead *beads.Issue, now time.Time) []memrecall.Ranked {
	q, paths := beadMemoryQuery(bead)
	return memrecall.Rank(q, paths, corpus, now, memrecall.Budget)
}

// selectedRows returns the rows the section carries, in the order it renders
// them: memrecall's, pinned memories first.
func selectedRows(rows []memrecall.Ranked) []memrecall.Ranked {
	var out []memrecall.Ranked
	for _, row := range rows {
		if row.Carried {
			out = append(out, row)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Order < out[j].Order })
	return out
}

// droppedRows returns the rows that matched the bead and that the section still
// does not carry: the score cut, the cap of 3 and the byte budget are the only
// reasons left, so these are the memories the footer counts.
func droppedRows(rows []memrecall.Ranked) []memrecall.Ranked {
	var out []memrecall.Ranked
	for _, row := range rows {
		if row.Matched() && !row.Carried {
			out = append(out, row)
		}
	}
	return out
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
	var fence codeFence
	for _, line := range strings.Split(description, "\n") {
		// A line inside a fenced code block is text, whatever it starts with:
		// a shell comment or a shebang is not a section heading.
		if fence.inside(line) {
			if want {
				kept = append(kept, line)
			}
			continue
		}
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
//
// It follows markdown's ATX rule: one to six #s, then a space, and an indent of
// at most three spaces. "#4821", "#!/bin/sh" and "#######" are text.
func markdownHeading(line string) (string, bool) {
	if indent := line[:len(line)-len(strings.TrimLeft(line, " \t"))]; len(indent) > 3 || strings.Contains(indent, "\t") {
		return "", false
	}
	trimmed := strings.TrimSpace(line)
	hashes := len(trimmed) - len(strings.TrimLeft(trimmed, "#"))
	if hashes == 0 || hashes > 6 || len(trimmed) == hashes || trimmed[hashes] != ' ' {
		return "", false
	}
	text := strings.TrimSpace(trimmed[hashes:])
	if text == "" {
		return "", false
	}
	name, _, _ := strings.Cut(text, " ")
	return strings.TrimSuffix(name, ":"), true
}

// codeFence tracks whether a line sits inside a fenced code block (``` or ~~~,
// three or more, closed by the same character at least as long).
type codeFence struct {
	char byte
	size int
}

// inside reports whether line belongs to the block, fence lines included, and
// advances the state past it.
func (f *codeFence) inside(line string) bool {
	trimmed := strings.TrimSpace(line)
	if f.size > 0 {
		if trimmed != "" && strings.Trim(trimmed, string(f.char)) == "" && len(trimmed) >= f.size {
			f.size = 0
		}
		return true
	}
	for _, c := range []byte{'`', '~'} {
		n := len(trimmed) - len(strings.TrimLeft(trimmed, string(c)))
		if n >= 3 && (c != '`' || !strings.Contains(trimmed[n:], "`")) {
			f.char, f.size = c, n
			return true
		}
	}
	return false
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

// renderBeadMemorySection renders the section: memrecall's carried texts in its
// order, then a line naming the memories that matched but did not fit, so the
// agent knows what matched its bead beyond what it carries. beadID is the bead
// the section was built for; the footer points at it.
//
// It returns "" when no memory matched, which is what omits the section, and a
// header over the footer alone when memories matched but none fit.
func renderBeadMemorySection(rows []memrecall.Ranked, beadID string) string {
	if len(droppedRows(rows)) == 0 && len(selectedRows(rows)) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\n%s\n\n", beadMemoryHeading)

	selectedBytes := 0
	for _, row := range selectedRows(rows) {
		b.WriteString(row.Text)
		b.WriteString("\n")
		selectedBytes += len(row.Text)
	}
	if footer := beadMemoryFooter(droppedRows(rows), beadID, memrecall.Budget-selectedBytes); footer != "" {
		b.WriteString("\n")
		b.WriteString(footer)
	}
	return b.String()
}

// beadMemoryFooter counts the memories that matched but did not fit and points
// at the command that prints them: `gt prime --explain-memory <beadID>`.
//
// The key list is a courtesy after that command. It names the highest-scoring
// beadMemoryFooterLookups of the dropped memories, and only when the line fits
// in room, the bytes memrecall's budget has left after the carried texts. Rank
// fills the budget with text first, so on a crowded bead the keys are what
// gives way and the count and the command stay: a bead can match more memories
// than this section, which shares the hook budget with the hooked work, has
// room to name.
//
// Prime's output is agent prose, which may name only the bd commands in
// cmdtree.AgentBdAllowed (TestAgentProseBdAllowlist, gt-7iwy0.6), and the bd
// key-value reads are not among them; the lookup is a gt command so the footer
// can name it. --explain-memory prints each memory's key and scores, not its
// text.
func beadMemoryFooter(dropped []memrecall.Ranked, beadID string, room int) string {
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
		keys = append(keys, d.Memory.Key)
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

// explainMemoryReport is the text --explain-memory prints: one line per rig
// memory with the idf-weighted raw score, the score the section ranks by, the
// distinctive hits that qualified it and, when the section does not carry it,
// why.
func explainMemoryReport(bead *beads.Issue, corpus []memrecall.Memory, now time.Time) string {
	rows := beadMemoryRows(corpus, bead, now)
	matched, kept := 0, 0
	for _, row := range rows {
		if row.Matched() {
			matched++
		}
		if row.Carried {
			kept++
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "explain-memory %s: %d rig memories, %d matched, %d selected (budget %d bytes, floor %.1f, distinctive df/N <= %.1f or a key or anchor hit in a pool under %d, keep >= %.0f%% of the top score, cap %d)\n",
		bead.ID, len(corpus), matched, kept, memrecall.Budget, memrecall.CandidateFloor,
		memrecall.DistinctiveShare, memrecall.SmallPool, memrecall.KeepShare*100, memrecall.MaxScored)
	for _, row := range rows {
		state := "selected"
		if !row.Carried {
			state = "dropped: " + string(row.Dropped)
		}
		fmt.Fprintf(&b, "  %-30s  raw %5.1f  final %5.1f  %s", state, row.Raw, row.Score, row.Memory.Key)
		if len(row.Distinctive) > 0 {
			fmt.Fprintf(&b, "  [%s]", strings.Join(row.Distinctive, ", "))
		}
		b.WriteString("\n")
	}
	if matched == 0 {
		fmt.Fprintf(&b, "  no match: no rig memory matched a distinctive keyword and scored raw %.1f or more\n",
			memrecall.CandidateFloor)
	}
	return b.String()
}

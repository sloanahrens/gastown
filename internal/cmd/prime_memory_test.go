package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/memrecall"
)

// The tests here run the section through primeTools with the fake runner the
// other prime tests use, so a corpus is a JSON string, not a beads database.

// primeMemoryBead is a shaped bead as a polecat receives one: an attached-
// molecule header, then the five sections the spec dispatcher writes.
func primeMemoryBead() *beads.Issue {
	return &beads.Issue{
		ID:    "gt-test1",
		Title: "gt prime: memories for this bead",
		Description: strings.Join([]string{
			"attached_molecule: gt-wisp-123",
			"attached_formula: mol-polecat-work",
			"",
			"## Goal",
			"Polecats receive a memories section in gt prime.",
			"",
			"## Constraints",
			"Edit internal/cmd/prime.go and add internal/cmd/prime_memory.go.",
			"",
			"## Out of scope",
			"The scoring rules of memrecall, and crew prime.",
			"",
			"## Gate",
			"make gate.",
		}, "\n"),
		Labels: []string{"prime", "hooks-budget"},
	}
}

// primeMemoryKVEntry builds one kv entry in the format memrecall parses.
func primeMemoryKVEntry(memType, slug, verified, body string) (key, value string) {
	key = "gt." + memType + "." + slug
	value = fmt.Sprintf("type: %s\nverified: %s\n\n%s\n", memType, verified, body)
	return key, value
}

// primeMemoryMatch builds the memory the section selects for primeMemoryBead:
// its key holds three of the bead's words, which three key hits reach the floor
// with at any pool size.
func primeMemoryMatch(verified, body string) (key, value string) {
	return primeMemoryKVEntry("feedback", "prime-memories-budget", verified, body)
}

// primeMemoryFiller returns n rig memories that share no term with
// primeMemoryBead, so a test's own memory is scored as one of a pool: a keyword
// is worth its rarity, and a pool of one weighs every word as common.
func primeMemoryFiller(n int) map[string]string {
	kvs := make(map[string]string, n)
	for i := range n {
		key, value := primeMemoryKVEntry("project", fmt.Sprintf("garden-beds-%02d", i), "2026-08-20",
			"Spread mulch on the beds in spring.")
		kvs[key] = value
	}
	return kvs
}

// primeMemoryCorpus builds the rig kv of a test: work's memories behind n
// unrelated ones.
func primeMemoryCorpus(n int, work map[string]string) map[string]string {
	kvs := primeMemoryFiller(n)
	for k, v := range work {
		kvs[k] = v
	}
	return kvs
}

// primeMemoryKV marshals a corpus into the shape of `bd kv list --json`.
func primeMemoryKV(t *testing.T, kvs map[string]string) string {
	t.Helper()
	data, err := json.Marshal(kvs)
	if err != nil {
		t.Fatalf("marshal kv: %v", err)
	}
	return string(data)
}

// writePrimeMemorySettings writes the town's settings file with the polecat
// memory flag at the given value.
func writePrimeMemorySettings(t *testing.T, townRoot string, polecat bool) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(townRoot, "settings"), 0o755); err != nil {
		t.Fatalf("mkdir settings: %v", err)
	}
	body := fmt.Sprintf(`{"type":"town-settings","version":1,"prime":{"memory":{"polecat":%t}}}`, polecat)
	if err := os.WriteFile(config.TownSettingsPath(townRoot), []byte(body), 0o644); err != nil {
		t.Fatalf("write settings: %v", err)
	}
}

// primeMemorySection renders the section for a polecat prime through the same
// call runPrime's memories part makes.
func primeMemorySection(t *testing.T, runner *fakePrimeRunner, ctx RoleContext, workDir string, bead *beads.Issue) (string, *fakePrimeRunner) {
	t.Helper()
	p, _, out := newFakePrimeTools(runner)
	p.memoryInject(ctx, workDir, bead)
	return out.String(), runner
}

// TestPrimeMemory_FlagOffRendersNothing is the byte-identity half of the
// acceptance: with the flag off — absent, omitted or explicitly false — a
// polecat prime renders no section and never reads the rig kv. The empty
// section is what makes the payload byte-identical: primePayload.add drops
// empty text, so a prime that renders this section renders exactly what it
// rendered before the section existed.
func TestPrimeMemory_FlagOffRendersNothing(t *testing.T) {
	t.Parallel()
	matchKey, matchValue := primeMemoryKVEntry("feedback", "prime-budget", "2026-08-20",
		"A polecat's gt prime spends its hook budget on the hooked work first.")
	corpus := primeMemoryKV(t, map[string]string{matchKey: matchValue})

	for _, tc := range []struct {
		name     string
		settings string
	}{
		{"no settings file", ""},
		{"settings without the flag", `{"type":"town-settings","version":1}`},
		{"flag explicitly off", `{"type":"town-settings","version":1,"prime":{"memory":{"polecat":false}}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			townRoot := t.TempDir()
			if tc.settings != "" {
				if err := os.MkdirAll(filepath.Join(townRoot, "settings"), 0o755); err != nil {
					t.Fatalf("mkdir settings: %v", err)
				}
				if err := os.WriteFile(config.TownSettingsPath(townRoot), []byte(tc.settings), 0o644); err != nil {
					t.Fatalf("write settings: %v", err)
				}
			}
			f := &fakePrimeRunner{answers: map[string]string{primeKVListCall: corpus}}

			out, f := primeMemorySection(t, f,
				RoleContext{Role: RolePolecat, TownRoot: townRoot}, t.TempDir(), primeMemoryBead())

			if out != "" {
				t.Errorf("flag off rendered %q, want no section", out)
			}
			if f.called(primeKVListCall) {
				t.Errorf("flag off read the rig kv: calls %q", f.callLines())
			}
		})
	}
}

// TestPrimeMemory_FlagOffPayloadIsByteIdentical pins the acceptance item at
// the payload seam: the assembled payload is the same text with the section's
// part wired in as it is with the part omitted.
func TestPrimeMemory_FlagOffPayloadIsByteIdentical(t *testing.T) {
	t.Parallel()
	ctx := RoleContext{Role: RolePolecat, TownRoot: t.TempDir()}
	workDir := t.TempDir()
	bead := primeMemoryBead()
	p, _, _ := newFakePrimeTools(&fakePrimeRunner{})

	payload := func(withMemories bool) string {
		parts := primeParts{hookedWork: func() string { return "HOOKED WORK\n" }}
		if withMemories {
			parts.memories = func() string {
				var b strings.Builder
				p.out = &b
				p.memoryInject(ctx, workDir, bead)
				return b.String()
			}
		}
		return assemblePrimePayload(parts, "", false, true).render(primeHookBudget)
	}

	if got, want := payload(true), payload(false); got != want {
		t.Errorf("flagged-off payload differs from the payload without the section:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// TestPrimeMemory_FlagOnRendersSectionAfterHookedWork is the section half of
// the acceptance: with the flag on, a matching memory renders under the
// heading, and the section sits after the hooked work so the hook budget
// evicts it before the assignment.
func TestPrimeMemory_FlagOnRendersSectionAfterHookedWork(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	writePrimeMemorySettings(t, townRoot, true)

	key, value := primeMemoryMatch("2026-08-20",
		"A polecat's gt prime spends its hook budget on the hooked work first; the memory section is what gets evicted.")
	f := &fakePrimeRunner{answers: map[string]string{primeKVListCall: primeMemoryKV(t, primeMemoryCorpus(2,
		map[string]string{key: value}))}}
	ctx := RoleContext{Role: RolePolecat, TownRoot: townRoot}

	section, _ := primeMemorySection(t, f, ctx, t.TempDir(), primeMemoryBead())

	for _, want := range []string{
		beadMemoryHeading,
		"### " + key + " (verified 2026-08-20)",
		"the memory section is what gets evicted.",
	} {
		if !strings.Contains(section, want) {
			t.Errorf("section missing %q:\n%s", want, section)
		}
	}

	parts := primeParts{
		hookedWork: func() string { return "HOOKED WORK MARKER\n" },
		memories:   func() string { return section },
	}
	payload := assemblePrimePayload(parts, "", false, true).render(primeHookBudget)
	hook, memories := strings.Index(payload, "HOOKED WORK MARKER"), strings.Index(payload, beadMemoryHeading)
	if hook < 0 || memories < 0 {
		t.Fatalf("payload missing a section:\n%s", payload)
	}
	if memories < hook {
		t.Errorf("memory section at %d renders before the hooked work at %d:\n%s", memories, hook, payload)
	}
}

// TestPrimeMemory_CapsTheSection pins the section's two bounds: at most
// memrecall.MaxScored memories, and no more of them than the 2048-byte budget
// holds. What neither bound fits is named in the footer.
func TestPrimeMemory_CapsTheSection(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	writePrimeMemorySettings(t, townRoot, true)

	// Four memories that all match, each too long for two of them to share the
	// budget: three clear the cap and two fit.
	corpus := map[string]string{}
	for i := range 4 {
		key, value := primeMemoryKVEntry("feedback", fmt.Sprintf("prime-memories-budget-%02d", i), "2026-08-20",
			strings.Repeat("A polecat's gt prime spends its hook budget on the hooked work first. ", 12))
		corpus[key] = value
	}
	corpus = primeMemoryCorpus(2, corpus)
	f := &fakePrimeRunner{answers: map[string]string{primeKVListCall: primeMemoryKV(t, corpus)}}
	ctx := RoleContext{Role: RolePolecat, TownRoot: townRoot}

	section, _ := primeMemorySection(t, f, ctx, t.TempDir(), primeMemoryBead())

	rows := beadMemoryRows(memoryCorpus(corpus), primeMemoryBead(), primeTestEpoch)
	selectedBytes := 0
	for _, row := range selectedRows(rows) {
		selectedBytes += len(row.Text)
	}
	carried, matched := len(selectedRows(rows)), 0
	for _, row := range rows {
		if row.Matched() {
			matched++
		}
	}
	if carried == 0 || matched <= carried {
		t.Fatalf("four matching memories carried %d and matched %d, want the bounds to leave some out", carried, matched)
	}
	if carried > memrecall.MaxScored {
		t.Errorf("the section carries %d memories, over the cap of %d", carried, memrecall.MaxScored)
	}
	if selectedBytes > memrecall.Budget {
		t.Errorf("selected text = %d bytes, over memrecall's %d-byte budget", selectedBytes, memrecall.Budget)
	}
	dropped := len(droppedRows(rows))
	if dropped == 0 {
		t.Fatalf("the bounds left nothing out of %d matches", matched)
	}
	want := fmt.Sprintf("%d more matched: gt prime --explain-memory %s", dropped, primeMemoryBead().ID)
	if !strings.Contains(section, want) {
		t.Errorf("footer missing %q:\n%s", want, section)
	}
	if strings.Contains(section, "bd kv") {
		t.Errorf("section names a bd kv command, which agent prose may not:\n%s", section)
	}
}

// TestPrimeMemory_RendersFooterWhenNoCandidateFits covers the corpus where a
// candidate is too big for the budget: the section keeps the footer, which
// still tells the agent the memory is there, and renders no text.
func TestPrimeMemory_RendersFooterWhenNoCandidateFits(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	writePrimeMemorySettings(t, townRoot, true)

	key, value := primeMemoryMatch("2026-08-20",
		strings.Repeat("The polecat's gt prime hook budget is what this memory is about. ", 100))
	f := &fakePrimeRunner{answers: map[string]string{primeKVListCall: primeMemoryKV(t, primeMemoryCorpus(2,
		map[string]string{key: value}))}}
	ctx := RoleContext{Role: RolePolecat, TownRoot: townRoot}

	section, _ := primeMemorySection(t, f, ctx, t.TempDir(), primeMemoryBead())

	if !strings.Contains(section, beadMemoryHeading) {
		t.Fatalf("section missing its heading:\n%s", section)
	}
	if want := "1 more matched: gt prime --explain-memory gt-test1 (" + key + ")\n"; !strings.Contains(section, want) {
		t.Errorf("section missing %q:\n%s", want, section)
	}
	if strings.Contains(section, "### ") {
		t.Errorf("section rendered a memory that does not fit:\n%s", section)
	}
}

// droppedMemories returns n rows the section matched and did not carry.
func droppedMemories(n int) []memrecall.Ranked {
	rows := make([]memrecall.Ranked, 0, n)
	for i := 0; i < n; i++ {
		rows = append(rows, memrecall.Ranked{Memory: memrecall.Memory{Key: fmt.Sprintf("gt.feedback.dropped-%02d", i)}})
	}
	return rows
}

// TestPrimeMemory_FooterPointsAtExplainMemory pins the footer's shape: the
// count of candidates that did not fit and the gt command that prints them for
// the bead the section was built for. With room to spare it also names the
// highest-scoring keys, bounded by beadMemoryFooterLookups, and counts the rest.
func TestPrimeMemory_FooterPointsAtExplainMemory(t *testing.T) {
	t.Parallel()
	const room = memrecall.Budget

	got := beadMemoryFooter(droppedMemories(1), "gt-abc", room)
	if want := "1 more matched: gt prime --explain-memory gt-abc (gt.feedback.dropped-00)\n"; got != want {
		t.Errorf("one dropped: footer = %q, want %q", got, want)
	}

	n := beadMemoryFooterLookups + 4
	got = beadMemoryFooter(droppedMemories(n), "gt-abc", room)
	if want := fmt.Sprintf("%d more matched: gt prime --explain-memory gt-abc (", n); !strings.HasPrefix(got, want) {
		t.Errorf("footer = %q, want prefix %q", got, want)
	}
	if want := ", and 4 more)\n"; !strings.HasSuffix(got, want) {
		t.Errorf("footer = %q, want suffix %q", got, want)
	}
	if c := strings.Count(got, "gt.feedback.dropped-"); c != beadMemoryFooterLookups {
		t.Errorf("footer names %d keys, want %d: %q", c, beadMemoryFooterLookups, got)
	}
	if strings.Contains(got, "bd kv") {
		t.Errorf("footer names a bd kv command, which agent prose may not: %q", got)
	}
}

// TestPrimeMemory_FooterDropsKeysThatDoNotFit pins the other half: when the
// key list would push the section past memrecall's budget, the footer keeps the
// count and the command and drops the keys.
func TestPrimeMemory_FooterDropsKeysThatDoNotFit(t *testing.T) {
	t.Parallel()
	bare := "3 more matched: gt prime --explain-memory gt-abc\n"
	for _, room := range []int{0, len(bare) - 1, len(bare) + 10} {
		if got := beadMemoryFooter(droppedMemories(3), "gt-abc", room); got != bare {
			t.Errorf("room %d: footer = %q, want %q", room, got, bare)
		}
	}
}

// TestPrimeMemory_FooterOmittedWhenNothingLeftOut covers the corpus that fits
// whole: every candidate is selected, so the section carries no footer and
// points at no command.
func TestPrimeMemory_FooterOmittedWhenNothingLeftOut(t *testing.T) {
	t.Parallel()
	if got := beadMemoryFooter(nil, "gt-abc", memrecall.Budget); got != "" {
		t.Errorf("footer for no dropped candidates = %q, want empty", got)
	}

	rows := []memrecall.Ranked{{
		Memory:  memrecall.Memory{Key: "gt.feedback.kept"},
		Text:    "### gt.feedback.kept (verified 2026-08-20)\nkept\n",
		Carried: true,
	}}
	section := renderBeadMemorySection(rows, "gt-abc")
	if !strings.Contains(section, "### gt.feedback.kept") {
		t.Fatalf("section missing the selected memory:\n%s", section)
	}
	if strings.Contains(section, "more matched") || strings.Contains(section, "--explain-memory") {
		t.Errorf("section carries a footer though nothing was left out:\n%s", section)
	}
}

// TestPrimeMemory_OmitsSectionWithoutCandidates pins the omission rule: a rig
// whose memories share nothing with the bead renders no section, and neither
// does a rig whose kv cannot be read — a failed read never fails prime.
func TestPrimeMemory_OmitsSectionWithoutCandidates(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	writePrimeMemorySettings(t, townRoot, true)
	unrelated := primeMemoryKV(t, map[string]string{
		"gt.project.garden-mulch":    "type: project\nverified: 2026-08-20\n\nSpread mulch on the beds in spring.\n",
		"gt.reference.bread-recipes": "type: reference\nverified: 2026-08-20\n\nA sourdough starter keeps for weeks.\n",
	})

	for _, tc := range []struct {
		name   string
		answer string
	}{
		{"no matching candidate", unrelated},
		{"empty corpus", "{}"},
		{"unreadable kv", ""}, // no answer: the fake runner fails the call
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := &fakePrimeRunner{answers: map[string]string{}}
			if tc.answer != "" {
				f.answers[primeKVListCall] = tc.answer
			}

			out, _ := primeMemorySection(t, f,
				RoleContext{Role: RolePolecat, TownRoot: townRoot}, t.TempDir(), primeMemoryBead())

			if out != "" {
				t.Errorf("rendered %q, want no section", out)
			}
		})
	}
}

// TestPrimeMemoryQuery_ScoresGoalConstraintsLabelsAndPaths pins what the query
// is built from: the bead's title, its Goal and Constraints text, its labels
// and the paths it names — and not the dispatch metadata, whose words would
// match memories about the pipeline on every bead.
func TestPrimeMemoryQuery_ScoresGoalConstraintsLabelsAndPaths(t *testing.T) {
	t.Parallel()
	q, paths := beadMemoryQuery(primeMemoryBead())

	for _, want := range []string{"memories", "receive", "edit", "budget"} {
		if _, ok := q[want]; !ok {
			t.Errorf("query missing %q", want)
		}
	}
	for _, unwanted := range []string{"attached", "wisp", "formula", "gate", "scoring", "crew"} {
		if _, ok := q[unwanted]; ok {
			t.Errorf("query carries %q, which the Goal and Constraints sections do not define", unwanted)
		}
	}

	for _, want := range []string{"internal/cmd/prime.go", "internal/cmd/prime_memory.go"} {
		if !containsString(paths, want) {
			t.Errorf("paths %v missing %q", paths, want)
		}
	}
}

// TestPrimeMemoryPaths_KeepsSourceRootDirectories covers the half of the path
// rule the shared file extractor drops: a token naming a source directory is
// still an area a memory can be about.
func TestPrimeMemoryPaths_KeepsSourceRootDirectories(t *testing.T) {
	t.Parallel()
	bead := &beads.Issue{
		ID:          "gt-test2",
		Title:       "search scripts/ for the installer",
		Description: "The fix lands under cmd/ and internal/.",
	}

	paths := beadMemoryPaths(bead)
	for _, want := range []string{"scripts", "cmd", "internal"} {
		if !containsString(paths, want) {
			t.Errorf("paths %v missing the source-root token %q", paths, want)
		}
	}
}

// TestPrimeMemoryPool_SkipsArchivedAndNonMemoryKeys covers the pool's filter:
// the rig kv also holds the memories the operator archived and keys that are no
// memory at all, and neither is a candidate for a bead.
func TestPrimeMemoryPool_SkipsArchivedAndNonMemoryKeys(t *testing.T) {
	t.Parallel()
	corpus := memoryCorpus(map[string]string{
		"gt.feedback.prime-memories-budget": "type: feedback\n\nBody.",
		"memory.checkpoint-note":            "Points at the checkpoint.",
		// The operator's archive: retired memories, still in the kv.
		"archive.gt.feedback.retired-note": "type: feedback\n\nBody.",
		"archive.memory.retired-note":      "Points at the checkpoint.",
		// Keys that name no memory: the kv's own schema, and bare namespaces.
		"schema_version":   "1",
		"gt.":              "nothing",
		"memory.":          "nothing",
		"memory.feedback.": "nothing",
	})

	var keys []string
	for _, mem := range corpus {
		keys = append(keys, mem.Key)
	}
	if want := []string{"gt.feedback.prime-memories-budget", "memory.checkpoint-note"}; !slices.Equal(keys, want) {
		t.Errorf("the pool holds %v, want only the live memories %v", keys, want)
	}
}

// TestPrimeExplainMemory_ReportsScoresAndReasons pins the dry run's output:
// every pool memory with the idf-weighted raw score, the final score, the
// distinctive hits that qualified it and why the section does not carry it.
func TestPrimeExplainMemory_ReportsScoresAndReasons(t *testing.T) {
	t.Parallel()
	bead := primeMemoryBead()
	now := primeTestEpoch
	matchKey, matchValue := primeMemoryMatch("2026-08-20",
		"A polecat's gt prime spends its hook budget on the hooked work first.")
	overBudgetKey, overBudgetValue := primeMemoryKVEntry("feedback", "prime-memories-budget-elsewhere", "2020-01-01",
		strings.Repeat("Unrelated filler about prime and memories. ", 200))
	corpus := memoryCorpus(primeMemoryCorpus(2, map[string]string{
		matchKey: matchValue, overBudgetKey: overBudgetValue,
	}))

	report := explainMemoryReport(bead, corpus, now)

	match := memoryRow(t, corpus, matchKey)
	if want := fmt.Sprintf("  %-30s  raw %5.1f  final %5.1f  %s  [key:budget, key:memories, key:prime]\n",
		"selected", match.Raw, match.Score, matchKey); !strings.Contains(report, want) {
		t.Errorf("report missing %q:\n%s", want, report)
	}
	over := memoryRow(t, corpus, overBudgetKey)
	if want := fmt.Sprintf("  %-30s  raw %5.1f  final %5.1f  %s  [key:budget, key:memories, key:prime]\n",
		"dropped: "+string(memrecall.DroppedNoBudget), over.Raw, over.Score, overBudgetKey); !strings.Contains(report, want) {
		t.Errorf("report missing %q:\n%s", want, report)
	}
	if header := fmt.Sprintf("explain-memory %s: %d rig memories, 2 matched, 1 selected", bead.ID, len(corpus)); !strings.Contains(report, header) {
		t.Errorf("report missing %q:\n%s", header, report)
	}
}

// TestPrimeExplainMemory_ReportsNoMatches covers the report's other shape: a
// pool nothing scores against, which is what leaves the section out.
func TestPrimeExplainMemory_ReportsNoMatches(t *testing.T) {
	t.Parallel()
	corpus := memoryCorpus(primeMemoryCorpus(2, map[string]string{
		"gt.project.garden-mulch": "type: project\nverified: 2026-08-20\n\nSpread mulch on the beds in spring.\n",
	}))

	report := explainMemoryReport(primeMemoryBead(), corpus, primeTestEpoch)

	if !strings.Contains(report, "0 matched, 0 selected") {
		t.Errorf("report missing the empty selection:\n%s", report)
	}
	if !strings.Contains(report, "no match:") {
		t.Errorf("report does not explain the empty selection:\n%s", report)
	}
}

// memoryRow returns the report row for one memory key.
func memoryRow(t *testing.T, corpus []memrecall.Memory, key string) memrecall.Ranked {
	t.Helper()
	for _, row := range beadMemoryRows(corpus, primeMemoryBead(), primeTestEpoch) {
		if row.Memory.Key == key {
			return row
		}
	}
	t.Fatalf("%s is not in the ranking", key)
	return memrecall.Ranked{}
}

// recallLogRecords reads the town's recall log back as records, one per line.
func recallLogRecords(t *testing.T, townRoot string) []recallRecord {
	t.Helper()
	data, err := os.ReadFile(recallLogPath(townRoot))
	if err != nil {
		t.Fatalf("read recall log: %v", err)
	}
	body := strings.TrimSuffix(string(data), "\n")
	if body == "" {
		return nil
	}
	var recs []recallRecord
	for i, line := range strings.Split(body, "\n") {
		var rec recallRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("recall log line %d is not a record: %v (%q)", i+1, err, line)
		}
		recs = append(recs, rec)
	}
	return recs
}

// TestPrimeMemoryRecallLog_RecordsTheInjection pins the line's content: one
// valid record per injection, naming the bead, the rig, the memories that
// reached the section and the bytes it cost.
func TestPrimeMemoryRecallLog_RecordsTheInjection(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	writePrimeMemorySettings(t, townRoot, true)

	key, value := primeMemoryMatch("2026-08-20",
		"A polecat's gt prime spends its hook budget on the hooked work first.")
	corpus := primeMemoryCorpus(2, map[string]string{key: value})
	f := &fakePrimeRunner{answers: map[string]string{primeKVListCall: primeMemoryKV(t, corpus)}}
	ctx := RoleContext{Role: RolePolecat, Rig: "gastown", TownRoot: townRoot}

	section, _ := primeMemorySection(t, f, ctx, t.TempDir(), primeMemoryBead())

	recs := recallLogRecords(t, townRoot)
	if len(recs) != 1 {
		t.Fatalf("one injection wrote %d lines, want 1: %+v", len(recs), recs)
	}
	rec := recs[0]
	if want := primeTestEpoch.Format(time.RFC3339); rec.TS != want {
		t.Errorf("ts = %q, want the injected clock in UTC, %q", rec.TS, want)
	}
	if rec.Bead != "gt-test1" || rec.Rig != "gastown" {
		t.Errorf("record names bead %q rig %q, want gt-test1 and gastown", rec.Bead, rec.Rig)
	}
	if want := recalledKeys(beadMemoryRows(memoryCorpus(corpus), primeMemoryBead(), primeTestEpoch)); !reflect.DeepEqual(rec.Keys, want) {
		t.Errorf("keys = %v, want the section's memories %v", rec.Keys, want)
	}
	if len(rec.Keys) == 0 {
		t.Error("record names no memory, so nothing about the selection is logged")
	}
	if rec.Bytes != len(section) {
		t.Errorf("bytes = %d, want the %d-byte section", rec.Bytes, len(section))
	}
}

// TestPrimeMemoryRecallLog_AppendsOnlyWhenTheSectionRenders pins both halves of
// the write rule: a prime that renders no section logs nothing, and every
// section appends its own line without touching the ones before it.
func TestPrimeMemoryRecallLog_AppendsOnlyWhenTheSectionRenders(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	writePrimeMemorySettings(t, townRoot, true)

	key, value := primeMemoryMatch("2026-08-20",
		"A polecat's gt prime spends its hook budget on the hooked work first.")
	f := &fakePrimeRunner{answers: map[string]string{primeKVListCall: primeMemoryKV(t, primeMemoryCorpus(2,
		map[string]string{key: value}))}}
	ctx := RoleContext{Role: RolePolecat, Rig: "gastown", TownRoot: townRoot}

	// A line another writer left behind survives every later append.
	if err := appendRecallRecord(recallLogPath(townRoot), recallRecord{TS: "earlier", Bead: "gt-earlier"}); err != nil {
		t.Fatalf("seed the recall log: %v", err)
	}

	// The omission shapes: the flag off, and a corpus nothing scores against.
	flagOff := RoleContext{Role: RolePolecat, Rig: "gastown", TownRoot: t.TempDir()}
	if out, _ := primeMemorySection(t, f, flagOff, t.TempDir(), primeMemoryBead()); out != "" {
		t.Fatalf("the flag off rendered %q, want no section", out)
	}
	unrelated := &fakePrimeRunner{answers: map[string]string{primeKVListCall: primeMemoryKV(t, map[string]string{
		"gt.project.garden-mulch": "type: project\nverified: 2026-08-20\n\nSpread mulch on the beds in spring.\n",
	})}}
	primeMemorySection(t, unrelated, ctx, t.TempDir(), primeMemoryBead())

	for i := 0; i < 2; i++ {
		primeMemorySection(t, f, ctx, t.TempDir(), primeMemoryBead())
	}

	recs := recallLogRecords(t, townRoot)
	if len(recs) != 3 {
		t.Fatalf("two injections over a seeded log wrote %d lines, want 3: %+v", len(recs), recs)
	}
	if recs[0].TS != "earlier" || recs[0].Bead != "gt-earlier" {
		t.Errorf("first line = %+v, want the seeded record untouched", recs[0])
	}
	for i, rec := range recs[1:] {
		if rec.Bead != "gt-test1" || rec.Rig != "gastown" || len(rec.Keys) == 0 {
			t.Errorf("line %d = %+v, want the injected bead, rig and keys", i+2, rec)
		}
	}
}

// TestPrimeMemoryRecallLog_FailureIsNonFatal pins the best-effort contract: a
// recall log that cannot be written — here because .runtime is a file — costs
// one stderr line and leaves the section, and prime, untouched.
func TestPrimeMemoryRecallLog_FailureIsNonFatal(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	writePrimeMemorySettings(t, townRoot, true)
	// A file where the log's directory belongs makes the write fail.
	if err := os.WriteFile(constants.TownRuntimePath(townRoot), []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("block the runtime directory: %v", err)
	}

	key, value := primeMemoryMatch("2026-08-20",
		"A polecat's gt prime spends its hook budget on the hooked work first.")
	f := &fakePrimeRunner{answers: map[string]string{primeKVListCall: primeMemoryKV(t, primeMemoryCorpus(2,
		map[string]string{key: value}))}}
	p, _, out := newFakePrimeTools(f)
	var warnings strings.Builder
	p.errs = &warnings

	owned := p.polecatBeadMemories(RoleContext{Role: RolePolecat, Rig: "gastown", TownRoot: townRoot}, t.TempDir(), primeMemoryBead())

	if !owned {
		t.Error("a failed recall write gave up the section's ownership")
	}
	if !strings.Contains(out.String(), beadMemoryHeading) {
		t.Errorf("failed recall write cost the section its heading:\n%s", out.String())
	}
	if lines := strings.Count(warnings.String(), "\n"); lines != 1 {
		t.Errorf("recall failure printed %d warning lines, want 1: %q", lines, warnings.String())
	}
	if !strings.Contains(warnings.String(), "gt prime: memory recall log:") {
		t.Errorf("warning does not name what failed: %q", warnings.String())
	}
}

// TestPrimeMemoryRecallLog_FileAndDirModes pins the log's on-disk shape: the
// file is 0600 and the directory this feature creates is 0700.
func TestPrimeMemoryRecallLog_FileAndDirModes(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	writePrimeMemorySettings(t, townRoot, true)

	key, value := primeMemoryMatch("2026-08-20",
		"A polecat's gt prime spends its hook budget on the hooked work first.")
	f := &fakePrimeRunner{answers: map[string]string{primeKVListCall: primeMemoryKV(t, primeMemoryCorpus(2,
		map[string]string{key: value}))}}
	primeMemorySection(t, f, RoleContext{Role: RolePolecat, Rig: "gastown", TownRoot: townRoot}, t.TempDir(), primeMemoryBead())

	info, err := os.Stat(recallLogPath(townRoot))
	if err != nil {
		t.Fatalf("stat recall log: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("recall log mode = %o, want 600", got)
	}
	dir, err := os.Stat(constants.TownRuntimePath(townRoot))
	if err != nil {
		t.Fatalf("stat runtime dir: %v", err)
	}
	if got := dir.Mode().Perm(); got != 0o700 {
		t.Errorf("runtime dir mode = %o, want 700", got)
	}
}

func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// TestMarkdownHeading pins what counts as a section heading: markdown's ATX
// rule, so a shell comment, a shebang or an issue number inside a Goal body is
// text rather than the end of the section (gt-40p93).
func TestMarkdownHeading(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		line     string
		wantName string
		wantOK   bool
	}{
		{"## Goal", "Goal", true},
		{"# Constraints", "Constraints", true},
		{"## Goal:", "Goal", true},
		{"### Out of scope", "Out", true},
		{"   ## Gate", "Gate", true},
		{"###### Size", "Size", true},
		{"# comment", "comment", true},
		{"#" + "!/bin/sh", "", false}, // split: a literal opening with "#!" reads to the no-exec-files policy as a script
		{"#4821 is the cause", "", false},
		{"#123", "", false},
		{"#Goal", "", false},
		{"####### Goal", "", false},
		{"#", "", false},
		{"## ", "", false},
		{"    ## indented code", "", false},
		{"\t## tab-indented code", "", false},
		{"not a heading", "", false},
		{"", "", false},
	} {
		name, ok := markdownHeading(tc.line)
		if name != tc.wantName || ok != tc.wantOK {
			t.Errorf("markdownHeading(%q) = (%q, %v), want (%q, %v)", tc.line, name, ok, tc.wantName, tc.wantOK)
		}
	}
}

// TestBeadGoalAndConstraints_KeepsHashLinesInsideTheBody covers the capture
// itself: the Goal and Constraints bodies come back whole when they carry #
// lines that are not headings, and fenced code never ends a section.
func TestBeadGoalAndConstraints_KeepsHashLinesInsideTheBody(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		desc string
		want string
	}{
		{
			name: "issue number and shebang line",
			desc: "## Goal\nfix the crash\n#4821 is the cause\n#!/bin/sh is mangled\nthen retest\n\n## Gate\nmake gate",
			want: "fix the crash\n#4821 is the cause\n#!/bin/sh is mangled\nthen retest\n",
		},
		{
			name: "shell comment inside a backtick fence",
			desc: "## Goal\nrun:\n```sh\n# comment\necho hi\n```\nthen stop\n## Out of scope\nignored",
			want: "run:\n```sh\n# comment\necho hi\n```\nthen stop",
		},
		{
			name: "heading-looking line inside a tilde fence",
			desc: "## Constraints\n~~~\n## Gate\n~~~\nkeep going\n## Size\nignored",
			want: "~~~\n## Gate\n~~~\nkeep going",
		},
		{
			name: "short closing fence does not close a long one",
			desc: "## Goal\n````\n```\n# still code\n````\nafter\n## Gate\nignored",
			want: "````\n```\n# still code\n````\nafter",
		},
		{
			name: "fence outside a wanted section is skipped whole",
			desc: "## Gate\n```\n## Goal\nnot the goal\n```\n## Goal\nreal goal",
			want: "real goal",
		},
		{
			name: "real headings still split sections",
			desc: "attached_molecule: gt-wisp-1\n\n## Goal\ng\n\n## Constraints\nc\n\n## Gate\nignored",
			want: "g\n\nc\n",
		},
	} {
		got := beadGoalAndConstraints(tc.desc)
		if strings.TrimRight(got, "\n") != strings.TrimRight(tc.want, "\n") {
			t.Errorf("%s: beadGoalAndConstraints =\n%q\nwant\n%q", tc.name, got, tc.want)
		}
	}
}

// TestPrimeMemory_RendersInSelectOrder pins the section's order: a pinned
// memory renders ahead of a higher-scoring ranked one, and the recall log
// names the keys in that same order.
func TestPrimeMemory_RendersInSelectOrder(t *testing.T) {
	t.Parallel()
	pinnedKey, pinnedValue := primeMemoryKVEntry("general", "operator-etiquette", "2020-01-01", "Be polite.")
	pinnedValue = strings.Replace(pinnedValue, "verified:", "always: true\ndurable: true\nverified:", 1)
	rankedKey, rankedValue := primeMemoryMatch("2026-08-20",
		"A polecat's gt prime spends its hook budget on the hooked work first.")
	corpus := memoryCorpus(map[string]string{pinnedKey: pinnedValue, rankedKey: rankedValue})

	rows := beadMemoryRows(corpus, primeMemoryBead(), primeTestEpoch)
	if len(rows) != 2 || rows[0].Memory.Key != rankedKey {
		t.Fatalf("rows = %v, want the ranked memory scoring first", rows)
	}
	section := renderBeadMemorySection(rows, "gt-test1")

	if p, r := strings.Index(section, pinnedKey), strings.Index(section, rankedKey); p < 0 || r < 0 || p > r {
		t.Errorf("pinned memory at %d, ranked memory at %d, want pinned first:\n%s", p, r, section)
	}
	if got, want := recalledKeys(rows), []string{pinnedKey, rankedKey}; !reflect.DeepEqual(got, want) {
		t.Errorf("recalledKeys = %v, want %v", got, want)
	}
}

// TestPrimeMemory_PinnedMemoriesPastTheCapAreNotMatches covers the pinned
// memories Select turns away for its per-scope cap: it never weighed them, so
// they are neither "matched" in the footer nor candidates in --explain-memory.
func TestPrimeMemory_PinnedMemoriesPastTheCapAreNotMatches(t *testing.T) {
	t.Parallel()
	corpus := map[string]string{}
	for i := 0; i < 5; i++ {
		key, value := primeMemoryKVEntry("general", fmt.Sprintf("pinned-%d", i), "2026-08-20", "Be polite.")
		corpus[key] = strings.Replace(value, "verified:", "always: true\ndurable: true\nverified:", 1)
	}
	mems := memoryCorpus(corpus)

	rows := beadMemoryRows(mems, primeMemoryBead(), primeTestEpoch)

	capped := len(memrecall.Select(nil, nil, mems, primeTestEpoch, memrecall.Budget))
	if capped == 0 || capped >= len(mems) {
		t.Fatalf("Select kept %d of %d pinned memories, want the cap to turn some away", capped, len(mems))
	}
	matched := 0
	for _, row := range rows {
		if row.Matched() {
			matched++
		}
	}
	if matched != capped {
		t.Errorf("%d memories matched, want the %d pinned memories Select took", matched, capped)
	}
	if section := renderBeadMemorySection(rows, "gt-test1"); strings.Contains(section, "more matched") {
		t.Errorf("footer reports capped pinned memories as matches:\n%s", section)
	}
	if report := explainMemoryReport(primeMemoryBead(), mems, primeTestEpoch); !strings.Contains(report,
		fmt.Sprintf("%d matched, %d selected", capped, capped)) {
		t.Errorf("report counts the capped memories as matches:\n%s", report)
	}
}

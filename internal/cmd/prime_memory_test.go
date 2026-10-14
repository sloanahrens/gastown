package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
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

	key, value := primeMemoryKVEntry("feedback", "prime-budget", "2026-08-20",
		"A polecat's gt prime spends its hook budget on the hooked work first; the memory section is what gets evicted.")
	f := &fakePrimeRunner{answers: map[string]string{primeKVListCall: primeMemoryKV(t, map[string]string{key: value})}}
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

// TestPrimeMemory_CapsTheSection pins memrecall's 2048-byte budget as the
// section's bound: a corpus of matching memories fills it and no more, and the
// keys that did not fit are named in the footer.
func TestPrimeMemory_CapsTheSection(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	writePrimeMemorySettings(t, townRoot, true)

	corpus := map[string]string{}
	for i := 0; i < 12; i++ {
		key, value := primeMemoryKVEntry("feedback", fmt.Sprintf("prime-budget-%02d", i), "2026-08-20",
			strings.Repeat("A polecat's gt prime spends its hook budget on the hooked work first. ", 5))
		corpus[key] = value
	}
	f := &fakePrimeRunner{answers: map[string]string{primeKVListCall: primeMemoryKV(t, corpus)}}
	ctx := RoleContext{Role: RolePolecat, TownRoot: townRoot}

	section, _ := primeMemorySection(t, f, ctx, t.TempDir(), primeMemoryBead())

	rows := beadMemoryRows(memoryCorpus(corpus), primeMemoryBead(), primeTestEpoch)
	selectedBytes, dropped := 0, 0
	for _, row := range rows {
		if row.selected {
			selectedBytes += len(row.text)
			continue
		}
		dropped++
	}
	if selectedBytes == 0 || dropped == 0 {
		t.Fatalf("12 matching memories selected %d bytes and dropped %d, want both non-zero", selectedBytes, dropped)
	}
	if selectedBytes > memrecall.Budget {
		t.Errorf("selected text = %d bytes, over memrecall's %d-byte budget", selectedBytes, memrecall.Budget)
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

	key, value := primeMemoryKVEntry("project", "prime-budget", "2026-08-20",
		strings.Repeat("The polecat's gt prime hook budget is what this memory is about. ", 100))
	f := &fakePrimeRunner{answers: map[string]string{primeKVListCall: primeMemoryKV(t, map[string]string{key: value})}}
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

// droppedMemories returns n candidates that scored but were not selected.
func droppedMemories(n int) []beadMemory {
	rows := make([]beadMemory, 0, n)
	for i := 0; i < n; i++ {
		rows = append(rows, beadMemory{mem: memrecall.Memory{Key: fmt.Sprintf("gt.feedback.dropped-%02d", i)}})
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

	rows := []beadMemory{{
		mem:      memrecall.Memory{Key: "gt.feedback.kept"},
		text:     "### gt.feedback.kept (verified 2026-08-20)\nkept\n",
		selected: true,
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

// TestPrimeExplainMemory_ReportsRawAndFinalScores pins the dry run's output:
// every candidate with both of memrecall's scores and whether the section
// would carry it.
func TestPrimeExplainMemory_ReportsRawAndFinalScores(t *testing.T) {
	t.Parallel()
	bead := primeMemoryBead()
	now := primeTestEpoch
	matchKey, matchValue := primeMemoryKVEntry("feedback", "prime-budget", "2026-08-20",
		"A polecat's gt prime spends its hook budget on the hooked work first.")
	overBudgetKey, overBudgetValue := primeMemoryKVEntry("general", "prime-and-memories-elsewhere", "2020-01-01",
		strings.Repeat("Unrelated filler about prime and memories. ", 200))

	corpus := []memrecall.Memory{
		memrecall.Parse(matchKey, matchValue),
		memrecall.Parse(overBudgetKey, overBudgetValue),
	}
	report := explainMemoryReport(bead, corpus, now)

	q, paths := beadMemoryQuery(bead)
	raw, score := memrecall.Score(q, paths, corpus[0], now)
	if want := fmt.Sprintf("  selected      raw %.1f  final %.1f  %s\n", raw, score, matchKey); !strings.Contains(report, want) {
		t.Errorf("report missing %q:\n%s", want, report)
	}
	raw, score = memrecall.Score(q, paths, corpus[1], now)
	if want := fmt.Sprintf("  not-selected  raw %.1f  final %.1f  %s\n", raw, score, overBudgetKey); !strings.Contains(report, want) {
		t.Errorf("report missing %q:\n%s", want, report)
	}
	if header := fmt.Sprintf("explain-memory %s: 2 rig memories, 2 candidates, 1 selected", bead.ID); !strings.Contains(report, header) {
		t.Errorf("report missing %q:\n%s", header, report)
	}
}

// TestPrimeExplainMemory_ReportsNoCandidates covers the report's other shape:
// a corpus nothing scores against, which is what leaves the section out.
func TestPrimeExplainMemory_ReportsNoCandidates(t *testing.T) {
	t.Parallel()
	corpus := []memrecall.Memory{
		memrecall.Parse("gt.project.garden-mulch", "type: project\nverified: 2026-08-20\n\nSpread mulch on the beds in spring.\n"),
	}

	report := explainMemoryReport(primeMemoryBead(), corpus, primeTestEpoch)

	if !strings.Contains(report, "0 candidates, 0 selected") {
		t.Errorf("report missing the empty selection:\n%s", report)
	}
	if !strings.Contains(report, "no candidate:") {
		t.Errorf("report does not explain the empty selection:\n%s", report)
	}
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

	corpus := map[string]string{}
	key, value := primeMemoryKVEntry("feedback", "prime-budget", "2026-08-20",
		"A polecat's gt prime spends its hook budget on the hooked work first.")
	corpus[key] = value
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

	key, value := primeMemoryKVEntry("feedback", "prime-budget", "2026-08-20",
		"A polecat's gt prime spends its hook budget on the hooked work first.")
	f := &fakePrimeRunner{answers: map[string]string{primeKVListCall: primeMemoryKV(t, map[string]string{key: value})}}
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

	key, value := primeMemoryKVEntry("feedback", "prime-budget", "2026-08-20",
		"A polecat's gt prime spends its hook budget on the hooked work first.")
	f := &fakePrimeRunner{answers: map[string]string{primeKVListCall: primeMemoryKV(t, map[string]string{key: value})}}
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

	key, value := primeMemoryKVEntry("feedback", "prime-budget", "2026-08-20",
		"A polecat's gt prime spends its hook budget on the hooked work first.")
	f := &fakePrimeRunner{answers: map[string]string{primeKVListCall: primeMemoryKV(t, map[string]string{key: value})}}
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

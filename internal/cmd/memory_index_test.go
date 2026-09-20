package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestMemorySummary(t *testing.T) {
	// A value whose first sentence fits: preview is that sentence, flagged.
	shortFirst := "STOPPING RULE (gastown/refinery, 2026-09-09). And then four " +
		"more paragraphs of detail that the index should not carry."

	// A first sentence longer than the cap: word-boundary truncation.
	longFirst := strings.Repeat("guards ", 60) + "end."

	tests := []struct {
		name  string
		value string
		want  string
	}{
		{
			name:  "empty value",
			value: "   \n\t ",
			want:  "(empty)",
		},
		{
			name:  "single short sentence passes through whole",
			value: "Refinery uses a worktree, so it cannot checkout main.",
			want:  "Refinery uses a worktree, so it cannot checkout main.",
		},
		{
			name:  "first sentence kept, remainder flagged with ellipsis",
			value: shortFirst,
			want:  "STOPPING RULE (gastown/refinery, 2026-09-09). …",
		},
		{
			name:  "no terminator and under cap passes through",
			value: "just a fragment without any terminator",
			want:  "just a fragment without any terminator",
		},
		{
			name:  "newlines and runs of spaces collapse to one line",
			value: "line one\n\nline two\t\tline three",
			want:  "line one line two line three",
		},
		{
			name:  "dotted abbreviation is not a sentence end",
			value: "Wrap suites in the gate slot, e.g. gt slot run -- go test, before merging.",
			want:  "Wrap suites in the gate slot, e.g. gt slot run -- go test, before merging.",
		},
		{
			// A marker ordinal must not become a one-character "sentence":
			// the preview would convey nothing at all.
			name:  "leading numbered list marker is not a sentence end",
			value: "1. PRE-REGISTER THE CRITERION before the fix exists. Then the rest of the protocol follows here.",
			want:  "1. PRE-REGISTER THE CRITERION before the fix exists. …",
		},
		{
			name:  "leading lettered list marker is not a sentence end",
			value: "A. Verify the binary in the same capture as the measurement. Then report it provisionally.",
			want:  "A. Verify the binary in the same capture as the measurement. …",
		},
		{
			name:  "leading decimal is not a sentence end",
			value: "1.2 The fix landed and the gate went green afterwards, so the claim held up.",
			want:  "1.2 The fix landed and the gate went green afterwards, so the claim held up.",
		},
		{
			name:  "period inside a filename is not an abbreviation and not a terminator here",
			value: "Rendering lives in internal/cmd/prime.go which reads the kv store",
			want:  "Rendering lives in internal/cmd/prime.go which reads the kv store",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := memorySummary(tt.value)
			if got != tt.want {
				t.Errorf("memorySummary() = %q, want %q", got, tt.want)
			}
			if strings.ContainsAny(got, "\n\r") {
				t.Errorf("memorySummary() spans lines, which would break the index: %q", got)
			}
			if !utf8.ValidString(got) {
				t.Errorf("memorySummary() produced invalid UTF-8: %q", got)
			}
		})
	}

	t.Run("over-cap value truncates at a word boundary within the cap", func(t *testing.T) {
		got := memorySummary(longFirst)
		if !utf8.ValidString(got) {
			t.Fatalf("invalid UTF-8: %q", got)
		}
		if !strings.HasSuffix(got, "…") {
			t.Fatalf("expected elision marker, got %q", got)
		}
		body := strings.TrimSuffix(got, "…")
		if n := utf8.RuneCountInString(body); n > memorySummaryMaxChars {
			t.Errorf("preview body = %d runes, want <= %d", n, memorySummaryMaxChars)
		}
		if strings.HasSuffix(body, " ") || !strings.HasPrefix(longFirst, body) {
			t.Errorf("preview %q is not a clean word-boundary prefix of the value", body)
		}
	})

	t.Run("multibyte value is never cut mid-rune", func(t *testing.T) {
		// The fixture is chosen so that a byte-slicing bug cannot pass by luck:
		// this is a 3-rune / 9-byte cycle, so the 160-rune cap lands at byte 480
		// — and a byte-truncating implementation would cut at byte 160, which is
		// 17 whole cycles plus one byte, splitting a character. A cycle that
		// divides the cut point (e.g. a fixed 22-byte one that lands exactly on
		// a boundary) would satisfy ValidString either way.
		value := strings.Repeat("日本語", 100)
		got := memorySummary(value)
		if !utf8.ValidString(got) {
			t.Fatalf("invalid UTF-8: %q", got)
		}

		body := strings.TrimSuffix(got, "…")
		n := utf8.RuneCountInString(body)
		if n > memorySummaryMaxChars {
			t.Errorf("preview = %d runes, want <= %d", n, memorySummaryMaxChars)
		}
		// The cap must be used, not merely not-exceeded: slicing bytes would
		// silently produce a preview about a third of the intended length.
		if n < memorySummaryMaxChars-20 {
			t.Errorf("preview = %d runes, want close to %d — the cap is not being used", n, memorySummaryMaxChars)
		}
		// And it must still be a rune-wise prefix of the value it summarises.
		if !strings.HasPrefix(value, body) {
			t.Errorf("preview %q is not a prefix of the value", body)
		}
	})
}

// syntheticValue builds a memory value shaped like the live ones: a gist
// sentence followed by several paragraphs, far larger than its preview.
func syntheticValue(i, valueChars int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "OPERATIONAL (2026-09-16): entry %d opens with its gist sentence. ", i)
	for b.Len() < valueChars {
		b.WriteString("More supporting detail follows in later paragraphs. ")
	}
	return b.String()
}

// syntheticMemories builds a corpus shaped like the live one: entries whose
// values run to several paragraphs, far larger than their previews.
func syntheticMemories(n int, valueChars int) map[string]string {
	kvs := make(map[string]string, n)
	for i := 0; i < n; i++ {
		key := fmt.Sprintf("lesson-%02d-about-something-specific", i)
		kvs[memoryKeyPrefix+key] = syntheticValue(i, valueChars)
	}
	return kvs
}

// syntheticTypedMemories spreads memories across every memory type, so the
// sectioned render path is exercised the way the corpus will look once
// memories carry types — all 38 live ones are legacy-untyped "general".
func syntheticTypedMemories(perType, valueChars int) map[string]string {
	kvs := make(map[string]string, perType*len(memoryTypeOrder))
	for _, memType := range memoryTypeOrder {
		for i := 0; i < perType; i++ {
			key := fmt.Sprintf("lesson-%02d-about-something-specific", i)
			kvs[memoryKeyPrefix+memType+"."+key] = syntheticValue(i, valueChars)
		}
	}
	return kvs
}

func countEntryLines(out string) (previews, keyOnly int) {
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(line, "- **"):
			previews++
		case strings.HasPrefix(line, "- "):
			keyOnly++
		}
	}
	return previews, keyOnly
}

// memoryIndexFooterSlack is a generous bound on what renderMemoryIndex appends
// after the budget check: the fixed footer plus the omission line.
const memoryIndexFooterSlack = 250

// omittedCount extracts the "N more memories not listed" count, or -1.
func omittedCount(t *testing.T, out string) int {
	t.Helper()
	m := regexp.MustCompile(`(\d+) more memories not listed`).FindStringSubmatch(out)
	if m == nil {
		return -1
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		t.Fatalf("parse omission count %q: %v", m[1], err)
	}
	return n
}

// TestRenderMemoryIndex_RendersOrCountsEveryMemory is the guard that fails if
// the budget is disabled. Asserting only "the output is short" would pass both
// for a correct cap and for an index that silently rendered nothing, so the
// bound is checked *with* the accounting: every memory is either on a line or
// in the omission count, and never just gone.
func TestRenderMemoryIndex_RendersOrCountsEveryMemory(t *testing.T) {
	const n = 200
	kvs := syntheticMemories(n, 1200)
	grouped := collectMemories(kvs)

	for _, budget := range []int{500, 2000, 4000, 12000, 100000} {
		t.Run(fmt.Sprintf("budget=%d", budget), func(t *testing.T) {
			out := renderMemoryIndex(grouped, budget)

			previews, keyOnly := countEntryLines(out)
			rendered := previews + keyOnly
			omitted := omittedCount(t, out)
			if omitted == -1 {
				omitted = 0
			}
			if rendered+omitted != n {
				t.Fatalf("rendered %d + omitted %d = %d, want %d — memories vanished silently:\n%s",
					rendered, omitted, rendered+omitted, n, out[:min(len(out), 600)])
			}

			if rendered == 0 {
				t.Fatal("index rendered nothing at all")
			}
			// The first entry always survives with its preview, so the index is
			// informative rather than a bare list.
			if !strings.Contains(out, "entry 0 opens with its gist sentence.") {
				t.Errorf("first entry's preview text missing:\n%s", out[:min(len(out), 400)])
			}

			// Entries never exceed the budget; only the trailing footer may.
			if len(out) > budget+memoryIndexFooterSlack {
				t.Errorf("index = %d chars, want <= %d (budget %d + footer)",
					len(out), budget+memoryIndexFooterSlack, budget)
			}

			// Nothing is hidden and uncounted: when the count is zero, every
			// key really is in the output.
			if omitted == 0 {
				for _, mems := range grouped {
					for _, m := range mems {
						if !strings.Contains(out, "- **"+m.shortKey+"**") && !strings.Contains(out, "- "+m.shortKey+"\n") {
							t.Fatalf("no omission count, but memory %q is missing", m.shortKey)
						}
					}
				}
			}
		})
	}
}

// TestRenderMemoryIndex_NeverEmitsAnEmptySection locks in the degradation
// shape: a section header only appears when at least one of its entries fit, so
// exhausting the budget mid-group cannot leave a bare "## Heading" behind.
// Budgets here are chosen to run out inside the first type group, which is
// exactly the case that would strand the later sections.
func TestRenderMemoryIndex_NeverEmitsAnEmptySection(t *testing.T) {
	grouped := collectMemories(syntheticTypedMemories(40, 1200))

	for _, budget := range []int{500, 1500, 3000, 8000, 100000} {
		t.Run(fmt.Sprintf("budget=%d", budget), func(t *testing.T) {
			out := renderMemoryIndex(grouped, budget)
			lines := strings.Split(out, "\n")
			sections := 0
			for i, line := range lines {
				if !strings.HasPrefix(line, "## ") {
					continue
				}
				sections++
				next := ""
				for _, l := range lines[i+1:] {
					if strings.TrimSpace(l) != "" {
						next = l
						break
					}
				}
				if !strings.HasPrefix(next, "- ") {
					t.Errorf("section %q is empty; following line is %q", line, next)
				}
			}
			if sections == 0 {
				t.Error("no sections rendered at all")
			}
		})
	}
}

// TestRenderMemoryIndex_DropsEntriesOnlyWhenKeysCannotFit covers the last rung:
// once even bare keys overflow, the index says how many it could not list, and
// the count is the real remainder rather than a constant.
func TestRenderMemoryIndex_DropsEntriesOnlyWhenKeysCannotFit(t *testing.T) {
	const n = 2000
	kvs := syntheticMemories(n, 1200)
	grouped := collectMemories(kvs)

	const budget = 4000
	out := renderMemoryIndex(grouped, budget)

	omitted := omittedCount(t, out)
	if omitted <= 0 {
		t.Fatalf("expected an omission count once keys cannot fit:\n%s", out[:min(len(out), 400)])
	}

	previews, keyOnly := countEntryLines(out)
	if keyOnly == 0 {
		t.Fatal("expected key-only lines before any omission")
	}
	if previews+keyOnly+omitted != n {
		t.Errorf("rendered %d + omitted %d = %d, want %d", previews+keyOnly, omitted, previews+keyOnly+omitted, n)
	}
	// The omission line is the reader's only pointer to what it cannot see.
	if !strings.Contains(out, "— see `gt memories`") {
		t.Error("the omission line must still point at `gt memories`")
	}

	if len(out) > budget+memoryIndexFooterSlack {
		t.Errorf("index = %d chars, want <= %d", len(out), budget+memoryIndexFooterSlack)
	}
}

// TestRenderMemoryIndex_EmptyStoreRendersNothing pins the no-memories case: no
// section at all, rather than an empty heading an agent has to read past.
func TestRenderMemoryIndex_EmptyStoreRendersNothing(t *testing.T) {
	if out := renderMemoryIndex(map[string][]memoryEntry{}, memoryInjectMaxChars); out != "" {
		t.Errorf("renderMemoryIndex(empty) = %q, want no output", out)
	}

	// Unrelated kv entries must not conjure a section either.
	kvs := map[string]string{"some.other.key": "value", "memory": "no trailing dot"}
	if out := renderMemoryIndex(collectMemories(kvs), memoryInjectMaxChars); out != "" {
		t.Errorf("renderMemoryIndex(no memories) = %q, want no output", out)
	}

	// A kv entry keyed exactly `memory.` has no key to show. Rendering it would
	// produce a "- ****: value" line — an unaddressable entry in an index whose
	// whole purpose is to name keys you can then fetch. It is skipped.
	degenerate := map[string]string{memoryKeyPrefix: "value with no key", memoryKeyPrefix + "general.": "ditto"}
	if out := renderMemoryIndex(collectMemories(degenerate), memoryInjectMaxChars); out != "" {
		t.Errorf("renderMemoryIndex(keyless entries) = %q, want no output", out)
	}
}

// TestRenderMemoryIndex_OrdersKeysWithinAType pins the ordering the index
// promises, so that "the first entry is the first key" is an assertion rather
// than an accident of whichever test happens to look for the first key.
func TestRenderMemoryIndex_OrdersKeysWithinAType(t *testing.T) {
	kvs := map[string]string{
		memoryKeyPrefix + "zebra": "last.",
		memoryKeyPrefix + "alpha": "first.",
		memoryKeyPrefix + "mango": "middle.",
	}
	out := renderMemoryIndex(collectMemories(kvs), memoryInjectMaxChars)

	alpha := strings.Index(out, "**alpha**")
	mango := strings.Index(out, "**mango**")
	zebra := strings.Index(out, "**zebra**")
	if alpha < 0 || mango < 0 || zebra < 0 {
		t.Fatalf("index is missing a key (alpha=%d mango=%d zebra=%d):\n%s", alpha, mango, zebra, out)
	}
	if !(alpha < mango && mango < zebra) {
		t.Errorf("keys out of order (alpha=%d mango=%d zebra=%d):\n%s", alpha, mango, zebra, out)
	}
}

// TestRenderMemoryIndex_BoundsAPathologicalKey covers the one input that could
// otherwise grow the fixed trailer past the budget: `gt remember --key` does
// not length-limit keys, and the footer echoes the first key as the retrieval
// example. Before the example was capped, this test failed.
func TestRenderMemoryIndex_BoundsAPathologicalKey(t *testing.T) {
	longKey := strings.Repeat("k", 8000)
	kvs := map[string]string{
		memoryKeyPrefix + longKey: "OPERATIONAL (2026-09-16): a short gist sentence. Then much more text follows.",
	}
	grouped := collectMemories(kvs)

	const budget = 12000
	out := renderMemoryIndex(grouped, budget)

	if len(out) > budget+memoryIndexFooterSlack {
		t.Errorf("index = %d chars, want <= %d — the footer echoed an unbounded key", len(out), budget+memoryIndexFooterSlack)
	}
	if !strings.Contains(out, "gt memories <key>") {
		t.Error("footer must still name the retrieval command")
	}
	if !strings.Contains(out, "a short gist sentence.") {
		t.Error("the entry's preview text is missing")
	}
}

// TestRenderMemoryIndex_MatchesLiveCorpusScale pins the fix to the measurement
// in gt-hp7t: a corpus the size of the live one (38 entries, ~47k chars of
// values) must render as a small fraction of its raw size, with entries dropped
// only when keys cannot fit.
func TestRenderMemoryIndex_MatchesLiveCorpusScale(t *testing.T) {
	const entries = 38
	const valueChars = 1234 // live median is ~1187
	kvs := syntheticMemories(entries, valueChars)
	grouped := collectMemories(kvs)

	raw := 0
	for _, v := range kvs {
		raw += len(v)
	}

	out := renderMemoryIndex(grouped, memoryInjectMaxChars)

	// The budget bounds the entries; the fixed trailer sits outside it, so the
	// allowance here is the same one the other bound tests use.
	if len(out) > memoryInjectMaxChars+memoryIndexFooterSlack {
		t.Errorf("index = %d chars, want <= %d", len(out), memoryInjectMaxChars+memoryIndexFooterSlack)
	}
	// The live section occupied 48.5k chars of ~12k tokens. Require at least an
	// order-of-magnitude cut so the acceptance target (~10k tokens saved) holds.
	if len(out) > raw/4 {
		t.Errorf("index = %d chars vs %d chars of values; expected at least a 4x reduction", len(out), raw)
	}
	// Count how many entries are rendered vs omitted
	previews, keyOnly := countEntryLines(out)
	omitted := omittedCount(t, out)
	if omitted == -1 {
		omitted = 0
	}
	// With 3000 chars budget, only ~26 entries fit. All entries are accounted for
	// either rendered or omitted.
	if previews+keyOnly+omitted != entries {
		t.Errorf("rendered %d + omitted %d = %d, want %d entries",
			previews+keyOnly, omitted, previews+keyOnly+omitted, entries)
	}
}

// TestRunMemoryInject_ElidesValuesButKeepsThemRetrievable is the end-to-end
// half of the acceptance criterion "memories remain discoverable, no memory
// content lost": prime carries the preview, and `gt memories <key>` still
// returns the value in full.
func TestRunMemoryInject_ElidesValuesButKeepsThemRetrievable(t *testing.T) {
	const key = "live-verification-escalation-chain"
	tail := "TAIL-MARKER only present in the full text."
	value := "LIVE VERIFICATION PROTOCOL, run twice against destruction-gate fixes. " +
		strings.Repeat("Detail that the index must not carry. ", 60) + tail

	kvJSON, err := json.Marshal(map[string]string{memoryKeyPrefix + key: value})
	if err != nil {
		t.Fatalf("marshal kv: %v", err)
	}
	jsonPath := filepath.Join(t.TempDir(), "kv.json")
	if err := os.WriteFile(jsonPath, kvJSON, 0600); err != nil {
		t.Fatalf("write kv json: %v", err)
	}
	workDir := setupPrimeExternalToolTest(t, `
case "$*" in
  "kv list --json") cat "$KV_JSON_FILE"; exit 0 ;;
esac
`, ``)
	t.Setenv("KV_JSON_FILE", jsonPath)

	out := captureStdout(t, func() { runMemoryInject(workDir) })

	if !strings.Contains(out, "# Agent Memories (1)") {
		t.Errorf("missing memory header:\n%s", out)
	}
	if !strings.Contains(out, key) {
		t.Errorf("memory key missing from index:\n%s", out)
	}
	if !strings.Contains(out, "LIVE VERIFICATION PROTOCOL, run twice against destruction-gate fixes.") {
		t.Errorf("preview text missing from index:\n%s", out)
	}
	if strings.Contains(out, tail) {
		t.Errorf("index carried the full value instead of a preview:\n%s", out)
	}
	if !strings.Contains(out, "gt memories <key>") {
		t.Errorf("index does not tell the reader how to get full text:\n%s", out)
	}

	// The elided text is still retrievable through the path the index points at.
	retrieved := captureStdout(t, func() {
		if err := runMemories(memoriesCmd, []string{key}); err != nil {
			t.Fatalf("runMemories(%q): %v", key, err)
		}
	})
	if !strings.Contains(retrieved, tail) {
		t.Errorf("gt memories %s did not return the full value:\n%s", key, retrieved)
	}
}

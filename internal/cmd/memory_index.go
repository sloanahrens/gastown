package cmd

import (
	"fmt"
	"sort"
	"strings"
)

// Memory index rendering for `gt prime`.
//
// Memories live in the beads kv store and prime used to render every value in
// full on every session. Measured 2026-09-16 on the gastown witness: 38
// entries, 48.5k chars (~12k tokens), 55% of the entire prime payload — and
// the whole thing is re-read on every turn of every long-lived witness,
// refinery, and deacon session (gt-hp7t).
//
// The index keeps the cheap part, which memories exist and what each is about,
// and defers the full text to `gt memories <key>`, a retrieval path that
// already existed and does a substring match over key and value.

const (
	// memorySummaryMaxChars bounds the preview rendered for a single memory.
	memorySummaryMaxChars = 160
	// memoryInjectMaxChars bounds the whole "# Agent Memories" section. The
	// per-entry cap alone would still let a large enough corpus crowd out the
	// rest of prime, and this corpus only grows, so the section as a whole is
	// capped too.
	memoryInjectMaxChars = 12000
	// memoryExampleMaxChars bounds the example key echoed in the footer, so the
	// fixed trailer cannot grow with a pathological key.
	memoryExampleMaxChars = 60
)

// memoryEntry is one stored memory selected for display.
type memoryEntry struct {
	memType  string
	shortKey string
	value    string
}

// collectMemories groups the kv store's memories by type, each group sorted by
// key so prime output is stable across sessions.
func collectMemories(kvs map[string]string) map[string][]memoryEntry {
	grouped := make(map[string][]memoryEntry)
	for k, v := range kvs {
		if !strings.HasPrefix(k, memoryKeyPrefix) {
			continue
		}
		memType, shortKey := parseMemoryKey(k)
		if shortKey == "" {
			// An entry keyed exactly `memory.` (or `memory.<type>.`) with nothing
			// after it has no key to show and cannot be addressed by the
			// `gt memories <key>` path the index points at, so it is not an
			// index entry. It is still listed by `gt memories`.
			continue
		}
		grouped[memType] = append(grouped[memType], memoryEntry{memType: memType, shortKey: shortKey, value: v})
	}
	for t := range grouped {
		sort.Slice(grouped[t], func(i, j int) bool {
			return grouped[t][i].shortKey < grouped[t][j].shortKey
		})
	}
	return grouped
}

// renderMemoryIndex renders the "# Agent Memories" section of prime output,
// bounded to maxChars.
//
// Every memory stays discoverable at every budget: an entry renders as a
// "key: preview" line, falls back to a bare key when the budget is nearly
// spent, and is only dropped, with a count and a pointer to `gt memories`, if
// even bare keys overflow.
func renderMemoryIndex(grouped map[string][]memoryEntry, maxChars int) string { //nolint:unparam // budget is parameterized so tests can drive the degradation ladder
	total := 0
	for _, mems := range grouped {
		total += len(mems)
	}
	if total == 0 {
		return ""
	}

	var b strings.Builder
	fmt.Fprintf(&b, "\n# Agent Memories (%d)\n", total)

	var example string
	omitted := 0

	for _, memType := range memoryTypeOrder {
		mems := grouped[memType]
		if len(mems) == 0 {
			continue
		}
		label := memoryTypeLabels[memType]
		if label == "" {
			label = memType
		}
		header := fmt.Sprintf("\n## %s\n\n", label)

		// Stage the group first: emitting the header before knowing whether any
		// of its entries fit would leave an empty section behind once the
		// budget runs out mid-group.
		var section strings.Builder
		for _, m := range mems {
			spent := b.Len() + len(header) + section.Len()
			full := fmt.Sprintf("- **%s**: %s\n", m.shortKey, memorySummary(m.value))
			keyOnly := fmt.Sprintf("- %s\n", m.shortKey)

			switch {
			case spent+len(full) <= maxChars:
				section.WriteString(full)
			case spent+len(keyOnly) <= maxChars:
				section.WriteString(keyOnly)
			default:
				omitted++
				continue
			}
			if example == "" {
				example = m.shortKey
			}
		}

		if section.Len() > 0 {
			b.WriteString(header)
			b.WriteString(section.String())
		}
	}

	if omitted > 0 {
		fmt.Fprintf(&b, "\n%d more memories not listed (index budget) — see `gt memories`.\n", omitted)
	}

	b.WriteString("\nPreviews only — full text: `gt memories <key>`")
	if example != "" {
		// Keys are short in practice, but `gt remember --key` does not length-
		// limit them, and an unbounded echo here would be the one way for this
		// section to blow past its budget. A truncated key still works as a
		// search term.
		fmt.Fprintf(&b, " (e.g. `gt memories %s`)", truncateWords([]rune(example), memoryExampleMaxChars))
	}
	b.WriteString(".\n")

	return b.String()
}

// memorySummary renders a one-line preview of a memory value. Values run to
// several paragraphs, so this flattens whitespace, prefers a complete first
// sentence (usually the memory's gist), and marks with an ellipsis anything it
// drops so the reader knows there is more.
func memorySummary(value string) string {
	flat := strings.Join(strings.Fields(value), " ")
	if flat == "" {
		return "(empty)"
	}

	runes := []rune(flat)
	if end := firstSentenceEnd(runes); end > 0 && end <= memorySummaryMaxChars {
		// runes[:end] already carries the terminator, so the ellipsis is
		// spaced off it rather than colliding with it ("sentence. …").
		return string(runes[:end]) + " …"
	}
	if len(runes) <= memorySummaryMaxChars {
		return flat
	}
	return truncateWords(runes, memorySummaryMaxChars) + "…"
}

// firstSentenceEnd returns the rune index just past the first sentence
// terminator in runes, or 0 if there is none.
//
// A terminator must be followed by a space, and the period of an abbreviation
// ("e.g.", "i.e.") or an ordinal ("1.", "A.") is not a terminator. A wrong
// guess costs only a longer or shorter preview, since the full text is one
// `gt memories <key>` away, so these rules stay deliberately simple.
func firstSentenceEnd(runes []rune) int {
	for i := 1; i < len(runes); i++ {
		if runes[i] != ' ' {
			continue
		}
		switch runes[i-1] {
		case '.', '!', '?':
		default:
			continue
		}
		if isAbbrevRatherThanSentence(runes[:i-1]) {
			continue
		}
		return i
	}
	return 0
}

// isAbbrevRatherThanSentence reports whether the text preceding a terminator is
// too short to be a sentence of its own, so the terminator belongs to an
// abbreviation or an ordinal instead of ending one:
//
//   - nothing at all, when the value opens with the terminator;
//   - a single letter or digit, the "1." / "A." / "I." of a list marker;
//   - a letter or digit following a period, the "e.g." / "i.e." / "1.2." shape.
//
// A period inside a filename or path ("prime.go", "events.jsonl") does not
// match any of these, because its preceding rune is a letter that is not itself
// preceded by a period.
func isAbbrevRatherThanSentence(text []rune) bool {
	if len(text) < 2 {
		return true
	}
	last, prev := text[len(text)-1], text[len(text)-2]
	return prev == '.' && isASCIIWordRune(last)
}

func isASCIIWordRune(r rune) bool {
	return isASCIILetter(r) || (r >= '0' && r <= '9')
}

func isASCIILetter(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
}

// truncateWords cuts runes to max, backing up to the last space so no word is
// split and no rune is cut in half.
func truncateWords(runes []rune, max int) string { //nolint:unparam // max is parameterized so callers can tune the preview bound
	if len(runes) <= max {
		return string(runes)
	}
	cut := runes[:max]
	for i := len(cut) - 1; i > 0; i-- {
		if cut[i] == ' ' {
			return string(cut[:i])
		}
	}
	return string(cut)
}

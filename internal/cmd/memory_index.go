package cmd

import (
	"bytes"
	"encoding/json"
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
// and defers the full text to `bd kv get <key>`. Entries show the full kv key
// so that lookup needs no translation from a display name.

// Memories live in two kv namespaces: gt.<type>.<key>, which the retired
// gt remember wrote, and memory.<key>, which `bd remember` writes. Both corpora
// are live, so every reader has to accept both prefixes (gt-o51s).
const (
	memoryKeyPrefix       = "gt."
	memoryLegacyKeyPrefix = "memory."
)

// isMemoryKey reports whether a beads kv key holds a memory, in either the gt.*
// or the legacy memory.* namespace.
func isMemoryKey(key string) bool {
	return strings.HasPrefix(key, memoryKeyPrefix) || strings.HasPrefix(key, memoryLegacyKeyPrefix)
}

// validMemoryTypes are the recognized memory type categories.
// Typed memories are stored as gt.<type>.<key> in the kv store.
// Legacy untyped memories (gt.<key>) are treated as "general".
var validMemoryTypes = map[string]string{
	"feedback":  "Guidance or corrections from users — behavioral rules for future work",
	"project":   "Ongoing work context, goals, deadlines, decisions",
	"user":      "Info about the user's role, preferences, expertise",
	"reference": "Pointers to external resources (URLs, tools, dashboards)",
	"general":   "Uncategorized memories (default)",
}

// memoryTypeOrder defines the injection priority during gt prime.
// Feedback first (behavioral corrections), then user context, then the rest.
var memoryTypeOrder = []string{"feedback", "user", "project", "reference", "general"}

const (
	// memorySummaryMaxChars bounds the preview rendered for a single memory.
	memorySummaryMaxChars = 160
	// memoryInjectMaxChars bounds the whole "# Agent Memories" section. The
	// per-entry cap alone would still let a large enough corpus crowd out the
	// rest of prime, and this corpus only grows, so the section as a whole is
	// capped too. A third of primeHookBudget: the index shares the hook with
	// the hooked work and the role text, so it cannot claim the whole budget.
	memoryInjectMaxChars = 3000
	// memoryExampleMaxChars bounds the example key echoed in the footer, so the
	// fixed trailer cannot grow with a pathological key.
	memoryExampleMaxChars = 60
)

// memoryEntry is one stored memory selected for display.
type memoryEntry struct {
	memType string
	key     string // full kv key, as bd kv get takes it
	value   string
}

// collectMemories groups the kv store's memories by type, each group sorted by
// key so prime output is stable across sessions. Reads both memory namespaces
// (see isMemoryKey).
func collectMemories(kvs map[string]string) map[string][]memoryEntry {
	grouped := make(map[string][]memoryEntry)
	for k, v := range kvs {
		if !isMemoryKey(k) {
			continue
		}
		memType, shortKey := parseMemoryKey(k)
		if shortKey == "" {
			// A key that is nothing but a namespace prefix (`gt.`, `memory.`,
			// or either one plus a bare type) names no memory, so it is not an
			// index entry. bd kv list still shows it.
			continue
		}
		grouped[memType] = append(grouped[memType], memoryEntry{memType: memType, key: k, value: v})
	}
	for t := range grouped {
		sort.Slice(grouped[t], func(i, j int) bool {
			return grouped[t][i].key < grouped[t][j].key
		})
	}
	return grouped
}

// renderMemoryIndex renders the "# Agent Memories" section of prime output,
// bounded to maxChars.
//
// Every memory stays discoverable at every budget: an entry renders as a
// "key: preview" line, falls back to a bare key when the budget is nearly
// spent, and is only dropped, with a count and a pointer to `bd kv list`, if
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
			full := fmt.Sprintf("- **%s**: %s\n", m.key, memorySummary(m.value))
			keyOnly := fmt.Sprintf("- %s\n", m.key)

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
				example = m.key
			}
		}

		if section.Len() > 0 {
			b.WriteString(header)
			b.WriteString(section.String())
		}
	}

	if omitted > 0 {
		fmt.Fprintf(&b, "\n%d more memories not listed (index budget) — see `bd kv list`.\n", omitted)
	}

	b.WriteString("\nPreviews only — full text: `bd kv get <key>`")
	if example != "" {
		// Keys are short in practice, but nothing length-limits them, and an
		// unbounded echo here would be the one way for this section to blow
		// past its budget.
		fmt.Fprintf(&b, " (e.g. `bd kv get %s`)", truncateWords([]rune(example), memoryExampleMaxChars))
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
// `bd kv get <key>` away, so these rules stay deliberately simple.
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

// parseMemoryKey extracts the type and short key from a full kv key.
// Handles both typed keys (gt.<type>.<key> or memory.<type>.<key>) and legacy
// untyped keys (gt.<key> or memory.<key>).
func parseMemoryKey(kvKey string) (memType, shortKey string) {
	prefix := memoryLegacyKeyPrefix
	if strings.HasPrefix(kvKey, memoryKeyPrefix) {
		prefix = memoryKeyPrefix
	}

	rest := strings.TrimPrefix(kvKey, prefix)
	if rest == "" {
		return "general", ""
	}

	// Check if first segment is a known type
	if dotIdx := strings.Index(rest, "."); dotIdx > 0 {
		candidate := rest[:dotIdx]
		if _, ok := validMemoryTypes[candidate]; ok {
			return candidate, rest[dotIdx+1:]
		}
	}

	// Legacy untyped memory
	return "general", rest
}

// parseBdKvListJSON parses bd kv list --json output into displayable string values.
func parseBdKvListJSON(data []byte) (map[string]string, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parsing kv list: %w", err)
	}

	kvs := make(map[string]string, len(raw))
	for k, v := range raw {
		var s *string
		if err := json.Unmarshal(v, &s); err == nil {
			if s != nil {
				kvs[k] = *s
			}
			continue
		}

		if !isMemoryKey(k) {
			continue
		}

		// Keep non-string memory values visible without promoting bd metadata.
		var compact bytes.Buffer
		if err := json.Compact(&compact, v); err != nil {
			continue
		}
		kvs[k] = compact.String()
	}
	return kvs, nil
}

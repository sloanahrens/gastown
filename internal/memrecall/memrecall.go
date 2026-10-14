// Package memrecall scores a rig's stored memories against a hooked bead and
// picks the few that fit a byte budget.
//
// Selection is deterministic keyword scoring: no model, no embeddings, and no
// clock, filesystem or beads access. The caller passes the memories, the
// bead's query terms and now, so the same inputs always render the same
// section (gt-u6swi).
package memrecall

import (
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Budget is the byte cap of the rendered memory section.
const Budget = 2048

// CandidateFloor is the raw score a memory needs to be a candidate at all. It
// is checked before the type weight, so one body hit alone (raw 1) never
// qualifies while one key hit (raw 2) does.
const CandidateFloor = 2.0

// maxAlwaysPerScope bounds the always-and-durable memories one scope
// contributes, so a corpus of pinned memories cannot own the whole budget.
const maxAlwaysPerScope = 3

// recencyWindow is the age inside which a verified memory earns recencyBonus.
// Exactly recencyWindow old is inside.
const recencyWindow = 30 * 24 * time.Hour

// recencyBonus is added after weighting, so it lifts a stale-but-relevant
// memory without reordering two memories of the same type.
const recencyBonus = 0.5

// types are the memory kinds the format defines, each with its score weight:
// guidance the agent is meant to act on outranks a pointer, and an unknown or
// absent type weighs as general.
var types = map[string]float64{
	"feedback":  1.3,
	"project":   1.0,
	"reference": 0.8,
	"general":   0.6,
}

// headerNames are the fields a memory value's header block defines. A value
// whose first line opens with any other name is legacy: the whole value is the
// body (gt-u6swi).
var headerNames = map[string]struct{}{
	"recorded":   {},
	"verified":   {},
	"type":       {},
	"durable":    {},
	"scope":      {},
	"always":     {},
	"anchors":    {},
	"supersedes": {},
}

// stopwords are the terms a memory and a bead share by accident: articles,
// auxiliaries, and the words every memory or every bead already carries.
var stopwords = map[string]struct{}{
	"the": {}, "and": {}, "for": {}, "with": {}, "not": {}, "from": {},
	"when": {}, "this": {}, "that": {}, "are": {}, "was": {}, "has": {},
	"bug": {}, "fix": {}, "note": {},
}

// Memory is one stored memory, parsed from a beads kv entry.
type Memory struct {
	// Key is the kv key as stored, the form `bd kv get` takes.
	Key string
	// Type is feedback, project, reference or general.
	Type string
	// Body is the value with its header block removed.
	Body string
	// Scope is rig or town, defaulting to rig.
	Scope string
	// Durable marks a memory that is not about this session's work.
	Durable bool
	// Always marks a memory that is injected regardless of score.
	Always bool
	// Anchors are the file:<path>, symbol:<name> and bead:<id> references the
	// memory names.
	Anchors []string
	// Verified is the date the memory was last checked, zero when unknown.
	Verified time.Time
}

// Selected is a memory chosen for the section, with the score it earned and
// the text it contributes.
type Selected struct {
	Memory
	Score float64
	Text  string
}

// Parse reads one kv entry into a Memory.
//
// A legacy value — a value whose first line names no header field — has no
// header block: the whole value is the body, the type comes from the key, and
// the verification date comes from a trailing `[recorded by: ... @ YYYY-MM-DD
// ...]` marker when there is one.
func Parse(key, value string) Memory {
	headers, body, modern := headerBlock(value)
	if !modern {
		return Memory{
			Key:      key,
			Type:     keyType(key),
			Body:     value,
			Scope:    "rig",
			Verified: markerDate(value),
		}
	}

	m := Memory{Key: key, Type: "general", Body: body, Scope: "rig"}
	for _, line := range headers {
		name, val, _ := strings.Cut(line, ":")
		switch name {
		case "type":
			if v := strings.ToLower(strings.TrimSpace(val)); v != "" {
				m.Type = v
			}
		case "scope":
			if v := strings.ToLower(strings.TrimSpace(val)); v != "" {
				m.Scope = v
			}
		case "durable":
			m.Durable = boolField(val)
		case "always":
			m.Always = boolField(val)
		case "verified":
			m.Verified = dateField(val)
		case "anchors":
			m.Anchors = anchorList(val)
		}
	}
	return m
}

// Keywords is a memory's search terms: its key slug plus the first non-empty
// body line. It is the shared-case rule the Python tooling reads.
func Keywords(key, body string) map[string]struct{} {
	out := tokens(keySlug(key))
	for _, line := range strings.Split(body, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		union(out, tokens(line))
		break
	}
	return out
}

// BodyKeywords is a memory's terms taken from the whole body.
func BodyKeywords(body string) map[string]struct{} {
	return tokens(body)
}

// Query is the bead side of the comparison: the terms of its title,
// description and labels, and of the paths it names.
func Query(title, description string, labels, paths []string) map[string]struct{} {
	q := tokens(title)
	union(q, tokens(description))
	for _, l := range labels {
		union(q, tokens(l))
	}
	for _, p := range paths {
		// Tokenization already breaks on / and ., so a path's segments arrive
		// as separate terms.
		union(q, tokens(p))
	}
	return q
}

// Score returns m's raw keyword weight against q and its weighted score.
//
// raw is the candidacy test against CandidateFloor and is computed before any
// weighting. A query term counts twice in the key slug, once in the body when
// it is not already a key hit, and a file anchor the bead also names adds two.
func Score(q map[string]struct{}, beadPaths []string, m Memory, now time.Time) (raw, score float64) {
	keyTerms := tokens(keySlug(m.Key))
	bodyTerms := tokens(m.Body)

	var keyHits, bodyHits float64
	for w := range q {
		if _, ok := keyTerms[w]; ok {
			keyHits++
			continue
		}
		if _, ok := bodyTerms[w]; ok {
			bodyHits++
		}
	}

	raw = 2*keyHits + bodyHits
	if anchorMatches(beadPaths, m.Anchors) {
		raw += 2
	}
	return raw, raw*weight(m.Type) + recency(m.Verified, now)
}

// Overlap is the fraction of the smaller of two term sets that both hold, and
// zero when either is empty.
func Overlap(a, b map[string]struct{}) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	small, large := a, b
	if len(b) < len(a) {
		small, large = b, a
	}
	shared := 0
	for w := range small {
		if _, ok := large[w]; ok {
			shared++
		}
	}
	return float64(shared) / float64(len(small))
}

// Select returns the memories to render, in the order they should appear.
//
// Always-and-durable memories come first regardless of score, at most
// maxAlwaysPerScope per scope; the scored candidates follow, ranked by score,
// then newer verification, then key. A memory whose text does not fit the
// remaining budget is skipped whole rather than truncated, so a later, shorter
// memory can still take the space. A non-positive budget means Budget.
func Select(q map[string]struct{}, beadPaths []string, mems []Memory, now time.Time, budget int) []Selected {
	if budget <= 0 {
		budget = Budget
	}

	var pinned, ranked []rankedMemory
	for _, m := range mems {
		raw, score := Score(q, beadPaths, m, now)
		switch {
		case m.Always && m.Durable:
			pinned = append(pinned, rankedMemory{mem: m, score: score})
		case raw >= CandidateFloor:
			ranked = append(ranked, rankedMemory{mem: m, score: score})
		}
	}
	sortRanked(pinned)
	sortRanked(ranked)

	seen := make(map[string]struct{}, len(mems))
	perScope := make(map[string]int)
	out := make([]Selected, 0, len(mems))
	used := 0

	fill := func(xs []rankedMemory, capped bool) {
		for _, x := range xs {
			if _, dup := seen[x.mem.Key]; dup {
				continue
			}
			if capped && perScope[x.mem.Scope] >= maxAlwaysPerScope {
				continue
			}
			text := renderMemory(x.mem)
			if used+len(text) > budget {
				continue
			}
			seen[x.mem.Key] = struct{}{}
			perScope[x.mem.Scope]++
			used += len(text)
			out = append(out, Selected{Memory: x.mem, Score: x.score, Text: text})
		}
	}
	fill(pinned, true)
	fill(ranked, false)
	return out
}

// rankedMemory is a memory with the score Select ranks it by.
type rankedMemory struct {
	mem   Memory
	score float64
}

// sortRanked orders by score, then newer verification, then key, so a corpus
// renders the same section on every run.
func sortRanked(xs []rankedMemory) {
	sort.Slice(xs, func(i, j int) bool {
		a, b := xs[i], xs[j]
		if a.score != b.score {
			return a.score > b.score
		}
		if !a.mem.Verified.Equal(b.mem.Verified) {
			return a.mem.Verified.After(b.mem.Verified)
		}
		return a.mem.Key < b.mem.Key
	})
}

// renderMemory is a memory's whole contribution to the section: its heading
// with the date it was last verified, then its body. A memory with no
// verification date shows the status instead of the date, never a zero date.
func renderMemory(m Memory) string {
	heading := "### " + m.Key + " (unverified)"
	if !m.Verified.IsZero() {
		heading = "### " + m.Key + " (verified " + m.Verified.Format("2006-01-02") + ")"
	}
	return heading + "\n" + m.Body + "\n"
}

// headerBlock splits a value's leading `name: value` block from its body. ok is
// false when the first line names no header field, which is what makes a value
// legacy.
func headerBlock(value string) (headers []string, body string, ok bool) {
	lines := strings.Split(value, "\n")
	if !isHeaderLine(lines[0]) {
		return nil, value, false
	}
	for i, line := range lines {
		if strings.TrimSpace(line) == "" {
			return lines[:i], strings.Join(lines[i+1:], "\n"), true
		}
		if !isHeaderLine(line) {
			// A body running on with no blank separator still ends the block,
			// so its text is not read as fields.
			return lines[:i], strings.Join(lines[i:], "\n"), true
		}
	}
	return lines, "", true
}

// isHeaderLine reports whether a line opens a header field, which the text
// before its first colon naming one of the format's fields makes it.
func isHeaderLine(line string) bool {
	name, _, found := strings.Cut(line, ":")
	if !found {
		return false
	}
	_, ok := headerNames[strings.TrimRight(name, " \t")]
	return ok
}

// markerDate reads the date out of a legacy value's trailing
// `[recorded by: <who> @ YYYY-MM-DD ...]` marker.
func markerDate(value string) time.Time {
	open := strings.LastIndexByte(value, '[')
	if open < 0 {
		return time.Time{}
	}
	rest := value[open+1:]
	end := strings.IndexByte(rest, ']')
	if end < 0 {
		return time.Time{}
	}
	at := strings.LastIndex(rest[:end], "@")
	if at < 0 {
		return time.Time{}
	}
	fields := strings.Fields(rest[at+1 : end])
	if len(fields) == 0 {
		return time.Time{}
	}
	return dateField(strings.TrimRight(fields[0], ".,;"))
}

// keySlug is a memory key's own words: the kv namespace, and the type segment
// of a gt.<type>.<slug> key, are not among them.
func keySlug(key string) string {
	if rest, ok := strings.CutPrefix(key, "memory."); ok {
		return rest
	}
	rest, ok := strings.CutPrefix(key, "gt.")
	if !ok {
		return key
	}
	if t, slug, found := strings.Cut(rest, "."); found && isType(t) {
		return slug
	}
	return rest
}

// keyType reads a legacy memory's type from its key: gt.<type>.<slug> and
// memory.<type>.<slug> name their type, anything else is general.
func keyType(key string) string {
	rest := strings.TrimPrefix(key, "gt.")
	if rest == key {
		rest = strings.TrimPrefix(key, "memory.")
	}
	if t, _, found := strings.Cut(rest, "."); found && isType(t) {
		return t
	}
	return "general"
}

// weight is a type's score weight; an unrecognized type weighs as general.
func weight(t string) float64 {
	if w, ok := types[t]; ok {
		return w
	}
	return types["general"]
}

// isType reports whether t is one of the types the format defines.
func isType(t string) bool {
	_, ok := types[t]
	return ok
}

// recency is recencyBonus while verified is inside recencyWindow of now, in
// either direction: a date ahead of now is a clock skew, not an old memory.
func recency(verified, now time.Time) float64 {
	if verified.IsZero() {
		return 0
	}
	if now.Sub(verified).Abs() <= recencyWindow {
		return recencyBonus
	}
	return 0
}

// anchorMatches reports whether m names a file the bead names. Only a
// file:<path> anchor counts: a symbol or bead anchor says where a memory came
// from, not what it is about.
func anchorMatches(beadPaths, anchors []string) bool {
	for _, a := range anchors {
		path, ok := strings.CutPrefix(a, "file:")
		if !ok {
			continue
		}
		if path = strings.TrimSpace(path); path == "" {
			continue
		}
		for _, p := range beadPaths {
			if strings.TrimSpace(p) == path {
				return true
			}
		}
	}
	return false
}

// anchorList splits an `anchors:` value into its entries.
func anchorList(val string) []string {
	var out []string
	for _, a := range strings.Split(val, ";") {
		if a = strings.TrimSpace(a); a != "" {
			out = append(out, a)
		}
	}
	return out
}

// boolField reads one of the format's true/false fields.
func boolField(val string) bool {
	return strings.EqualFold(strings.TrimSpace(val), "true")
}

// dateField parses one of the format's YYYY-MM-DD dates. A malformed or absent
// date leaves the zero time, which renders as unverified and earns no recency.
func dateField(val string) time.Time {
	t, err := time.Parse("2006-01-02", strings.TrimSpace(val))
	if err != nil {
		return time.Time{}
	}
	return t
}

// tokens is the one tokenizer every term set comes from: the lower-cased
// alphanumeric runs of at least three characters, minus the stopwords. Runs,
// not words — punctuation separates, so a path's segments arrive as terms.
func tokens(text string) map[string]struct{} {
	out := make(map[string]struct{})
	start := -1
	flush := func(end int) {
		if start < 0 {
			return
		}
		addToken(out, text[start:end])
		start = -1
	}
	for i, r := range text {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			if start < 0 {
				start = i
			}
			continue
		}
		flush(i)
	}
	flush(len(text))
	return out
}

// addToken records w's token when it is long enough and not a stopword.
func addToken(dst map[string]struct{}, w string) {
	w = strings.ToLower(w)
	if utf8.RuneCountInString(w) < 3 {
		return
	}
	if _, stop := stopwords[w]; stop {
		return
	}
	dst[w] = struct{}{}
}

// union adds src's terms to dst.
func union(dst, src map[string]struct{}) {
	for w := range src {
		dst[w] = struct{}{}
	}
}

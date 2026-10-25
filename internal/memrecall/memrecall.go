// Package memrecall scores a rig's stored memories against a hooked bead and
// picks the few that fit a byte budget.
//
// Selection is deterministic keyword scoring: no model, no embeddings, and no
// clock, filesystem or beads access. The caller passes the memories, the
// bead's query terms and now, so the same inputs always render the same
// section (gt-u6swi).
//
// A matched keyword is worth its rarity: the pool's own statistics say how many
// memories hold each of the bead's words, and a word every memory holds weighs
// almost nothing beside a word one memory holds (gt-bvnbr). That is what stops
// a long memory from winning on the town's common words alone.
package memrecall

import (
	"math"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Budget is the byte cap of the rendered memory section.
const Budget = 2048

// CandidateFloor is the idf-weighted raw score a memory needs to be a candidate
// at all, checked before the type weight. A word every memory holds contributes
// ln 2 to the raw score, so the pool's common words alone cannot reach the
// floor (gt-bvnbr).
const CandidateFloor = 4.0

// DistinctiveShare is the largest share of the pool a matched keyword may
// appear in and still count as distinctive: df/N <= 0.2.
const DistinctiveShare = 0.2

// SmallPool is the pool size below which the share test is not used: at this
// size a key hit or a file anchor is what makes a memory distinctive, and a
// body hit is not (gt-bvnbr).
const SmallPool = 10

// MaxScored bounds the scored memories the section carries, however many clear
// the bar.
const MaxScored = 3

// KeepShare is the fraction of the top score a scored memory must reach to stay
// in the running. One memory that is rarely relevant is not worth the next
// three.
const KeepShare = 0.6

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
//
// This map is the one type registry: the prime index parses keys through the
// same resolver (ParseKey), so a type added here is typed in both places and,
// paired with a memoryTypeOrder entry in internal/cmd, injected under it
// (gt-fg4rh).
var types = map[string]float64{
	"feedback":  1.3,
	"user":      1.1,
	"project":   1.0,
	"reference": 0.8,
	"general":   0.6,
}

// Key namespaces: gt.<type>.<slug> is what the retired gt remember wrote,
// memory.<slug> is what bd remember writes. Both corpora are live (gt-o51s).
const (
	keyPrefix       = "gt."
	legacyKeyPrefix = "memory."
)

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
	// Type is feedback, user, project, reference or general.
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

// DropReason is why the section does not carry a memory. The empty reason means
// it does.
type DropReason string

const (
	// DroppedNoDistinctive: no matched keyword was distinctive enough to say
	// the memory is about this bead rather than about the town.
	DroppedNoDistinctive DropReason = "no distinctive hit"
	// DroppedBelowFloor: the memory matched, but not by enough to reach
	// CandidateFloor.
	DroppedBelowFloor DropReason = "below floor"
	// DroppedBelowTop: another memory scored more than 1/KeepShare of it.
	DroppedBelowTop DropReason = "below 60% of top"
	// DroppedOverCap: the section was already carrying MaxScored memories.
	DroppedOverCap DropReason = "over the cap"
	// DroppedOverPinnedCap: the memory's scope already contributed
	// maxAlwaysPerScope pinned memories, so this one was never weighed
	// (gt-40p93).
	DroppedOverPinnedCap DropReason = "past the pinned cap"
	// DroppedNoBudget: the rendered memory did not fit the byte budget.
	DroppedNoBudget DropReason = "no room in the budget"
)

// Ranked is one pool memory's standing against a query.
type Ranked struct {
	// Memory is the memory that was scored.
	Memory Memory
	// Raw is the idf-weighted keyword score the floor is checked against.
	Raw float64
	// Score is Raw times the type weight plus recency: the score the section
	// ranks memories by.
	Score float64
	// Distinctive are the matched keywords that qualified the memory on their
	// own, each "key:<word>", "body:<word>" or "anchor:<path>", in word order.
	Distinctive []string
	// Text is the memory's rendered contribution, empty when the section does
	// not carry it.
	Text string
	// Carried reports whether the section carries the memory.
	Carried bool
	// Order is the memory's position in the section. It means nothing unless
	// Carried.
	Order int
	// Dropped is why the section does not carry the memory, empty when it does.
	Dropped DropReason
}

// Matched reports whether the bead matched the memory: it cleared the
// distinctive-hit test and the floor, or it is pinned. A matched memory the
// section does not carry — cut, capped or over the budget — is still a match,
// which is what the section's footer counts and --explain-memory explains. A
// pinned memory past its scope's cap was never weighed, so it is no match
// (gt-40p93).
func (r Ranked) Matched() bool {
	switch r.Dropped {
	case DroppedNoDistinctive, DroppedBelowFloor, DroppedOverPinnedCap:
		return false
	}
	return true
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
			Type:     KeyType(key),
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
	out := tokens(KeySlug(key))
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

// Corpus is the pool a query is scored against: how many memories it holds and
// in how many of them each keyword appears. Every idf weight comes from here,
// so a selection is measured against the whole rig corpus rather than memory by
// memory.
//
// The zero value is an empty pool, which weighs every keyword at zero.
type Corpus struct {
	// N is the number of memories in the pool.
	N int
	// df counts, per keyword, the memories whose key slug or body holds it.
	df map[string]int
}

// NewCorpus counts a pool's keywords. A keyword is counted once per memory,
// over the terms Score reads: the key's slug and the whole body.
func NewCorpus(mems []Memory) Corpus {
	c := Corpus{N: len(mems), df: make(map[string]int, len(mems))}
	for _, m := range mems {
		terms := tokens(KeySlug(m.Key))
		union(terms, tokens(m.Body))
		for w := range terms {
			c.df[w]++
		}
	}
	return c
}

// IDF is a keyword's inverse document frequency, ln(1 + N/df): a word every
// memory holds weighs ln 2, a word a single memory holds weighs ln(1+N), and a
// word no memory holds weighs zero.
func (c Corpus) IDF(w string) float64 {
	df := c.df[w]
	if df == 0 {
		return 0
	}
	return math.Log(1 + float64(c.N)/float64(df))
}

// Score returns m's idf-weighted raw score against q in the pool c and the
// score the section ranks by.
//
// raw is the candidacy test against CandidateFloor and is computed before any
// weighting. A query term contributes its idf twice in the key slug, once in
// the body when the key does not already hold it, and a file anchor the bead
// also names contributes 2*ln(1+N), the weight a word only this memory holds
// would earn.
func Score(q map[string]struct{}, beadPaths []string, m Memory, c Corpus, now time.Time) (raw, score float64) {
	a := c.assess(q, beadPaths, m, now)
	return a.raw, a.score
}

// assessment is a memory's fit to a query, before the section's ranking rules
// keep or drop it.
type assessment struct {
	raw   float64
	score float64
	// distinctive names the matched keywords that qualify the memory on their
	// own, each "key:<word>", "body:<word>" or "anchor:<path>".
	distinctive []string
}

// assess scores m against q in the pool c and reports the hits that earned it.
func (c Corpus) assess(q map[string]struct{}, beadPaths []string, m Memory, now time.Time) assessment {
	keyTerms, bodyTerms := tokens(KeySlug(m.Key)), tokens(m.Body)

	a := assessment{}
	for _, w := range sortedWords(q) {
		if _, ok := keyTerms[w]; ok {
			a.raw += 2 * c.IDF(w)
			if c.distinctive(w, true) {
				a.distinctive = append(a.distinctive, "key:"+w)
			}
			continue
		}
		if _, ok := bodyTerms[w]; ok {
			a.raw += c.IDF(w)
			if c.distinctive(w, false) {
				a.distinctive = append(a.distinctive, "body:"+w)
			}
		}
	}
	if path, ok := matchingAnchor(beadPaths, m.Anchors); ok {
		a.raw += 2 * math.Log(1+float64(c.N))
		// A file the memory and the bead both name is distinctive by
		// definition: it says where the work is, not what the town talks about.
		a.distinctive = append(a.distinctive, "anchor:"+path)
	}

	a.score = a.raw*weight(m.Type) + recency(m.Verified, now)
	return a
}

// candidate reports whether the memory cleared the distinctive-hit test and the
// floor. The scored half of the section is drawn from the memories that did.
func (c Corpus) candidate(a assessment) bool {
	return a.raw >= CandidateFloor && len(a.distinctive) > 0
}

// dropped is why a memory that is no candidate is out of the running.
func (a assessment) dropped() DropReason {
	if len(a.distinctive) == 0 {
		return DroppedNoDistinctive
	}
	return DroppedBelowFloor
}

// distinctive reports whether a matched keyword qualifies a memory on its own.
// In a pool of SmallPool or more memories that means the keyword appears in at
// most DistinctiveShare of them; below that the share says nothing, so only a
// key hit is distinctive (gt-bvnbr).
func (c Corpus) distinctive(word string, inKey bool) bool {
	if c.N < SmallPool {
		return inKey
	}
	return float64(c.df[word])/float64(c.N) <= DistinctiveShare
}

// sortedWords lists a query's terms in a stable order, so the hits a memory
// reports are the same ones on every run.
func sortedWords(q map[string]struct{}) []string {
	words := make([]string, 0, len(q))
	for w := range q {
		words = append(words, w)
	}
	sort.Strings(words)
	return words
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

// Rank scores every memory in the pool against a query and returns them all
// ordered by score, then newer verification, then key.
//
// Every pool memory comes back, not only the ones the section carries: each row
// says whether the section carries it, where it renders (Order), and why not
// when it does not, which is what --explain-memory reports. A non-positive
// budget means Budget.
//
// The section's rules, in the order it applies them (gt-bvnbr): a memory is a
// candidate when a distinctive keyword matched it and its raw score reached
// CandidateFloor; pinned memories (always and durable) come first whatever they
// scored, at most maxAlwaysPerScope per scope; of the candidates the section
// keeps those within KeepShare of the top score, at most MaxScored of them, and
// carries what the byte budget holds.
func Rank(q map[string]struct{}, beadPaths []string, mems []Memory, now time.Time, budget int) []Ranked {
	if budget <= 0 {
		budget = Budget
	}
	pool := NewCorpus(mems)

	rows := make([]Ranked, len(mems))
	var pinned, scored []int
	for i, m := range mems {
		a := pool.assess(q, beadPaths, m, now)
		rows[i] = Ranked{Memory: m, Raw: a.raw, Score: a.score, Distinctive: a.distinctive}
		switch {
		case m.Always && m.Durable:
			pinned = append(pinned, i)
		case pool.candidate(a):
			scored = append(scored, i)
		default:
			rows[i].Dropped = a.dropped()
		}
	}
	sortByScore(rows, pinned)
	sortByScore(rows, scored)

	// One memory rarely relevant is not worth the next three: the scored half
	// is cut to KeepShare of its top score and to MaxScored memories before the
	// budget sees it.
	if len(scored) > 0 {
		top := rows[scored[0]].Score
		keep := len(scored)
		for i, idx := range scored {
			if rows[idx].Score < KeepShare*top {
				keep = i
				break
			}
		}
		for _, idx := range scored[keep:] {
			rows[idx].Dropped = DroppedBelowTop
		}
		scored = scored[:keep]
	}
	if len(scored) > MaxScored {
		for _, idx := range scored[MaxScored:] {
			rows[idx].Dropped = DroppedOverCap
		}
		scored = scored[:MaxScored]
	}

	perScope := make(map[string]int)
	used, order := 0, 0
	fill := func(idxs []int, capped bool) {
		for _, i := range idxs {
			if capped && perScope[rows[i].Memory.Scope] >= maxAlwaysPerScope {
				rows[i].Dropped = DroppedOverPinnedCap
				continue
			}
			text := renderMemory(rows[i].Memory)
			if used+len(text) > budget {
				// A memory that does not fit is skipped whole, so a later,
				// shorter one can still take the room.
				rows[i].Dropped = DroppedNoBudget
				continue
			}
			perScope[rows[i].Memory.Scope]++
			used += len(text)
			rows[i].Text, rows[i].Carried, rows[i].Order = text, true, order
			order++
		}
	}
	fill(pinned, true)
	fill(scored, false)

	sort.Slice(rows, func(i, j int) bool { return lessRanked(rows[i], rows[j]) })
	return rows
}

// Select returns the memories to render, in the order they should appear: the
// rows Rank carries, in Order. A non-positive budget means Budget.
func Select(q map[string]struct{}, beadPaths []string, mems []Memory, now time.Time, budget int) []Selected {
	var carried []Ranked
	for _, r := range Rank(q, beadPaths, mems, now, budget) {
		if r.Carried {
			carried = append(carried, r)
		}
	}
	sort.Slice(carried, func(i, j int) bool { return carried[i].Order < carried[j].Order })

	out := make([]Selected, 0, len(carried))
	for _, r := range carried {
		out = append(out, Selected{Memory: r.Memory, Score: r.Score, Text: r.Text})
	}
	return out
}

// sortByScore orders row indices the way the section fills from them.
func sortByScore(rows []Ranked, idxs []int) {
	sort.Slice(idxs, func(i, j int) bool { return lessRanked(rows[idxs[i]], rows[idxs[j]]) })
}

// lessRanked is the one row order: score, then newer verification, then key, so
// a corpus renders the same section on every run.
func lessRanked(a, b Ranked) bool {
	if a.Score != b.Score {
		return a.Score > b.Score
	}
	if !a.Memory.Verified.Equal(b.Memory.Verified) {
		return a.Memory.Verified.After(b.Memory.Verified)
	}
	return a.Memory.Key < b.Memory.Key
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

// ParseKey splits a stored memory key into its type and slug. A gt.<type>.<slug>
// or memory.<type>.<slug> key names its type; anything else is a legacy untyped
// key: general, with the kv namespace stripped from its slug.
//
// It is the one resolver: the prime index (internal/cmd parseMemoryKey) parses
// through it, so a key the index shows under a type is typed for scoring too,
// and the type segment never doubles as one of the key's own words (gt-fg4rh).
func ParseKey(key string) (memType, slug string) {
	switch {
	case strings.HasPrefix(key, keyPrefix):
		key = strings.TrimPrefix(key, keyPrefix)
	case strings.HasPrefix(key, legacyKeyPrefix):
		key = strings.TrimPrefix(key, legacyKeyPrefix)
	}
	if t, rest, found := strings.Cut(key, "."); found && isType(t) {
		return t, rest
	}
	return "general", key
}

// KeyType is the memory type ParseKey reads from a key.
func KeyType(key string) string {
	t, _ := ParseKey(key)
	return t
}

// KeySlug is a memory key's own words: the kv namespace, and the type segment
// of a typed key, are not among them.
func KeySlug(key string) string {
	_, slug := ParseKey(key)
	return slug
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

// matchingAnchor returns the file m names that the bead names too. Only a
// file:<path> anchor counts: a symbol or bead anchor says where a memory came
// from, not what it is about.
func matchingAnchor(beadPaths, anchors []string) (string, bool) {
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
				return path, true
			}
		}
	}
	return "", false
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

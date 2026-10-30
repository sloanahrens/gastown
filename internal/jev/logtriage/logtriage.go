// Package logtriage is the advisory battery on the town daemon's log. A pass
// reads the lines appended since its cursor, keeps the warning and error
// lines, groups them by pattern key, asks Jev one question set about each
// group and appends one line per group to a log at .runtime/jev/logtriage.jsonl.
// It never raises an attention item, suppresses a line, pages anyone or
// changes the daemon: the log is the only thing it writes.
//
// The pass owns no I/O beyond the daemon log it reads, its cursor and its log.
// The caller supplies the Jev client, the daemon log path and the cap, so a
// test runs a pass with fixtures and an httptest Jev. Open wires the production
// version.
package logtriage

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/steveyegge/gastown/internal/jev"
	"github.com/steveyegge/gastown/internal/jev/battery"
)

const (
	// Caller names the pass's Jev calls in the usage ledger.
	Caller = "jev-logtriage"

	// MaxGroupsPerPass caps the groups one pass asks about, largest first.
	MaxGroupsPerPass = 50

	// MaxSamples caps the sample lines quoted in each group's state.
	MaxSamples = 3

	// maxSampleRunes caps one sample line quoted in the state.
	maxSampleRunes = 300

	// logTimeLayout is the daemon log's own timestamp prefix.
	logTimeLayout = "2006/01/02 15:04:05"
	logTimeWidth  = len(logTimeLayout)
)

// keywords are the words that make a daemon line one the pass reads, tried in
// this order. They match without regard to case, except warning, which
// matchedOn holds to the start of a message, to WARNING:, or to a count above
// zero.
var keywords = []string{"warning", "error", "failed", "refused", "timeout", "crash", "panic"}

// reCountBefore matches a whole count at the end of the text before a keyword.
// A count may carry thousands separators, so 1,000 is one count, and a count
// is not the tail of a longer number.
var reCountBefore = regexp.MustCompile(`(?:^|[^\w,])(\d{1,3}(?:,\d{3})+|\d+)\s+$`)

// Asker asks one multi-question Ask. *jev.Client satisfies it.
type Asker interface {
	Ask(ctx context.Context, state any, qs map[string]jev.Question) (*jev.Answers, error)
}

// Pass is one run of the triage battery, with every dependency injected.
type Pass struct {
	Asker Asker

	// DaemonLog is the daemon's log. A pass reads it from its cursor to the
	// last complete line, and a missing file is an empty log.
	DaemonLog string

	// LogPath is the log every group's answers append to.
	LogPath string
	// CursorPath remembers how far into DaemonLog a pass read.
	CursorPath string

	// Allows says whether the day's Jev spend is still under the cap. The pass
	// asks it before the first call, and before every call after.
	Allows func() (bool, error)

	// Sampled says whether a group is asked again to measure answer noise. Nil
	// means battery.Sampled.
	Sampled func(key string, version int) bool

	// Now is the clock. Nil means time.Now.
	Now func() time.Time
}

// Summary says what one pass did.
type Summary struct {
	// Matched is the number of warning or error lines the pass read.
	Matched int
	// Groups is the number of distinct pattern keys among them.
	Groups int
	// Asked is the number of groups that got a line with answers.
	Asked int
	// Unanswered is the number of groups that got a line with a reason and no
	// answers.
	Unanswered int
	// Repeated is the number of repeat asks whose answers the pass logged to
	// the repeat log.
	Repeated int
	// RepeatsSkipped is the number of repeat asks the pass did not log: those
	// whose call failed, and those the cap, the deadline or an error stopped.
	RepeatsSkipped int
	// Deferred is the number of groups the pass's deadline ended before it asked
	// about them. The next pass merges their lines into its own groups.
	Deferred int
	// Dropped is the number of groups the pass did not log: those past
	// MaxGroupsPerPass, and those the day's cap stopped it before. Their lines
	// are read once and not asked about again.
	Dropped int
	// CapReached is true when the day's spend was used up. A pass that finds
	// the cap spent before it starts reads nothing, and leaves its cursor where
	// it was.
	CapReached bool
}

// group is the lines one pattern key holds in a pass.
type group struct {
	key       string
	count     int
	first     time.Time
	last      time.Time
	samples   []string
	matchedOn string
}

// add counts one line into the group, keeping its time range and the first
// MaxSamples lines.
func (g *group) add(line string, at time.Time) {
	g.count++
	if !at.IsZero() {
		if g.first.IsZero() || at.Before(g.first) {
			g.first = at
		}
		if g.last.IsZero() || at.After(g.last) {
			g.last = at
		}
	}
	if len(g.samples) < MaxSamples {
		g.samples = append(g.samples, truncate(line, maxSampleRunes))
	}
}

// seedGroups starts a pass's groups from the ones the last pass carried over,
// so their lines read now add to the counts they already hold. A group carried
// from a cursor written before matched_on existed has none, so its keyword is
// read from its first sample, the line that kept it.
func seedGroups(pending []pendingGroup) map[string]*group {
	groups := make(map[string]*group, len(pending))
	for _, p := range pending {
		kw := p.MatchedOn
		if kw == "" && len(p.Samples) > 0 {
			kw = matchedOn(p.Samples[0])
		}
		groups[p.Key] = &group{key: p.Key, count: p.Count, first: p.First, last: p.Last, samples: p.Samples, matchedOn: kw}
	}
	return groups
}

// pendingFrom carries the groups a deadline ended before asking, nil when none.
func pendingFrom(groups []*group) []pendingGroup {
	var out []pendingGroup
	for _, g := range groups {
		out = append(out, pendingGroup{Key: g.key, Count: g.count, First: g.first, Last: g.last, Samples: g.samples, MatchedOn: g.matchedOn})
	}
	return out
}

// groupState is the state one group's question is asked about. Every value is
// data, quoted in JSON.
type groupState struct {
	Key     string   `json:"key"`
	Count   int      `json:"count"`
	First   string   `json:"first"`
	Last    string   `json:"last"`
	Samples []string `json:"samples"`
}

// Run does one pass. The lines are read, grouped and asked, and the answers
// collected are written with the cursor. When the context ends partway (the
// pass's budget runs out), the answers so far are still written, and the
// groups not yet asked are carried in the cursor, so the next pass asks them
// with the lines it reads then. Run returns ErrBusy when another pass holds
// the run lock.
func (p *Pass) Run(ctx context.Context) (Summary, error) {
	var sum Summary
	release, err := lockRun(p.CursorPath)
	if err != nil {
		return sum, err
	}
	defer release()

	allowed, err := p.Allows()
	if err != nil {
		return sum, fmt.Errorf("reading today's spend: %w", err)
	}
	if !allowed {
		sum.CapReached = true
		return sum, nil
	}

	var cur Cursor
	if _, err := battery.ReadState(p.CursorPath, &cur); err != nil {
		return sum, err
	}
	groups := seedGroups(cur.Pending)
	matched, end, err := p.scan(groups, cur)
	if err != nil {
		return sum, err
	}
	sum.Matched = matched
	sum.Groups = len(groups)

	ordered := orderGroups(groups)
	if len(ordered) > MaxGroupsPerPass {
		sum.Dropped += len(ordered) - MaxGroupsPerPass
		ordered = ordered[:MaxGroupsPerPass]
	}

	now := p.now()
	var lines []Line
	var deferred []*group
	var repeatsWanted []repeatAsk
	var rep repeats
	for i, g := range ordered {
		if ctx.Err() != nil {
			deferred = ordered[i:]
			break
		}
		allowed, err := p.Allows()
		if err != nil {
			return sum, fmt.Errorf("reading today's spend: %w", err)
		}
		if !allowed {
			sum.CapReached = true
			sum.Dropped += len(ordered) - i
			break
		}
		state := stateOf(g)
		line, err := p.ask(ctx, g, state, now)
		if err != nil {
			deferred = ordered[i:]
			break
		}
		if line.Reason != "" {
			sum.Unanswered++
		} else {
			sum.Asked++
		}
		lines = append(lines, line)
		if line.Reason == "" && p.sampled(g.key) {
			repeatsWanted = append(repeatsWanted, repeatAsk{key: g.key, state: state})
		}
	}
	sum.Deferred = len(deferred)

	for _, line := range lines {
		if err := battery.Append(p.LogPath, line); err != nil {
			return sum, fmt.Errorf("writing the triage log: %w", err)
		}
	}
	end.Pending = pendingFrom(deferred)
	if err := battery.WriteState(p.CursorPath, end); err != nil {
		return sum, err
	}

	// The repeats come last, once the pass's own lines and cursor are written,
	// so no repeat's answer or failure can change either. Every repeat is
	// billed, so this also keeps them from spending the pass's deadline on a
	// group it has not asked about yet.
	var repErr error
	for _, r := range repeatsWanted {
		if err := p.repeat(ctx, r, now, &rep, &sum); err != nil && repErr == nil {
			repErr = err
		}
	}
	return sum, repErr
}

// scan reads the daemon log from the cursor to its last complete line and adds
// the matching lines to groups, by pattern key, and returns how many it
// matched. The file's inode and the cursor's offset decide where the read
// starts: a rotated file, or one shorter than the offset, starts at its first
// line. A last line with no newline yet is not read, so the next pass reads it
// whole.
func (p *Pass) scan(groups map[string]*group, cur Cursor) (int, Cursor, error) {
	f, err := os.Open(p.DaemonLog)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, cur, nil
	}
	if err != nil {
		return 0, cur, fmt.Errorf("opening the daemon log: %w", err)
	}
	defer func() { _ = f.Close() }()

	fi, err := f.Stat()
	if err != nil {
		return 0, cur, fmt.Errorf("reading the daemon log: %w", err)
	}
	id := fileID(fi)
	var start int64
	if id == cur.Inode && cur.Offset <= fi.Size() {
		start = cur.Offset
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return 0, cur, fmt.Errorf("seeking the daemon log: %w", err)
	}

	br := bufio.NewReaderSize(f, 64*1024)
	pos := start
	matched := 0
	for {
		raw, err := br.ReadBytes('\n')
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return 0, cur, fmt.Errorf("reading the daemon log: %w", err)
		}
		pos += int64(len(raw))
		text := strings.TrimRight(string(raw), "\r\n")
		kw := matchedOn(text)
		if kw == "" {
			continue
		}
		matched++
		key := PatternKey(text)
		g := groups[key]
		if g == nil {
			g = &group{key: key, matchedOn: kw}
			groups[key] = g
		}
		g.add(text, parseLogTime(text))
	}
	return matched, Cursor{Inode: id, Offset: pos}, nil
}

// stateOf is the state one group's questions are asked about, the same for the
// original ask and every repeat.
func stateOf(g *group) groupState {
	return groupState{
		Key:     g.key,
		Count:   g.count,
		First:   formatTime(g.first),
		Last:    formatTime(g.last),
		Samples: g.samples,
	}
}

// ask asks one group's question set in one Jev call and builds its line. A
// call that got an unusable reply or no reply at all logs a reason and no
// answers; the lines are not read again. Only a done context returns an error,
// and the caller carries the group to the next pass.
func (p *Pass) ask(ctx context.Context, g *group, state groupState, now time.Time) (Line, error) {
	line := Line{
		TS:              now,
		Key:             g.key,
		Count:           g.count,
		First:           g.first,
		Last:            g.last,
		Samples:         g.samples,
		QuestionVersion: Version,
		MatchedOn:       g.matchedOn,
	}
	qs := Questions()
	got, err := p.Asker.Ask(ctx, state, qs)
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return Line{}, cerr
		}
		if errors.Is(err, jev.ErrBadResponse) {
			line.Reason = ReasonBadResponse
			explain(&line, err, state, qs)
		} else {
			line.Reason = ReasonAskFailed
		}
		return line, nil
	}
	line.Model = got.Model
	line.Answers = answersFrom(got)
	return line, nil
}

// repeats is what a pass's groups share about repeating: once the cap, the
// deadline or an error stops it, no later group repeats either.
type repeats struct {
	stopped bool
}

// sampled reports whether a group is asked again, through the injected seam
// when a test sets one.
func (p *Pass) sampled(key string) bool {
	if p.Sampled == nil {
		return battery.Sampled(key, Version)
	}
	return p.Sampled(key, Version)
}

// repeatAsk is one sampled group the pass repeats: the key its lines name and
// the state its first ask sent.
type repeatAsk struct {
	key   string
	state groupState
}

// repeat asks r.state battery.ReaskRepeats times through the same Asker,
// appending every answer to the repeat log. A repeat measures how far Jev's
// answers move when nothing changes, so a failed repeat is counted and skipped
// without a retry. The pass writes its lines and cursor before any repeat runs,
// so an error here ends the repeats for the rest of the pass and is returned for
// the caller to log, after everything the pass writes has landed.
func (p *Pass) repeat(ctx context.Context, r repeatAsk, now time.Time, rep *repeats, sum *Summary) error {
	qs := Questions()
	for n := 1; n <= battery.ReaskRepeats; n++ {
		remaining := battery.ReaskRepeats - n + 1
		if rep.stopped || ctx.Err() != nil {
			rep.stopped = true
			sum.RepeatsSkipped += remaining
			return nil
		}
		allowed, err := p.Allows()
		if err != nil {
			rep.stopped = true
			sum.RepeatsSkipped += remaining
			return fmt.Errorf("reading today's spend: %w", err)
		}
		if !allowed {
			rep.stopped = true
			sum.CapReached = true
			sum.RepeatsSkipped += remaining
			return nil
		}
		got, err := p.Asker.Ask(ctx, r.state, qs)
		if err != nil {
			sum.RepeatsSkipped++
			continue
		}
		line := battery.ReaskLine{
			TS:              now,
			Key:             r.key,
			Repeat:          n,
			QuestionVersion: Version,
			Model:           got.Model,
			Answers:         answersFrom(got),
		}
		if err := battery.AppendReask(p.LogPath, line); err != nil {
			rep.stopped = true
			sum.RepeatsSkipped += remaining
			return fmt.Errorf("writing the repeat log: %w", err)
		}
		sum.Repeated++
	}
	return nil
}

// explain fills a bad_response line with the check the reply failed, the
// question and number that check read, and the size of the request. It takes
// those from the typed error and the request alone, so the line never holds
// the error text or anything Jev wrote.
func explain(line *Line, err error, state groupState, qs map[string]jev.Question) {
	var bad *jev.BadResponse
	if errors.As(err, &bad) {
		line.Detail = bad.Check
		line.Question = bad.Question
		line.Value = bad.Value
	}
	line.RequestChars = requestChars(state, qs)
	line.SampleCount = len(state.Samples)
}

// requestChars counts the characters a call sends: the state's JSON and every
// question's instructions. groupState holds only strings and ints, so its JSON
// cannot fail to encode.
func requestChars(state groupState, qs map[string]jev.Question) int {
	body, _ := json.Marshal(state)
	n := utf8.RuneCount(body)
	for _, q := range qs {
		n += utf8.RuneCountInString(q.Instructions)
	}
	return n
}

func (p *Pass) now() time.Time {
	if p.Now == nil {
		return time.Now()
	}
	return p.Now()
}

// matchedOn returns the first keyword that keeps a daemon line, or "" when none
// does. A keyword preceded by a count keeps the line only when the count is
// above zero, so a routine summary of zero counts is dropped. warning keeps a
// line at the start of its message, as WARNING:, or after a count above zero,
// and nothing else.
func matchedOn(line string) string {
	msg := messageOf(line)
	lower := strings.ToLower(msg)
	for _, kw := range keywords {
		if kw == "warning" {
			if strings.HasPrefix(lower, kw) || strings.Contains(line, "WARNING:") || occurs(lower, kw, aboveZero) {
				return kw
			}
			continue
		}
		if occurs(lower, kw, notZero) {
			return kw
		}
	}
	return ""
}

// occurs reports whether keep accepts some occurrence of kw in lower, the
// lower-cased message. keep takes the count in front of the occurrence, or ""
// when none is.
func occurs(lower, kw string, keep func(count string) bool) bool {
	for from := 0; ; {
		i := strings.Index(lower[from:], kw)
		if i < 0 {
			return false
		}
		i += from
		if keep(countBefore(lower[:i])) {
			return true
		}
		from = i + len(kw)
	}
}

// countBefore is the count that ends s, with its thousands separators removed,
// or "" when s does not end in one.
func countBefore(s string) string {
	m := reCountBefore.FindStringSubmatch(s)
	if m == nil {
		return ""
	}
	return strings.ReplaceAll(m[1], ",", "")
}

// notZero keeps an occurrence with no count in front of it, or a count above zero.
func notZero(count string) bool {
	return count == "" || aboveZero(count)
}

// aboveZero keeps an occurrence with a count above zero in front of it.
func aboveZero(count string) bool {
	return strings.Trim(count, "0") != ""
}

// messageOf is the line after the daemon's timestamp prefix, or the whole line
// when it has none.
func messageOf(line string) string {
	if parseLogTime(line).IsZero() {
		return line
	}
	return strings.TrimSpace(line[logTimeWidth:])
}

// parseLogTime reads the daemon's timestamp prefix, zero when the line has none.
func parseLogTime(line string) time.Time {
	if len(line) < logTimeWidth {
		return time.Time{}
	}
	at, err := time.ParseInLocation(logTimeLayout, line[:logTimeWidth], time.Local)
	if err != nil {
		return time.Time{}
	}
	return at
}

// orderGroups returns the groups largest first, ties by key, so the cap keeps
// the same groups every time the same lines are read.
func orderGroups(groups map[string]*group) []*group {
	out := make([]*group, 0, len(groups))
	for _, g := range groups {
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].count != out[j].count {
			return out[i].count > out[j].count
		}
		return out[i].key < out[j].key
	})
	return out
}

// formatTime is t in RFC 3339, empty when t is unknown.
func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339)
}

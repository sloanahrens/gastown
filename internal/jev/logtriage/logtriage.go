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
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"sort"
	"strings"
	"time"

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

// keywords are the words that make a daemon line one the pass reads. They
// match without regard to case.
var keywords = []string{"warning", "error", "failed", "refused", "timeout", "crash", "panic"}

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
	key     string
	count   int
	first   time.Time
	last    time.Time
	samples []string
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

// groupState is the state one group's question is asked about. Every value is
// data, quoted in JSON.
type groupState struct {
	Key     string   `json:"key"`
	Count   int      `json:"count"`
	First   string   `json:"first"`
	Last    string   `json:"last"`
	Samples []string `json:"samples"`
}

// Run does one pass. The lines are read, grouped and asked in one go, and the
// log lines and the cursor are written only once every group has been asked,
// so a context canceled partway asks the same lines again next pass and
// writes nothing twice. Run returns ErrBusy when another pass holds the run
// lock.
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
	groups, end, err := p.scan(cur)
	if err != nil {
		return sum, err
	}
	for _, g := range groups {
		sum.Matched += g.count
	}
	sum.Groups = len(groups)

	ordered := orderGroups(groups)
	if len(ordered) > MaxGroupsPerPass {
		sum.Dropped += len(ordered) - MaxGroupsPerPass
		ordered = ordered[:MaxGroupsPerPass]
	}

	now := p.now()
	var lines []Line
	for i, g := range ordered {
		if err := ctx.Err(); err != nil {
			return sum, err
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
		line, err := p.ask(ctx, g, now)
		if err != nil {
			return sum, err
		}
		if line.Reason != "" {
			sum.Unanswered++
		} else {
			sum.Asked++
		}
		lines = append(lines, line)
	}

	for _, line := range lines {
		if err := battery.Append(p.LogPath, line); err != nil {
			return sum, fmt.Errorf("writing the triage log: %w", err)
		}
	}
	if err := battery.WriteState(p.CursorPath, end); err != nil {
		return sum, err
	}
	return sum, nil
}

// scan reads the daemon log from the cursor to its last complete line and
// groups the matching lines by pattern key. The file's inode and the cursor's
// offset decide where the read starts: a rotated file, or one shorter than the
// offset, starts at its first line. A last line with no newline yet is not
// read, so the next pass reads it whole.
func (p *Pass) scan(cur Cursor) (map[string]*group, Cursor, error) {
	groups := map[string]*group{}
	f, err := os.Open(p.DaemonLog)
	if errors.Is(err, fs.ErrNotExist) {
		return groups, cur, nil
	}
	if err != nil {
		return nil, cur, fmt.Errorf("opening the daemon log: %w", err)
	}
	defer func() { _ = f.Close() }()

	fi, err := f.Stat()
	if err != nil {
		return nil, cur, fmt.Errorf("reading the daemon log: %w", err)
	}
	id := fileID(fi)
	var start int64
	if id == cur.Inode && cur.Offset <= fi.Size() {
		start = cur.Offset
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return nil, cur, fmt.Errorf("seeking the daemon log: %w", err)
	}

	br := bufio.NewReaderSize(f, 64*1024)
	pos := start
	for {
		raw, err := br.ReadBytes('\n')
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, cur, fmt.Errorf("reading the daemon log: %w", err)
		}
		pos += int64(len(raw))
		text := strings.TrimRight(string(raw), "\r\n")
		if !matches(text) {
			continue
		}
		key := PatternKey(text)
		g := groups[key]
		if g == nil {
			g = &group{key: key}
			groups[key] = g
		}
		g.add(text, parseLogTime(text))
	}
	return groups, Cursor{Inode: id, Offset: pos}, nil
}

// ask asks one group's question set in one Jev call and builds its line. A
// call that got an unusable reply or no reply at all logs a reason and no
// answers; the lines are not read again. Only a canceled context returns an
// error, so the pass stops and nothing is written.
func (p *Pass) ask(ctx context.Context, g *group, now time.Time) (Line, error) {
	line := Line{
		TS:              now,
		Key:             g.key,
		Count:           g.count,
		First:           g.first,
		Last:            g.last,
		Samples:         g.samples,
		QuestionVersion: Version,
	}
	state := groupState{
		Key:     g.key,
		Count:   g.count,
		First:   formatTime(g.first),
		Last:    formatTime(g.last),
		Samples: g.samples,
	}
	got, err := p.Asker.Ask(ctx, state, Questions())
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return Line{}, cerr
		}
		if errors.Is(err, jev.ErrBadResponse) {
			line.Reason = ReasonBadResponse
		} else {
			line.Reason = ReasonAskFailed
		}
		return line, nil
	}
	line.Model = got.Model
	line.Answers = answersFrom(got)
	return line, nil
}

func (p *Pass) now() time.Time {
	if p.Now == nil {
		return time.Now()
	}
	return p.Now()
}

// matches reports whether a daemon line holds one of the keywords.
func matches(line string) bool {
	lower := strings.ToLower(line)
	for _, kw := range keywords {
		if strings.Contains(lower, kw) {
			return true
		}
	}
	return false
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

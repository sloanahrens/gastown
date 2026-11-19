package cmd

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/bdgateway"
)

// tailKindGateway tags the gateway's call log on the dashboard feed. It is not
// in tailKinds, so gt tail's --kind never offers it.
const tailKindGateway = "gateway"

// tailGatewayLimit is how many calls one poll asks for: the gateway's cap on a
// calls read, so a poll at the log's peak rate fits in one answer.
const tailGatewayLimit = 500

// tailGatewayReader reads the gateway's recent calls. GatewayPusher is the
// production reader; a test injects its own.
type tailGatewayReader interface {
	Calls(ctx context.Context, since time.Time, limit int) (bdgateway.CallsPage, error)
}

// tailGatewaySource shows the gateway's call log: a line for each write and
// each failed call, and one count of the successful reads a poll skipped. The
// cursor is the newest stamp returned, and the calls already returned at that
// stamp are kept so a call is never shown twice, even when a read serves its
// stamp again.
type tailGatewaySource struct {
	reader tailGatewayReader
	cursor time.Time
	// returned holds the keys of the calls shown at cursor.
	returned map[string]bool
	now      func() time.Time
	failed   tailOnce
	// unstamped reports a poll's calls with no readable time, once per text.
	unstamped tailOnce
	// dropped is the answer's Dropped count at the last poll that read one; it
	// is set only once droppedSeen is, so the first poll sets the baseline.
	dropped     uint64
	droppedSeen bool
}

// newTailGatewaySource starts the source at cutoff, so the first poll shows the
// calls stamped after it.
func newTailGatewaySource(reader tailGatewayReader, cutoff time.Time, now func() time.Time) *tailGatewaySource {
	return &tailGatewaySource{reader: reader, cursor: cutoff, now: now}
}

func (s *tailGatewaySource) line(at time.Time, format string, args ...any) tailLine {
	return tailLine{At: at, Rig: "town", Kind: tailKindGateway, Text: fmt.Sprintf(format, args...)}
}

// readError reports a read failure once per distinct text, as tailOnce does for
// the other sources, and leaves the cursor where it was.
func (s *tailGatewaySource) readError(now time.Time, err error) []tailLine {
	if s.failed.first(err.Error()) {
		return []tailLine{s.line(now, "cannot read the gateway's calls: %v", err)}
	}
	return nil
}

// gatewayCallKey identifies a call by everything the log recorded for it, so
// two calls that differ in any field are two keys.
func gatewayCallKey(e bdgateway.CallEntry) string {
	return fmt.Sprintf("%#v", e)
}

// gatewayCallText is the caller, the verb and the beads a call named, with the
// exit and the duration when the call failed. Flags, the error text and the
// request body are never printed.
func gatewayCallText(e bdgateway.CallEntry) string {
	words := append([]string{e.Caller, e.Verb}, e.Beads...)
	text := strings.Join(strings.Fields(strings.Join(words, " ")), " ")
	if e.ExitCode != 0 {
		text += fmt.Sprintf(" failed exit %d, %dms", e.ExitCode, e.DurationMS)
	}
	return text
}

// fresh is a call the poll has not shown, with its stamp and key.
type fresh struct {
	call bdgateway.CallEntry
	at   time.Time
	key  string
}

// Poll returns the lines for the calls stamped after the cursor and moves the
// cursor past them.
func (s *tailGatewaySource) Poll() []tailLine {
	now := s.now()
	page, err := s.reader.Calls(context.Background(), s.cursor, tailGatewayLimit)
	if err != nil {
		return s.readError(now, err)
	}
	s.failed.clear()

	var out []tailLine
	if s.droppedSeen && page.Dropped > s.dropped {
		out = append(out, s.line(now, "%d calls were not logged", page.Dropped-s.dropped))
	}
	s.dropped, s.droppedSeen = page.Dropped, true

	// A call with no readable time cannot be placed against the cursor. It is
	// skipped and counted, and the cursor moves past it, so it cannot pin the feed.
	var batch []fresh
	seen := map[string]bool{}
	unreadable := 0
	anyStamp := false
	var newestStamp time.Time
	for _, e := range page.Entries {
		at, err := time.Parse(time.RFC3339Nano, e.TS)
		if err != nil {
			unreadable++
			continue
		}
		if !anyStamp || at.After(newestStamp) {
			newestStamp = at
		}
		anyStamp = true
		key := gatewayCallKey(e)
		if at.Before(s.cursor) || (at.Equal(s.cursor) && s.returned[key]) || seen[key] {
			continue
		}
		seen[key] = true
		batch = append(batch, fresh{call: e, at: at, key: key})
	}
	slices.SortStableFunc(batch, func(a, b fresh) int { return a.at.Compare(b.at) })

	// The cursor moves to the newest readable stamp in the page. When the page
	// had calls and none had a readable time, it moves to this poll's clock. It
	// never moves back. At an unmoved cursor the calls already shown there stay
	// in the set; at a moved one the set is only the calls shown at the new stamp.
	target := s.cursor
	switch {
	case anyStamp && newestStamp.After(s.cursor):
		target = newestStamp
	case !anyStamp && unreadable > 0 && now.After(s.cursor):
		target = now
	}
	next := map[string]bool{}
	if target.Equal(s.cursor) {
		maps.Copy(next, s.returned)
	}
	for _, f := range batch {
		if f.at.Equal(target) {
			next[f.key] = true
		}
	}
	s.cursor = target
	s.returned = next

	// A successful read is counted, not shown. A failed call, read or write, is
	// a line of its own, so it is never counted as a read.
	reads := 0
	for _, f := range batch {
		if !f.call.Write && f.call.ExitCode == 0 {
			reads++
			continue
		}
		class := tailClassPlain
		if f.call.ExitCode != 0 {
			class = tailClassFailure
		}
		out = append(out, tailLine{At: f.at, Rig: "town", Kind: tailKindGateway, Class: class, Text: gatewayCallText(f.call)})
	}
	if reads > 0 {
		out = append(out, s.line(now, "%d gateway reads", reads))
	}
	// ReadCalls keeps the oldest calls after the cursor, so a full page leaves the
	// rest for the next poll: the cursor above is at the page's last call.
	if len(page.Entries) >= tailGatewayLimit {
		out = append(out, s.line(now, "more calls waiting"))
	}
	if unreadable > 0 {
		text := fmt.Sprintf("%d calls had an unreadable time and were skipped", unreadable)
		if s.unstamped.first(text) {
			out = append(out, s.line(now, "%s", text))
		}
	} else {
		s.unstamped.clear()
	}
	return out
}

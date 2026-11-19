package cmd

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/bdgateway"
)

// tailKindGateway tags the gateway's call log on the dashboard feed. It is not
// in tailKinds, so gt tail's --kind never offers it.
const tailKindGateway = "gateway"

// tailGatewayLimit is how many calls one poll asks for: the recent window a
// feed needs, under the gateway's cap on a calls read.
const tailGatewayLimit = 200

// tailGatewayReader reads the gateway's recent calls. GatewayPusher is the
// production reader; a test injects its own.
type tailGatewayReader interface {
	Calls(ctx context.Context, since time.Time, limit int) (bdgateway.CallsPage, error)
}

// tailGatewaySource shows the gateway's call log: a line for each write and
// each failed call, and one count of the calls a poll read. The cursor is the
// newest stamp returned, and the calls already returned at that stamp are kept
// so a call is never shown twice, even when a read serves its stamp again.
type tailGatewaySource struct {
	reader tailGatewayReader
	cursor time.Time
	// returned holds the keys of the calls shown at cursor.
	returned map[string]bool
	now      func() time.Time
	failed   tailOnce
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

func (s *tailGatewaySource) Poll() []tailLine {
	now := s.now()
	page, err := s.reader.Calls(context.Background(), s.cursor, tailGatewayLimit)
	if err != nil {
		return s.readError(now, err)
	}
	// A call stamp the cursor cannot be placed against is a read failure: the
	// feed must not skip it silently or show it twice.
	stamps := make([]time.Time, len(page.Entries))
	for i, e := range page.Entries {
		at, err := time.Parse(time.RFC3339Nano, e.TS)
		if err != nil {
			return s.readError(now, fmt.Errorf("call %d has no readable time: %w", i, err))
		}
		stamps[i] = at
	}
	s.failed.clear()

	var batch []fresh
	seen := map[string]bool{}
	for i, e := range page.Entries {
		key := gatewayCallKey(e)
		at := stamps[i]
		if at.Before(s.cursor) || (at.Equal(s.cursor) && s.returned[key]) || seen[key] {
			continue
		}
		seen[key] = true
		batch = append(batch, fresh{call: e, at: at, key: key})
	}
	if len(batch) == 0 {
		return nil
	}
	slices.SortStableFunc(batch, func(a, b fresh) int { return a.at.Compare(b.at) })

	// The cursor moves to the newest stamp this poll shows. At an unmoved
	// cursor the calls already shown there stay in the set; at a moved one the
	// set is only the calls this poll showed at the new stamp.
	newest := batch[len(batch)-1].at
	next := map[string]bool{}
	if newest.Equal(s.cursor) {
		for k := range s.returned {
			next[k] = true
		}
	}
	for _, f := range batch {
		if f.at.Equal(newest) {
			next[f.key] = true
		}
	}
	s.cursor = newest
	s.returned = next

	var out []tailLine
	reads := false
	for _, f := range batch {
		if !f.call.Write {
			reads = true
		}
		if !f.call.Write && f.call.ExitCode == 0 {
			continue
		}
		class := tailClassPlain
		if f.call.ExitCode != 0 {
			class = tailClassFailure
		}
		out = append(out, tailLine{At: f.at, Rig: "town", Kind: tailKindGateway, Class: class, Text: gatewayCallText(f.call)})
	}
	if reads {
		out = append(out, s.line(now, "%d gateway reads", len(batch)))
	}
	return out
}

package cmd

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/steveyegge/gastown/internal/bdgateway"
	"github.com/steveyegge/gastown/internal/dashboard"
)

// The gateway section of the Dolt panel: the load the bd gateway carries right
// now, and the ten-minute rows its counted columns fill, so a gateway that is
// slow or refusing shows on the dashboard instead of being dug out by hand. It
// polls GET /v1/stats through GatewayPusher.Stats with the URL and token file
// the dashboard already holds for the gateway feed.

const (
	// doltGatewaySectionKey is the section's key in the Dolt panel.
	doltGatewaySectionKey = "gateway"
	// doltGatewayTitle is the section's heading.
	doltGatewayTitle = "bd gateway load"
	// doltGatewaySpan is the width of one bucket, the same ten minutes the
	// dashboard's bucket counter counts in.
	doltGatewaySpan = 10 * time.Minute
	// doltGatewayShown is how many ten-minute buckets the section draws.
	doltGatewayShown = 6

	// The bucket keys are also the column headings, so one name cannot drift
	// from the other.
	doltGatewayRefusalsKey = "429 refusals"
)

func init() {
	registerDoltSection(doltGatewaySectionKey, newDoltGatewayReader(os.Getenv))
}

// doltGatewayStats reads the gateway's counters, cumulative since its start. A
// failure is the section's error, never a result with zero counters.
type doltGatewayStats func(ctx context.Context) (bdgateway.GatewayStats, error)

// newDoltGatewayReader is the section's registered read: the pusher the
// dashboard already reads the gateway with, built from BD_GATEWAY_URL and
// BD_GATEWAY_TOKEN_FILE, and the URL's host, which names a failed read without
// its token.
func newDoltGatewayReader(getenv func(string) string) doltSectionRead {
	pusher := bdGatewayPusherFromEnv(getenv)
	return newDoltGatewaySection(pusher.Stats, doltGatewayHost(pusher.URL)).read
}

// doltGatewayHost is the host and port a stats failure is reported under. The
// token lives in a file the URL never names, and the userinfo, query and path
// are dropped, so the host is all of the URL that may reach the page.
func doltGatewayHost(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Host
}

// doltGatewayCounter is one cumulative counter as the previous read left it. A
// counter an answer did not carry is unknown, not zero: seen is false, so a
// later answer starts a new baseline rather than counting the gap.
type doltGatewayCounter struct {
	seen bool
	prev uint64
}

// doltGatewaySeries is one counter of an answer and where its previous value is
// held. cur is nil when the answer did not carry the field.
type doltGatewaySeries struct {
	key  string
	cur  *uint64
	last *doltGatewayCounter
}

// doltGatewaySection polls the gateway's stats on each panel read and counts
// the deltas between reads into ten-minute buckets. The panel runs its reads on
// several goroutines, so a read holds the lock while it counts and draws.
type doltGatewaySection struct {
	stats doltGatewayStats
	host  string

	mu sync.Mutex
	// seq numbers the reads in the order they took their fetch, and applied is
	// the newest one whose answer was counted: a fetch that comes back after a
	// newer one was counted is older than the baseline and counts nothing.
	seq, applied uint64
	refusals     doltGatewayCounter
	buckets      dashboard.Buckets
	// restarted is whether the read now drawn saw a counter below its
	// previous value: the gateway began again, so that read counts nothing.
	restarted bool
}

func newDoltGatewaySection(stats doltGatewayStats, host string) *doltGatewaySection {
	return &doltGatewaySection{stats: stats, host: host}
}

// series is every counter the section counts, in column order. A counter the
// answer does not carry has a nil cur: its column is left out and its baseline
// cleared.
func (s *doltGatewaySection) series(stats bdgateway.GatewayStats) []doltGatewaySeries {
	refusals := doltGatewaySeries{key: doltGatewayRefusalsKey, last: &s.refusals}
	if stats.CapsPresent {
		refusals.cur = &stats.TooManyCalls
	}
	return []doltGatewaySeries{refusals}
}

// read polls the gateway, counts what each counter gained since the previous
// read into the bucket now falls in, and draws the last six buckets. A read the
// gateway does not answer fails the section: an empty table would read as a
// gateway carrying no load.
func (s *doltGatewaySection) read(ctx context.Context, now time.Time) (dashboard.DoltSection, error) {
	s.mu.Lock()
	s.seq++
	seq := s.seq
	s.mu.Unlock()
	// The fetch runs outside the lock so a slow gateway does not hold the panel's
	// other reads; the sequence taken above orders the answers when they come back.
	stats, err := s.stats(ctx)
	if err != nil {
		return dashboard.DoltSection{}, doltGatewayError(s.host, err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if seq < s.applied {
		// A newer read's answer was counted while this fetch was out. This answer
		// is older than the baseline: counting it would read as a restart and
		// rewind the baseline, so the next read would count the same refusals twice.
		return s.section(stats, now), nil
	}
	s.applied = seq
	series := s.series(stats)
	restarted := false
	for _, sr := range series {
		// A cumulative counter only goes down when the gateway it counts for
		// started again; subtracting then would underflow into a huge count.
		if sr.cur != nil && sr.last.seen && *sr.cur < sr.last.prev {
			restarted = true
		}
	}
	s.restarted = restarted
	// Buckets drops a count dated more than a minute after the latest time it
	// has been shown, and only a Series call moves that time to the read: a read
	// ten minutes after the previous one would otherwise lose its deltas, so
	// this read's time is shown to the buckets first.
	s.buckets.Series(now, "")
	for _, sr := range series {
		if sr.cur == nil {
			sr.last.seen = false
			continue
		}
		// The first read counts nothing: the counters began before the dashboard
		// did, so they are a baseline rather than a delta. A read that saw a
		// restart counts nothing either, since its counts span the restart.
		if sr.last.seen && !restarted {
			s.buckets.Add(now, sr.key, int(*sr.cur-sr.last.prev))
		}
		sr.last.seen, sr.last.prev = true, *sr.cur
	}
	return s.section(stats, now), nil
}

// section draws the last doltGatewayShown ten-minute buckets, oldest first, with
// the bucket holding now last. The head carries a column per counter the answer
// carried.
func (s *doltGatewaySection) section(stats bdgateway.GatewayStats, now time.Time) dashboard.DoltSection {
	out := dashboard.DoltSection{Title: doltGatewayTitle, Head: []string{"ten minutes"}}
	series := s.series(stats)
	var shown [][]int
	for _, sr := range series {
		if sr.cur == nil {
			continue
		}
		out.Head = append(out.Head, sr.key)
		buckets := s.buckets.Series(now, sr.key)
		shown = append(shown, buckets[:])
	}
	// The counter's buckets are the ten minutes counted from the Unix epoch,
	// and the epoch falls on one of their boundaries, so Truncate gives the
	// window the counter counted in.
	end := now.Truncate(doltGatewaySpan).Add(doltGatewaySpan)
	for i := doltGatewayShown; i > 0; i-- {
		start := end.Add(-time.Duration(i) * doltGatewaySpan)
		row := []string{start.Format("15:04") + "-" + start.Add(doltGatewaySpan).Format("15:04")}
		for _, vals := range shown {
			row = append(row, strconv.Itoa(vals[len(vals)-i]))
		}
		out.Rows = append(out.Rows, row)
	}
	out.Note = doltGatewayNote(stats, s.restarted)
	return out
}

// doltGatewayNote is the section's explanation: the load right now, when the
// counts began, and each reason a column or a read is missing.
func doltGatewayNote(stats bdgateway.GatewayStats, restarted bool) string {
	var note strings.Builder
	if stats.CapsPresent {
		fmt.Fprintf(&note, "in flight now: %d; peak in flight since start: %d", stats.InFlight, stats.PeakInFlight)
	} else {
		note.WriteString("the gateway's stats carry no in-flight or 429 counters")
	}
	note.WriteString(". Counts per ten minutes start when the dashboard starts")
	if restarted {
		note.WriteString("; the gateway restarted since the previous read, so that read added nothing")
	}
	note.WriteString(".")
	return note.String()
}

// doltGatewayURL matches an http or https URL inside an error message, up to
// the next space or quote.
var doltGatewayURL = regexp.MustCompile(`https?://[^\s"'<>)]+`)

// doltGatewayError is a failed stats read, naming the gateway's host and the
// cause. The pusher's errors and a transport's repeat the request URL, which can
// carry credentials, a path and a query, so every http or https URL in the cause
// is replaced by the host: the host is the only part of the endpoint that may
// reach the page. The bearer token is read from a file the pusher never prints,
// so it is not in the cause. The replacement covers a URL that is spelled out in
// the text, not other forms of the same endpoint.
func doltGatewayError(host string, err error) error {
	msg := err.Error()
	if host == "" {
		return fmt.Errorf("reading the gateway's stats: %s", doltGatewayURL.ReplaceAllString(msg, "the gateway"))
	}
	return fmt.Errorf("reading the gateway's stats at %s: %s", host, doltGatewayURL.ReplaceAllString(msg, host))
}

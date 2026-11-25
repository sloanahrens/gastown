package cmd

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"
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
	// doltGatewaySpan is the width of one row, the same ten minutes the
	// dashboard's bucket counter counts in.
	doltGatewaySpan = 10 * time.Minute
	// doltGatewayShown is how many ten-minute rows the section draws.
	doltGatewayShown = 6
)

func init() {
	registerDoltSection(doltGatewaySectionKey, newDoltGatewayReader(os.Getenv))
}

// doltGatewayStats reads the gateway's counters. A failure is the section's
// error, never a result with zero counters.
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

// doltGatewaySection polls the gateway's stats on each panel read and draws the
// last six ten-minute rows with the load now in its note.
type doltGatewaySection struct {
	stats doltGatewayStats
	host  string
}

func newDoltGatewaySection(stats doltGatewayStats, host string) *doltGatewaySection {
	return &doltGatewaySection{stats: stats, host: host}
}

// read polls the gateway and draws the last six ten-minute rows. A read the
// gateway does not answer fails the section: an empty table would read as a
// gateway carrying no load.
func (s *doltGatewaySection) read(ctx context.Context, now time.Time) (dashboard.DoltSection, error) {
	stats, err := s.stats(ctx)
	if err != nil {
		return dashboard.DoltSection{}, doltGatewayError(s.host, err)
	}
	return s.section(stats, now), nil
}

// section draws the last doltGatewayShown ten-minute rows, oldest first, with
// the row holding now last.
func (s *doltGatewaySection) section(stats bdgateway.GatewayStats, now time.Time) dashboard.DoltSection {
	out := dashboard.DoltSection{Title: doltGatewayTitle, Head: []string{"ten minutes"}}
	// The counter's buckets are the ten minutes counted from the Unix epoch,
	// and the epoch falls on one of their boundaries, so Truncate gives the
	// window the counter counted in.
	end := now.Truncate(doltGatewaySpan).Add(doltGatewaySpan)
	for i := doltGatewayShown; i > 0; i-- {
		start := end.Add(-time.Duration(i) * doltGatewaySpan)
		out.Rows = append(out.Rows, []string{start.Format("15:04") + "-" + start.Add(doltGatewaySpan).Format("15:04")})
	}
	out.Note = doltGatewayNote(stats)
	return out
}

// doltGatewayNote is the section's explanation: the load right now and when the
// counts began.
func doltGatewayNote(stats bdgateway.GatewayStats) string {
	var note strings.Builder
	if stats.CapsPresent {
		fmt.Fprintf(&note, "in flight now: %d; peak in flight since start: %d", stats.InFlight, stats.PeakInFlight)
	} else {
		note.WriteString("the gateway's stats carry no in-flight or 429 counters")
	}
	note.WriteString(". Counts per ten minutes start when the dashboard starts.")
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

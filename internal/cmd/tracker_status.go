package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/backend"
	"github.com/steveyegge/gastown/internal/workspace"
	"github.com/steveyegge/gastown/tracker/api"
	"github.com/steveyegge/gastown/tracker/client"
)

// trackerStatusTimeout bounds each call the screen makes. A status screen that
// waits on a dead tracker says nothing, so every piece gets this long at most.
const trackerStatusTimeout = 5 * time.Second

var trackerStatusJSON bool

var trackerStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show the town tracker's health on one screen",
	Long: `Show the town tracker's health on one screen: its URL, whether it is ready and
its schema level, the issue and wisp counts of each rig routed to it, and the
request count and slowest route from its /metrics.

The exit code follows readiness: 0 when the tracker answers ready, 1 when it
does not, with the reason on stderr. A tracker that cannot be reached gives
nothing to show and prints no screen; one that answers not ready still prints
its screen and then fails. A piece the tracker does not give (a missing stats or
metrics route, a failed count) prints as unavailable and never fails the command.

Examples:
  gt tracker status
  gt tracker status --json`,
	Args:         cobra.NoArgs,
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return runTrackerStatus(liveStatusEnv(), cmd.OutOrStdout(), trackerStatusJSON)
	},
}

func init() {
	trackerStatusCmd.Flags().BoolVar(&trackerStatusJSON, "json", false, "Print the status as JSON")
	trackerCmd.AddCommand(trackerStatusCmd)
}

// trackerStatusSource is the set of tracker reads the screen makes. The client
// satisfies it; the seam in statusEnv lets a caller substitute another source.
type trackerStatusSource interface {
	Readyz(ctx context.Context) (client.ReadyzStatus, error)
	Stats(ctx context.Context, rig string) (api.Stats, error)
	ListAll(ctx context.Context, f api.ListFilter) ([]api.Issue, error)
	Metrics(ctx context.Context) (string, error)
}

// statusEnv is what the screen reads from outside the process: the town and
// its route table, the environment, and the tracker. Each is a field so the
// refusals are unit tests.
type statusEnv struct {
	findTown   func() (string, error)
	loadRoutes func(beadsDir string) ([]beads.Route, error)
	getenv     func(string) string
	newSource  func(cfg client.Config) (trackerStatusSource, error)
}

// liveStatusEnv is the production environment.
func liveStatusEnv() statusEnv {
	return statusEnv{
		findTown:   workspace.FindFromCwdOrError,
		loadRoutes: beads.LoadRoutes,
		getenv:     os.Getenv,
		newSource:  newTrackerStatusSource,
	}
}

func newTrackerStatusSource(cfg client.Config) (trackerStatusSource, error) {
	tc, err := client.New(cfg)
	if err != nil {
		return nil, err
	}
	return tc, nil
}

// trackerStatus is the screen's content. The text and the JSON print this one
// value, so the two carry the same fields. A piece that did not answer is left
// nil and its reason says why.
type trackerStatus struct {
	URL         string             `json:"url"`
	Ready       bool               `json:"ready"`
	SchemaLevel int                `json:"schema_level"`
	Rigs        []trackerRigStatus `json:"rigs"`
	// RigsUnavailable is the reason the rig list could not be read.
	RigsUnavailable string `json:"rigs_unavailable,omitempty"`
	// Requests is nil when /metrics did not answer; RequestsUnavailable says why.
	Requests            *trackerRequests `json:"requests,omitempty"`
	RequestsUnavailable string           `json:"requests_unavailable,omitempty"`
}

// trackerRigStatus is one rig routed to the tracker. Issues counts durable
// issues, closed ones included; Wisps counts the rig's wisps, which the stats
// route leaves out.
type trackerRigStatus struct {
	Rig               string         `json:"rig"`
	Issues            *int           `json:"issues,omitempty"`
	ByStatus          map[string]int `json:"by_status,omitempty"`
	IssuesUnavailable string         `json:"issues_unavailable,omitempty"`
	Wisps             *int           `json:"wisps,omitempty"`
	WispsUnavailable  string         `json:"wisps_unavailable,omitempty"`
}

// trackerRequests is what /metrics says about the requests the server has
// served since it started: a count, and the slowest route by its histogram.
// The counts are totals, not a rate: one scrape carries no interval to divide by.
type trackerRequests struct {
	Total   uint64          `json:"total"`
	Slowest *trackerSlowest `json:"slowest,omitempty"`
}

// trackerSlowest is the route whose slowest observation is the largest. The
// histogram keeps buckets, not samples, so Seconds is the upper bound of the
// smallest bucket that holds every observation of that route. Over means some
// observation exceeded the largest bucket, and Seconds is then that bucket.
// Seconds is nil when the histogram has no finite bucket at all: nothing then
// bounds the observations, so the screen says the time is unknown rather than
// naming a bound of zero.
type trackerSlowest struct {
	Route   string   `json:"route"`
	Seconds *float64 `json:"seconds,omitempty"`
	Over    bool     `json:"over_largest_bucket,omitempty"`
}

// runTrackerStatus reads the tracker and prints the screen. A tracker that
// cannot be reached fails the run before there is anything to show. One that
// answers not ready still gets its screen printed, and the run fails after it,
// so the exit code follows readiness in both cases.
func runTrackerStatus(env statusEnv, out io.Writer, asJSON bool) error {
	cfg := backend.FromEnv(env.getenv).Tracker
	if cfg.BaseURL == "" {
		return errors.New("gt tracker status: TRACKER_URL is not set, so the tracker cannot be reached")
	}
	// Each call is one attempt: the screen states what answers now, and a
	// retry would only make a dead tracker look slow.
	cfg.Timeout = trackerStatusTimeout
	cfg.MaxRetries = -1
	endpoint := statusEndpoint(cfg.BaseURL)
	src, err := env.newSource(cfg)
	if err != nil {
		return fmt.Errorf("gt tracker status: the tracker at %s cannot be reached: %w", terminalSafe(endpoint), err)
	}

	ready, err := withinStatus(src.Readyz)
	if err != nil {
		return fmt.Errorf("gt tracker status: the tracker at %s does not answer ready: %w", terminalSafe(endpoint), err)
	}
	rep := trackerStatus{
		URL:         endpoint,
		Ready:       ready.Status == "ready",
		SchemaLevel: ready.SchemaLevel,
		Rigs:        []trackerRigStatus{},
	}

	rigs, err := trackerRigNames(env)
	if err != nil {
		rep.RigsUnavailable = err.Error()
	}
	for _, rig := range rigs {
		rep.Rigs = append(rep.Rigs, trackerRigCounts(src, rig))
	}

	page, err := withinStatus(src.Metrics)
	if err == nil {
		var req trackerRequests
		if req, err = parseTrackerMetrics(page); err == nil {
			rep.Requests = &req
		}
	}
	if err != nil {
		rep.RequestsUnavailable = unavailableReason(err)
	}

	if asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rep); err != nil {
			return fmt.Errorf("gt tracker status: write: %w", err)
		}
	} else {
		writeTrackerStatusText(out, rep)
	}

	// A server that answered with a status other than ready has had its screen
	// printed; the run still fails, so the exit code follows readiness.
	if !rep.Ready {
		return fmt.Errorf("gt tracker status: the tracker at %s answers %q, not ready", terminalSafe(rep.URL), statusWord(ready.Status))
	}
	return nil
}

// withinStatus runs one tracker call under the screen's per-call bound.
func withinStatus[T any](call func(ctx context.Context) (T, error)) (T, error) {
	ctx, cancel := context.WithTimeout(context.Background(), trackerStatusTimeout)
	defer cancel()
	return call(ctx)
}

// trackerRigCounts reads one rig's issue and wisp counts. A count that did not
// answer is marked unavailable and the other count is still read.
func trackerRigCounts(src trackerStatusSource, rig string) trackerRigStatus {
	rs := trackerRigStatus{Rig: rig}
	st, err := withinStatus(func(ctx context.Context) (api.Stats, error) {
		return src.Stats(ctx, rig)
	})
	if err != nil {
		rs.IssuesUnavailable = unavailableReason(err)
	} else {
		n := st.Total
		rs.Issues = &n
		rs.ByStatus = st.ByStatus
	}
	wisps, err := withinStatus(func(ctx context.Context) ([]api.Issue, error) {
		return src.ListAll(ctx, api.ListFilter{Rig: rig, Ephemeral: true, IncludeInfra: true})
	})
	if err != nil {
		rs.WispsUnavailable = unavailableReason(err)
	} else {
		n := len(wisps)
		rs.Wisps = &n
	}
	return rs
}

// unavailableReason is a failed read's text for a line that already says
// "unavailable": the client puts its kind in front of every error, so an
// unavailable kind would otherwise stutter. Any other kind is kept, since it
// tells the operator something the label does not.
func unavailableReason(err error) string {
	var e *api.Error
	if errors.As(err, &e) && e.Kind == api.KindUnavailable {
		return e.Message
	}
	return err.Error()
}

// trackerRigNames is the rig names routed to the tracker. A tracker rig is
// named by its beads prefix, as the backend names it when it opens the rig.
func trackerRigNames(env statusEnv) ([]string, error) {
	townRoot, err := env.findTown()
	if err != nil {
		return nil, fmt.Errorf("not in a Gas Town workspace: %w", err)
	}
	routes, err := env.loadRoutes(filepath.Join(townRoot, ".beads"))
	if err != nil {
		return nil, err
	}
	var rigs []string
	for _, r := range routes {
		if r.BackendName() == beads.BackendTracker {
			rigs = append(rigs, strings.TrimSuffix(r.Prefix, "-"))
		}
	}
	return rigs, nil
}

// statusEndpoint is the configured tracker address with any credentials
// stripped, so the screen can name the endpoint without carrying them.
func statusEndpoint(baseURL string) string {
	u, err := url.Parse(baseURL)
	if err != nil {
		return "(unparsable tracker URL)"
	}
	u.User = nil
	return u.String()
}

// writeTrackerStatusText prints the screen. Each line is one fact, so the
// whole status fits on one screen. Every string the tracker chose is escaped
// on the way out, so none of them can move the cursor or forge a line.
func writeTrackerStatusText(w io.Writer, s trackerStatus) {
	fmt.Fprintf(w, "%-11s%s\n", "Tracker", terminalSafe(s.URL))
	ready := "no"
	if s.Ready {
		ready = fmt.Sprintf("yes (schema level %d)", s.SchemaLevel)
	}
	fmt.Fprintf(w, "%-11s%s\n", "Ready", ready)

	switch {
	case s.RigsUnavailable != "":
		fmt.Fprintf(w, "%-11s%s\n", "Rigs", "unavailable: "+terminalSafe(s.RigsUnavailable))
	case len(s.Rigs) == 0:
		fmt.Fprintf(w, "%-11s%s\n", "Rigs", "none routed to the tracker")
	default:
		fmt.Fprintln(w, "Rigs")
		names := make([]string, len(s.Rigs))
		issues := make([]string, len(s.Rigs))
		wisps := make([]string, len(s.Rigs))
		nameW, issuesW := 0, 0
		for i, r := range s.Rigs {
			names[i] = terminalSafe(r.Rig)
			issues[i] = terminalSafe(trackerIssuesText(r))
			wisps[i] = terminalSafe(trackerWispsText(r))
			nameW = max(nameW, len(names[i]))
			issuesW = max(issuesW, len(issues[i]))
		}
		for i := range names {
			fmt.Fprintf(w, "  %-*s  %-*s  %s\n", nameW, names[i], issuesW, issues[i], wisps[i])
		}
	}

	if s.Requests == nil {
		fmt.Fprintf(w, "%-11s%s\n", "Requests", "unavailable: "+terminalSafe(s.RequestsUnavailable))
		fmt.Fprintf(w, "%-11s%s\n", "Slowest", "unavailable")
		return
	}
	fmt.Fprintf(w, "%-11s%d since start\n", "Requests", s.Requests.Total)
	fmt.Fprintf(w, "%-11s%s\n", "Slowest", trackerSlowestText(s.Requests.Slowest))
}

// statusWord is the server's readiness word as a failure line carries it:
// escaped for the terminal and cut to a line's worth, since the server chose it
// and the reason has to stay one line.
func statusWord(s string) string {
	const maxRunes = 64
	s = terminalSafe(s)
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	return string(runes[:maxRunes]) + "..."
}

// terminalSafe returns s with every non-printable rune escaped, so a string the
// tracker chose (a rig name, a route, a status name, an error it sent) cannot
// recolour the screen or break its one-line-per-fact shape. Printable text
// comes through unchanged.
func terminalSafe(s string) string {
	clean := true
	for _, r := range s {
		if !strconv.IsPrint(r) {
			clean = false
			break
		}
	}
	if clean {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 8)
	for _, r := range s {
		switch {
		case strconv.IsPrint(r):
			b.WriteRune(r)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r <= 0xFF:
			fmt.Fprintf(&b, `\x%02x`, r)
		case r <= 0xFFFF:
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			fmt.Fprintf(&b, `\U%08x`, r)
		}
	}
	return b.String()
}

// trackerIssuesText is a rig's issue count with its status breakdown.
func trackerIssuesText(r trackerRigStatus) string {
	if r.Issues == nil {
		return "issues unavailable: " + r.IssuesUnavailable
	}
	statuses := make([]string, 0, len(r.ByStatus))
	for status := range r.ByStatus {
		statuses = append(statuses, status)
	}
	sort.Strings(statuses)
	parts := make([]string, 0, len(statuses))
	for _, status := range statuses {
		parts = append(parts, fmt.Sprintf("%s %d", status, r.ByStatus[status]))
	}
	if len(parts) == 0 {
		return fmt.Sprintf("issues %d", *r.Issues)
	}
	return fmt.Sprintf("issues %d (%s)", *r.Issues, strings.Join(parts, ", "))
}

func trackerWispsText(r trackerRigStatus) string {
	if r.Wisps == nil {
		return "wisps unavailable: " + r.WispsUnavailable
	}
	return fmt.Sprintf("wisps %d", *r.Wisps)
}

// trackerSlowestText names the slowest route and the bound that holds its
// observations. A route whose histogram has no finite bucket has no bound, so
// it says unknown rather than a time of zero.
func trackerSlowestText(s *trackerSlowest) string {
	if s == nil {
		return "none yet"
	}
	route := terminalSafe(s.Route)
	if s.Seconds == nil {
		return "unknown on " + route + " (no finite bucket)"
	}
	secs := strconv.FormatFloat(*s.Seconds, 'g', -1, 64)
	if s.Over {
		return fmt.Sprintf("over %s s on %s", secs, route)
	}
	return fmt.Sprintf("at most %s s on %s", secs, route)
}

// The metric lines the screen reads. Label values are Go-quoted by the server,
// so a route's quoted form is decoded with strconv.Unquote.
var (
	trackerRequestsLine = regexp.MustCompile(`^tracker_requests_total\{route=("(?:[^"\\]|\\.)*"),status="(?:[^"\\]|\\.)*"\} (\d+)$`)
	trackerBucketLine   = regexp.MustCompile(`^tracker_request_seconds_bucket\{route=("(?:[^"\\]|\\.)*"),le="([^"]*)"\} (\d+)$`)
)

// trackerRouteBuckets is one route's latency histogram as /metrics renders it:
// the cumulative count at each finite bound, and the count at +Inf.
type trackerRouteBuckets struct {
	bounds []float64
	cum    []uint64
	count  uint64
	hasInf bool
}

// parseTrackerMetrics reads the request total and the slowest route from the
// /metrics page. A page that is not the tracker's exposition is an error, not
// zero requests: a missing family would otherwise read as a quiet server.
func parseTrackerMetrics(page string) (trackerRequests, error) {
	if !strings.Contains(page, "# TYPE tracker_requests_total counter") {
		return trackerRequests{}, errors.New("the metrics page has no tracker_requests_total family")
	}
	var total uint64
	routes := map[string]*trackerRouteBuckets{}
	for _, line := range strings.Split(page, "\n") {
		switch {
		case strings.HasPrefix(line, "tracker_requests_total{"):
			m := trackerRequestsLine.FindStringSubmatch(line)
			if m == nil {
				return trackerRequests{}, errors.New("the metrics page has an unreadable tracker_requests_total line")
			}
			n, err := strconv.ParseUint(m[2], 10, 64)
			if err != nil {
				return trackerRequests{}, fmt.Errorf("the metrics page has an unreadable request count: %w", err)
			}
			total += n
		case strings.HasPrefix(line, "tracker_request_seconds_bucket{"):
			m := trackerBucketLine.FindStringSubmatch(line)
			if m == nil {
				return trackerRequests{}, errors.New("the metrics page has an unreadable tracker_request_seconds_bucket line")
			}
			route, err := strconv.Unquote(m[1])
			if err != nil {
				return trackerRequests{}, fmt.Errorf("the metrics page has an unreadable route label: %w", err)
			}
			cum, err := strconv.ParseUint(m[3], 10, 64)
			if err != nil {
				return trackerRequests{}, fmt.Errorf("the metrics page has an unreadable bucket count: %w", err)
			}
			h := routes[route]
			if h == nil {
				h = &trackerRouteBuckets{}
				routes[route] = h
			}
			if m[2] == "+Inf" {
				h.count, h.hasInf = cum, true
				continue
			}
			le, err := strconv.ParseFloat(m[2], 64)
			if err != nil {
				return trackerRequests{}, fmt.Errorf("the metrics page has an unreadable bucket bound: %w", err)
			}
			if math.IsNaN(le) || math.IsInf(le, 0) {
				return trackerRequests{}, errors.New("the metrics page has a non-finite bucket bound")
			}
			h.bounds = append(h.bounds, le)
			h.cum = append(h.cum, cum)
		}
	}
	for route, h := range routes {
		if !h.hasInf {
			return trackerRequests{}, fmt.Errorf("the latency histogram for %s has no +Inf bucket", route)
		}
	}
	return trackerRequests{Total: total, Slowest: trackerSlowestRoute(routes)}, nil
}

// trackerSlowestRoute picks the route with the slowest observation: one over
// the largest bucket beats any within it, a larger bound beats a smaller one,
// and a tie goes to the route name that sorts first, so the answer is stable.
func trackerSlowestRoute(routes map[string]*trackerRouteBuckets) *trackerSlowest {
	var best *trackerSlowest
	for route, h := range routes {
		if h.count == 0 {
			continue
		}
		cand := trackerRouteSlowest(route, h)
		if best == nil || slower(cand, best) {
			best = cand
		}
	}
	return best
}

// trackerRouteSlowest is the smallest bound that holds all of a route's
// observations, or the largest bound with Over set when some exceed it. A
// histogram with no finite bucket has no bound to report: its answer carries a
// nil Seconds.
func trackerRouteSlowest(route string, h *trackerRouteBuckets) *trackerSlowest {
	var largest float64
	finite, found := false, false
	var bound float64
	for i, le := range h.bounds {
		finite = true
		largest = max(largest, le)
		if h.cum[i] == h.count && (!found || le < bound) {
			bound, found = le, true
		}
	}
	if !found {
		if !finite {
			return &trackerSlowest{Route: route}
		}
		return &trackerSlowest{Route: route, Seconds: &largest, Over: true}
	}
	return &trackerSlowest{Route: route, Seconds: &bound}
}

// slower reports whether a is a slower answer than b, with a route name as the
// last tie-breaker. A route with no finite bucket outranks any bounded one: its
// observations are past every bucket it declares.
func slower(a, b *trackerSlowest) bool {
	if (a.Seconds == nil) != (b.Seconds == nil) {
		return a.Seconds == nil
	}
	if a.Over != b.Over {
		return a.Over
	}
	if a.Seconds != nil && *a.Seconds != *b.Seconds {
		return *a.Seconds > *b.Seconds
	}
	return a.Route < b.Route
}

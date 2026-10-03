package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/dashboard"
	"github.com/steveyegge/gastown/internal/doltserver"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/townstatus"
	"github.com/steveyegge/gastown/internal/workspace"
)

var (
	dashboardPort     int
	dashboardBind     string
	dashboardSince    string
	dashboardSpendCmd string
	dashboardOpen     bool
)

var dashboardCmd = &cobra.Command{
	Use:     "dashboard",
	GroupID: GroupDiag,
	Short:   "Serve a read-only localhost page that shows what gt tail -f shows",
	Long: `Serve one page on localhost that shows the town the way gt tail -f does:
the health verdict, the queue waiting to land, the polecat seats and what they
hold, the machine's load and what burns it, the DeepSeek spend, and the same
default-view feed gt tail prints.

It reads what gt tail reads — the events journals, the landings files, the
daemon log, the watch feed, the daemon's health report — through the same
sources and the same filter. It forks no bd, tmux, gh or git child per page.
One hub polls on its own clock and pushes to every open page, and when no page
is open it polls nothing.

The page is read-only. It binds loopback only and refuses any request whose
Host is not a loopback name.

--spend-cmd names a command that prints the DeepSeek spend report as JSON
(default: ~/.claude/tools/deepseek-spend.py --json when that file exists). It
runs every five minutes while a page is open; with none, the spend panel is
left out.

Examples:
  gt dashboard                 # http://127.0.0.1:8787
  gt dashboard --port 9000 --open`,
	Args: cobra.NoArgs,
	RunE: runDashboard,
}

func init() {
	dashboardCmd.Flags().IntVar(&dashboardPort, "port", 8787, "Port to listen on")
	dashboardCmd.Flags().StringVar(&dashboardBind, "bind", "127.0.0.1", "Address to bind; loopback only")
	dashboardCmd.Flags().StringVar(&dashboardSince, "since", "2h", "How far back the feed starts (as gt tail --since)")
	dashboardCmd.Flags().StringVar(&dashboardSpendCmd, "spend-cmd", "", "Command that prints the DeepSeek spend report as JSON (default: the ~/.claude tool when present)")
	dashboardCmd.Flags().BoolVar(&dashboardOpen, "open", false, "Open the page in the default browser")
	rootCmd.AddCommand(dashboardCmd)
}

func runDashboard(cmd *cobra.Command, _ []string) error {
	addr := net.JoinHostPort(dashboardBind, fmt.Sprint(dashboardPort))
	if !dashboard.IsLoopbackAddr(addr) {
		return fmt.Errorf("--bind %s: the dashboard binds loopback only", dashboardBind)
	}
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return err
	}
	loc := time.Local
	cutoff, err := parseTailSince(dashboardSince, time.Now(), loc)
	if err != nil {
		return err
	}
	hub, err := newDashboardHub(townRoot, cutoff, loc, dashboardSpendCmd)
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go hub.Run(ctx)

	srv := &http.Server{Handler: hub.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	url := "http://" + addr
	fmt.Fprintf(cmd.OutOrStdout(), "gt dashboard: %s (read-only, loopback only; polls only while a page is open)\n", url)
	if dashboardOpen {
		_ = exec.Command("open", url).Start()
	}
	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// newDashboardHub wires a hub to the readers gt tail -f uses.
func newDashboardHub(townRoot string, cutoff time.Time, loc *time.Location, spendCmd string) (*dashboard.Hub, error) {
	beadReads := openTailBeads(townRoot)
	deploys := newTailDeploys(time.Now, tailGitAncestry(townRoot), cutoff)
	sources, preface, err := buildTailSources(tailOptions{
		townRoot: townRoot, kinds: allTailKinds(), cutoff: cutoff, loc: loc, now: time.Now,
		rigNames: knownRigNames, journalFor: openTailJournal, beads: beadReads, deploys: deploys,
	})
	if err != nil {
		return nil, err
	}
	view := newTailView(loc, tailViewOptions{
		gitUser: tailGitUser(townRoot),
		beads:   beadReads,
	})
	pending := preface
	feed := func() []dashboard.Entry {
		batches := pollTailSources(sources)
		all := append([]tailLine(nil), pending...)
		pending = nil
		for _, b := range batches {
			all = append(all, b...)
		}
		var out []dashboard.Entry
		for _, l := range mergeTail(all) {
			if view.Show != nil {
				var ok bool
				if l, ok = view.Show(l); !ok {
					continue
				}
			}
			out = append(out, dashboardEntry(view, l))
		}
		return out
	}
	recs := newDashLandings(townRoot)
	seatCache := newDashSeatCache()
	om := newOMReader(townRoot, recs)
	return dashboard.NewHub(dashboard.Config{
		Feed:     feed,
		Summary:  func() dashboard.Summary { return dashboardSummary(townRoot, deploys, recs, seatCache) },
		Health:   func() dashboard.Health { return dashboardHealth(townRoot) },
		Machine:  dashboard.SampleMachine,
		Spend:    dashboardSpend(resolveSpendCmd(spendCmd)),
		OM:       func() *dashboard.OM { return om.read(time.Now()) },
		Dispatch: om.dispatch,
	}), nil
}

// dashboardEntry renders a tail line the way the stream's default view does,
// without the clock and rig columns the page draws itself.
func dashboardEntry(v tailView, l tailLine) dashboard.Entry {
	if v.Trim {
		l = tailTrimEventFields(l, v.GitUser)
	}
	if l.Title == "" && l.Verdict == "" && v.Beads != nil {
		l.Title = v.Beads.lineTitle(l)
	}
	text := tailText(l.Text)
	annotation := l.Verdict
	if annotation == "" {
		annotation = l.Title
	}
	if annotation != "" {
		text += tailAnnotationSep + tailText(annotation)
	}
	return dashboard.Entry{At: l.At, Rig: tailToken(l.Rig), Kind: l.Kind, Text: text, Class: dashboardClassName(tailLineClass(l))}
}

func dashboardClassName(c tailClass) string {
	switch c {
	case tailClassFailure:
		return "failure"
	case tailClassWarning:
		return "warning"
	case tailClassSuccess:
		return "success"
	case tailClassLanding:
		return "landing"
	case tailClassDispatch:
		return "dispatch"
	case tailClassRestart:
		return "restart"
	}
	return "plain"
}

var dashboardVerdicts = []string{"green", "degraded", "red", "unknown"}

// dashboardHealth is gt status --line: the daemon's report, read from disk.
func dashboardHealth(townRoot string) dashboard.Health {
	now := time.Now()
	lines, v, _ := townstatus.HealthView(townRoot, now)
	h := dashboard.Health{Verdict: "unknown", ReadAt: now}
	if len(lines) > 0 {
		h.Line = lines[0]
	}
	if code := v.ExitCode(); code >= 0 && code < len(dashboardVerdicts) {
		h.Verdict = dashboardVerdicts[code]
	}
	return h
}

// dashboardSummary is the summary line's state as fields, the polecat table,
// and the age of the oldest bead waiting to land. A reader that fails leaves
// its fields out.
func dashboardSummary(townRoot string, deploys *tailDeploys, recs *dashLandings, seatCache *dashSeatCache) dashboard.Summary {
	var s dashboard.Summary
	ready := map[string]bool{}
	if n, oldest, ids, err := dashboardReadyToLand(townRoot); err == nil {
		s.ReadyToLand = &n
		s.OldestReady = oldest
		ready = ids
	}
	sessions, sessionsKnown := dashSessions()
	names := make([]string, 0, len(sessions))
	for n := range sessions {
		names = append(names, n)
	}
	if seats, err := dashSeats(townRoot, names, sessionsKnown, seatCache); err == nil {
		s.Polecats = buildDashPolecats(dashPolecatInputs{
			Now: time.Now(), Seats: seats, Ready: ready, Sessions: sessions, SessionsKnown: sessionsKnown, Records: recs.get(),
		})
		if max, err := configuredSchedulerMaxPolecats(townRoot); err == nil && max > 0 {
			s.SeatsCap = &max
		}
	}
	if tip, installed, behind, err := tailMainPicture(townRoot); err == nil {
		s.MainTip, s.InstalledGT, s.Behind = tip, installed, &behind
	}
	if n, err := tailOpenEscalations(townRoot); err == nil {
		s.Escalations = &n
	}
	if d := deploys.snapshot(); d.Landed > 0 {
		if d.HasMedian {
			m := d.MedianMin
			s.MedianDeployMin = &m
		}
		w := d.Waiting
		s.DeployWaiting = &w
		if d.HasOldest {
			sec := int64(d.OldestWaiting / time.Second)
			s.OldestDeploySec = &sec
		}
	}
	return s
}

// dashboardReadyToLand is tailReadyToLand plus the oldest waiting bead's last
// update, which is when it was last touched, not strictly when it was
// submitted: the label write is normally the bead's last write until the
// landing worker picks it up.
func dashboardReadyToLand(townRoot string) (int, *time.Time, map[string]bool, error) {
	stores := []string{"hq"}
	rigs, err := knownRigNames(townRoot)
	if err != nil {
		return 0, nil, nil, err
	}
	stores = append(stores, rigs...)
	total := 0
	var oldest *time.Time
	ids := map[string]bool{}
	for _, store := range stores {
		dir := doltserver.FindRigBeadsDir(townRoot, store)
		if dir == "" {
			continue
		}
		issues, err := beads.NewWithBeadsDir(townRoot, dir).List(beads.ListOptions{Label: land.LabelReadyToLand, Priority: -1})
		if err != nil {
			return 0, nil, nil, err
		}
		for _, issue := range issues {
			if !beads.HasLabel(issue, land.LabelReadyToLand) || !beads.IssueStatus(issue.Status).IsActionable() {
				continue
			}
			total++
			ids[issue.ID] = true
			if at, err := time.Parse(time.RFC3339, issue.UpdatedAt); err == nil && (oldest == nil || at.Before(*oldest)) {
				at := at
				oldest = &at
			}
		}
	}
	return total, oldest, ids, nil
}

const dashboardSpendTimeout = 25 * time.Second

// resolveSpendCmd is the command that prints the spend report: the flag's, or
// the ~/.claude tool when it exists, or nothing.
func resolveSpendCmd(flagValue string) []string {
	if f := strings.Fields(flagValue); len(f) > 0 {
		return f
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	tool := filepath.Join(home, ".claude", "tools", "deepseek-spend.py")
	if _, err := os.Stat(tool); err != nil {
		return nil
	}
	return []string{tool, "--json"}
}

// dashboardSpend runs the spend command and returns its JSON, nil when it
// fails or prints something that is not JSON. nil argv is no spend panel.
func dashboardSpend(argv []string) func() json.RawMessage {
	if len(argv) == 0 {
		return nil
	}
	return func() json.RawMessage {
		ctx, cancel := context.WithTimeout(context.Background(), dashboardSpendTimeout)
		defer cancel()
		out, err := exec.CommandContext(ctx, argv[0], argv[1:]...).Output()
		if err != nil || !json.Valid(out) {
			return nil
		}
		return json.RawMessage(out)
	}
}

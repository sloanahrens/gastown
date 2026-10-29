package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/shlex"
	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/dashboard"
	"github.com/steveyegge/gastown/internal/doltserver"
	"github.com/steveyegge/gastown/internal/events"
	"github.com/steveyegge/gastown/internal/forgejo"
	"github.com/steveyegge/gastown/internal/jev/scorecard"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/procutil"
	"github.com/steveyegge/gastown/internal/report"
	"github.com/steveyegge/gastown/internal/rig"
	"github.com/steveyegge/gastown/internal/townhealth"
	"github.com/steveyegge/gastown/internal/townstatus"
	"github.com/steveyegge/gastown/internal/workspace"
)

var (
	dashboardPort                 int
	dashboardBind                 string
	dashboardSince                string
	dashboardSpendCmd             string
	dashboardJevSpendCmd          string
	dashboardJevUSDPerBillion     float64
	dashboardJevScorecardCmd      string
	dashboardForgejoRepos         []string
	dashboardClaudePlanUSD        float64
	dashboardClaudePlanRenewalDay int
	dashboardMangoDir             string
	dashboardOpen                 bool
)

var dashboardCmd = &cobra.Command{
	Use:     "dashboard",
	GroupID: GroupDiag,
	Short:   "Serve a read-only localhost page that shows what gt tail -f shows",
	Long: `Serve one page on localhost that shows the town the way gt tail -f does:
the health verdict, the queue waiting to land, the polecat seats and what they
hold, the machine's load and what burns it, the cloud patrol's latest report
when this machine runs one, the overseer's own hourly report, the questions the
overseer has filed for Sloan, the DeepSeek spend, the recent activity of the
Forgejo repos the viewer can see, and the same default-view feed gt tail prints.

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

--jev-spend-cmd names the command that prints the Jev ledger's spend as JSON
(default: gt jev spend --json --hours 24). The command is split like a shell
line, so quote a path that holds a space. The ledger stores tokens and never
dollars, so --jev-usd-per-billion-tokens (default 42) prices them: it is
appended to the command as --usd-per-billion-tokens unless the command already
names one. The command runs every five minutes while a page is open and draws the Jev row of the Cost panel
beside the DeepSeek and Claude lines: the calls and dollars over the command's
--hours window, and the caller that cost the most. A command that fails or
prints anything else shows the row as unavailable and leaves the other spend
lines alone; an empty value leaves the row out.

--jev-scorecard-cmd names the command that prints one Jev battery's scorecard as
JSON (default: gt jev scorecard --json --since 7d). It is split like a shell
line and run once per registered battery with --battery added, every 15 minutes
while a page is open. The JEV panel, under the Tier sweeps, draws one line per
battery: its items logged, how many of them are labeled, how many the report
skipped as unanswered, and each question's verdict (earning, not-yet or
no-signal), such as finding 212/140 fix_worthy:earning any_duplicate:not-yet. A
battery with no log in the window shows -. The test-selection battery counts
packages rather than items: its line says packages and the title line leaves it
out, so its packages cannot swamp a headline about items. Each run has 60
seconds. A battery whose run fails, times out or prints another battery's report
drops only its own line (its last good line stays, marked stale) and the title
names it as unavailable; only when every battery fails does the title read
unavailable on its own. The Cost panel's spend row is left alone; an empty value
leaves the panel out.

--claude-plan-usd prices the plan meter beside it: the Claude weekly and
five-hour limits, read every minute from the samples the status line appends to
.runtime/claude-plan/samples.jsonl, with the pace, the projection to the reset
and the week's dollars drawn against that monthly price. The same read rolls the
weeks up into the billing month that starts on the plan's renewal day
(--claude-plan-renewal-day, the 18th by default): each week is recorded once, as
its reset passes, in .runtime/claude-plan/weeks.jsonl, and the month reports what
those weeks came to against the plan's price, how many ran to 90% of the limit
and how it compares with the month before. The raw series is trimmed to ninety
days as it is polled; the weekly history is what outlives it.

The same samples draw a Claude series in the Cost panel's daily and hourly
charts, in the same buckets as the DeepSeek bars: each rise in the weekly
percentage is worth its share of the week at that price, the drop across a
weekly reset is not spend, a bucket with no sample is hatched rather than drawn
as zero, and a rise that spans such a bucket is priced nowhere. The series is
labeled a plan-equivalent estimate.

The Forgejo panel reads the viewer role's token file
(~/.config/gt/forgejo-viewer.env) and lists the recent activity of every rig
cut over to Forgejo, every three minutes, GET only: the rigs whose
merge_queue.forgejo block names a remote, or the repos the token can see when
no rig has one. A rig repo the viewer's token cannot read is named in the panel
rather than left out, so a missing rig never reads as a quiet one.
--forgejo-repo owner/name reads exactly the repos named instead, and may be
repeated. With no token file the panel is left out, and a refresh that fails
shows the last good feed marked stale.

The News pane above the Machine panel lists the current top five Hacker News
stories, one line each, with the story's points, comments and domain in the
line's hover. The hub reads them from the public Hacker News API and holds what
it read for five minutes, so the page never calls Hacker News itself. A refresh
that fails keeps the last good five dimmed under a "stale" note; before any
success the pane says it is unavailable. Neither raises an alert or moves the
health pill.

The Deploys block under the Cloud panel's findings reads those same repos'
deploy.yml and staging.yml runs — every run of either workflow, whatever its
event — with a landing's staging run tagged as one, each run's stages in the
order their needs give, refreshed every minute. It infers runner trouble from
the run itself: the runners API is owner-only and answers the viewer 403, so a
run nothing has picked up, and one that has not moved, are said as inferences.

The block's scheduled rows are the apps' hourly health.yml checks (dv-4qtt),
folded rather than listed: one row per app and environment — staging and prod —
with the newest result, the last green and the pass count over the newest 24
runs, the runs the reader could not read counted beside it. A check skipped
because the app is off reads "off on purpose" and is never green, and the header
carries a red marker naming every repo whose newest check failed, or a degraded
one naming a repo whose newest check nobody has a reading of. A repo with no
health.yml run shows nothing extra.

The Cloud panel reads the report the cloud patrol writes — report.json with a
heartbeat beside it — from /Users/Shared/gt-cloud/reports, or from the
directory GT_CLOUD_REPORTS_DIR names. It shows when the patrol last ran,
whether that is overdue, the projects it watches and its open findings by
severity, and it writes nothing there. On a machine with no reports directory
the panel says so and is re-read at a slow interval rather than every minute.

The Overseer panel is the overseer's whole side of the town in one panel, in
five blocks. The heartbeat reads .runtime/overseer: alive or silent against
overseer.silent_after, the last active time and state, the newest eight journal
lines, the last seven days of interventions by kind and the open question count.
The digest reads the daemon's own report from .runtime/overseer/reports — the
newest file named for the instant it was written — and draws its DONE, IN
PROGRESS and LEFT sections, naming any heading it does not draw, with when the
writes before that one happened behind a disclosure; a report that is late is
marked, and an earlier one can be read in its place. The Deep review block lists every schedule
daemon.patrols.scheduled_slings configures — each with when it is next due, or
that the patrol is switched off — the "paused: <reason>" a hold file or ESTOP
parks dispatch with, and the newest run it filed with its findings and the beads
filed. The Dispatcher block reads the spec dispatcher's last tick from the
daemon log: its roster, counts and the beads it skipped. The last block is the
rows of the attention list the overseer owes a move on. Every read that fails
says so rather than reading as an empty block, the panel writes nothing, and a
deep-review run's own digest is drawn as its bead and comment count, never as
the comment's text. gt overseer beat and gt overseer log write the heartbeat and
journal.

The Attention panel is the one list of what waits on a decision or an action:
the town's open escalation beads, the open overseer questions and the attention
queue's standing wake events, each row tagged with the owner who has to move and
filtered by that tag. A question's row carries its question, the answer the
overseer recommends, the work it holds up, how long it has waited and the bead
id to answer — Sloan answers with gt answer, which comments on the bead. The
page draws it and never writes: there is no form, and a question is closed from
a terminal.

Lifecycle: the dashboard watches its own binary and the town registry. When make
install replaces the binary, or a rig is added to or removed from the town, the
dashboard restarts itself in place: same PID, same flags, same listening socket,
so the port never closes. The pollers are stopped first and the bd and git
reads in flight, queue row expansions included, are waited for (killed after 10
seconds), so the restart leaves no defunct child; a row expanded during the
restart gets a retryable error. Open pages reconnect and reload. If the new binary
fails to run or the restart fails, the running build keeps serving and says so.
--open fires on the first launch only. Not available on Windows.

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
	dashboardCmd.Flags().StringVar(&dashboardJevSpendCmd, "jev-spend-cmd", defaultJevSpendCmd, "Command that prints the Jev ledger's spend as JSON; empty leaves the Jev row out")
	dashboardCmd.Flags().Float64Var(&dashboardJevUSDPerBillion, "jev-usd-per-billion-tokens", defaultJevUSDPerBillionTokens, "Price of Jev's tokens in US dollars per billion, passed to the --jev-spend-cmd command")
	dashboardCmd.Flags().StringVar(&dashboardJevScorecardCmd, "jev-scorecard-cmd", defaultJevScorecardCmd, "Command that prints a Jev battery's scorecard as JSON, run once per registered battery with --battery added; empty leaves the scorecard lines out")
	dashboardCmd.Flags().StringArrayVar(&dashboardForgejoRepos, "forgejo-repo", nil, "Repo (owner/name) the Forgejo panel reads; repeatable (default: every rig cut over to Forgejo)")
	dashboardCmd.Flags().BoolVar(&dashboardOpen, "open", false, "Open the page in the default browser")
	dashboardCmd.Flags().Float64Var(&dashboardClaudePlanUSD, "claude-plan-usd", dashboard.ClaudePlanDefaultUSD, "Monthly price of the Claude plan the plan meter's percentages are drawn against")
	dashboardCmd.Flags().IntVar(&dashboardClaudePlanRenewalDay, "claude-plan-renewal-day", dashboard.ClaudePlanDefaultRenewalDay, "Day of the month the Claude plan renews on; the plan meter's month rollup runs from this day to the next")
	dashboardCmd.Flags().StringVar(&dashboardMangoDir, "mango-dir", "", "Directory the town-metrics panel.json is read from (default <town>/.runtime/metrics/latest)")
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
	jevArgv, err := jevSpendArgv(dashboardJevSpendCmd, dashboardJevUSDPerBillion)
	if err != nil {
		return err
	}
	jevScorecardArgv, err := jevScorecardArgv(dashboardJevScorecardCmd)
	if err != nil {
		return err
	}
	hub, closeSources, err := newDashboardHub(townRoot, cutoff, loc, dashboardSpendCmd, jevArgv, jevScorecardArgv, dashboardClaudePlanUSD, dashboardClaudePlanRenewalDay, dashboardMangoDir, cmd.OutOrStdout())
	if err != nil {
		return err
	}
	// The feed's journal connections are held for the server's lifetime, and
	// handed back when it ends with it (gt-jw9sr).
	defer closeSources()
	ln, inherited, err := dashboardListener(addr, os.Getenv(dashboardListenFDEnv))
	if err != nil {
		return err
	}
	if inherited {
		// A child the replaced image stranded (a poll hung past the kill bound)
		// is ours now and defunct; no Wait will ever claim it. Reap it before
		// the hub starts any child of its own.
		if n := procutil.ReapZombieChildren(); n > 0 {
			dashboardLogf(cmd.OutOrStdout())("reaped %d defunct child(ren) inherited from the previous image", n)
		}
	}
	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	run := procutil.StartStoppable(ctx, hub.Run)

	// A new gt binary on disk (make install) or a rig added to the town
	// restarts the dashboard in place; see the lifecycle note in the command
	// help.
	if exe, err := os.Executable(); err == nil && dashboardCanReexec {
		go superviseRestart(ctx, exe, dashboardRegistryPath(townRoot), ln, run, cmd.OutOrStdout())
	}

	srv := &http.Server{Handler: hub.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	url := "http://" + addr
	fmt.Fprintf(cmd.OutOrStdout(), "gt dashboard: %s (read-only, loopback only; polls only while a page is open)\n", url)
	if dashboardOpen && !inherited { // a restart is not a second launch
		_, _ = procutil.Start("open", url) // procutil waits on the child, so it leaves no zombie
	}
	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// dashboardRegistryPath is the town registry file a restart follows. The rig
// registry is the machine config file's "registry" section (config layout.go),
// and gt rig add rewrites that file to add or remove a rig.
func dashboardRegistryPath(townRoot string) string {
	return filepath.Join(townRoot, filepath.FromSlash(config.MachineConfigFile))
}

// dashboardReportsDir is where the overseer's report tool writes inside the
// town: one file per hourly run, named for the instant it was written, beside
// the latest.md copy it leaves for a human to read. The dashboard only reads
// it; the tool is outside the repo.
func dashboardReportsDir(townRoot string) string {
	return report.ReportsDir(townRoot)
}

// dashboardCloudChecksPath is where the devops rig's cloud-check plugin writes
// its status inside the town: one JSON file per run, overwritten each time.
func dashboardCloudChecksPath(townRoot string) string {
	return filepath.Join(townRoot, ".runtime", "cloud-check", "status.json")
}

// dashboardMangoPath is the directory the town-metrics panel is read from: the
// --mango-dir value when it is set, else the metrics writer's latest directory
// inside the town.
func dashboardMangoPath(townRoot, mangoDir string) string {
	if mangoDir != "" {
		return mangoDir
	}
	return filepath.Join(townRoot, ".runtime", "metrics", "latest")
}

// newDashboardClaudePlanReader builds the Cost panel's plan meter: the town's
// own status line series, priced by the flag, with the usage scan's records
// behind it so the plan line can lay what the town's Claude tokens would have
// cost on the API beside the flat plan price (gt-d8sem). The records come
// through the panel's own reader, whose cache the two share, so a month of
// files is decoded once for both panels.
func newDashboardClaudePlanReader(townRoot string, usage *dashUsageReader, planUSDMonth float64) *dashboard.ClaudePlanReader {
	return dashboard.NewClaudePlanReader(dashboardClaudePlanPath(townRoot), planUSDMonth).
		WithUsage(usage.monthRecords)
}

// dashboardClaudePlanPath is where the status line appends its Claude plan
// samples inside the town: one JSON line per reading. The sampler is
// ~/.claude/statusline-command.sh, outside the repo; the dashboard's reader
// trims lines older than the retention.
func dashboardClaudePlanPath(townRoot string) string {
	return filepath.Join(townRoot, ".runtime", "claude-plan", "samples.jsonl")
}

// newDashboardHub wires a hub to the readers gt tail -f uses. The returned
// function releases the connections the feed holds open; the caller runs it
// when the server stops.
func newDashboardHub(townRoot string, cutoff time.Time, loc *time.Location, spendCmd string, jevSpendArgv, jevScorecardArgv []string, claudePlanUSD float64, claudePlanRenewalDay int, mangoDir string, out io.Writer) (*dashboard.Hub, func(), error) {
	beadReads := openTailBeads(townRoot)
	rigOfBead := landingRig(townRoot)
	// The deploy block reads the same repos through the same viewer client as
	// the Forgejo panel, so the two are wired together from one token read;
	// the staging ship reader dates the app rigs' landings off the same runs.
	forgejoFeed, forgejoDeploys, stagingShip := dashboardForgejoReaders(townRoot, dashboardForgejoRepos)
	news, err := dashboardNewsReader(townRoot)
	if err != nil {
		return nil, nil, err
	}
	deploys := dashboardDeploys(townRoot, rigOfBead, cutoff, stagingShip)
	sources, preface, err := buildTailSources(tailOptions{
		townRoot: townRoot, kinds: allTailKinds(), cutoff: cutoff, loc: loc, now: time.Now,
		rigNames: knownRigNames, journalFor: openTailJournal, beads: beadReads, deploys: deploys,
	})
	if err != nil {
		return nil, nil, err
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
	// Who submitted a landing, for the rows whose own record names no branch:
	// read from the landing worker's submission record, never from the bead,
	// which a rejection reassigns (gt-gclus).
	submitters := newDashSubmitters(townRoot, dashboardLogf(out))
	seatCache := newDashSeatCache()
	// The dispatch clock the Age column reads and the overrun guard judges, one
	// reader for the panel's whole life so each poll reads only the log's
	// appended lines (gt-tzbtf).
	dispatches := events.NewDispatchReader(townRoot)
	// Who holds a bead, read once per row: the Landings pane's live rows and
	// the om panel's running reviews both name a landing's polecat, and they
	// ask the same reader for it.
	polecatFor := func(rig, id string) string {
		if issue := beadReads.issue(rig, id); issue != nil {
			return polecatOfAssignee(issue.Assignee)
		}
		return ""
	}
	// The polecat time a rejected or running Landings row draws, read from the
	// spans log through the landing worker's reader (gt-kmp7c).
	polecatTimes := spanPolecatTimes(townRoot)
	om := newOMReader(townRoot, recs, polecatFor)
	models := newModelsReader(townRoot)
	tierSweeps := newTierSweepReader(townRoot)
	escalations := newDashEscalationReader(townRoot)
	backoff := newDashBackoff(townRoot)
	queue := newDashQueueReader(townRoot)
	cloud := dashboard.NewCloudReader(config.CloudReportsDir()).
		WithChecks(dashboard.NewCloudChecksReader(dashboardCloudChecksPath(townRoot))).
		WithApps(dashboard.NewAppPowerReader(config.CloudOffDir(), config.CloudReleasesDir()))
	reports := dashboard.NewReportsReader(dashboardReportsDir(townRoot))
	questions := newDashQuestionsReader(townRoot)
	overseerPane := newDashOverseerReader(townRoot, func() (int, error) {
		open, err := questions.list()
		return len(open), err
	})
	deepReview := newDashDeepReviewReader(townRoot)
	usage := newDashUsageReader(townRoot)
	claudePlan := newDashboardClaudePlanReader(townRoot, usage, claudePlanUSD).
		WithRenewalDay(claudePlanRenewalDay)
	rework := newDashReworkReader(townRoot).withStages(om.stagesSince)
	loads := newDashLoads(townRoot, time.Now)
	mango := dashboardMangoPath(townRoot, mangoDir)
	// One reader for the panel's whole life: it reads only what Jev appended
	// since the last publish, and it is read from the shared attention package
	// so the dashboard does not reach into the battery's own (gt-b9j3o.2).
	attentionScoreReader := attention.NewScoreReader(attention.AnswersPath(townRoot))
	return dashboard.NewHub(dashboard.Config{
		Feed:           feed,
		Summary:        func() dashboard.Summary { return dashboardSummary(townRoot, deploys, recs, seatCache, dispatches) },
		Health:         func() dashboard.Health { return dashboardHealth(townRoot) },
		Machine:        dashboard.SampleMachine,
		Cloud:          func() *dashboard.Cloud { return cloud.Read(time.Now()) },
		Mango:          func() *dashboard.Mango { return dashboard.ReadMango(mango, time.Now()) },
		Spend:          dashboardSpend(resolveSpendCmd(spendCmd)),
		SpendHours:     dashboardSpendHours(),
		JevSpend:       dashboardJevSpend(jevSpendArgv),
		JevScorecard:   dashboardJevScorecard(jevScorecardArgv),
		OM:             func() *dashboard.OM { return om.read(time.Now()) },
		TierSweep:      func() *dashboard.TierSweep { return tierSweeps.read(time.Now()) },
		Forgejo:        forgejoFeed,
		News:           news,
		Deploys:        forgejoDeploys,
		Reports:        func() *dashboard.Reports { return reports.Read(time.Now()) },
		ReportsDir:     dashboardReportsDir(townRoot),
		Questions:      func() *dashboard.Questions { return questions.read(time.Now()) },
		Overseer:       func() *dashboard.Overseer { return overseerPane.read(time.Now()) },
		DeepReview:     func() *dashboard.DeepReview { return deepReview.read(time.Now()) },
		Usage:          func() *dashboard.Usage { return usage.read(time.Now()) },
		ClaudePlan:     func() *dashboard.ClaudePlan { return claudePlan.Read(time.Now()) },
		Rework:         func() *dashboard.Rework { return rework.read(time.Now()) },
		Escalation:     func() *dashboard.Escalations { return escalations.read(time.Now()) },
		Dispatch:       om.dispatch,
		Attention:      dashboard.NewAttentionFollower(townRoot).Read,
		AttentionItems: dashboard.NewAttentionFollower(townRoot).Items,
		NeedsHuman:     attentionScoreReader.Scores,
		Models:         models.read,
		SeatRoster:     func() dashboard.SeatRoster { return dashboardSeatRoster(poolLiveFor(townRoot)) },
		Queue:          func() *dashboard.Queue { return queue.read(time.Now()) },
		Bead:           queue.detail,
		RigTheme:       newDashRigThemes(townRoot),
		Trend: func() *dashboard.Trend {
			now := time.Now()
			stages, rejects := om.trendInputs(trendWindowStart(now))
			tr := buildTrend(now, recs.get(), stages, rejects, loads.points())
			who := submitters.get()
			live := runningRows(om.running(now.Add(-trendHours*time.Hour)), rigOfBead, livePolecatName(polecatFor, who))
			title := func(rig, id string) string { return beadReads.title(rig, id) }
			recent := buildRecentLandings(now, recs.get(), stages, rejects, live, deploys.shipStatus, title, who, polecatTimes, recentLandingRows)
			tr.Recent = withBackoffRows(recent, backoff.get(), title, who)
			return tr
		},
		LoadSample: loads.append,
	}), func() { closeTailSources(sources) }, nil
}

// dashboardDeploys builds the deploy tracker the Landings pane reads: the
// town's ship definitions, and the horizon the pane's table needs.
//
// The table spans a whole day of landings while the feed's own window is the
// --since it was started with. Without the horizon a row older than that
// window has no record, so its Ship cell draws a dash however it shipped
// (gt-b5hw2).
func dashboardDeploys(townRoot string, rigOf func(bead string) string, cutoff time.Time, staging tailShipStaging) *tailDeploys {
	deploys := newTailDeploys(time.Now, tailGitAncestry(townRoot), cutoff)
	deploys.setShipDefinitions(tailRestartRig(townRoot), rigOf, staging)
	deploys.setHorizon(trendHours * time.Hour)
	return deploys
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
	lines, v, rep := townstatus.HealthView(townRoot, now)
	h := dashboard.Health{Verdict: "unknown", ReadAt: now}
	if len(lines) > 0 {
		h.Line = lines[0]
	}
	if code := v.ExitCode(); code >= 0 && code < len(dashboardVerdicts) {
		h.Verdict = dashboardVerdicts[code]
	}
	h.Causes = dashboardHealthCauses(rep)
	return h
}

// dashboardHealthCauseMax caps the causes the pill carries. The header has one
// line for the first cause; the rest ride its title.
const dashboardHealthCauseMax = 3

// dashboardHealthCauses is rep's non-green fields, worst verdict first, capped
// at dashboardHealthCauseMax. Ordering and rank match townhealth.Line, so the
// pill's first cause is the line's first token for a reader cross-checking
// (gt-70aa6). A missing report reads as no causes.
func dashboardHealthCauses(rep *townhealth.Report) []dashboard.HealthCause {
	if rep == nil {
		return nil
	}
	bad := make([]townhealth.Field, 0, len(rep.Fields))
	for _, f := range rep.Fields {
		if f.Verdict != townhealth.Green {
			bad = append(bad, f)
		}
	}
	sort.SliceStable(bad, func(i, j int) bool { return bad[i].Verdict.ExitCode() > bad[j].Verdict.ExitCode() })
	if len(bad) > dashboardHealthCauseMax {
		bad = bad[:dashboardHealthCauseMax]
	}
	if len(bad) == 0 {
		return nil
	}
	causes := make([]dashboard.HealthCause, 0, len(bad))
	for _, f := range bad {
		causes = append(causes, dashboard.HealthCause{Name: f.Name, Rig: f.Rig, Subject: f.Subject, Detail: f.Detail})
	}
	return causes
}

// dashboardSummary is the summary line's state as fields, the polecat table,
// and the age of the oldest bead waiting to land. A reader that fails leaves
// its fields out and flags the failure, so the hub keeps the previous reading
// rather than showing the empty one (gt-q6h8e).
func dashboardSummary(townRoot string, deploys *tailDeploys, recs *dashLandings, seatCache *dashSeatCache, dispatches *events.DispatchReader) dashboard.Summary {
	var s dashboard.Summary
	ready := map[string]bool{}
	if n, oldest, ids, err := dashboardReadyToLand(townRoot); err == nil {
		s.ReadyToLand = &n
		s.OldestReady = oldest
		ready = ids
	} else {
		// The hub keeps the previous reading and the page marks it stale: an
		// unread queue must not read as an empty one (gt-q6h8e).
		s.ReadyError = true
	}
	sessions, sessionsKnown := dashSessions()
	names := make([]string, 0, len(sessions))
	for n := range sessions {
		names = append(names, n)
	}
	if seats, err := dashSeats(townRoot, names, sessionsKnown, seatCache, dispatches, newPoolSessionLister()); err == nil {
		s.Polecats = buildDashPolecats(dashPolecatInputs{
			Now: time.Now(), Seats: seats, Ready: ready, Sessions: sessions, SessionsKnown: sessionsKnown, Records: recs.get(),
		})
		if max, err := configuredSchedulerMaxPolecats(townRoot); err == nil && max > 0 {
			s.SeatsCap = &max
		}
	} else {
		// Likewise the seats: the hub keeps the last good list and the page says
		// unavailable rather than "no polecats" (gt-q6h8e).
		s.SeatsError = true
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

// dashboardJevScorecardTimeout bounds one battery's scorecard run. It is not
// dashboardSpendTimeout: the slowest battery takes about 32s to answer, past
// the spend command's 25s, and a run that outlives this loses only its own line.
const dashboardJevScorecardTimeout = 60 * time.Second

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

// defaultJevSpendCmd is --jev-spend-cmd's default: the last day of the Jev
// ledger. Its price comes from --jev-usd-per-billion-tokens, because the ledger
// stores tokens and never dollars.
const defaultJevSpendCmd = "gt jev spend --json --hours 24"

// defaultJevUSDPerBillionTokens is --jev-usd-per-billion-tokens's default, the
// $42 per billion the Jev attention design prices a call at.
const defaultJevUSDPerBillionTokens = 42

// jevSpendArgv turns --jev-spend-cmd into the argv the dashboard runs: the
// command split like a shell line, so a quoted path with a space stays one
// word, plus --usd-per-billion-tokens unless the command names its own price.
// An empty command is no argv, which leaves the Jev row out. A command with an
// unbalanced quote, or a price the spend command would refuse, is an error at
// startup rather than a row that silently reads as unavailable.
func jevSpendArgv(cmd string, usdPerBillion float64) ([]string, error) {
	argv, err := shlex.Split(cmd)
	if err != nil {
		return nil, fmt.Errorf("--jev-spend-cmd %q: %w", cmd, err)
	}
	if len(argv) == 0 {
		return nil, nil
	}
	for _, a := range argv {
		if a == "--usd-per-billion-tokens" || strings.HasPrefix(a, "--usd-per-billion-tokens=") {
			return argv, nil
		}
	}
	if !(usdPerBillion > 0 && !math.IsInf(usdPerBillion, 0) && !math.IsNaN(usdPerBillion)) {
		return nil, fmt.Errorf("--jev-usd-per-billion-tokens must be finite and above zero, not %v", usdPerBillion)
	}
	return append(argv, "--usd-per-billion-tokens", strconv.FormatFloat(usdPerBillion, 'f', -1, 64)), nil
}

// dashboardJevSpend runs the Jev spend command and folds its JSON into the
// row, nil when the command fails, times out, or prints anything but the
// report. A nil argv is no Jev row. It shares the DeepSeek command's timeout
// and fails apart from it: neither reader sees the other's command.
func dashboardJevSpend(argv []string) func() *dashboard.JevSpend {
	return dashboardJevSpendWith(argv, func(ctx context.Context, argv []string) ([]byte, error) {
		return exec.CommandContext(ctx, argv[0], argv[1:]...).Output()
	})
}

// dashboardJevSpendWith is dashboardJevSpend over run, the one place the
// command is started, so a test stands in for the command without a process.
func dashboardJevSpendWith(argv []string, run func(ctx context.Context, argv []string) ([]byte, error)) func() *dashboard.JevSpend {
	if len(argv) == 0 {
		return nil
	}
	window := dashboard.JevSpendWindow(argv)
	return func() *dashboard.JevSpend {
		ctx, cancel := context.WithTimeout(context.Background(), dashboardSpendTimeout)
		defer cancel()
		out, err := run(ctx, argv)
		if err != nil {
			return nil
		}
		j, err := dashboard.ParseJevSpend(out)
		if err != nil {
			return nil
		}
		j.Window = window
		return j
	}
}

// defaultJevScorecardCmd is --jev-scorecard-cmd's default: the last week of a
// battery's log. The scorecard names one battery per run, so the reader adds
// --battery for each registered battery.
const defaultJevScorecardCmd = "gt jev scorecard --json --since 7d"

// jevScorecardArgv turns --jev-scorecard-cmd into the argv the dashboard runs,
// split like a shell line. An empty command is no argv, which leaves the
// scorecard lines out; an unbalanced quote is an error at startup.
func jevScorecardArgv(cmd string) ([]string, error) {
	argv, err := shlex.Split(cmd)
	if err != nil {
		return nil, fmt.Errorf("--jev-scorecard-cmd %q: %w", cmd, err)
	}
	if len(argv) == 0 {
		return nil, nil
	}
	return argv, nil
}

// dashboardJevScorecard runs the scorecard command for every registered
// battery and folds each report into its line. A nil argv is no scorecard
// lines.
func dashboardJevScorecard(argv []string) func() *dashboard.JevScorecard {
	return dashboardJevScorecardWith(argv, scorecard.Registered(), dashboardJevScorecardTimeout, func(ctx context.Context, argv []string) ([]byte, error) {
		return exec.CommandContext(ctx, argv[0], argv[1:]...).Output()
	})
}

// dashboardJevScorecardWith is dashboardJevScorecard over run, the battery
// names and each run's timeout, so a test stands in for the process. The runs
// go out together, so the read takes as long as its slowest battery, not the
// sum of them. A battery whose run fails, times out or prints anything but its
// own report loses only its line: it stays in the result as a Failed
// placeholder, which the hub puts its last good line back over. Only when every
// battery fails is the read nil, which the hub shows as the scorecard
// unavailable.
func dashboardJevScorecardWith(argv, batteries []string, timeout time.Duration, run func(ctx context.Context, argv []string) ([]byte, error)) func() *dashboard.JevScorecard {
	if len(argv) == 0 {
		return nil
	}
	window := dashboard.JevScorecardWindow(argv)
	return func() *dashboard.JevScorecard {
		sc := &dashboard.JevScorecard{Batteries: make([]dashboard.JevBattery, len(batteries)), Window: window}
		var wg sync.WaitGroup
		for i, name := range batteries {
			wg.Add(1)
			go func() {
				defer wg.Done()
				b, err := readJevBattery(argv, name, timeout, run)
				if err != nil {
					b = &dashboard.JevBattery{Name: name, Failed: true}
				}
				sc.Batteries[i] = *b
			}()
		}
		wg.Wait()
		failed := 0
		for _, b := range sc.Batteries {
			if b.Failed {
				failed++
			}
		}
		if failed > 0 && failed == len(batteries) {
			return nil
		}
		sc.Tally()
		return sc
	}
}

// readJevBattery runs the scorecard command for one battery under its own
// timeout and parses the report, refusing one that names another battery.
func readJevBattery(argv []string, name string, timeout time.Duration, run func(ctx context.Context, argv []string) ([]byte, error)) (*dashboard.JevBattery, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	out, err := run(ctx, append(append([]string{}, argv...), "--battery", name))
	if err != nil {
		return nil, err
	}
	b, err := dashboard.ParseJevScorecard(out)
	if err != nil {
		return nil, err
	}
	if b.Name != name {
		return nil, fmt.Errorf("jev scorecard: asked for %q, got %q", name, b.Name)
	}
	return b, nil
}

// dashboardForgejoReaders wires the readers the viewer's token feeds — the
// Forgejo activity feed, the Cloud section's deploy runs, and the staging ship
// reader the Landings table dates an app rig's landing with — to the read-only
// viewer role: repos, when non-empty, is the explicit owner/name list to read,
// and empty reads every rig that has cut over to Forgejo, so the panels name a
// rig repo the viewer cannot see instead of silently reading a subset
// (gt-faml5). No viewer token file means none of them, like the spend panel
// with no spend command. The token is read once, here, and never leaves the
// client.
func dashboardForgejoReaders(townRoot string, repos []string) (func() *dashboard.ForgejoFeed, func() *dashboard.Deploys, tailShipStaging) {
	if len(repos) == 0 {
		repos = rigForgejoRepos(townRoot)
	}
	client, err := forgejo.NewClient(config.ForgejoRoleViewer)
	if err != nil {
		return nil, nil, nil
	}
	staging := dashboard.NewStagingReader(client, rigShipRepos(townRoot))
	return dashboard.NewForgejoReader(client, repos).Read, dashboard.NewDeployReader(client, repos).Read, staging.Ship
}

// dashboardNewsReader wires the News pane's reader, or none when
// settings/config.json's dashboard.news.enabled is false. The pane reads the
// public Hacker News API, so it needs no token and no flag.
func dashboardNewsReader(townRoot string) (func() *dashboard.NewsFeed, error) {
	on, err := dashboardNewsEnabled(townRoot)
	if err != nil {
		return nil, err
	}
	if !on {
		return nil, nil
	}
	return dashboard.NewNewsReader(nil).Read, nil
}

// dashboardNewsEnabled reads settings/config.json's dashboard.news.enabled: the
// pane runs unless that key turns it off. A settings file the town cannot read
// is an error rather than a default, so a mistyped config cannot quietly
// reinstate a panel the operator switched off (gt-9ktrr).
func dashboardNewsEnabled(townRoot string) (bool, error) {
	path := config.TownSettingsPath(townRoot)
	settings, err := config.LoadOrCreateTownSettings(path)
	if err != nil {
		return false, fmt.Errorf("reading %s for dashboard.news.enabled: %w", path, err)
	}
	return settings.Dashboard.NewsEnabled(), nil
}

// rigShipRepos is every rig's repository as rig name -> owner/name, for the
// staging ship reader: the repository is the rig's own config.json git_url,
// not a list kept here, so a rig added to the town is read without a code
// change. A rig with no config, no git_url, or a URL that names no repository
// is left out, and its landings then have no staging ship definition. gastown's
// own URL is a repository too and is not special-cased: no staging runs are
// read for it, and its landings ship by the daemon restart instead.
func rigShipRepos(townRoot string) map[string]string {
	names, err := knownRigNames(townRoot)
	if err != nil {
		return nil
	}
	out := map[string]string{}
	for _, name := range names {
		cfg, err := config.LoadRigConfig(filepath.Join(townRoot, name, "config.json"))
		if err != nil || cfg.GitURL == "" {
			continue
		}
		owner, repo, err := land.RepoFromRemoteURL(cfg.GitURL)
		if err != nil {
			continue
		}
		out[name] = owner + "/" + repo
	}
	return out
}

// rigForgejoRepos is every rig's Forgejo repository as owner/name, in rig-name
// order: the rigs whose merge_queue.forgejo block names a remote. A rig with no
// block, or one whose remote URL names no repository, is left out. With none
// named the panel falls back to the repos the viewer's token can see.
func rigForgejoRepos(townRoot string) []string {
	names, err := knownRigNames(townRoot)
	if err != nil {
		return nil
	}
	var out []string
	for _, name := range names {
		fc := rig.ResolveForgejoConfig(townRoot, name)
		if fc == nil || fc.RemoteURL == "" {
			continue
		}
		owner, repo, err := land.RepoFromRemoteURL(fc.RemoteURL)
		if err != nil {
			continue
		}
		out = append(out, owner+"/"+repo)
	}
	return out
}

// dashboardSpendHours reads the day's spend by hour from the balance ledger the
// spend watcher appends to, the same file the spend command reads its history
// from. Without a home directory there is no ledger, so there is no read.
func dashboardSpendHours() func() dashboard.SpendRead {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	return dashboardSpendHoursFrom(filepath.Join(home, ".claude", "docs", "research", "deepseek-spend", "balance.log"))
}

// dashboardSpendHoursFrom is the ledger read for the given path. A read that
// fails is an empty read, so the hourly chart is absent instead of drawing a day
// of zeros, and the malformed lines it skipped are carried to the chart.
func dashboardSpendHoursFrom(ledger string) func() dashboard.SpendRead {
	return func() dashboard.SpendRead {
		hours, skipped, err := dashboard.ReadSpendHours(ledger, time.Now())
		if err != nil {
			return dashboard.SpendRead{}
		}
		return dashboard.SpendRead{Hours: hours, Skipped: skipped}
	}
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

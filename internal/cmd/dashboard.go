package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path"
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
	"github.com/steveyegge/gastown/internal/cloudledger"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/dashboard"
	"github.com/steveyegge/gastown/internal/doltserver"
	"github.com/steveyegge/gastown/internal/events"
	"github.com/steveyegge/gastown/internal/forgejo"
	"github.com/steveyegge/gastown/internal/jev/scorecard"
	"github.com/steveyegge/gastown/internal/jev/scrub"
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
overseer has filed for the operator, the DeepSeek spend, the recent activity
of the Forgejo repos the viewer can see, and the same default-view feed gt tail
prints.

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
left out. Each report it prints also rewrites the town's watch reading,
<town>/.runtime/watch/spend.json, as {ts, per_hour, balance} — what the hourly
snapshot, gt tail and gt report read as the live DeepSeek rate — with ts the
instant of the read. A report that carries no balance leaves the file as it
was, and a file that cannot be rewritten is marked on the Reads pane and
leaves the panel drawing the report it read.

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

The JEV panel, under the Tier sweeps, reads the scorecard cache that
gt jev scorecard --refresh writes (<town>/.runtime/jev/scorecard.json) every
minute while a page is open and starts no process for it. It draws one line per
battery: its items logged, how many of them are labeled, how many the report
skipped as unanswered, and each question's verdict (earning, not-yet or
no-signal), such as finding 212/140 fix_worthy:earning any_duplicate:not-yet,
and how long ago that battery's score was computed. A battery with no log in
the window shows -. The test-selection battery counts packages rather than
items: its line says packages and the title line leaves it out, so its packages
cannot swamp a headline about items. A battery whose refresh failed draws the
last good block the cache kept and says so; one with no block to keep shows
unavailable and the title names it. A missing cache reads "no scorecard yet: run
gt jev scorecard --refresh". A cache that cannot be read shows the scorecard
unavailable and keeps the last good panel. The Cost panel's spend row is left
alone.

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

The overseer's two readings that go stale ride the top strip: "overseer" says
alive, a silent window, no heartbeat or unavailable, judged against
overseer.silent_after (amber unless alive), and "dispatcher last tick" says how
long ago the spec dispatcher last ticked, amber past 150 seconds and red past
300, with "not ticking" beside the age once it is red. The state still carries
the heartbeat and journal (overseer), the dispatcher's last tick and skipped
beads (dispatch), the deep review schedules and runs (deepreview) and the
daemon's reports (reports) on /api/state and their routes; the Cost panel joins
the deep review's runs to its usage, and no panel draws the rest. gt overseer
beat and gt overseer log write the heartbeat and journal.

The Birds panel lists the daily reports the town's birds leave for the operator:
one row per bird with a report under <town>/.runtime/<bird>/reports, from its
daily.json, with the daily.html page beside it openable from the row. A summary
older than 26 hours is marked stale. The panel reads those files and writes
nothing.

The Attention panel is the one list of what waits on a decision or an action:
the town's open escalation beads, the open overseer questions and the attention
queue's standing wake events, each row tagged with the owner who has to move and
filtered by that tag. A question's row carries its question, the answer the
overseer recommends, the work it holds up, how long it has waited and the bead
id to answer — the operator answers with gt answer, which comments on the
bead. The page draws it and never writes: there is no form, and a question is
closed from a terminal.

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
	hub, closeSources, err := newDashboardHub(townRoot, cutoff, loc, dashboardSpendCmd, jevArgv, dashboardClaudePlanUSD, dashboardClaudePlanRenewalDay, dashboardMangoDir, cmd.OutOrStdout())
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

// dashboardRuntimeDir is the town's runtime directory: the birds each write a
// directory here holding their daily reports, beside the town's other runtime
// state. The dashboard only reads it.
func dashboardRuntimeDir(townRoot string) string {
	return filepath.Join(townRoot, ".runtime")
}

// dashboardWatchDir is the town's watch directory, where the hub keeps the
// DeepSeek spend reading current for the hourly snapshot, gt tail and gt
// report: <town>/.runtime/watch.
func dashboardWatchDir(townRoot string) string {
	return filepath.Join(dashboardRuntimeDir(townRoot), "watch")
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

// closeCloudLedger releases the cloud step's database pool when the source holds
// one. The hub is stopping, so a close error is reported on stderr, the one
// place left to report it.
func closeCloudLedger(s cloudledger.Source) {
	if c, ok := s.(io.Closer); ok {
		if err := c.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "dashboard: closing the cloud ledger: %v\n", err)
		}
	}
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
func newDashboardHub(townRoot string, cutoff time.Time, loc *time.Location, spendCmd string, jevSpendArgv []string, claudePlanUSD float64, claudePlanRenewalDay int, mangoDir string, out io.Writer) (*dashboard.Hub, func(), error) {
	// The cloud step's source comes from these settings, so a file that does
	// not parse stops the hub here rather than showing a pane read from a step
	// the town meant to configure differently.
	townSettings, err := config.LoadTownSettings(config.TownSettingsPath(townRoot))
	if err != nil {
		return nil, nil, fmt.Errorf("dashboard: reading town settings: %w", err)
	}
	beadReads := openTailBeads(townRoot)
	rigOfBead := landingRig(townRoot)
	// Who submitted a bead, for a landing on the operator's reserved ops branch:
	// gt done records it as the author of the bead's newest "Submitted for
	// landing" comment, asked of the bead reader the rows already use (gt-6ua0sn).
	submittedBy := func(rig, id string) string { return beadReads.submittedBy(rig, id) }
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
	// read from the landing worker's submission record, never from the bead's
	// holder, which a rejection reassigns (gt-gclus). The reserved ops branch
	// reads the bead's submission comment through the same lookup (gt-6ua0sn).
	submitters := newDashSubmitters(townRoot, dashboardLogf(out)).withSubmittedBy(submittedBy)
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
	// The operator's dispatch hold file, for the header's chip: it is a stat
	// and the file's first line, so it keeps the minute clock the other slow
	// readers use.
	hold := dashboard.NewHoldReader(townRoot)
	tierSweeps := newTierSweepReader(townRoot)
	// The gt update job's own record, for the feed's update section: the page
	// draws it beside the dispatch hold so the operator sees why nothing runs.
	// A record that cannot be read is logged, because otherwise it looks the
	// same as a town where no run has happened.
	updateStatus := newUpdateStatusReader(townRoot).withLogf(dashboardLogf(out))
	escalations := newDashEscalationReader(townRoot)
	backoff := newDashBackoff(townRoot)
	queue := newDashQueueReader(townRoot)
	cloudSettings := townSettings.ResolveCloud(os.Getenv)
	cloudSource := cloudledger.NewSource(cloudSettings)
	cloud := dashboard.NewCloudReaderFromSource(cloudSource).
		WithChecks(dashboard.NewCloudChecksReader(dashboardCloudChecksPath(townRoot))).
		WithApps(dashboard.NewAppPowerReaderFromSource(cloudSettings.OffDir, cloudSource))
	reports := dashboard.NewReportsReader(dashboardReportsDir(townRoot))
	birds := dashboard.NewBirdReportsReader(dashboardRuntimeDir(townRoot)).WithHeadline(dashboardHeadlineBird(townRoot))
	// The reasons behind a failed seats, ready or questions read are logged
	// once per change, so a read that stays down is one line (gt-6qc3nw.2).
	whyLog := newDashWhyLog(dashboardLogf(out))
	questions := newDashQuestionsReader(townRoot).withWhy(whyLog)
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
	landingRiskReader := attention.NewLandingRiskReader(attention.LandingRiskPath(townRoot))
	return dashboard.NewHub(dashboard.Config{
		Feed: feed,
		Summary: func() dashboard.Summary {
			return dashboardSummary(townRoot, deploys, recs, seatCache, dispatches, whyLog)
		},
		Health:         func() dashboard.Health { return dashboardHealth(townRoot) },
		Machine:        dashboard.SampleMachine,
		Cloud:          func() *dashboard.Cloud { return cloud.Read(time.Now()) },
		Mango:          func() *dashboard.Mango { return dashboard.ReadMango(mango, time.Now()) },
		Spend:          dashboardSpend(resolveSpendCmd(spendCmd)),
		WatchDir:       dashboardWatchDir(townRoot),
		SpendHours:     dashboardSpendHours(),
		JevSpend:       dashboardJevSpend(jevSpendArgv),
		JevScorecard:   dashboardJevScorecard(townRoot),
		OM:             func() *dashboard.OM { return om.read(time.Now()) },
		TierSweep:      func() *dashboard.TierSweep { return tierSweeps.read(time.Now()) },
		Update:         func() *dashboard.UpdateStatus { return updateStatus.read(time.Now()) },
		Forgejo:        forgejoFeed,
		News:           news,
		Deploys:        forgejoDeploys,
		Reports:        func() *dashboard.Reports { return reports.Read(time.Now()) },
		ReportsDir:     dashboardReportsDir(townRoot),
		Birds:          func() []dashboard.BirdReport { return birds.Read(time.Now()) },
		BirdsDir:       dashboardRuntimeDir(townRoot),
		BirdsEvery:     60 * time.Second,
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
		LandingRisk:    landingRiskReader.Risks,
		Models:         models.read,
		Hold:           func() *dashboard.Hold { return hold.Read(time.Now()) },
		SeatRoster:     func() dashboard.SeatRoster { return dashboardSeatRoster(poolLiveFor(townRoot)) },
		Queue:          func() *dashboard.Queue { return queue.read(time.Now()) },
		Bead:           queue.detail,
		RigTheme:       newDashRigThemes(townRoot),
		Trend: func() *dashboard.Trend {
			now := time.Now()
			stages, rejects := om.trendInputs(trendWindowStart(now))
			tr := buildTrend(now, recs.get(), stages, rejects, loads.points())
			names := landingNames{who: submitters.get(), submitter: submittedBy}
			crew := submitters.crewLookup()
			live := runningRows(om.running(now.Add(-trendHours*time.Hour)), rigOfBead, livePolecatName(polecatFor, names.who))
			title := func(rig, id string) string { return beadReads.title(rig, id) }
			recent := buildRecentLandings(now, recs.get(), stages, rejects, live, deploys.shipStatus, title, names, crew, polecatTimes, rigOfBead, recentLandingRows)
			tr.Recent = withBackoffRows(recent, backoff.get(), title, names.who)
			return tr
		},
		LoadSample: loads.append,
	}), func() { closeTailSources(sources); closeCloudLedger(cloudSource) }, nil
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
// its fields out, flags the failure and names its reason, so the hub keeps the
// previous reading rather than showing the empty one (gt-q6h8e, gt-6qc3nw.2).
func dashboardSummary(townRoot string, deploys *tailDeploys, recs *dashLandings, seatCache *dashSeatCache, dispatches *events.DispatchReader, why *dashWhyLog) dashboard.Summary {
	var s dashboard.Summary
	ready := dashboardReadyPart(&s, func() (int, *time.Time, map[string]bool, error) {
		return dashboardReadyToLand(townRoot)
	})
	sessions, sessionsKnown := dashSessions()
	names := make([]string, 0, len(sessions))
	for n := range sessions {
		names = append(names, n)
	}
	dashboardPolecats(&s, townRoot, names, sessions, sessionsKnown, ready, recs, seatCache, dispatches, newPoolSessionLister())
	why.summary(&s)
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

// dashboardPolecats fills the summary's polecat table from the seats. The seats
// the daemon keeps for the jev_rules battery are read with them: a kept record
// that cannot be read leaves every seat readable, the seats read as they did
// before the kept fact, and the summary names the failed read so the page can
// show it beside the table (gt-ddg96x). Only a failed seats read sets
// SeatsError, which hides the table.
func dashboardPolecats(s *dashboard.Summary, townRoot string, names []string, sessions map[string]time.Time, sessionsKnown bool, ready map[string]bool, recs *dashLandings, seatCache *dashSeatCache, dispatches *events.DispatchReader, lister sessionLister) {
	dashboardPolecatsWith(defaultDashPolecatReaders, s, townRoot, names, sessions, sessionsKnown, ready, recs, seatCache, dispatches, lister)
}

// dashPolecatReaders are the two reads dashboardPolecats makes before it
// builds the table: the records of the seats the daemon keeps, then the seats
// themselves. A test injects both, so a failed kept read is shown to leave the
// seats drawn and to hand the seats reader no kept set (gt-ddg96x).
type dashPolecatReaders struct {
	kept  func(townRoot string) (map[string]time.Time, error)
	seats func(townRoot string, names []string, sessionsKnown bool, cache *dashSeatCache, dispatches *events.DispatchReader, lister sessionLister, kept map[string]time.Time) ([]dashSeat, error)
}

// defaultDashPolecatReaders are the real reads: the daemon's seat-end records
// and the rigs' seats.
var defaultDashPolecatReaders = dashPolecatReaders{kept: dashKeptSeats, seats: dashSeats}

// dashboardPolecatsWith is dashboardPolecats with its two reads injected. A
// kept read that fails is named in KeptError and the seats are still read and
// drawn, with no kept set, so every kept seat shows as the recovery it is kept
// from; only a failed seats read hides the table (SeatsError).
func dashboardPolecatsWith(r dashPolecatReaders, s *dashboard.Summary, townRoot string, names []string, sessions map[string]time.Time, sessionsKnown bool, ready map[string]bool, recs *dashLandings, seatCache *dashSeatCache, dispatches *events.DispatchReader, lister sessionLister) {
	kept, keptErr := r.kept(townRoot)
	if keptErr != nil {
		s.KeptError = keptErr.Error()
		kept = nil
	}
	seats, err := r.seats(townRoot, names, sessionsKnown, seatCache, dispatches, lister, kept)
	if err != nil {
		// The hub keeps the last good list and the page says unavailable rather
		// than "no polecats" (gt-q6h8e). The page names why (gt-6qc3nw.2).
		s.SeatsError = true
		s.SeatsWhy = dashWhy(err)
		return
	}
	s.Polecats = buildDashPolecats(dashPolecatInputs{
		Now: time.Now(), Seats: seats, Ready: ready, Sessions: sessions, SessionsKnown: sessionsKnown, Records: recs.get(),
	})
	if max, err := configuredSchedulerMaxPolecats(townRoot); err == nil && max > 0 {
		s.SeatsCap = &max
	}
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

// dashboardReadyPart fills the summary's ready-to-land fields from the read it
// is handed, and returns the ids of the beads waiting for the polecat rows. A
// read that fails keeps no count, so the page cannot draw a drained queue for
// it, and it names its reason (gt-q6h8e, gt-6qc3nw.2).
func dashboardReadyPart(s *dashboard.Summary, read func() (int, *time.Time, map[string]bool, error)) map[string]bool {
	n, oldest, ids, err := read()
	if err != nil {
		// The hub keeps the previous reading and the page marks it stale: an
		// unread queue must not read as an empty one (gt-q6h8e).
		s.ReadyError = true
		s.ReadyWhy = dashWhy(err)
		return map[string]bool{}
	}
	s.ReadyToLand = &n
	s.OldestReady = oldest
	return ids
}

// dashWhyMax is how many characters of a failed read's reason the page is sent.
const dashWhyMax = 200

// dashWhyNoText is the reason of a failed read whose error has no text to name,
// so a failure never reads as a success (gt-6qc3nw.2).
const dashWhyNoText = "read failed (no error text)"

// dashWhyScrub redacts secret-shaped text from a reason before it is logged or
// sent to the page.
var dashWhyScrub = scrub.Default()

// dashWhy is the reason a failed read names: the error's first line made safe
// by dashWhyText, or dashWhyNoText when that leaves nothing. A nil error is a
// success and names no reason (gt-6qc3nw.2).
func dashWhy(err error) string {
	if err == nil {
		return ""
	}
	line, _, _ := strings.Cut(err.Error(), "\n")
	if why := dashWhyText(line); why != "" {
		return why
	}
	return dashWhyNoText
}

// dashWhyText is the text a reason is logged and drawn at. Control characters
// go first, so the scrubber sees the text an operator would read. The scrubber
// then redacts it, and each path is cut to its file name: the page shows a
// reason beside a pane, so a directory layout does not belong in it. A URL is
// kept whole, since the scrubber has already masked its credentials and cutting
// it to a file name would drop its host. The result is held to dashWhyMax
// characters (gt-6qc3nw.2).
func dashWhyText(s string) string {
	var clean strings.Builder
	for _, r := range s {
		switch {
		case r == '\n':
			clean.WriteByte(' ')
		case r < 0x20, r == 0x7f:
			// Other control characters can rewrite the line that shows them.
		default:
			clean.WriteRune(r)
		}
	}
	redacted, _ := dashWhyScrub.Scrub(clean.String())
	fields := strings.Fields(redacted)
	for i, f := range fields {
		if strings.Contains(f, "/") && !strings.Contains(f, "://") {
			fields[i] = path.Base(f)
		}
	}
	why := []rune(strings.Join(fields, " "))
	if len(why) > dashWhyMax {
		why = why[:dashWhyMax]
	}
	return string(why)
}

// dashWhyLog logs a failed read's reason once per change, so a read that fails
// on every poll is one line in the log rather than one a minute. A read that
// comes back logs once more. The page gets the reason from the summary or the
// questions list; this only keeps the log quiet (gt-6qc3nw.2).
type dashWhyLog struct {
	logf func(format string, args ...any)

	mu sync.Mutex
	// last is the reason each source reported on its latest poll, "" when its
	// read succeeded.
	last map[string]string
}

// newDashWhyLog logs through the dashboard's logf.
func newDashWhyLog(logf func(format string, args ...any)) *dashWhyLog {
	return &dashWhyLog{logf: logf, last: map[string]string{}}
}

// note records what source's read reported on this poll: why is "" when the
// read succeeded. A nil log records nothing.
func (l *dashWhyLog) note(source, why string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.last[source] == why {
		return
	}
	l.last[source] = why
	if why == "" {
		l.logf("%s read is back", source)
		return
	}
	l.logf("%s read failed: %s", source, why)
}

// summary notes the seats and ready-to-land reads of one summary.
func (l *dashWhyLog) summary(s *dashboard.Summary) {
	l.note(dashboard.SourceSeats, s.SeatsWhy)
	l.note(dashboard.SourceReady, s.ReadyWhy)
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

// dashboardJevScorecard reads the cached scorecard that gt jev scorecard
// --refresh writes. The dashboard starts no process for it: the cache is the
// one source, so opening the page costs a file read rather than a score of
// every battery.
func dashboardJevScorecard(townRoot string) func() *dashboard.JevScorecard {
	return dashboardJevScorecardWith(townRoot, scorecard.Cache{})
}

// dashboardJevScorecardWith is dashboardJevScorecard over cache, so a test
// stands in for a file it cannot read. A cache that does not exist yet is a
// value with NoScorecard set, which the page answers with how to make one; any
// other read error is nil, which the hub shows as the scorecard unavailable
// and keeps the last good panel over.
func dashboardJevScorecardWith(townRoot string, cache scorecard.Cache) func() *dashboard.JevScorecard {
	return func() *dashboard.JevScorecard {
		doc, err := cache.Read(scorecard.CachePath(townRoot))
		if err != nil {
			if errors.Is(err, scorecard.ErrNoScorecard) {
				return &dashboard.JevScorecard{NoScorecard: true}
			}
			return nil
		}
		sc := &dashboard.JevScorecard{
			Batteries: make([]dashboard.JevBattery, 0, len(doc.Batteries)),
			Window:    doc.Since,
		}
		for _, block := range doc.Batteries {
			sc.Batteries = append(sc.Batteries, jevBatteryFromCache(block))
		}
		sc.Tally()
		return sc
	}
}

// jevBatteryFromCache folds one cache block into the panel's line. A block a
// failed refresh kept carries its report and the time it was scored; one with
// no report is a placeholder that has only its name and failure.
func jevBatteryFromCache(block scorecard.BatteryBlock) dashboard.JevBattery {
	b := dashboard.JevBattery{Name: block.Name, Failed: block.Failed, Kept: block.Kept}
	if block.Report == nil {
		return b
	}
	scored := block.ScoredAt
	b.ScoredAt = &scored
	b.Items = block.Report.Items
	b.Labeled = block.Report.Labeled
	b.Unanswered = block.Report.LinesSkipped
	if b.Name == dashboard.JevPackagesBattery {
		b.Unit = "packages"
	}
	for _, q := range block.Report.Questions {
		b.Questions = append(b.Questions, dashboard.JevQuestion{ID: q.ID, Verdict: q.Verdict, Labeled: q.Labeled, BaseRate: q.BaseRate, Precision: q.Precision})
	}
	return b
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

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
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/dashboard"
	"github.com/steveyegge/gastown/internal/doltserver"
	"github.com/steveyegge/gastown/internal/forgejo"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/rig"
	"github.com/steveyegge/gastown/internal/townhealth"
	"github.com/steveyegge/gastown/internal/townstatus"
	"github.com/steveyegge/gastown/internal/workspace"
)

var (
	dashboardPort         int
	dashboardBind         string
	dashboardSince        string
	dashboardSpendCmd     string
	dashboardForgejoRepos []string
	dashboardOpen         bool
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

The Forgejo panel reads the viewer role's token file
(~/.config/gt/forgejo-viewer.env) and lists the recent activity of every rig
cut over to Forgejo, every three minutes, GET only: the rigs whose
merge_queue.forgejo block names a remote, or the repos the token can see when
no rig has one. A rig repo the viewer's token cannot read is named in the panel
rather than left out, so a missing rig never reads as a quiet one.
--forgejo-repo owner/name reads exactly the repos named instead, and may be
repeated. With no token file the panel is left out, and a refresh that fails
shows the last good feed marked stale.

The Deploys block under the Cloud panel's findings reads those same repos'
deploy.yml and staging.yml runs — every run of either workflow, whatever its
event — with a landing's staging run tagged as one, each run's stages in the
order their needs give, refreshed every minute. It infers runner trouble from
the run itself: the runners API is owner-only and answers the viewer 403, so a
run nothing has picked up, and one that has not moved, are said as inferences.

The Cloud panel reads the report the cloud patrol writes — report.json with a
heartbeat beside it — from /Users/Shared/gt-cloud/reports, or from the
directory GT_CLOUD_REPORTS_DIR names. It shows when the patrol last ran,
whether that is overdue, the projects it watches and its open findings by
severity, and it writes nothing there. On a machine with no reports directory
the panel says so and is re-read at a slow interval rather than every minute.

The Report panel reads the overseer's own hourly report from
.runtime/overseer/reports under the town root: the newest file named for the
instant it was written, its text, and when the writes before it happened. An
hourly report that is late is marked, the text is drawn as text and never as
markup, and a directory the reader cannot read says so rather than reading as
one with no reports in it. The panel writes nothing there.

The Questions panel reads the town's open overseer-question beads with bd —
each one's question, the answer the overseer recommends, the work it holds up,
how long it has waited and whether Sloan has answered it — so he can see what is
waiting on him and answer it by commenting on the bead. The page draws it and
never writes: there is no form, and a question is closed from a terminal.

Lifecycle: the dashboard watches its own binary and the town registry. When make
install replaces the binary, or a rig is added to or removed from the town, the
dashboard restarts itself in place: same PID, same flags, same listening socket,
so the port never closes. Open pages reconnect and reload. If the new binary
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
	dashboardCmd.Flags().StringArrayVar(&dashboardForgejoRepos, "forgejo-repo", nil, "Repo (owner/name) the Forgejo panel reads; repeatable (default: every rig cut over to Forgejo)")
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
	ln, inherited, err := dashboardListener(addr, os.Getenv(dashboardListenFDEnv))
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go hub.Run(ctx)

	// A new gt binary on disk (make install) or a rig added to the town
	// restarts the dashboard in place; see the lifecycle note in the command
	// help.
	if exe, err := os.Executable(); err == nil && dashboardCanReexec {
		go superviseRestart(ctx, exe, dashboardRegistryPath(townRoot), ln, cmd.OutOrStdout())
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
		_ = exec.Command("open", url).Start()
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
	return filepath.Join(townRoot, ".runtime", "overseer", "reports")
}

// dashboardCloudChecksPath is where the devops rig's cloud-check plugin writes
// its status inside the town: one JSON file per run, overwritten each time.
func dashboardCloudChecksPath(townRoot string) string {
	return filepath.Join(townRoot, ".runtime", "cloud-check", "status.json")
}

// newDashboardHub wires a hub to the readers gt tail -f uses.
func newDashboardHub(townRoot string, cutoff time.Time, loc *time.Location, spendCmd string) (*dashboard.Hub, error) {
	beadReads := openTailBeads(townRoot)
	rigOfBead := landingRig(townRoot)
	// The deploy block reads the same repos through the same viewer client as
	// the Forgejo panel, so the two are wired together from one token read;
	// the staging ship reader dates the app rigs' landings off the same runs.
	forgejoFeed, forgejoDeploys, stagingShip := dashboardForgejoReaders(townRoot, dashboardForgejoRepos)
	deploys := dashboardDeploys(townRoot, rigOfBead, cutoff, stagingShip)
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
	models := newModelsReader(townRoot)
	tierSweeps := newTierSweepReader(townRoot)
	escalations := newDashEscalationReader(townRoot)
	backoff := newDashBackoff(townRoot)
	queue := newDashQueueReader(townRoot)
	cloud := dashboard.NewCloudReader(config.CloudReportsDir()).
		WithChecks(dashboard.NewCloudChecksReader(dashboardCloudChecksPath(townRoot)))
	reports := dashboard.NewReportsReader(dashboardReportsDir(townRoot))
	questions := newDashQuestionsReader(townRoot)
	loads := newDashLoads(townRoot, time.Now)
	return dashboard.NewHub(dashboard.Config{
		Feed:       feed,
		Summary:    func() dashboard.Summary { return dashboardSummary(townRoot, deploys, recs, seatCache) },
		Health:     func() dashboard.Health { return dashboardHealth(townRoot) },
		Machine:    dashboard.SampleMachine,
		Cloud:      func() *dashboard.Cloud { return cloud.Read(time.Now()) },
		Spend:      dashboardSpend(resolveSpendCmd(spendCmd)),
		OM:         func() *dashboard.OM { return om.read(time.Now()) },
		TierSweep:  func() *dashboard.TierSweep { return tierSweeps.read(time.Now()) },
		Forgejo:    forgejoFeed,
		Deploys:    forgejoDeploys,
		Reports:    func() *dashboard.Reports { return reports.Read(time.Now()) },
		ReportsDir: dashboardReportsDir(townRoot),
		Questions:  func() *dashboard.Questions { return questions.read(time.Now()) },
		Escalation: func() *dashboard.Escalations { return escalations.read(time.Now()) },
		Dispatch:   om.dispatch,
		Models:     models.read,
		Queue:      func() *dashboard.Queue { return queue.read(time.Now()) },
		Bead:       queue.detail,
		RigTheme:   newDashRigThemes(townRoot),
		Trend: func() *dashboard.Trend {
			now := time.Now()
			stages, rejects := om.trendInputs(trendWindowStart(now))
			tr := buildTrend(now, recs.get(), stages, rejects, loads.points())
			live := runningRows(om.running(now.Add(-trendHours*time.Hour)), rigOfBead, func(rig, id string) string {
				if issue := beadReads.issue(rig, id); issue != nil {
					return polecatOfAssignee(issue.Assignee)
				}
				return ""
			})
			title := func(rig, id string) string { return beadReads.title(rig, id) }
			recent := buildRecentLandings(now, recs.get(), stages, rejects, live, deploys.shipStatus, title, recentLandingRows)
			tr.Recent = withBackoffRows(recent, backoff.get(), title)
			return tr
		},
		LoadSample: loads.append,
	}), nil
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
func dashboardSummary(townRoot string, deploys *tailDeploys, recs *dashLandings, seatCache *dashSeatCache) dashboard.Summary {
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
	if seats, err := dashSeats(townRoot, names, sessionsKnown, seatCache); err == nil {
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

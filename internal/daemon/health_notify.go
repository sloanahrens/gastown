package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/townhealth"
	"github.com/steveyegge/gastown/internal/util"
)

// notifyCommandTimeout bounds the operator's command: a pager that hangs
// must cost one heartbeat, not the daemon.
const notifyCommandTimeout = 30 * time.Second

// notifyPipeGrace is how long a finished pager may leave its output pipe held
// open by a grandchild it spawned. Without it Wait blocks until that
// grandchild exits, which wedged a piped call for hours once (internal/notify).
const notifyPipeGrace = 5 * time.Second

// notifyBeadTimeout bounds filing the notice bead in the town database.
const notifyBeadTimeout = 30 * time.Second

// maxNotifyTitleLen is the longest notice title bd is asked to take: the
// signal line runs long, and the whole of it stays in the description.
const maxNotifyTitleLen = 200

// notifyBeadWriter is the slice of the beads client the notice is filed
// through. The notice belongs to the town database, where escalations do:
// the signal it records is the town's, not one rig's.
type notifyBeadWriter interface {
	Create(opts beads.CreateOptions) (*beads.Issue, error)
}

// notifyCrossing reports whether a move between two verdicts is one the
// operator hears about: green to red, or green to unknown. Green is the only
// state the signal leaves for the pager, so a degraded town never pages and a
// town that is already red pages once, on the tick it left green. Unknown
// counts because the signal could not be read, which is the state a pager
// most needs to hear about.
func notifyCrossing(prev, cur townhealth.Verdict) bool {
	return prev == townhealth.Green && (cur == townhealth.Red || cur == townhealth.VerdictUnknown)
}

// notifyArgv is the operator's command line with the signal appended as the
// last argument. Splitting on spaces keeps the command out of a shell; a
// command or argument that needs a space in it is a script. A command line
// with no program in it yields no argv — never the line standing alone where
// the program belongs.
func notifyArgv(command, line string) []string {
	fields := strings.Fields(command)
	if len(fields) == 0 {
		return nil
	}
	return append(fields, line)
}

// previousHealth is the report the previous tick left on disk, the baseline
// the transition notice compares against. A file that cannot be read falls
// back to the daemon's own copy of that report, so a file gone bad does not
// cost the town the page for a crossing it makes on the same tick. No
// baseline at all is a first tick: with nothing to compare, there is no
// crossing to announce and no pager to wake.
func (d *Daemon) previousHealth() *townhealth.Report {
	r, err := townhealth.Read(d.config.TownRoot)
	if err == nil {
		return &r
	}
	if !errors.Is(err, os.ErrNotExist) {
		d.logger.Printf("townhealth: reading the previous report: %v", err)
	}
	return d.lastTownHealth
}

// healthNotifyCommand is the operator's notify command, empty when none is
// configured. The health settings block is optional, and a block that does
// not load is already the report's own red config field.
func (d *Daemon) healthNotifyCommand() string {
	health := d.loadOperationalConfig().GetHealthSettings()
	if health == nil {
		return ""
	}
	return strings.TrimSpace(health.NotifyCommand)
}

// notifyHealthTransition is the notify-on-transition step (gt-s3rec.3): it
// runs the operator's one notify command — the pager behind
// `gt status --line`, config field operational.health.notify_command — once
// for a crossing of the signal from green into red or unknown, with the line
// as the command's last argument, and files one bead recording the crossing.
// Nothing else goes out: no nudge, no mail.
//
// prev is the report the last tick left on disk, so a daemon that restarted
// onto a non-green town keeps its baseline: it says nothing about a crossing
// the operator already heard, and catches one that happened while the daemon
// was down. A nil prev is a first tick with no baseline, which is no
// crossing. The health file is written before this runs, so a daemon that
// dies mid-page files nothing rather than paging twice on its next tick.
func (d *Daemon) notifyHealthTransition(prev *townhealth.Report, cur townhealth.Report, line string) {
	if prev == nil || !notifyCrossing(prev.Verdict, cur.Verdict) {
		return
	}
	argv := notifyArgv(d.healthNotifyCommand(), line)
	if len(argv) == 0 {
		// Nothing is configured to page the operator. Filing the bead
		// anyway would leave one open notice per crossing forever, for a
		// town that never asked to be told.
		return
	}
	runErr := d.runNotifyCommand(argv)
	if runErr != nil {
		d.logger.Printf("townhealth: notify command %s failed: %v", argv[0], runErr)
	}
	if err := d.fileHealthNotice(prev.Verdict, cur.Verdict, line, argv[0], runErr); err != nil {
		d.logger.Printf("townhealth: filing the %s->%s notice: %v", prev.Verdict, cur.Verdict, err)
		return
	}
	d.logger.Printf("townhealth: %s -> %s, ran %s", prev.Verdict, cur.Verdict, argv[0])
}

// runNotifyCommand runs argv, which already ends with the signal line, and
// folds the command's output into its error so a failure says why in the
// daemon log.
func (d *Daemon) runNotifyCommand(argv []string) error {
	if len(argv) == 0 {
		return fmt.Errorf("notify command is empty")
	}
	ctx, cancel := context.WithTimeout(context.Background(), notifyCommandTimeout)
	defer cancel()
	if d.notifyRun != nil {
		return d.notifyRun(ctx, argv)
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // G204: the operator's own configured command
	cmd.Dir = d.config.TownRoot
	// Its own process group, so the timeout reaps whatever the pager started
	// rather than leaving a stray child behind, and a grace period after it
	// for a grandchild still holding the output pipe.
	util.SetProcessGroup(cmd)
	cmd.WaitDelay = notifyPipeGrace
	out, err := cmd.CombinedOutput()
	if errors.Is(err, exec.ErrWaitDelay) && cmd.ProcessState != nil && cmd.ProcessState.Success() {
		// The pager succeeded; only a grandchild kept the pipe open.
		err = nil
	}
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// noticeBeads opens the client the notice is created through; nil is bd
// against the town's own database.
func (d *Daemon) noticeBeads() notifyBeadWriter {
	if d.openNoticeBeads != nil {
		return d.openNoticeBeads()
	}
	b := beads.NewPlain(d.config.TownRoot, bdMutationRoutingEnv(d.config.TownRoot), beads.WithBin(d.bdPathOrDefault()))
	return b.WithTimeout(notifyBeadTimeout)
}

// fileHealthNotice creates the one bead that records the crossing: the line
// the operator was paged with, and whether the page got through.
//
// Only the program name goes in, never the configured arguments: the notice
// is exported to the beads JSONL and pushed, and a pager that carries a token
// in an argument must not leave it there. What went wrong with a failed page
// is in the daemon log, which does not leave the host.
func (d *Daemon) fileHealthNotice(prev, cur townhealth.Verdict, line, program string, runErr error) error {
	outcome := "ran"
	if runErr != nil {
		outcome = "failed"
	}
	body := fmt.Sprintf("The town health signal crossed from %s to %s.\n\n  %s\n\nNotify command: %s\nOutcome: %s\n",
		prev, cur, line, program, outcome)
	_, err := d.noticeBeads().Create(beads.CreateOptions{
		Title:       notifyTitle(prev, cur, line),
		Labels:      []string{constants.LabelTownHealth},
		Priority:    2,
		Description: body,
		Actor:       daemonActor,
	})
	return err
}

// notifyTitle builds the notice's single-line title: which way the signal
// moved, then the line that was paged, collapsed to its first line and
// truncated to what bd takes as a title.
func notifyTitle(prev, cur townhealth.Verdict, line string) string {
	if idx := strings.IndexAny(line, "\r\n"); idx >= 0 {
		line = line[:idx]
	}
	title := fmt.Sprintf("townhealth %s->%s: %s", prev, cur, strings.TrimSpace(line))
	if runes := []rune(title); len(runes) > maxNotifyTitleLen {
		title = string(runes[:maxNotifyTitleLen-1]) + "…"
	}
	return title
}

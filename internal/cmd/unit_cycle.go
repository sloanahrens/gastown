package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/events"
	"github.com/steveyegge/gastown/internal/mail"
	"github.com/steveyegge/gastown/internal/protocol"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/style"
	"github.com/steveyegge/gastown/internal/tmux"
)

type unitMode int

const (
	unitSingle unitMode = iota // gt mq post-merge: the agent's per-MR chores move here
	unitBatch                  // gt mq batch run: HandleMRInfoSuccess already sent MERGED per member
)

type unitMR struct {
	ID, Branch, Worker, SourceIssue, Target string
}

type unitCycleParams struct {
	Rig                  string
	Mode                 unitMode
	RefinerySession      string // refinerySessionFor(Rig)
	WorkDir              string // refineryWorkDir(rig path): <rig>/refinery/rig
	MRs                  []unitMR
	MergeCommit          string
	LandedCommitAttested bool // --landed-commit was passed
	CycleEnabled         bool // merge_queue.cycle_session_after_merge
	CycleSuppressed      bool // gt mq post-merge --no-cycle: chores only, never close the patrol or respawn
	BatchErrored         bool // batch mode: the batch landed but result.Error is set; never cycle
}

// unitCycleDeps holds every side effect completeUnitAndCycle has, so tests
// never touch real mail, beads, git, patrol wisps or tmux.
type unitCycleDeps struct {
	Send        func(*mail.Message) error
	WaitSent    func()                          // blocks until Send's recipient notifications are delivered (bounded)
	ListInbox   func() ([]*mail.Message, error) // the refinery's own inbox
	Archive     func(id string) error
	AddComment  func(beadID, text string) error
	DeleteTemp  func(workDir string) error
	ClosePatrol func(summary string) error
	Respawn     func() error
	Escalate    func(fingerprint, severity, msg string)
	RecordCycle func() // cooldown timestamp + handoff marker + town log
	PaneSession func() (string, error)
	HandoffAge  func() (time.Duration, bool)
	Getenv      func(string) string
	Out         io.Writer
}

// errNoTempBranch is what DeleteTemp returns when there is no temp branch to
// delete: nothing to do, not a failure.
var errNoTempBranch = errors.New("no temp branch")

// unitEscalateTimeout bounds the `gt escalate` exec: the MERGED-failure
// escalation fires mostly when Dolt is down, and must not hang post-merge.
const unitEscalateTimeout = 30 * time.Second

// runBoundedEscalate runs `gt escalate` best-effort, bounded by
// unitEscalateTimeout: it is called after a merge has already landed, when a
// hung escalation must not hang post-merge. A failure is warned, never
// returned.
func runBoundedEscalate(reason, source, fingerprint, severity, msg string) {
	ctx, cancel := context.WithTimeout(context.Background(), unitEscalateTimeout)
	defer cancel()
	c := exec.CommandContext(ctx, "gt", "escalate",
		"--severity", severity,
		"--reason", reason,
		"--source", source,
		"--fingerprint", fingerprint,
		msg)
	if err := c.Run(); err != nil {
		if ctx.Err() != nil {
			err = fmt.Errorf("timed out after %v: %w", unitEscalateTimeout, err)
		}
		style.PrintWarning("escalation %s failed: %v", fingerprint, err)
	}
}

type unitCycleReport struct {
	Respawned bool
	SkipCause string // why the patrol and/or respawn steps did not run; empty when the pane respawned
}

// completeUnitAndCycle ends a landed unit (one MR, or one batch): it does the
// per-MR chores the refinery formula used to leave to the agent, then, only
// when the session will actually cycle (this rig's refinery in its own pane,
// flag on, batch clean, outside the cooldown), closes the patrol cycle and
// respawns the refinery pane in place so the next unit starts fresh.
// Every step is best-effort: a landed merge is never failed from here.
//
// Deliberately not called: cleanupMoleculeOnHandoff (it would close the
// patrol wisp step 2 just poured), sendHandoffMail (the refinery needs
// nothing from it) and enforceHandoffCooldown (it sleeps; the unit skips).
func completeUnitAndCycle(p unitCycleParams, d unitCycleDeps) unitCycleReport {
	ids := make([]string, 0, len(p.MRs))
	for _, mr := range p.MRs {
		ids = append(ids, mr.ID)
	}

	// 1. Chores. Always run: a human running post-merge by hand still wants them.
	inbox, inboxErr := d.ListInbox()
	sends := 0
	for _, mr := range p.MRs {
		if p.Mode == unitSingle && strings.HasPrefix(mr.Branch, "polecat/") {
			polecat := strings.TrimPrefix(mr.Worker, "polecats/")
			msg := protocol.NewMergedMessage(p.Rig, polecat, mr.Branch, mr.SourceIssue, mr.Target, p.MergeCommit)
			sends++
			if err := d.Send(msg); err != nil {
				fmt.Fprintf(d.Out, "  %s MERGED not sent for %s: %v\n", style.Error.Render("✗"), mr.ID, err)
				d.Escalate("refinery-unit-chore:"+p.Rig, "medium",
					fmt.Sprintf("MERGED to %s/witness failed for %s (%s): %v; the polecat worktree will not be reaped until it is sent", p.Rig, mr.ID, mr.Branch, err))
			} else {
				fmt.Fprintf(d.Out, "  %s MERGED sent to %s/witness\n", style.Success.Render("✓"), p.Rig)
			}
		}
		if inboxErr != nil {
			fmt.Fprintf(d.Out, "  %s MERGE_READY not archived for %s: inbox unreadable: %v\n", style.Error.Render("✗"), mr.ID, inboxErr)
		} else {
			archiveMergeReady(mr, inbox, d)
		}
		if p.Mode == unitSingle && p.LandedCommitAttested {
			text := fmt.Sprintf("post-merge: attested --landed-commit %s", p.MergeCommit)
			if err := d.AddComment(mr.ID, text); err != nil {
				fmt.Fprintf(d.Out, "  %s attestation comment failed on %s: %v\n", style.Error.Render("✗"), mr.ID, err)
			}
		}
	}
	// Send delivers the witness's wake-up nudge in the background; wait for
	// it (bounded by the router's idle timeout) before this process can exit
	// or respawn, or the nudge is lost and the witness sees MERGED only on
	// its next patrol pass.
	if sends > 0 && d.WaitSent != nil {
		d.WaitSent()
	}
	if p.Mode == unitSingle {
		if err := d.DeleteTemp(p.WorkDir); errors.Is(err, errNoTempBranch) {
			fmt.Fprintf(d.Out, "  %s no temp branch to delete\n", style.Dim.Render("○"))
		} else if err != nil {
			fmt.Fprintf(d.Out, "  %s temp branch not deleted: %v\n", style.Error.Render("✗"), err)
		} else {
			fmt.Fprintf(d.Out, "  %s temp branch deleted\n", style.Success.Render("✓"))
		}
	}

	// Steps 2 and 3 run together or not at all, and every gate is decided
	// before either starts. Closing the patrol wisp without respawning would
	// leave the live session holding a closed wisp id, so when the session
	// is kept it keeps its own patrol wisp, exactly as without this step.
	if cause := unitCycleSkipCause(p, d); cause != "" {
		return unitCycleReport{SkipCause: cause}
	}

	// 2. Close this patrol cycle and pour the next, so the successor starts clean.
	summary := fmt.Sprintf("unit landed: %s @ %s", strings.Join(ids, ","), p.MergeCommit)
	if err := d.ClosePatrol(summary); err != nil {
		fmt.Fprintf(d.Out, "  %s patrol cycle not closed: %v\n", style.Dim.Render("○"), err)
	}

	// 3. Respawn in place.
	fmt.Fprintf(d.Out, "  %s unit complete, respawning %s for the next unit\n", style.Bold.Render("🔄"), p.RefinerySession)
	d.RecordCycle()
	if err := d.Respawn(); err != nil {
		fmt.Fprintf(d.Out, "  %s respawn failed: %v (continuing in this session)\n", style.Error.Render("✗"), err)
		d.Escalate("refinery-respawn-failed:"+p.Rig, "medium",
			fmt.Sprintf("refinery %s could not respawn after %s: %v", p.RefinerySession, summary, err))
		return unitCycleReport{SkipCause: "respawn failed: " + err.Error()}
	}
	return unitCycleReport{Respawned: true}
}

// unitCycleSkipCause returns why this unit must not close the patrol wisp and
// respawn, or "" when it should. The role and pane guards come first: from
// any other caller a respawn would kill the caller's own pane.
func unitCycleSkipCause(p unitCycleParams, d unitCycleDeps) string {
	if cause := unitCycleCallerMismatch(p, d); cause != "" {
		return cause
	}
	if p.CycleSuppressed {
		return "cycle suppressed (--no-cycle)"
	}
	if !p.CycleEnabled {
		return "cycle disabled (merge_queue.cycle_session_after_merge off)"
	}
	if p.Mode == unitBatch && p.BatchErrored {
		return "batch had errors; the session stays to recover the failed members"
	}
	if age, ok := d.HandoffAge(); ok && age < constants.MinHandoffCooldown {
		return fmt.Sprintf("last handoff %v ago (< %v); the next unit cycles",
			age.Round(time.Second), constants.MinHandoffCooldown)
	}
	return ""
}

// unitCycleCallerMismatch returns why the caller is not this rig's refinery
// running in its own pane, or "" when it is.
func unitCycleCallerMismatch(p unitCycleParams, d unitCycleDeps) string {
	if role := d.Getenv("GT_ROLE"); role != p.Rig+"/refinery" {
		return fmt.Sprintf("caller is not %s/refinery (GT_ROLE=%q)", p.Rig, role)
	}
	if d.Getenv("TMUX_PANE") == "" {
		return "not inside tmux"
	}
	sess, err := d.PaneSession()
	if err != nil {
		return fmt.Sprintf("pane session unknown: %v", err)
	}
	if sess != p.RefinerySession {
		return fmt.Sprintf("pane is in %s, not %s", sess, p.RefinerySession)
	}
	return ""
}

// archiveMergeReady archives the MERGE_READY mail for mr, matched by its
// "Branch: <branch>" body line (protocol.formatMergeReadyBody).
func archiveMergeReady(mr unitMR, inbox []*mail.Message, d unitCycleDeps) {
	found := false
	if mr.Branch != "" {
		want := "Branch: " + mr.Branch
		for _, m := range inbox {
			if m == nil || !strings.HasPrefix(m.Subject, "MERGE_READY ") || !bodyHasLine(m.Body, want) {
				continue
			}
			found = true
			if err := d.Archive(m.ID); err != nil {
				fmt.Fprintf(d.Out, "  %s MERGE_READY %s not archived: %v\n", style.Error.Render("✗"), m.ID, err)
			} else {
				fmt.Fprintf(d.Out, "  %s MERGE_READY archived: %s\n", style.Success.Render("✓"), m.ID)
			}
		}
	}
	if !found {
		fmt.Fprintf(d.Out, "  %s no MERGE_READY mail for %s\n", style.Dim.Render("○"), mr.ID)
	}
}

func bodyHasLine(body, want string) bool {
	for _, line := range strings.Split(body, "\n") {
		if strings.TrimSpace(line) == want {
			return true
		}
	}
	return false
}

// defaultUnitCycleDeps wires the real mail, beads, git, patrol and tmux seams.
// out receives every line, including the patrol report's: the batch path
// passes stderr so `gt mq batch run --json` keeps stdout a single JSON document.
func defaultUnitCycleDeps(townRoot, beadsPath, workDir, refinerySession string, out io.Writer) unitCycleDeps {
	router := mail.NewRouter(townRoot)
	// MERGE_READY lives in the refinery's mailbox (<rig>/refinery), not the
	// caller's: a human running post-merge must still archive it.
	refineryMailbox := func() (*mail.Mailbox, error) {
		addr := sessionToGTRole(refinerySession)
		if addr == "" {
			return nil, fmt.Errorf("cannot resolve a mail address for session %s", refinerySession)
		}
		return getMailbox(addr)
	}
	return unitCycleDeps{
		Send:     router.Send,
		WaitSent: router.WaitPendingNotifications,
		ListInbox: func() ([]*mail.Message, error) {
			mb, err := refineryMailbox()
			if err != nil {
				return nil, err
			}
			return mb.List()
		},
		Archive: func(id string) error {
			mb, err := refineryMailbox()
			if err != nil {
				return err
			}
			// Delete is what `gt mail archive` does.
			return mb.Delete(id)
		},
		AddComment: func(id, text string) error { return beads.New(beadsPath).AddComment(id, text) },
		DeleteTemp: func(dir string) error {
			// Absent temp (merged-pr-sweep, a clean batch recovery) is
			// nothing to do, not a failure: rev-parse --quiet exits 1.
			probe := exec.Command("git", "rev-parse", "--verify", "--quiet", "refs/heads/temp")
			probe.Dir = dir
			if err := probe.Run(); err != nil {
				var exitErr *exec.ExitError
				if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
					return errNoTempBranch
				}
				return fmt.Errorf("checking for temp branch: %w", err)
			}
			// -D, not -d: temp is the disposable rehearsal branch and is never
			// itself merged, so -d can refuse it as "not fully merged".
			c := exec.Command("git", "branch", "-D", "temp")
			c.Dir = dir
			if b, err := c.CombinedOutput(); err != nil {
				return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(b)))
			}
			return nil
		},
		ClosePatrol: func(summary string) error {
			roleInfo, err := GetRole()
			if err != nil {
				return err
			}
			// drainNudges=false: queued nudges stay queued for the fresh session.
			return runPatrolReportFor(out, roleInfo, summary, "", false)
		},
		Respawn: func() error {
			pane := os.Getenv("TMUX_PANE")
			restartCmd, err := buildRestartCommandWithOpts(refinerySession, buildRestartCommandOpts{ContinueSession: false})
			if err != nil {
				return err
			}
			t := tmux.NewTmuxWithSocket(tmux.SocketFromEnv())
			updateSessionEnvForHandoff(t, refinerySession)
			return respawnOwnPane(t, refinerySession, pane, restartCmd)
		},
		Escalate: func(fp, sev, msg string) {
			runBoundedEscalate("refinery-unit-cycle", "refinery:unit-cycle", fp, sev, msg)
		},
		RecordCycle: func() {
			recordHandoffTimeIn(workDir)
			writeHandoffMarker(workDir, refinerySession, "unit-cycle")
			agent := sessionToGTRole(refinerySession)
			if agent == "" {
				agent = refinerySession
			}
			_ = LogHandoff(townRoot, agent, "unit-cycle")
			_ = events.LogFeed(events.TypeHandoff, agent, events.HandoffPayload("unit-cycle", true))
		},
		PaneSession: func() (string, error) { return tmuxSessionForPane(os.Getenv("TMUX_PANE")) },
		HandoffAge:  func() (time.Duration, bool) { return lastHandoffAge(workDir) },
		Getenv:      os.Getenv,
		Out:         out,
	}
}

// refineryWorkDir is where the refinery session runs and where its handoff
// runtime files live.
func refineryWorkDir(rigPath string) string { return filepath.Join(rigPath, "refinery", "rig") }

func refinerySessionFor(rigName string) string {
	return session.RefinerySessionName(session.PrefixFor(rigName))
}

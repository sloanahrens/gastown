package cmd

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/crew"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/mail"
	"github.com/steveyegge/gastown/internal/rig"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/style"
	"github.com/steveyegge/gastown/internal/tmux"
)

// CrewStatusItem represents detailed status for a crew worker.
type CrewStatusItem struct {
	Name         string   `json:"name"`
	Rig          string   `json:"rig"`
	Path         string   `json:"path"`
	Branch       string   `json:"branch"`
	HasSession   bool     `json:"has_session"`
	SessionID    string   `json:"session_id,omitempty"`
	GitClean     bool     `json:"git_clean"`
	GitModified  []string `json:"git_modified,omitempty"`
	GitUntracked []string `json:"git_untracked,omitempty"`
	// MailTotal and MailUnread are nil (JSON null) when the mailbox could not
	// be read; MailError then says why. A failed read never reports 0.
	MailTotal  *int   `json:"mail_total"`
	MailUnread *int   `json:"mail_unread"`
	MailError  string `json:"mail_error,omitempty"`
}

// crewMailCounter returns the total and unread message counts for a crew
// member's beads mailbox. It is a package var so tests can stub the mailbox
// without a live Dolt server.
var crewMailCounter = countCrewBeadsMail

// countCrewBeadsMail counts mail through the router's beads mailbox, the same
// store gt mail send writes to, rather than the legacy <clone>/mail inbox file.
func countCrewBeadsMail(address string) (total, unread int, err error) {
	workDir, err := findMailWorkDir()
	if err != nil {
		return 0, 0, fmt.Errorf("not in a Gas Town workspace: %w", err)
	}
	mailbox, err := mail.NewRouter(workDir, townRegistry()).GetMailbox(address)
	if err != nil {
		return 0, 0, fmt.Errorf("getting mailbox: %w", err)
	}
	return mailbox.Count()
}

func runCrewStatus(cmd *cobra.Command, args []string) error {
	reg := townRegistry()
	// Parse rig/name format before getting manager (e.g., "beads/emma" -> rig=beads, name=emma)
	var targetName string
	if len(args) > 0 {
		targetName = args[0]
		if rig, crewName, ok := parseRigSlashName(targetName); ok {
			if crewRig == "" {
				crewRig = rig
			}
			targetName = crewName
		} else if crewRig == "" {
			// Check if single arg (without "/") is a valid rig name
			// If so, show status for all crew in that rig
			if _, _, err := getRig(targetName); err == nil {
				crewRig = targetName
				targetName = "" // Show all crew in the rig
			}
		}
	}

	t := tmux.NewTmux()
	var items []CrewStatusItem

	if targetName == "" && crewRig == "" {
		rigs, err := getAllRigs()
		if err != nil {
			return err
		}

		for _, r := range rigs {
			rigItems, err := listCrewStatusItems(reg, r, t)
			if err != nil {
				fmt.Fprintf(os.Stderr, "warning: failed to list crew workers in %s: %v\n", r.Name, err)
				continue
			}
			items = append(items, rigItems...)
		}
	} else {
		crewMgr, r, err := getCrewManagerForMember(crewRig, targetName)
		if err != nil {
			return err
		}

		var workers []*crew.CrewWorker

		if targetName != "" {
			// Specific worker
			worker, err := crewMgr.Get(targetName)
			if err != nil {
				if err == crew.ErrCrewNotFound {
					return fmt.Errorf("crew workspace '%s' not found", targetName)
				}
				return fmt.Errorf("getting crew worker: %w", err)
			}
			workers = []*crew.CrewWorker{worker}
		} else {
			// All workers
			workers, err = crewMgr.List()
			if err != nil {
				return fmt.Errorf("listing crew workers: %w", err)
			}
		}

		items = append(items, buildCrewStatusItems(reg, r, workers, t)...)
	}

	return printCrewStatus(os.Stdout, items, crewJSON)
}

// printCrewStatus writes items to w as JSON or as the operator's status listing.
func printCrewStatus(w io.Writer, items []CrewStatusItem, asJSON bool) error {
	if asJSON {
		return encodeCrewJSON(w, items)
	}

	if len(items) == 0 {
		fmt.Fprintln(w, "No crew workspaces found.")
		return nil
	}

	for i, item := range items {
		if i > 0 {
			fmt.Fprintln(w)
		}

		sessionStatus := style.Dim.Render("○ stopped")
		if item.HasSession {
			sessionStatus = style.Bold.Render("● running")
		}

		fmt.Fprintf(w, "%s %s/%s\n", sessionStatus, item.Rig, item.Name)
		fmt.Fprintf(w, "  Path:   %s\n", item.Path)
		fmt.Fprintf(w, "  Branch: %s\n", item.Branch)

		if item.GitClean {
			fmt.Fprintf(w, "  Git:    %s\n", style.Dim.Render("clean"))
		} else {
			fmt.Fprintf(w, "  Git:    %s\n", style.Bold.Render("dirty"))
			if len(item.GitModified) > 0 {
				fmt.Fprintf(w, "          Modified: %s\n", strings.Join(item.GitModified, ", "))
			}
			if len(item.GitUntracked) > 0 {
				fmt.Fprintf(w, "          Untracked: %s\n", strings.Join(item.GitUntracked, ", "))
			}
		}

		fmt.Fprintf(w, "  Mail:   %s\n", crewMailSummary(item))
	}

	return nil
}

// crewMailCounts reads a crew member's beads mail counts. Non-fatal: a failed
// read returns nil counts and the error text, so the status shows the count as
// unknown instead of a false 0.
func crewMailCounts(rigName, crewName string) (total, unread *int, errText string) {
	t, u, err := crewMailCounter(fmt.Sprintf("%s/crew/%s", rigName, crewName))
	if err != nil {
		return nil, nil, err.Error()
	}
	return &t, &u, ""
}

// crewMailSummary renders the Mail line of the text output.
func crewMailSummary(item CrewStatusItem) string {
	switch {
	case item.MailTotal == nil || item.MailUnread == nil:
		return style.Dim.Render("unknown (" + item.MailError + ")")
	case *item.MailUnread > 0:
		return fmt.Sprintf("%d unread / %d total", *item.MailUnread, *item.MailTotal)
	default:
		return style.Dim.Render(fmt.Sprintf("%d messages", *item.MailTotal))
	}
}

func listCrewStatusItems(reg *session.PrefixRegistry, r *rig.Rig, t *tmux.Tmux) ([]CrewStatusItem, error) {
	crewMgr := crew.NewManager(r, git.NewGit(r.Path), reg)
	workers, err := crewMgr.List()
	if err != nil {
		return nil, fmt.Errorf("listing crew workers: %w", err)
	}
	return buildCrewStatusItems(reg, r, workers, t), nil
}

func buildCrewStatusItems(reg *session.PrefixRegistry, r *rig.Rig, workers []*crew.CrewWorker, t *tmux.Tmux) []CrewStatusItem {
	items := make([]CrewStatusItem, 0, len(workers))

	for _, w := range workers {
		sessionID := crewSessionName(reg, r.Name, w.Name)
		hasSession, _ := t.HasSession(sessionID)

		// Git status
		crewGit := git.NewGit(w.ClonePath)
		gitStatus, _ := crewGit.Status()
		branch, _ := crewGit.CurrentBranch()

		gitClean := true
		var modified, untracked []string
		if gitStatus != nil {
			gitClean = gitStatus.Clean
			modified = append(gitStatus.Modified, gitStatus.Added...)
			modified = append(modified, gitStatus.Deleted...)
			untracked = gitStatus.Untracked
		}

		mailTotal, mailUnread, mailErr := crewMailCounts(r.Name, w.Name)

		item := CrewStatusItem{
			Name:         w.Name,
			Rig:          r.Name,
			Path:         w.ClonePath,
			Branch:       branch,
			HasSession:   hasSession,
			GitClean:     gitClean,
			GitModified:  modified,
			GitUntracked: untracked,
			MailTotal:    mailTotal,
			MailUnread:   mailUnread,
			MailError:    mailErr,
		}
		if hasSession {
			item.SessionID = sessionID
		}

		items = append(items, item)
	}

	return items
}

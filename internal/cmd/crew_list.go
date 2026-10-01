package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/crew"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/rig"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/style"
	"github.com/steveyegge/gastown/internal/tmux"
)

// CrewListItem represents a crew worker in list output.
type CrewListItem struct {
	Name       string `json:"name"`
	Rig        string `json:"rig"`
	Branch     string `json:"branch"`
	Path       string `json:"path"`
	HasSession bool   `json:"has_session"`
	GitClean   bool   `json:"git_clean"`
}

func runCrewList(cmd *cobra.Command, args []string) error {
	rigName, err := crewListScope(crewRig, crewListAll, args)
	if err != nil {
		return err
	}

	var rigs []*rig.Rig
	if crewListAll {
		allRigs, err := getAllRigs()
		if err != nil {
			return err
		}
		rigs = allRigs
	} else {
		_, r, err := getCrewManager(rigName)
		if err != nil {
			return err
		}
		rigs = []*rig.Rig{r}
	}

	t := tmux.NewTmux()
	probe := crewWorkerProbe{
		list: func(r *rig.Rig) ([]*crew.CrewWorker, error) {
			return crew.NewManager(r, git.NewGit(r.Path), townRegistry()).List()
		},
		hasSession: func(sessionID string) bool {
			has, _ := t.HasSession(sessionID)
			return has
		},
		gitClean: func(clonePath string) bool {
			status, err := git.NewGit(clonePath).Status()
			return err != nil || status.Clean
		},
	}
	return printCrewList(os.Stdout, crewListItems(townRegistry(), rigs, probe), crewJSON)
}

// crewListScope resolves the rig gt crew list covers from its --rig flag,
// --all and an optional positional rig: "" with all set means every rig, ""
// without it means the rig the cwd is in.
func crewListScope(rigFlag string, all bool, args []string) (string, error) {
	rigName := rigFlag
	// Accept positional rig argument: gt crew list <rig>
	if len(args) > 0 {
		if rigFlag != "" {
			return "", fmt.Errorf("cannot specify both positional rig argument and --rig flag")
		}
		rigName = args[0]
	}
	if all && rigName != "" {
		return "", fmt.Errorf("cannot use --all with a rig filter (--rig flag or positional argument)")
	}
	return rigName, nil
}

// crewWorkerProbe is how gt crew list reads a rig's crew workers, whether a
// worker's session is up, and whether its clone is clean (an unreadable
// status counts as clean).
type crewWorkerProbe struct {
	list       func(r *rig.Rig) ([]*crew.CrewWorker, error)
	hasSession func(sessionID string) bool
	gitClean   func(clonePath string) bool
}

// crewListItems lists every crew worker of rigs, warning about and skipping a
// rig whose workers cannot be listed.
func crewListItems(reg *session.PrefixRegistry, rigs []*rig.Rig, probe crewWorkerProbe) []CrewListItem {
	var items []CrewListItem
	for _, r := range rigs {
		workers, err := probe.list(r)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to list crew workers in %s: %v\n", r.Name, err)
			continue
		}
		for _, w := range workers {
			items = append(items, CrewListItem{
				Name:       w.Name,
				Rig:        r.Name,
				Branch:     w.Branch,
				Path:       w.ClonePath,
				HasSession: probe.hasSession(crewSessionName(reg, r.Name, w.Name)),
				GitClean:   probe.gitClean(w.ClonePath),
			})
		}
	}
	return items
}

// printCrewList writes items to w as JSON or as the operator's listing.
func printCrewList(w io.Writer, items []CrewListItem, asJSON bool) error {
	if len(items) == 0 {
		fmt.Fprintln(w, "No crew workspaces found.")
		return nil
	}

	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(items)
	}

	// Text output
	fmt.Fprintf(w, "%s\n\n", style.Bold.Render("Crew Workspaces"))
	for _, item := range items {
		status := style.Dim.Render("○")
		if item.HasSession {
			status = style.Bold.Render("●")
		}

		gitStatus := style.Dim.Render("clean")
		if !item.GitClean {
			gitStatus = style.Bold.Render("dirty")
		}

		fmt.Fprintf(w, "  %s %s/%s\n", status, item.Rig, item.Name)
		fmt.Fprintf(w, "    Branch: %s  Git: %s\n", item.Branch, gitStatus)
		fmt.Fprintf(w, "    %s\n", style.Dim.Render(item.Path))
	}

	return nil
}

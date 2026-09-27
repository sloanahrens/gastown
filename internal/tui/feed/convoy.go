package feed

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/lipgloss"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/util"
)

// convoyIDPattern validates convoy IDs.
var convoyIDPattern = regexp.MustCompile(`^hq-[a-zA-Z0-9-]+$`)

// Convoy represents a convoy's status for the dashboard
type Convoy struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Status    string    `json:"status"`
	Completed int       `json:"completed"`
	Total     int       `json:"total"`
	CreatedAt time.Time `json:"created_at"`
	ClosedAt  time.Time `json:"closed_at,omitempty"`
}

// MQEntry represents a single merge request in the merge queue
type MQEntry struct {
	ID      string // Bead ID (e.g., "gt-mr-abc")
	Branch  string // Source branch name
	Status  string // queued, merging, merged, failed
	Polecat string // Polecat that submitted (e.g., "nux")
	Rig     string // Which rig this MR belongs to
}

// ConvoyState holds all convoy data for the panel
type ConvoyState struct {
	InProgress []Convoy
	Landed     []Convoy
	MQEntries  []MQEntry
	LastUpdate time.Time
}

// FetchConvoys retrieves convoy status from town-level beads
func FetchConvoys(townRoot string) (*ConvoyState, error) {
	townBeads := filepath.Join(townRoot, ".beads")

	state := &ConvoyState{
		InProgress: make([]Convoy, 0),
		Landed:     make([]Convoy, 0),
		LastUpdate: time.Now(),
	}

	// Fetch open convoys. Filtered server-side by label so the response is
	// just the convoys, not the whole town's open beads.
	openConvoys, err := listConvoys(townBeads, "open", "")
	if err != nil {
		// Return the state with the error rather than swallowing it: the
		// feed's poll backoff keys on that error (gt-nzt0).
		return state, fmt.Errorf("list open convoys: %w", err)
	}

	// Fetch recently closed convoys (landed in last 24h). closedAfter is
	// applied server-side too — this used to fetch every closed issue in
	// the town (1800+ rows) just to filter down to a handful of convoys
	// closed today.
	cutoff := time.Now().Add(-24 * time.Hour)
	closedConvoys, closedErr := listConvoys(townBeads, "closed", cutoff.Format(time.RFC3339))

	// Enrich open and closed convoys together in one batch: gather every
	// tracked-issue ID first (one `bd dep list` per convoy, serial, and
	// usually served from cache — see trackedIssueIDs), then resolve all of
	// their statuses with a single `bd show` call. This is the "one batched
	// query, not N concurrent bd dep list calls per convoy" fix for
	// gt-05vk — a prior fix (gt-3ony) fanned this out across up to 4
	// goroutines per FetchConvoys call, which is exactly the concurrent bd
	// child pileup that starved every other bd caller town-wide. bd
	// contention is disk/fsync-bound, not CPU-bound, so that concurrency
	// never actually reduced wall-clock time — it just multiplied load.
	allItems := append(append([]convoyListItem{}, openConvoys...), closedConvoys...)
	enriched := enrichConvoys(townBeads, allItems)

	state.InProgress = enriched[:len(openConvoys)]
	// A failed closed-convoy listing stays tolerated — the guard below drops
	// the landed list and keeps in-progress — where reporting it as an error
	// would freeze the whole panel (gt-nzt0).
	if closedErr == nil {
		for _, convoy := range enriched[len(openConvoys):] {
			if !convoy.ClosedAt.IsZero() && convoy.ClosedAt.After(cutoff) {
				state.Landed = append(state.Landed, convoy)
			}
		}
	}

	// Sort: in-progress by created (oldest first), landed by closed (newest first)
	sort.Slice(state.InProgress, func(i, j int) bool {
		return state.InProgress[i].CreatedAt.Before(state.InProgress[j].CreatedAt)
	})
	sort.Slice(state.Landed, func(i, j int) bool {
		return state.Landed[i].ClosedAt.After(state.Landed[j].ClosedAt)
	})

	// Fetch merge queue entries from all rigs
	state.MQEntries = fetchMQEntries(townRoot)

	return state, nil
}

// listConvoys returns convoys with the given status. closedAfter, if
// non-empty (RFC3339), additionally restricts results to issues closed after
// that time — used to fetch only recently-landed convoys instead of every
// closed convoy the town has ever had.
//
// Filtering by --label=gt:convoy server-side is what keeps this call cheap:
// listing status=closed with no filter returns every closed issue in the
// town (thousands of rows, megabytes of JSON) just to find the handful that
// are convoys.
func listConvoys(beadsDir, status, closedAfter string) ([]convoyListItem, error) {
	listArgs := []string{"list", "--label=gt:convoy", "--status=" + status, "--json", "--limit=0"}
	if closedAfter != "" {
		listArgs = append(listArgs, "--closed-after="+closedAfter)
	}

	ctx, cancel := context.WithTimeout(context.Background(), constants.BdSubprocessTimeout)
	defer cancel()

	cmd := beads.CommandContextWithEnv(ctx, beadsDir, nil, listArgs...)
	util.SetDetachedProcessGroup(cmd)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout

	if err := cmd.Run(); err != nil {
		return nil, err
	}

	var rawItems []convoyListItem
	if err := json.Unmarshal(stdout.Bytes(), &rawItems); err != nil {
		return nil, err
	}

	items := make([]convoyListItem, 0, len(rawItems))
	for _, item := range rawItems {
		if item.IssueType == "convoy" || feedConvoyHasLabel(item.Labels, "gt:convoy") {
			items = append(items, item)
		}
	}
	return items, nil
}

// enrichConvoys resolves tracked-issue counts for every convoy in one batch.
// It fetches each convoy's tracked-issue IDs serially (one bd child at a
// time; trackedIssueIDs caches the result so most calls are free — see
// gt-05vk) and then resolves every distinct tracked issue's status with a
// single batched `bd show` call, instead of one `bd show` per convoy.
func enrichConvoys(beadsDir string, items []convoyListItem) []Convoy {
	result := make([]Convoy, len(items))
	perConvoyIDs := make([][]string, len(items))

	seen := make(map[string]bool)
	var allIDs []string
	for i, item := range items {
		ids := trackedIssueIDs(beadsDir, item.ID)
		perConvoyIDs[i] = ids
		for _, id := range ids {
			if !seen[id] {
				seen[id] = true
				allIDs = append(allIDs, id)
			}
		}
	}

	status := batchIssueStatus(allIDs)

	for i, item := range items {
		result[i] = buildConvoy(item, perConvoyIDs[i], status)
	}
	return result
}

type convoyListItem struct {
	ID        string   `json:"id"`
	Title     string   `json:"title"`
	Status    string   `json:"status"`
	CreatedAt string   `json:"created_at"`
	ClosedAt  string   `json:"closed_at,omitempty"`
	IssueType string   `json:"issue_type"`
	Labels    []string `json:"labels"`
}

func feedConvoyHasLabel(labels []string, target string) bool {
	for _, label := range labels {
		if label == target {
			return true
		}
	}
	return false
}

// buildConvoy assembles a Convoy from a listing item, its tracked-issue IDs,
// and a status map covering those IDs (see enrichConvoys).
func buildConvoy(item convoyListItem, trackedIDs []string, status map[string]string) Convoy {
	convoy := Convoy{
		ID:     item.ID,
		Title:  item.Title,
		Status: item.Status,
	}

	// Parse timestamps
	if t, err := time.Parse(time.RFC3339, item.CreatedAt); err == nil {
		convoy.CreatedAt = t
	} else if t, err := time.Parse("2006-01-02 15:04", item.CreatedAt); err == nil {
		convoy.CreatedAt = t
	}
	if t, err := time.Parse(time.RFC3339, item.ClosedAt); err == nil {
		convoy.ClosedAt = t
	} else if t, err := time.Parse("2006-01-02 15:04", item.ClosedAt); err == nil {
		convoy.ClosedAt = t
	}

	convoy.Total = len(trackedIDs)
	for _, id := range trackedIDs {
		if status[id] == "closed" {
			convoy.Completed++
		}
	}

	return convoy
}

// Convoy panel styles
var (
	ConvoyPanelStyle = lipgloss.NewStyle().
				Border(lipgloss.RoundedBorder()).
				BorderForeground(colorDim).
				Padding(0, 1)

	ConvoyTitleStyle = lipgloss.NewStyle().
				Bold(true).
				Foreground(colorPrimary)

	ConvoySectionStyle = lipgloss.NewStyle().
				Foreground(colorDim).
				Bold(true)

	ConvoyIDStyle = lipgloss.NewStyle().
			Foreground(colorHighlight)

	ConvoyNameStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("15"))

	ConvoyProgressStyle = lipgloss.NewStyle().
				Foreground(colorSuccess)

	ConvoyLandedStyle = lipgloss.NewStyle().
				Foreground(colorSuccess).
				Bold(true)

	ConvoyAgeStyle = lipgloss.NewStyle().
			Foreground(colorDim)
)

// renderConvoyPanel renders the convoy status panel
func (m *Model) renderConvoyPanel() string {
	style := ConvoyPanelStyle
	if m.focusedPanel == PanelConvoy {
		style = FocusedBorderStyle
	}
	// Add title before content
	title := ConvoyTitleStyle.Render("🚚 Convoys")
	content := title + "\n" + m.convoyViewport.View()
	return style.Width(m.width - 2).Render(content)
}

// renderConvoys renders the convoy panel content
// renderConvoys renders the convoy status content.
// Caller must hold m.mu.
func (m *Model) renderConvoys() string {
	if m.convoyState == nil {
		return AgentIdleStyle.Render("Loading convoys...")
	}

	var lines []string

	// In Progress section
	lines = append(lines, ConvoySectionStyle.Render("IN PROGRESS"))
	if len(m.convoyState.InProgress) == 0 {
		lines = append(lines, "  "+AgentIdleStyle.Render("No active convoys"))
	} else {
		for _, c := range m.convoyState.InProgress {
			lines = append(lines, renderConvoyLine(c, false))
		}
	}

	lines = append(lines, "")

	// Recently Landed section
	lines = append(lines, ConvoySectionStyle.Render("RECENTLY LANDED (24h)"))
	if len(m.convoyState.Landed) == 0 {
		lines = append(lines, "  "+AgentIdleStyle.Render("No recent landings"))
	} else {
		for _, c := range m.convoyState.Landed {
			lines = append(lines, renderConvoyLine(c, true))
		}
	}

	// Merge Queue section
	lines = append(lines, "")
	lines = append(lines, MQTitleStyle.Render("⚙ Merge Queue"))
	if len(m.convoyState.MQEntries) == 0 {
		lines = append(lines, "  "+AgentIdleStyle.Render("No pending merges"))
	} else {
		for _, entry := range m.convoyState.MQEntries {
			lines = append(lines, renderMQLine(entry))
		}
	}

	return strings.Join(lines, "\n")
}

// renderConvoyLine renders a single convoy status line
func renderConvoyLine(c Convoy, landed bool) string {
	// Format: "  hq-xyz  Title       2/4 ●●○○" or "  hq-xyz  Title       ✓ 2h ago"
	id := ConvoyIDStyle.Render(c.ID)

	// Truncate title if too long (rune-safe to avoid splitting multi-byte UTF-8)
	title := c.Title
	if utf8.RuneCountInString(title) > 20 {
		runes := []rune(title)
		title = string(runes[:17]) + "..."
	}
	title = ConvoyNameStyle.Render(title)

	if landed {
		// Show checkmark and time since landing
		age := formatAge(time.Since(c.ClosedAt))
		status := ConvoyLandedStyle.Render("✓") + " " + ConvoyAgeStyle.Render(age+" ago")
		return fmt.Sprintf("  %s  %-20s  %s", id, title, status)
	}

	// Show progress bar
	progress := renderProgressBar(c.Completed, c.Total)
	count := ConvoyProgressStyle.Render(fmt.Sprintf("%d/%d", c.Completed, c.Total))
	return fmt.Sprintf("  %s  %-20s  %s %s", id, title, count, progress)
}

// renderMQLine renders a single merge queue entry
func renderMQLine(entry MQEntry) string {
	// Format: "  ⚙ polecat/nux  branch-name       merging"
	var statusStyle lipgloss.Style
	var statusIcon string
	switch entry.Status {
	case "merging":
		statusStyle = MQStatusMerging
		statusIcon = "⚙"
	case "queued":
		statusStyle = MQStatusQueued
		statusIcon = "○"
	case "merged":
		statusStyle = MQStatusMerged
		statusIcon = "✓"
	case "failed":
		statusStyle = MQStatusFailed
		statusIcon = "✗"
	default:
		statusStyle = MQStatusQueued
		statusIcon = "?"
	}

	// Truncate branch name if too long (rune-safe)
	branch := entry.Branch
	if utf8.RuneCountInString(branch) > 30 {
		runes := []rune(branch)
		branch = string(runes[:27]) + "..."
	}

	// Build the line
	status := statusStyle.Render(statusIcon + " " + entry.Status)
	branchPart := MQBranchStyle.Render(branch)

	polecatPart := ""
	if entry.Polecat != "" {
		polecatPart = MQPolecatStyle.Render(entry.Polecat)
	}

	return fmt.Sprintf("  %s  %-30s  %s", status, branchPart, polecatPart)
}

// MQ panel styles
var (
	MQTitleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(colorPrimary)

	MQStatusQueued = lipgloss.NewStyle().
			Foreground(colorDim)

	MQStatusMerging = lipgloss.NewStyle().
			Foreground(colorPrimary)

	MQStatusMerged = lipgloss.NewStyle().
			Foreground(colorSuccess).
			Bold(true)

	MQStatusFailed = lipgloss.NewStyle().
			Foreground(colorError).
			Bold(true)

	MQBranchStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("15"))

	MQPolecatStyle = lipgloss.NewStyle().
			Foreground(colorAccent)
)

// mqListItem represents a raw MR bead from bd list --json output
type mqListItem struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Status    string `json:"status"`
	CreatedBy string `json:"created_by,omitempty"`
	Assignee  string `json:"assignee,omitempty"`
}

// fetchMQEntries queries all rigs for merge-request beads, one rig at a time
// (see gt-05vk: fanning bd calls out across goroutines only piles up
// concurrent bd children against a single-threaded, disk/fsync-bound Dolt
// server — it doesn't reduce wall-clock time). Each rig is a single bd call
// for both open and in_progress statuses combined — this used to be two
// serial bd calls per rig (8 calls, ~1-1.6s each, for 4 rigs).
func fetchMQEntries(townRoot string) []MQEntry {
	// Load rigs config to discover rigs
	rigsConfigPath := constants.MayorRigsPath(townRoot)
	rigsConfig, err := config.LoadRigsConfig(rigsConfigPath)
	if err != nil {
		return nil
	}

	var entries []MQEntry
	for rigName := range rigsConfig.Rigs {
		rigPath := filepath.Join(townRoot, rigName)
		// Check rig directory exists
		if _, err := os.Stat(rigPath); err != nil {
			continue
		}
		items := listMQBeads(rigPath, "open,in_progress")
		for _, item := range items {
			entries = append(entries, mqItemToEntry(item, rigName))
		}
	}

	// Sort: in-progress (merging) first, then open (queued)
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Status != entries[j].Status {
			return entries[i].Status == "merging"
		}
		return entries[i].ID < entries[j].ID
	})

	return entries
}

// listMQBeads queries bd for merge-request beads with given status.
// status may be comma-separated (e.g. "open,in_progress") to fetch multiple
// statuses in a single call.
func listMQBeads(rigPath, status string) []mqListItem {
	ctx, cancel := context.WithTimeout(context.Background(), constants.BdSubprocessTimeout)
	defer cancel()

	cmd := beads.CommandContextWithEnv(ctx, rigPath, nil, "list",
		"--label=gt:merge-request",
		"--status="+status,
		"--json",
	)
	util.SetDetachedProcessGroup(cmd)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout

	if err := cmd.Run(); err != nil {
		return nil
	}

	var items []mqListItem
	if err := json.Unmarshal(stdout.Bytes(), &items); err != nil {
		return nil
	}
	return items
}

// mqItemToEntry converts a raw MQ bead to an MQEntry with display-friendly fields
func mqItemToEntry(item mqListItem, rigName string) MQEntry {
	entry := MQEntry{
		ID:  item.ID,
		Rig: rigName,
	}

	// Map bead status to display status
	switch item.Status {
	case "in_progress":
		entry.Status = "merging"
	case "open":
		entry.Status = "queued"
	case "closed":
		entry.Status = "merged"
	default:
		entry.Status = item.Status
	}

	// Extract branch name from title (MR beads typically titled with branch name)
	entry.Branch = item.Title
	if entry.Branch == "" {
		entry.Branch = item.ID
	}

	// Extract polecat name from assignee or created_by
	polecat := item.Assignee
	if polecat == "" {
		polecat = item.CreatedBy
	}
	// Shorten: "gastown/polecats/nux" -> "nux", "gastown/nux" -> "nux"
	if parts := strings.Split(polecat, "/"); len(parts) > 0 {
		polecat = parts[len(parts)-1]
	}
	entry.Polecat = polecat

	return entry
}

// renderProgressBar creates a simple progress bar: ●●○○
func renderProgressBar(completed, total int) string {
	if total == 0 {
		return ""
	}

	// Cap at 5 dots for display
	displayTotal := total
	if displayTotal > 5 {
		displayTotal = 5
	}

	filled := (completed * displayTotal) / total
	if filled > displayTotal {
		filled = displayTotal
	}

	bar := strings.Repeat("●", filled) + strings.Repeat("○", displayTotal-filled)
	return ConvoyProgressStyle.Render(bar)
}

package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/events"
	"github.com/steveyegge/gastown/internal/style"
	"github.com/steveyegge/gastown/internal/workspace"
)

// Log command flags
var (
	logTail   int
	logType   string
	logAgent  string
	logSince  string
	logFollow bool

	// log crash flags
	crashAgent    string
	crashSession  string
	crashExitCode int

	// log prune-worktree flags
	prunedWorktreeKind  string
	prunedWorktreeOwner string
	prunedWorktreePath  string
)

// lifecycleEventTypes are the event types gt log shows: the agent lifecycle
// vocabulary the retired townlog carried (gt-i057g). The events log holds every
// type the town emits — slot telemetry, scheduler dispatches, refinery merges —
// and gt log stays scoped to these so it remains the lifecycle view operators
// read it as rather than becoming a firehose.
var lifecycleEventTypes = map[string]bool{
	events.TypeSpawn:            true,
	events.TypeWake:             true,
	events.TypeNudge:            true,
	events.TypeHandoff:          true,
	events.TypeHandoffNoPersist: true,
	events.TypeDone:             true,
	events.TypeKill:             true,
	events.TypeSessionDeath:     true,
}

// logTypeAliases maps filter values that do not name their event type to the
// type that records them. A crash is one kind of session death. Every other
// documented value is spelled the same as its type and needs no entry.
var logTypeAliases = map[string]string{
	"crash":             events.TypeSessionDeath,
	"handoff-NOPERSIST": events.TypeHandoffNoPersist,
	"handoff-nopersist": events.TypeHandoffNoPersist,
}

var logCmd = &cobra.Command{
	Use:     "log",
	GroupID: GroupDiag,
	Short:   "View town activity log",
	Long: `View the centralized log of Gas Town agent lifecycle events.

The log is the town's events log (~/gt/.events.jsonl); gt log renders its
lifecycle events.

Events logged include:
  spawn   - new agent created
  wake    - agent session resumed by gt session start or restart
  nudge   - message delivered to an agent
  handoff - agent handed off to fresh session
  done    - agent finished work
  crash   - agent session ended unexpectedly (shown as session_death)
  kill    - agent session stopped on purpose

Examples:
  gt log                     # Show last 20 events
  gt log -n 50               # Show last 50 events
  gt log --type spawn        # Show only spawn events
  gt log --agent greenplace/    # Show events whose actor is in that rig
  gt log --since 1h          # Show events from last hour
  gt log -f                  # Follow log (like tail -f)`,
	RunE: runLog,
}

var logCrashCmd = &cobra.Command{
	Use:   "crash",
	Short: "Record a session exit (called by tmux pane-died hook)",
	Long: `Record an agent session exit as an event.

This command is called automatically by tmux when a pane exits. It's not
typically run manually.

The exit code determines how the exit is recorded:
  - Exit code 0: Expected exit, recorded for gt log but kept out of the feed
  - Exit code 130: Ctrl+C, recorded for gt log but kept out of the feed
  - Exit code non-zero: Crash, recorded as a feed-visible session death

Examples:
  gt log crash --agent greenplace/Toast --session gt-greenplace-Toast --exit-code 1`,
	RunE: runLogCrash,
}

var logPruneWorktreeCmd = &cobra.Command{
	Use:   "prune-worktree",
	Short: "Record a worktree-prune feed event (called by patrol cleanup steps)",
	Long: `Record a feed event for an orphaned worktree removed by a bash-executed
patrol cleanup step.

Go call sites emit a feed event directly via events.LogFeed whenever they
destroy a polecat or its worktree (e.g. nukePolecatFullWithOptions logs
TypeKill). Bash-executed cleanup steps — like the dead-dog-worktree step in
mol-deacon-patrol.formula.toml — have no Go call site to do the same from,
so this command gives them that primitive instead of leaving the removal
unlogged.

Examples:
  gt log prune-worktree --kind dog --owner rex --path ~/gt/deacon/dogs/rex/gastown`,
	RunE: runLogPruneWorktree,
}

func init() {
	logCmd.Flags().IntVarP(&logTail, "tail", "n", 20, "Number of events to show")
	logCmd.Flags().StringVarP(&logType, "type", "t", "", "Filter by event type (spawn,wake,nudge,handoff,done,crash,kill)")
	logCmd.Flags().StringVarP(&logAgent, "agent", "a", "", "Filter by actor prefix (e.g., gastown/, greenplace/crew/max)")
	logCmd.Flags().StringVar(&logSince, "since", "", "Show events since duration (e.g., 1h, 30m, 24h)")
	logCmd.Flags().BoolVarP(&logFollow, "follow", "f", false, "Follow log output (like tail -f)")

	// crash subcommand flags
	logCrashCmd.Flags().StringVar(&crashAgent, "agent", "", "Agent ID (e.g., greenplace/Toast)")
	logCrashCmd.Flags().StringVar(&crashSession, "session", "", "Tmux session name")
	logCrashCmd.Flags().IntVar(&crashExitCode, "exit-code", -1, "Exit code from pane")
	_ = logCrashCmd.MarkFlagRequired("agent")

	// prune-worktree subcommand flags
	logPruneWorktreeCmd.Flags().StringVar(&prunedWorktreeKind, "kind", "dog", "What kind of orphaned worktree this was")
	logPruneWorktreeCmd.Flags().StringVar(&prunedWorktreeOwner, "owner", "", "Name of the entity that owned the worktree (e.g. dog name)")
	logPruneWorktreeCmd.Flags().StringVar(&prunedWorktreePath, "path", "", "Filesystem path of the pruned worktree")
	_ = logPruneWorktreeCmd.MarkFlagRequired("path")

	logCmd.AddCommand(logCrashCmd)
	logCmd.AddCommand(logPruneWorktreeCmd)
	rootCmd.AddCommand(logCmd)
}

// logQuery is one gt log view: which type, actor and time window to show, and
// how many of the newest matches to keep.
type logQuery struct {
	Type  string
	Actor string
	Since time.Time
	Tail  int
}

func runLog(cmd *cobra.Command, args []string) error {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}

	eventsPath := filepath.Join(townRoot, events.EventsFile)

	if logFollow {
		return followLog(eventsPath)
	}

	if _, err := os.Stat(eventsPath); os.IsNotExist(err) {
		fmt.Printf("%s No events recorded yet\n", style.Dim.Render("○"))
		return nil
	}

	raw, err := events.Read(townRoot)
	if err != nil {
		return fmt.Errorf("reading events: %w", err)
	}
	if len(raw) == 0 {
		fmt.Printf("%s No events in log\n", style.Dim.Render("○"))
		return nil
	}

	query, err := buildLogQuery()
	if err != nil {
		return err
	}

	selected := filterLogEvents(raw, query)
	if len(selected) == 0 {
		fmt.Printf("%s No events match filter\n", style.Dim.Render("○"))
		return nil
	}

	writeLogEvents(os.Stdout, selected)
	return nil
}

// buildLogQuery turns the command's flags into a query.
func buildLogQuery() (logQuery, error) {
	query := logQuery{
		Type:  logType,
		Actor: logAgent,
		Tail:  logTail,
	}

	if logSince != "" {
		duration, err := time.ParseDuration(logSince)
		if err != nil {
			return logQuery{}, fmt.Errorf("invalid --since duration: %w", err)
		}
		query.Since = time.Now().Add(-duration)
	}

	return query, nil
}

// filterLogEvents applies a query to events, newest last. Events outside the
// lifecycle vocabulary are dropped first: they are not gt log's subject, and
// applying --type to them would suggest they are.
func filterLogEvents(raw []events.Event, query logQuery) []events.Event {
	wantType := query.Type
	if alias, ok := logTypeAliases[wantType]; ok {
		wantType = alias
	}

	var selected []events.Event
	for _, e := range raw {
		if !lifecycleEventTypes[e.Type] {
			continue
		}
		if wantType != "" && e.Type != wantType {
			continue
		}
		if query.Actor != "" && !strings.HasPrefix(e.Actor, query.Actor) {
			continue
		}
		if !query.Since.IsZero() && eventTime(e).Before(query.Since) {
			continue
		}
		selected = append(selected, e)
	}

	if query.Tail > 0 && len(selected) > query.Tail {
		selected = selected[len(selected)-query.Tail:]
	}
	return selected
}

// writeLogEvents prints one line per event.
func writeLogEvents(w io.Writer, evs []events.Event) {
	for _, e := range evs {
		fmt.Fprintln(w, renderEventLine(e))
	}
}

// followLog streams the events file, printing each new lifecycle event as it
// lands. It uses events.Tail rather than `tail -f` so it survives the daemon's
// prune rotating the file out from under it (claude-9jq).
func followLog(eventsPath string) error {
	tail, err := events.OpenTail(eventsPath)
	if err != nil {
		return err
	}
	defer tail.Close() //nolint:errcheck // read-only

	fmt.Printf("%s Following %s (Ctrl+C to stop)\n\n", style.Dim.Render("○"), eventsPath)

	ticker := time.NewTicker(eventsPollInterval)
	defer ticker.Stop()

	for {
		lines, err := tail.Poll()
		if err != nil {
			return err
		}
		for _, line := range lines {
			if rendered, ok := renderRawEventLine(line); ok {
				fmt.Println(rendered)
			}
		}
		<-ticker.C
	}
}

// renderRawEventLine renders one line of the events file, reporting false when
// the line is not a lifecycle event or is not a complete event at all.
func renderRawEventLine(line string) (string, bool) {
	var event events.Event
	if err := json.Unmarshal([]byte(strings.TrimSpace(line)), &event); err != nil {
		return "", false
	}
	if !lifecycleEventTypes[event.Type] {
		return "", false
	}
	return renderEventLine(event), true
}

// renderEventLine formats an event as an operator-readable line, keeping the
// phrasing the retired townlog used for the lifecycle types (gt-i057g):
// 2025-12-26 15:30:45 [spawn] gt spawned for gastown/shale
func renderEventLine(e events.Event) string {
	ts := eventTime(e).Format("2006-01-02 15:04:05")

	var typeStr string
	switch e.Type {
	case events.TypeSpawn:
		typeStr = style.Success.Render("[spawn]")
	case events.TypeWake:
		typeStr = style.Bold.Render("[wake]")
	case events.TypeNudge:
		typeStr = style.Dim.Render("[nudge]")
	case events.TypeHandoff:
		typeStr = style.Bold.Render("[handoff]")
	case events.TypeHandoffNoPersist:
		typeStr = style.Error.Render("[handoff-NOPERSIST]")
	case events.TypeDone:
		typeStr = style.Success.Render("[done]")
	case events.TypeKill:
		typeStr = style.Warning.Render("[kill]")
	case events.TypeSessionDeath:
		typeStr = style.Error.Render("[session_death]")
	default:
		typeStr = fmt.Sprintf("[%s]", e.Type)
	}

	return fmt.Sprintf("%s %s %s %s", style.Dim.Render(ts), typeStr, e.Actor, eventDetail(e))
}

// eventDetail renders the human-readable tail of an event line from its
// payload.
func eventDetail(e events.Event) string {
	switch e.Type {
	case events.TypeSpawn:
		rig, polecat := payloadString(e, "rig"), payloadString(e, "polecat")
		if rig != "" && polecat != "" {
			return fmt.Sprintf("spawned for %s/%s", rig, polecat)
		}
		return "spawned"
	case events.TypeWake:
		if context := payloadString(e, "context"); context != "" {
			return fmt.Sprintf("resumed (%s)", context)
		}
		return "resumed"
	case events.TypeNudge:
		message := truncateStr(payloadString(e, "reason"), 40)
		if target := payloadString(e, "target"); target != "" {
			return fmt.Sprintf("nudged %s with %q", target, message)
		}
		return fmt.Sprintf("nudged with %q", message)
	case events.TypeHandoff:
		if subject := payloadString(e, "subject"); subject != "" {
			return fmt.Sprintf("handed off (%s)", subject)
		}
		return "handed off"
	case events.TypeHandoffNoPersist:
		detail := payloadString(e, "subject")
		if errText := payloadString(e, "error"); errText != "" {
			if detail != "" {
				return fmt.Sprintf("handoff FAILED (%s): %s", detail, errText)
			}
			return fmt.Sprintf("handoff FAILED: %s", errText)
		}
		if detail != "" {
			return fmt.Sprintf("handoff FAILED (%s)", detail)
		}
		return "handoff FAILED"
	case events.TypeDone:
		if bead := payloadString(e, "bead"); bead != "" {
			return fmt.Sprintf("completed %s", bead)
		}
		return "completed work"
	case events.TypeKill:
		if reason := payloadString(e, "reason"); reason != "" {
			return fmt.Sprintf("killed (%s)", reason)
		}
		return "killed"
	case events.TypeSessionDeath:
		if reason := payloadString(e, "reason"); reason != "" {
			return fmt.Sprintf("exited (%s)", reason)
		}
		return "exited"
	default:
		return e.Type
	}
}

// payloadString reads a string field from an event payload.
func payloadString(e events.Event, key string) string {
	s, _ := e.Payload[key].(string)
	return s
}

// eventTime is an event's timestamp, or the zero time when it cannot be read.
func eventTime(e events.Event) time.Time {
	ts, err := time.Parse(time.RFC3339, e.Timestamp)
	if err != nil {
		return time.Time{}
	}
	return ts.Local()
}

func truncateStr(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-3] + "..."
}

// runLogCrash handles the "gt log crash" command from tmux pane-died hooks.
func runLogCrash(cmd *cobra.Command, args []string) error {
	townRoot, err := workspace.FindFromCwd()
	if err != nil || townRoot == "" {
		// Try to find town root from conventional location
		// This is called from tmux hook which may not have proper cwd
		home := os.Getenv("HOME")
		defaultRoot := home + "/gt"
		if _, statErr := os.Stat(defaultRoot + "/mayor"); statErr == nil {
			townRoot = defaultRoot
		}
		if townRoot == "" {
			return fmt.Errorf("cannot find town root (tried cwd and ~/gt)")
		}
	}
	return logCrash(townRoot, crashAgent, crashSession, crashExitCode)
}

// logCrash records a session exit from townRoot's pane-died hook.
//
// The exit class picks the visibility rather than the type. A crash is what the
// feed is for. A clean or interrupted exit is a session ending the way it was
// supposed to, and the hook fires on every one of them — putting those on the
// feed would append a session death beside every gt done. They are audit-only,
// which still leaves them in the log gt log reads.
func logCrash(townRoot, crashAgent, crashSession string, crashExitCode int) error {
	var reason, visibility string

	switch {
	case crashExitCode == 0:
		reason = "exited normally"
		visibility = events.VisibilityAudit
	case crashExitCode == 130:
		reason = fmt.Sprintf("interrupted (exit %d)", crashExitCode)
		visibility = events.VisibilityAudit
	default:
		reason = fmt.Sprintf("crashed with exit code %d", crashExitCode)
		visibility = events.VisibilityFeed
	}

	if crashSession == "" {
		crashSession = "unknown"
	}

	payload := events.SessionDeathPayload(crashSession, crashAgent, reason, "gt log crash")
	payload["exit_code"] = crashExitCode
	// LogTo (not the ambient Log) since we already have townRoot: the ambient
	// variant resolves from cwd and is a hard no-op under go test
	// (events.writeVia, gt-x9o/gt-lwi), regardless of any chdir here.
	return events.LogTo(townRoot, events.TypeSessionDeath, crashAgent, payload, visibility)
}

// runLogPruneWorktree handles "gt log prune-worktree", called by bash-executed
// patrol cleanup steps (e.g. the dead-dog-worktree step in
// mol-deacon-patrol.formula.toml) after they remove an orphaned worktree.
// Those steps have no Go call site to emit events.LogFeed from directly, so
// this command exists to give them the same durable, feed-visible record
// Go-side destructive paths already produce (e.g. TypeKill from
// nukePolecatFullWithOptions).
func runLogPruneWorktree(cmd *cobra.Command, args []string) error {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}

	return logPruneWorktree(townRoot, detectActor(), prunedWorktreeKind, prunedWorktreeOwner, prunedWorktreePath)
}

// logPruneWorktree records a removed worktree as a feed-visible event.
func logPruneWorktree(townRoot, actor, kind, owner, path string) error {
	payload := events.WorktreePrunePayload(kind, owner, path)
	return events.LogFeedTo(townRoot, events.TypeWorktreePrune, actor, payload)
}

// logSessionWake records an explicit agent-session resume. `gt session start`
// and `gt session restart` are the callers; the context is what each of them
// knew — the hooked issue, or who asked for the restart (gt-tcrgb, gt-i057g).
func logSessionWake(townRoot, agent, rig, context string) error {
	return events.LogFeedTo(townRoot, events.TypeWake, agent, events.WakePayload(rig, context))
}

// logSessionKill records an agent session stopped on purpose, by `gt session
// stop` or `gt crew stop` (gt-i057g).
func logSessionKill(townRoot, agent, rig, target, reason string) error {
	return events.LogFeedTo(townRoot, events.TypeKill, agent, events.KillPayload(rig, target, reason))
}

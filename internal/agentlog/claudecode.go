package agentlog

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/steveyegge/gastown/internal/config"
)

const (
	// claudeProjectsDir is the path under $HOME where Claude Code stores
	// projects — the pre-CLAUDE_CONFIG_DIR location, kept for fallback lookups.
	claudeProjectsDir = ".claude/projects"

	// claudeProjectsSubdir is the projects directory inside the resolved Claude
	// config dir (config.ClaudeConfigDir(), i.e. $CLAUDE_CONFIG_DIR or ~/.claude).
	claudeProjectsSubdir = "projects"

	// watchPollInterval is how often we poll for new JSONL content or files.
	watchPollInterval = 500 * time.Millisecond

	// watchFileTimeout is how long we wait for a JSONL file to appear after startup.
	watchFileTimeout = 30 * time.Second
)

// ClaudeCodeAdapter watches Claude Code JSONL conversation files.
//
// The adapter finds the most recently modified JSONL file created after the Gas
// Town session start time (since), tails it, and automatically switches to a
// newer file when a new Claude session starts for the same working directory.
// This handles Claude instances that are frequently created and destroyed.
type ClaudeCodeAdapter struct{}

func (a *ClaudeCodeAdapter) AgentType() string { return "claudecode" }

// Watch starts tailing the Claude Code JSONL log for sessionID.
// workDir is the agent's CWD and is used to locate the project hash directory
// under each config root that can hold one.
// since is the Gas Town session start time: only JSONL files modified at or
// after this time are considered, so unrelated Claude instances (user sessions,
// other Gas Town rigs) running in the same work dir are excluded.
// Pass zero since to watch any file regardless of age.
//
// When Claude exits and a new session starts (new JSONL file), Watch
// automatically switches to the new file within one poll interval (500ms).
func (a *ClaudeCodeAdapter) Watch(ctx context.Context, sessionID, workDir string, since time.Time) (<-chan AgentEvent, error) {
	projectDirs, err := claudeProjectDirsFor(workDir)
	if err != nil {
		return nil, fmt.Errorf("resolving project dir: %w", err)
	}

	ch := make(chan AgentEvent, 64)
	go func() {
		defer close(ch)

		// Loop indefinitely: find the active JSONL file, tail it, then switch when a newer
		// one appears (new Claude session). ctx cancellation is the only exit.
		// Timeouts from waitForNewestJSONL are retried so that agent restarts or late
		// session starts (JSONL file appearing after the 30s window) are picked up.
		var currentPath string
		for {
			if ctx.Err() != nil {
				return
			}
			jsonlPath, err := waitForNewestJSONL(ctx, projectDirs, since)
			if err != nil {
				// ctx was canceled — clean exit.
				if ctx.Err() != nil {
					return
				}
				// Timeout: no JSONL appeared in 30s. Claude may not have started yet
				// or the agent restarted. Reset `since` so we pick up any new file.
				since = time.Now().Add(-watchPollInterval)
				continue
			}

			currentPath = jsonlPath

			// Tail the file; returns when a newer file appears or ctx is done.
			tailJSONL(ctx, currentPath, projectDirs, since, sessionID, a.AgentType(), ch)

			if ctx.Err() != nil {
				return
			}
			// A newer file was detected — loop immediately to pick it up.
		}
	}()
	return ch, nil
}

// claudeProjectDirFor returns the primary Claude Code project directory for
// workDir — the configured config dir's, first of claudeProjectDirsFor.
//
// Formula: <config-dir>/projects/<hash>, where <hash> is the absolute path with
// every non-alphanumeric character replaced by '-'. Claude Code encodes "."
// and "_" as well as "/" this way — /Users/me/.claude becomes
// -Users-me--claude, and <town>/gastown/.repo.git becomes
// -…-gastown--repo-git. On Windows, backslashes are converted to forward
// slashes and the drive letter (e.g. "C:") is stripped before hashing.
func claudeProjectDirFor(workDir string) (string, error) {
	dirs, err := claudeProjectDirsFor(workDir)
	if err != nil {
		return "", err
	}
	return dirs[0], nil
}

// claudeProjectDirsFor returns every Claude Code project directory that can
// hold transcripts for workDir: the configured config dir's first, then the
// default ~/.claude one.
//
// The config dir is a property of the process, not of the worktree. A seat
// spawned with CLAUDE_CONFIG_DIR (…/gt/.claude-town) logs under that root,
// while a seat that inherits the default (~/.claude — the claude-sonnet preset
// needs the credentials only that root holds) logs under the other. Resolving
// against one root hides the other root's live sessions, which reads as
// activity_source=none and silently disables transcript-based liveness checks
// for that whole seat class (gt-jxfe).
func claudeProjectDirsFor(workDir string) ([]string, error) {
	abs, err := filepath.Abs(workDir)
	if err != nil {
		return nil, fmt.Errorf("resolving absolute path: %w", err)
	}
	// Normalize to forward slashes (no-op on Unix).
	normalized := filepath.ToSlash(abs)
	// Strip Windows drive letter prefix (e.g. "C:") so the hash matches
	// what Claude Code stores on Windows (hash starts with '-', not 'C:').
	if len(normalized) >= 2 && normalized[1] == ':' {
		normalized = normalized[2:]
	}
	hash := claudeProjectHash(normalized)

	configDir, err := config.ClaudeConfigDir()
	if err != nil {
		return nil, fmt.Errorf("resolving Claude config dir: %w", err)
	}
	dirs := []string{filepath.Join(configDir, claudeProjectsSubdir, hash)}

	// Second root: the pre-CLAUDE_CONFIG_DIR location. Identical to the first
	// when the variable is unset, so the duplicate is dropped rather than
	// scanned twice.
	if home, herr := os.UserHomeDir(); herr == nil {
		if legacy := filepath.Join(home, claudeProjectsDir, hash); legacy != dirs[0] {
			dirs = append(dirs, legacy)
		}
	}
	return dirs, nil
}

// claudeProjectHash encodes an absolute path the way Claude Code names its
// project directories: every character that is not a letter or digit becomes
// '-'.
func claudeProjectHash(normalizedPath string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return r
		}
		return '-'
	}, normalizedPath)
}

// ClaudeProjectDirFor returns the primary Claude Code project directory for
// workDir — the configured config dir's. Exported for callers that need to
// inspect an agent's conversation log directly (e.g. witness liveness checks,
// gt-xb27); use LatestTranscript to find a session's transcript across roots.
func ClaudeProjectDirFor(workDir string) (string, error) {
	return claudeProjectDirFor(workDir)
}

// LatestTranscript returns the most recently modified transcript (JSONL) for
// workDir across every root that can hold one, or "" when none does.
//
// Its modification time is the authoritative "last real work" timestamp for a
// Claude Code session: Claude appends to it on conversation events (tool calls,
// file writes, assistant messages) and not on terminal redraws, so it
// distinguishes a long turn from a stall — unlike pane scraping (gt-xb27).
// Callers that judge staleness must still check that the transcript they get
// postdates the session they are judging, since a reused worktree keeps its
// predecessor's transcripts.
func LatestTranscript(workDir string) (string, error) {
	dirs, err := claudeProjectDirsFor(workDir)
	if err != nil {
		return "", err
	}
	if path, ok := newestJSONLIn(dirs, time.Time{}); ok {
		return path, nil
	}
	return "", nil
}

// waitForNewestJSONL polls dirs until a qualifying .jsonl file appears.
// "Qualifying" means mod time >= since (or any file if since is zero).
// Returns the path of the most recently modified qualifying file.
func waitForNewestJSONL(ctx context.Context, dirs []string, since time.Time) (string, error) {
	deadline := time.Now().Add(watchFileTimeout)
	for {
		if path, ok := newestJSONLIn(dirs, since); ok {
			return path, nil
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("timeout: no JSONL file appeared in %s within %s",
				strings.Join(dirs, ", "), watchFileTimeout)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(watchPollInterval):
		}
	}
}

// newestJSONLIn returns the most recently modified .jsonl file across dirs
// whose modification time is >= since (skip if since is zero). A directory that
// cannot be read is skipped: the reason to search several roots is that any one
// of them may hold the live transcript.
func newestJSONLIn(dirs []string, since time.Time) (string, bool) {
	var bestPath string
	var bestTime time.Time
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !strings.HasSuffix(e.Name(), ".jsonl") {
				continue
			}
			info, err := e.Info()
			if err != nil {
				continue
			}
			// Skip files older than the Gas Town session start — they belong
			// to previous Claude sessions or unrelated Claude instances.
			if !since.IsZero() && info.ModTime().Before(since) {
				continue
			}
			if bestPath == "" || info.ModTime().After(bestTime) {
				bestPath = filepath.Join(dir, e.Name())
				bestTime = info.ModTime()
			}
		}
	}
	return bestPath, bestPath != ""
}

// nativeSessionIDFromPath extracts the Claude Code session UUID from a JSONL file path.
// The filename is <uuid>.jsonl, so we strip the extension.
func nativeSessionIDFromPath(path string) string {
	base := filepath.Base(path)
	return strings.TrimSuffix(base, ".jsonl")
}

// tailJSONL reads all existing lines in path then polls for new ones, emitting
// AgentEvents on ch. It returns (without closing ch) when:
//   - a newer JSONL file appears in dirs (new Claude session detected), or
//   - ctx is canceled.
//
// Callers loop back to waitForNewestJSONL after this returns to pick up the
// new session file. This handles Claude instances that are created and destroyed
// frequently: no events are lost because the file is tailed until we switch.
func tailJSONL(ctx context.Context, path string, dirs []string, since time.Time, sessionID, agentType string, ch chan<- AgentEvent) {
	nativeID := nativeSessionIDFromPath(path)

	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	reader := bufio.NewReaderSize(f, 256*1024)
	var partial strings.Builder

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		line, err := reader.ReadString('\n')
		if len(line) > 0 {
			partial.WriteString(line)
		}
		if err == nil || (err == io.EOF && strings.HasSuffix(partial.String(), "\n")) {
			fullLine := strings.TrimRight(partial.String(), "\r\n")
			partial.Reset()
			if fullLine != "" {
				for _, ev := range parseClaudeCodeLine(fullLine, sessionID, agentType, nativeID) {
					select {
					case ch <- ev:
					case <-ctx.Done():
						return
					}
				}
			}
		}
		if err == io.EOF {
			// At EOF: check every poll whether a newer file has appeared.
			// This detects new Claude sessions within one poll interval (500ms).
			if newer, ok := newestJSONLIn(dirs, since); ok && newer != path {
				return // newer Claude session detected — caller switches
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(watchPollInterval):
			}
		} else if err != nil {
			return // unexpected read error
		}
	}
}

// ── Claude Code JSONL structures ──────────────────────────────────────────────

// ccEntry is a top-level line in a Claude Code JSONL file.
type ccEntry struct {
	Type      string     `json:"type"`
	Message   *ccMessage `json:"message,omitempty"`
	Timestamp string     `json:"timestamp,omitempty"`
}

// ccMessage is the message field of a ccEntry.
type ccMessage struct {
	Role    string      `json:"role"`
	Content []ccContent `json:"content"`
	Usage   *ccUsage    `json:"usage,omitempty"`
}

// ccUsage holds Claude API token usage counts for an assistant turn.
type ccUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
}

// ccContent is one content block inside a ccMessage.
type ccContent struct {
	Type string `json:"type"`

	// text
	Text string `json:"text,omitempty"`

	// thinking (extended thinking models only)
	Thinking string `json:"thinking,omitempty"`

	// tool_use
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`

	// tool_result (content is a string in the simple case)
	Content string `json:"content,omitempty"`
}

// parseClaudeCodeLine parses one JSONL line and returns 0 or more AgentEvents.
func parseClaudeCodeLine(line, sessionID, agentType, nativeSessionID string) []AgentEvent {
	var entry ccEntry
	if err := json.Unmarshal([]byte(line), &entry); err != nil {
		return nil
	}
	// Only emit events for real conversation turns.
	if entry.Type != "assistant" && entry.Type != "user" {
		return nil
	}
	if entry.Message == nil {
		return nil
	}

	ts := time.Now()
	if entry.Timestamp != "" {
		if t, err := time.Parse(time.RFC3339, entry.Timestamp); err == nil {
			ts = t
		}
	}

	var events []AgentEvent
	for _, c := range entry.Message.Content {
		var eventType, content string
		switch c.Type {
		case "text":
			eventType = "text"
			content = c.Text
		case "thinking":
			eventType = "thinking"
			content = c.Thinking
		case "tool_use":
			eventType = "tool_use"
			// Log tool name + full JSON input.
			content = c.Name + ": " + string(c.Input)
		case "tool_result":
			eventType = "tool_result"
			content = c.Content
		default:
			continue
		}
		if content == "" {
			continue
		}
		events = append(events, AgentEvent{
			AgentType:       agentType,
			SessionID:       sessionID,
			NativeSessionID: nativeSessionID,
			EventType:       eventType,
			Role:            entry.Message.Role,
			Content:         content,
			Timestamp:       ts,
		})
	}

	// Emit a dedicated "usage" event once per assistant turn so token counts
	// are not duplicated across content blocks of the same message.
	// Check all four token fields: a cache-only turn has InputTokens == 0 and
	// OutputTokens == 0 but non-zero CacheReadInputTokens, which must still be
	// recorded for accurate cost accounting.
	if entry.Type == "assistant" && entry.Message.Usage != nil {
		u := entry.Message.Usage
		if u.InputTokens > 0 || u.OutputTokens > 0 || u.CacheReadInputTokens > 0 || u.CacheCreationInputTokens > 0 {
			events = append(events, AgentEvent{
				AgentType:           agentType,
				SessionID:           sessionID,
				NativeSessionID:     nativeSessionID,
				EventType:           "usage",
				Role:                "assistant",
				Timestamp:           ts,
				InputTokens:         u.InputTokens,
				OutputTokens:        u.OutputTokens,
				CacheReadTokens:     u.CacheReadInputTokens,
				CacheCreationTokens: u.CacheCreationInputTokens,
			})
		}
	}

	return events
}

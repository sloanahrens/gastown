// Package agentlog locates AI agent conversation logs (Claude Code JSONL
// transcripts) for a working directory.
package agentlog

import (
	"fmt"
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
)

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
	hash, err := claudeProjectHashFor(workDir)
	if err != nil {
		return nil, err
	}
	configDir, err := config.ClaudeConfigDir()
	if err != nil {
		return nil, fmt.Errorf("resolving Claude config dir: %w", err)
	}
	home, herr := os.UserHomeDir()
	if herr != nil {
		home = ""
	}
	return claudeProjectRoots(hash, configDir, home), nil
}

// claudeProjectHashFor returns the project-directory name Claude Code uses
// for workDir, from its absolute path.
func claudeProjectHashFor(workDir string) (string, error) {
	abs, err := filepath.Abs(workDir)
	if err != nil {
		return "", fmt.Errorf("resolving absolute path: %w", err)
	}
	// Normalize to forward slashes (no-op on Unix).
	normalized := filepath.ToSlash(abs)
	// Strip Windows drive letter prefix (e.g. "C:") so the hash matches
	// what Claude Code stores on Windows (hash starts with '-', not 'C:').
	if len(normalized) >= 2 && normalized[1] == ':' {
		normalized = normalized[2:]
	}
	return claudeProjectHash(normalized), nil
}

// claudeProjectRoots returns the project directories for hash under the
// resolved Claude config dir and, when home is known (non-empty), under the
// pre-CLAUDE_CONFIG_DIR ~/.claude.
func claudeProjectRoots(hash, configDir, home string) []string {
	dirs := []string{filepath.Join(configDir, claudeProjectsSubdir, hash)}

	// Second root: the pre-CLAUDE_CONFIG_DIR location. Identical to the first
	// when the variable is unset, so the duplicate is dropped rather than
	// scanned twice.
	if home != "" {
		if legacy := filepath.Join(home, claudeProjectsDir, hash); legacy != dirs[0] {
			dirs = append(dirs, legacy)
		}
	}
	return dirs
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
	return latestTranscriptIn(dirs), nil
}

// latestTranscriptIn is LatestTranscript over already-resolved project dirs:
// the most recently modified .jsonl file across dirs, or "" when none holds
// one. A directory that cannot be read is skipped: the reason to search
// several roots is that any one of them may hold the live transcript.
func latestTranscriptIn(dirs []string) string {
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
			if bestPath == "" || info.ModTime().After(bestTime) {
				bestPath = filepath.Join(dir, e.Name())
				bestTime = info.ModTime()
			}
		}
	}
	return bestPath
}

package agentlog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/config"
)

// testEpoch dates every transcript fixture in this package.
var testEpoch = time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)

func TestClaudeProjectDirFor(t *testing.T) {
	t.Parallel()
	configDir := filepath.Join(t.TempDir(), "claude-town")

	// Every non-alphanumeric character becomes '-', so the leading slash does,
	// and so do the dots and underscores Claude Code encodes that way.
	// e.g., /some/work/.dir_name → <config>/projects/-some-work--dir-name
	input := "/some/work/.dir_name"
	wantSuffix := "-some-work--dir-name"
	wantDir := filepath.Join(configDir, claudeProjectsSubdir, wantSuffix)

	hash, err := claudeProjectHashFor(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := claudeProjectRoots(hash, configDir, t.TempDir())[0]; got != wantDir {
		t.Errorf("primary project dir for %q = %q, want %q", input, got, wantDir)
	}
}

// TestClaudeProjectHashFor pins that a relative work dir is hashed by its
// absolute path, as Claude Code names the directory after its cwd.
func TestClaudeProjectHashFor(t *testing.T) {
	t.Parallel()
	abs, err := filepath.Abs("rel/dir")
	if err != nil {
		t.Fatal(err)
	}
	got, err := claudeProjectHashFor("rel/dir")
	if err != nil {
		t.Fatalf("claudeProjectHashFor: %v", err)
	}
	if want := claudeProjectHash(filepath.ToSlash(abs)); got != want {
		t.Errorf("claudeProjectHashFor(rel/dir) = %q, want %q", got, want)
	}
}

// TestClaudeProjectHash pins the encoding against directory names observed on
// disk, including the dot-bearing one Gas Town itself creates (.repo.git).
func TestClaudeProjectHash(t *testing.T) {
	t.Parallel()
	tests := []struct {
		path string
		want string
	}{
		{"/Users/me/gt", "-Users-me-gt"},
		{"/Users/me/.claude", "-Users-me--claude"},
		{"/Users/me/gt/gastown/.repo.git", "-Users-me-gt-gastown--repo-git"},
		{"/Users/me/gt/gastown/polecats/topaz/gastown", "-Users-me-gt-gastown-polecats-topaz-gastown"},
	}
	for _, tt := range tests {
		if got := claudeProjectHash(tt.path); got != tt.want {
			t.Errorf("claudeProjectHash(%q) = %q, want %q", tt.path, got, tt.want)
		}
	}
}

// roots returns the project dirs for workDir under configDir and home.
func roots(t *testing.T, workDir, configDir, home string) []string {
	t.Helper()
	hash, err := claudeProjectHashFor(workDir)
	if err != nil {
		t.Fatalf("claudeProjectHashFor(%q): %v", workDir, err)
	}
	return claudeProjectRoots(hash, configDir, home)
}

// TestLatestTranscript_ResolvesAgainstConfigDir is the gt-xb27 regression:
// with CLAUDE_CONFIG_DIR set (as Gas Town sets it town-wide), the transcript
// must be found there rather than in ~/.claude, or every live agent looks
// undated.
func TestLatestTranscript_ResolvesAgainstConfigDir(t *testing.T) {
	t.Parallel()
	configDir := t.TempDir()
	// Hermetic: the legacy root is searched too, so it must hold nothing here.
	home := t.TempDir()

	workDir := filepath.Join(t.TempDir(), "polecats", "topaz", "gastown")
	projectDir := filepath.Join(configDir, claudeProjectsSubdir, claudeProjectHash(filepath.ToSlash(workDir)))
	if err := os.MkdirAll(projectDir, 0755); err != nil {
		t.Fatalf("creating project dir: %v", err)
	}

	older := filepath.Join(projectDir, "aaaaaaaa-0000-0000-0000-000000000000.jsonl")
	newer := filepath.Join(projectDir, "bbbbbbbb-1111-1111-1111-111111111111.jsonl")
	writeTranscript(t, older, testEpoch.Add(-time.Hour))
	writeTranscript(t, newer, testEpoch)

	if got := latestTranscriptIn(roots(t, workDir, configDir, home)); got != newer {
		t.Errorf("LatestTranscript() = %q, want the newest transcript %q", got, newer)
	}
}

// TestLatestTranscript_NoTranscriptIsEmptyNotAnError keeps the absent case
// distinguishable from a failure: callers report "last activity unknown"
// rather than acting on it.
func TestLatestTranscript_NoTranscriptIsEmptyNotAnError(t *testing.T) {
	t.Parallel()
	dirs := roots(t, filepath.Join(t.TempDir(), "nothing-here"), t.TempDir(), t.TempDir())
	if got := latestTranscriptIn(dirs); got != "" {
		t.Errorf("LatestTranscript() = %q, want empty", got)
	}
}

// TestClaudeProjectDirsFor covers both roots, and their collapse to one when
// CLAUDE_CONFIG_DIR is unset (where the "configured" dir IS ~/.claude).
func TestClaudeProjectDirsFor(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	hash := "-some-work-dir"

	t.Run("config dir set: configured root first, legacy root second", func(t *testing.T) {
		t.Parallel()
		configDir := filepath.Join(t.TempDir(), "claude-town")

		got := claudeProjectRoots(hash, configDir, home)
		want := []string{
			filepath.Join(configDir, claudeProjectsSubdir, hash),
			filepath.Join(home, claudeProjectsDir, hash),
		}
		if len(got) != len(want) {
			t.Fatalf("claudeProjectDirsFor = %q, want %q", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("dirs[%d] = %q, want %q", i, got[i], want[i])
			}
		}
	})

	t.Run("config dir unset: one root, not the same path twice", func(t *testing.T) {
		t.Parallel()
		// config.ClaudeConfigDir resolves an unset CLAUDE_CONFIG_DIR to ~/.claude.
		got := claudeProjectRoots(hash, filepath.Join(home, ".claude"), home)
		want := []string{filepath.Join(home, claudeProjectsDir, hash)}
		if len(got) != len(want) || got[0] != want[0] {
			t.Errorf("claudeProjectDirsFor = %q, want %q", got, want)
		}
	})

	t.Run("no home dir: configured root only", func(t *testing.T) {
		t.Parallel()
		configDir := filepath.Join(t.TempDir(), "claude-town")
		got := claudeProjectRoots(hash, configDir, "")
		want := []string{filepath.Join(configDir, claudeProjectsSubdir, hash)}
		if len(got) != len(want) || got[0] != want[0] {
			t.Errorf("claudeProjectDirsFor = %q, want %q", got, want)
		}
	})
}

// TestClaudeProjectDirsFor_ResolvesProcessRoots pins the exported entry
// points to the process's own config dir and home.
func TestClaudeProjectDirsFor_ResolvesProcessRoots(t *testing.T) {
	t.Parallel()
	workDir := filepath.Join(t.TempDir(), "polecats", "onyx", "gastown")
	configDir, err := config.ClaudeConfigDir()
	if err != nil {
		t.Fatalf("config.ClaudeConfigDir: %v", err)
	}
	home, _ := os.UserHomeDir()
	want := roots(t, workDir, configDir, home)

	got, err := claudeProjectDirsFor(workDir)
	if err != nil {
		t.Fatalf("claudeProjectDirsFor: %v", err)
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("claudeProjectDirsFor = %q, want %q", got, want)
	}
	if primary, err := ClaudeProjectDirFor(workDir); err != nil || primary != want[0] {
		t.Errorf("ClaudeProjectDirFor = %q, %v; want %q, nil", primary, err, want[0])
	}
	// workDir is fresh, so neither root holds a transcript for it.
	if latest, err := LatestTranscript(workDir); err != nil || latest != "" {
		t.Errorf("LatestTranscript = %q, %v; want \"\", nil", latest, err)
	}
}

// TestLatestTranscript_SearchesLegacyRoot is the gt-jxfe regression.
//
// A seat can log to ~/.claude (the claude-sonnet preset keeps its credentials
// there) while the town's CLAUDE_CONFIG_DIR root holds only a previous
// session's transcript. Picking the configured root's newest — which is what a
// single-root lookup does — hands the caller a transcript that predates the
// live session, and the session is reported as having no dated activity.
func TestLatestTranscript_SearchesLegacyRoot(t *testing.T) {
	t.Parallel()
	configDir := t.TempDir()
	home := t.TempDir()

	workDir := filepath.Join(t.TempDir(), "polecats", "emerald", "gastown")
	hash := claudeProjectHash(filepath.ToSlash(workDir))

	staleDir := filepath.Join(configDir, claudeProjectsSubdir, hash)
	liveDir := filepath.Join(home, claudeProjectsDir, hash)
	for _, dir := range []string{staleDir, liveDir} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatalf("creating project dir: %v", err)
		}
	}

	stale := filepath.Join(staleDir, "9b478215-0000-0000-0000-000000000000.jsonl")
	live := filepath.Join(liveDir, "a5aef6ba-1111-1111-1111-111111111111.jsonl")
	writeTranscript(t, stale, testEpoch.Add(-time.Hour))
	writeTranscript(t, live, testEpoch)

	if got := latestTranscriptIn(roots(t, workDir, configDir, home)); got != live {
		t.Errorf("LatestTranscript() = %q, want the live transcript in the legacy root %q", got, live)
	}
}

// TestLatestTranscript_NewestWinsAcrossRoots guards the direction of the
// multi-root pick: the configured root must not be preferred when the legacy
// root holds only an older transcript.
func TestLatestTranscript_NewestWinsAcrossRoots(t *testing.T) {
	t.Parallel()
	configDir := t.TempDir()
	home := t.TempDir()

	workDir := filepath.Join(t.TempDir(), "polecats", "quartz", "gastown")
	hash := claudeProjectHash(filepath.ToSlash(workDir))

	configuredDir := filepath.Join(configDir, claudeProjectsSubdir, hash)
	legacyDir := filepath.Join(home, claudeProjectsDir, hash)
	for _, dir := range []string{configuredDir, legacyDir} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatalf("creating project dir: %v", err)
		}
	}

	live := filepath.Join(configuredDir, "bbbbbbbb-1111-1111-1111-111111111111.jsonl")
	stale := filepath.Join(legacyDir, "aaaaaaaa-0000-0000-0000-000000000000.jsonl")
	writeTranscript(t, stale, testEpoch.Add(-time.Hour))
	writeTranscript(t, live, testEpoch)

	if got := latestTranscriptIn(roots(t, workDir, configDir, home)); got != live {
		t.Errorf("LatestTranscript() = %q, want the newest transcript across roots %q", got, live)
	}
}

// writeTranscript creates a transcript file with an explicit modification time.
func writeTranscript(t *testing.T, path string, mtime time.Time) {
	t.Helper()
	if err := os.WriteFile(path, []byte("{}\n"), 0644); err != nil {
		t.Fatalf("writing transcript: %v", err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatalf("aging transcript: %v", err)
	}
}

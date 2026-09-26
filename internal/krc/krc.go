// Package krc provides the Key Record Chronicle - configurable TTL management
// and auto-pruning for Level 0 ephemeral operational data.
//
// Per DOLT-STORAGE-DESIGN-V3.md, Level 0 includes:
// - Patrol heartbeats
// - Status checks
// - Session events
// - Operational noise that decays in forensic value
//
// KRC provides:
// - Configurable TTLs per event type (default: 7 days)
// - Auto-pruning on daemon startup and periodic intervals
// - Stats and visibility into ephemeral data lifecycle
package krc

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/gofrs/flock"
	"github.com/steveyegge/gastown/internal/events"
)

// Config defines TTL settings for ephemeral records.
type Config struct {
	// DefaultTTL is the fallback TTL for unspecified event types.
	// Default: 7 days
	DefaultTTL time.Duration `json:"default_ttl"`

	// TTLs maps event type patterns to their TTL durations.
	// Patterns support glob-style matching (e.g., "patrol_*" matches patrol_started, patrol_complete).
	// Special pattern "*" matches any event type.
	TTLs map[string]time.Duration `json:"ttls"`

	// PruneInterval is how often auto-pruning runs when the daemon is active.
	// Default: 1 hour
	PruneInterval time.Duration `json:"prune_interval"`

	// MinRetainCount keeps at least N events even if expired (for debugging).
	// Default: 100
	MinRetainCount int `json:"min_retain_count"`
}

// DefaultConfig returns the default KRC configuration.
func DefaultConfig() *Config {
	return &Config{
		DefaultTTL:    7 * 24 * time.Hour, // 7 days
		PruneInterval: 1 * time.Hour,
		MinRetainCount: 100,
		TTLs: map[string]time.Duration{
			// Patrol events decay fastest - low forensic value after hours
			"patrol_*":       24 * time.Hour,  // 1 day
			"polecat_checked": 24 * time.Hour, // 1 day
			"polecat_nudged":  24 * time.Hour, // 1 day

			// Session heartbeats
			"session_start": 3 * 24 * time.Hour, // 3 days
			"session_end":   3 * 24 * time.Hour, // 3 days

			// Operational events - moderate TTL
			"nudge":    3 * 24 * time.Hour,  // 3 days
			"handoff":  7 * 24 * time.Hour,  // 7 days

			// Higher-value events - longer TTL
			"mail":          30 * 24 * time.Hour, // 30 days
			"sling":         14 * 24 * time.Hour, // 14 days
			"done":          14 * 24 * time.Hour, // 14 days
			"hook":          14 * 24 * time.Hour, // 14 days
			"unhook":        14 * 24 * time.Hour, // 14 days

			// Death events - keep for forensics
			"session_death": 30 * 24 * time.Hour, // 30 days
			"mass_death":    90 * 24 * time.Hour, // 90 days

			// Merge events - important for audit
			"merge_*":       30 * 24 * time.Hour, // 30 days
		},
	}
}

// ConfigFile returns the path to the KRC config file.
func ConfigFile(townRoot string) string {
	return filepath.Join(townRoot, ".krc.yaml")
}

// LoadConfig loads KRC configuration from the town root.
// Returns DefaultConfig if no config file exists.
func LoadConfig(townRoot string) (*Config, error) {
	configPath := ConfigFile(townRoot)
	data, err := os.ReadFile(configPath)
	if os.IsNotExist(err) {
		return DefaultConfig(), nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading krc config: %w", err)
	}

	config := DefaultConfig()
	if err := json.Unmarshal(data, config); err != nil {
		return nil, fmt.Errorf("parsing krc config: %w", err)
	}

	return config, nil
}

// SaveConfig writes the KRC configuration to the town root.
func SaveConfig(townRoot string, config *Config) error {
	configPath := ConfigFile(townRoot)
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling krc config: %w", err)
	}
	if err := os.WriteFile(configPath, data, 0644); err != nil {
		return fmt.Errorf("writing krc config: %w", err)
	}
	return nil
}

// GetTTL returns the TTL for a given event type based on config.
// Matches patterns in order of specificity (exact match > glob > default).
func (c *Config) GetTTL(eventType string) time.Duration {
	// Check for exact match first
	if ttl, ok := c.TTLs[eventType]; ok {
		return ttl
	}

	// Check for glob patterns (sorted by length for specificity)
	var patterns []string
	for pattern := range c.TTLs {
		if strings.Contains(pattern, "*") {
			patterns = append(patterns, pattern)
		}
	}
	sort.Slice(patterns, func(i, j int) bool {
		return len(patterns[i]) > len(patterns[j])
	})

	for _, pattern := range patterns {
		if matchGlob(pattern, eventType) {
			return c.TTLs[pattern]
		}
	}

	return c.DefaultTTL
}

// matchGlob performs simple glob matching (only * is supported).
func matchGlob(pattern, s string) bool {
	// Convert glob to regex
	regexPattern := "^" + regexp.QuoteMeta(pattern) + "$"
	regexPattern = strings.ReplaceAll(regexPattern, `\*`, `.*`)
	re, err := regexp.Compile(regexPattern)
	if err != nil {
		return false
	}
	return re.MatchString(s)
}

// PruneResult contains statistics from a prune operation.
type PruneResult struct {
	EventsProcessed int            `json:"events_processed"`
	EventsPruned    int            `json:"events_pruned"`
	EventsRetained  int            `json:"events_retained"`
	BytesBefore     int64          `json:"bytes_before"`
	BytesAfter      int64          `json:"bytes_after"`
	PrunedByType    map[string]int `json:"pruned_by_type"`
	Duration        time.Duration  `json:"duration"`
}

// Pruner handles the pruning of expired events.
type Pruner struct {
	townRoot string
	config   *Config
}

// NewPruner creates a new Pruner instance.
func NewPruner(townRoot string, config *Config) *Pruner {
	return &Pruner{
		townRoot: townRoot,
		config:   config,
	}
}

// Prune removes expired events from the events and feed files.
// Each file is rewritten atomically (temp file + rename) under its writer
// lock, and only when something expired; see pruneFile.
func (p *Pruner) Prune() (*PruneResult, error) {
	return p.PruneContext(context.Background())
}

// PruneContext is Prune with a context that bounds the wait for each file's
// writer lock; canceling it abandons a prune still waiting for a lock.
func (p *Pruner) PruneContext(ctx context.Context) (*PruneResult, error) {
	start := time.Now()
	result := &PruneResult{
		PrunedByType: make(map[string]int),
	}

	// Prune events file
	eventsResult, err := p.pruneFile(ctx, filepath.Join(p.townRoot, events.EventsFile))
	if err != nil {
		return nil, fmt.Errorf("pruning events: %w", err)
	}
	result.EventsProcessed += eventsResult.EventsProcessed
	result.EventsPruned += eventsResult.EventsPruned
	result.EventsRetained += eventsResult.EventsRetained
	result.BytesBefore += eventsResult.BytesBefore
	result.BytesAfter += eventsResult.BytesAfter
	for k, v := range eventsResult.PrunedByType {
		result.PrunedByType[k] += v
	}

	// Prune feed file
	feedResult, err := p.pruneFile(ctx, filepath.Join(p.townRoot, ".feed.jsonl"))
	if err != nil {
		return nil, fmt.Errorf("pruning feed: %w", err)
	}
	result.EventsProcessed += feedResult.EventsProcessed
	result.EventsPruned += feedResult.EventsPruned
	result.EventsRetained += feedResult.EventsRetained
	result.BytesBefore += feedResult.BytesBefore
	result.BytesAfter += feedResult.BytesAfter
	for k, v := range feedResult.PrunedByType {
		result.PrunedByType[k] += v
	}

	result.Duration = time.Since(start)
	return result, nil
}

// pruneLockTimeout bounds how long a prune waits for a file's writer lock.
// Writers hold it for one append, so this only trips when a writer is wedged;
// the prune then fails and the next scheduled run retries.
var pruneLockTimeout = 30 * time.Second

// pruneFile prunes a single JSONL file.
//
// Append-safety guarantee: the pruner holds <file>.lock, the same flock that
// events.writeTo (for .events.jsonl) and the feed curator (for .feed.jsonl)
// take around each append, for the whole scan-and-rename. An append through
// either writer therefore lands entirely before the scan (and is kept or
// pruned on its TTL) or entirely after the rename (in the new file, because
// the writers open the path after taking the lock). A writer that bypasses
// the lock can still lose a line appended between the scan and the rename.
//
// The file is only replaced when at least one line is pruned. A rename gives
// the path a new inode, and every tail of the old one must notice and reopen
// (events.Tail does); a rotation that removes nothing is pure risk
// (claude-9jq: no-op rotations at each daemon start blinded await-signal).
func (p *Pruner) pruneFile(ctx context.Context, filePath string) (*PruneResult, error) {
	if _, err := os.Stat(filePath); os.IsNotExist(err) {
		return &PruneResult{PrunedByType: make(map[string]int)}, nil
	} else if err != nil {
		return nil, err
	}

	fl := flock.New(filePath + ".lock")
	ctx, cancel := context.WithTimeout(ctx, pruneLockTimeout)
	defer cancel()
	locked, err := fl.TryLockContext(ctx, 50*time.Millisecond)
	if err != nil || !locked {
		if err == nil {
			err = ctx.Err()
		}
		return nil, fmt.Errorf("locking %s for prune: %w", filePath, err)
	}
	defer fl.Unlock() //nolint:errcheck // best-effort unlock

	return p.pruneLocked(filePath)
}

// pruneLocked does the work of pruneFile. The caller holds the file's lock.
func (p *Pruner) pruneLocked(filePath string) (*PruneResult, error) {
	result := &PruneResult{
		PrunedByType: make(map[string]int),
	}

	info, err := os.Stat(filePath)
	if os.IsNotExist(err) {
		return result, nil
	}
	if err != nil {
		return nil, err
	}
	result.BytesBefore = info.Size()

	retained, err := p.scanRetained(filePath, result)
	if err != nil {
		return nil, err
	}
	result.EventsRetained = len(retained)

	if result.EventsPruned == 0 {
		// Nothing expired: leave the file (and its inode) alone.
		result.BytesAfter = result.BytesBefore
		return result, nil
	}

	bytesAfter, err := replaceWithLines(filePath, info.Mode().Perm(), retained)
	if err != nil {
		return nil, err
	}
	result.BytesAfter = bytesAfter
	return result, nil
}

// scanRetained reads filePath and returns the lines to keep, counting the
// processed and pruned events into result.
func (p *Pruner) scanRetained(filePath string, result *PruneResult) ([]string, error) {
	srcFile, err := os.Open(filePath) //nolint:gosec // G304: path is the town's own events/feed file
	if err != nil {
		return nil, err
	}
	defer srcFile.Close() //nolint:errcheck // read-only handle

	now := time.Now()
	scanner := bufio.NewScanner(srcFile)
	// Increase buffer size for potentially long lines
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	var retained []string
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		result.EventsProcessed++

		// Parse event to check TTL
		var event struct {
			Timestamp string `json:"ts"`
			Type      string `json:"type"`
		}
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			// Keep malformed lines (might be important)
			retained = append(retained, line)
			continue
		}

		// Parse timestamp
		ts, err := time.Parse(time.RFC3339, event.Timestamp)
		if err != nil {
			// Keep events with unparseable timestamps
			retained = append(retained, line)
			continue
		}

		// Check if event has expired
		ttl := p.config.GetTTL(event.Type)
		if now.Sub(ts) > ttl {
			result.EventsPruned++
			result.PrunedByType[event.Type]++
		} else {
			retained = append(retained, line)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scanning file: %w", err)
	}
	return retained, nil
}

// ReplaceTempSuffix is the suffix replaceWithLines appends to filePath while
// building its atomic-write temp, before renaming it into place. Exported so
// the hermetic tripwire (gt-lqri) can recognize this temp's exact shape
// instead of duplicating ".tmp" as a second, driftable source of truth.
const ReplaceTempSuffix = ".tmp"

// replaceWithLines atomically replaces filePath with lines (tmp + rename) and
// returns the new size.
//
// A hard crash (SIGKILL mid-prune) leaves <filePath>.tmp behind. The hermetic
// tripwire reports such residue as a leak (gt-lqri) instead of silently
// tolerating the .tmp suffix; the next prune removes it as its first step, so
// it is self-healing and its one reporting pass is the intended signal that a
// crash happened.
func replaceWithLines(filePath string, perm os.FileMode, lines []string) (size int64, err error) {
	tmpPath := filePath + ReplaceTempSuffix
	// Remove a .tmp left by a crashed prune and create a fresh one: opening
	// an existing file with O_TRUNC would keep its old permissions.
	if err := os.Remove(tmpPath); err != nil && !os.IsNotExist(err) {
		return 0, fmt.Errorf("removing stale temp file: %w", err)
	}
	tmpFile, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, perm) //nolint:gosec // G304: sibling of the town's own events/feed file
	if err != nil {
		return 0, err
	}
	defer func() {
		if err != nil {
			_ = tmpFile.Close()
			_ = os.Remove(tmpPath)
		}
	}()

	w := bufio.NewWriter(tmpFile)
	for _, line := range lines {
		if _, err = w.WriteString(line + "\n"); err != nil {
			return 0, err
		}
	}
	if err = w.Flush(); err != nil {
		return 0, err
	}
	tmpInfo, err := tmpFile.Stat()
	if err != nil {
		return 0, err
	}
	if err = tmpFile.Close(); err != nil {
		return 0, err
	}
	if err = os.Rename(tmpPath, filePath); err != nil {
		return 0, fmt.Errorf("replacing file: %w", err)
	}
	return tmpInfo.Size(), nil
}

// Stats contains statistics about the current ephemeral data.
type Stats struct {
	EventsFile   FileStats          `json:"events_file"`
	FeedFile     FileStats          `json:"feed_file"`
	ByType       map[string]int     `json:"by_type"`
	ByAge        map[string]int     `json:"by_age"` // "0-1d", "1-7d", "7-30d", "30d+"
	OldestEvent  time.Time          `json:"oldest_event"`
	NewestEvent  time.Time          `json:"newest_event"`
	TTLBreakdown map[string]TTLInfo `json:"ttl_breakdown"`
}

// FileStats contains statistics for a single file.
type FileStats struct {
	Path       string `json:"path"`
	Size       int64  `json:"size"`
	EventCount int    `json:"event_count"`
}

// TTLInfo contains TTL information for an event type.
type TTLInfo struct {
	TTL       time.Duration `json:"ttl"`
	Count     int           `json:"count"`
	Expired   int           `json:"expired"`
	ExpiresIn time.Duration `json:"expires_in"` // Time until oldest expires
}

// GetStats returns statistics about ephemeral data.
func GetStats(townRoot string, config *Config) (*Stats, error) {
	stats := &Stats{
		ByType:       make(map[string]int),
		ByAge:        make(map[string]int),
		TTLBreakdown: make(map[string]TTLInfo),
	}

	now := time.Now()

	// Process events file
	eventsPath := filepath.Join(townRoot, events.EventsFile)
	eventsStats, oldest, newest, err := getFileStats(eventsPath, config, now, stats.ByType, stats.ByAge, stats.TTLBreakdown)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	stats.EventsFile = eventsStats
	if !oldest.IsZero() {
		stats.OldestEvent = oldest
	}
	if !newest.IsZero() {
		stats.NewestEvent = newest
	}

	// Process feed file
	feedPath := filepath.Join(townRoot, ".feed.jsonl")
	feedStats, oldest2, newest2, err := getFileStats(feedPath, config, now, stats.ByType, stats.ByAge, stats.TTLBreakdown)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	stats.FeedFile = feedStats
	if !oldest2.IsZero() && (stats.OldestEvent.IsZero() || oldest2.Before(stats.OldestEvent)) {
		stats.OldestEvent = oldest2
	}
	if !newest2.IsZero() && newest2.After(stats.NewestEvent) {
		stats.NewestEvent = newest2
	}

	return stats, nil
}

func getFileStats(filePath string, config *Config, now time.Time, byType, byAge map[string]int, ttlBreakdown map[string]TTLInfo) (FileStats, time.Time, time.Time, error) {
	stats := FileStats{Path: filePath}
	var oldest, newest time.Time

	info, err := os.Stat(filePath)
	if err != nil {
		return stats, oldest, newest, err
	}
	stats.Size = info.Size()

	file, err := os.Open(filePath)
	if err != nil {
		return stats, oldest, newest, err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		stats.EventCount++

		var event struct {
			Timestamp string `json:"ts"`
			Type      string `json:"type"`
		}
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			continue
		}

		byType[event.Type]++

		ts, err := time.Parse(time.RFC3339, event.Timestamp)
		if err != nil {
			continue
		}

		// Track oldest/newest
		if oldest.IsZero() || ts.Before(oldest) {
			oldest = ts
		}
		if newest.IsZero() || ts.After(newest) {
			newest = ts
		}

		// Age bucket
		age := now.Sub(ts)
		switch {
		case age < 24*time.Hour:
			byAge["0-1d"]++
		case age < 7*24*time.Hour:
			byAge["1-7d"]++
		case age < 30*24*time.Hour:
			byAge["7-30d"]++
		default:
			byAge["30d+"]++
		}

		// TTL breakdown
		ttl := config.GetTTL(event.Type)
		info := ttlBreakdown[event.Type]
		info.TTL = ttl
		info.Count++
		if age > ttl {
			info.Expired++
		} else {
			// Calculate time until this event expires
			expiresIn := ttl - age
			if info.ExpiresIn == 0 || expiresIn < info.ExpiresIn {
				info.ExpiresIn = expiresIn
			}
		}
		ttlBreakdown[event.Type] = info
	}

	return stats, oldest, newest, scanner.Err()
}

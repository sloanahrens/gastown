// Package nudge provides non-destructive nudge delivery for Gas Town agents.
//
// The nudge queue allows messages to be delivered cooperatively: instead of
// sending text directly to a tmux session (which cancels in-flight tool calls),
// nudges are written to a queue directory and picked up by the agent's
// UserPromptSubmit hook at the next natural turn boundary.
//
// Queue location: <townRoot>/.runtime/nudge_queue/<session>/
// Each nudge is a JSON file named by timestamp for FIFO ordering.
package nudge

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/util"
)

// Priority levels for nudge delivery.
const (
	// PriorityNormal is the default — delivered at next turn boundary.
	PriorityNormal = "normal"
	// PriorityUrgent means the agent should handle this promptly.
	PriorityUrgent = "urgent"
)

// Operational limits and defaults.
// These are compiled-in fallbacks. Configurable via operational.nudge
// in settings/config.json (ZFC pattern).
const (
	// DefaultNormalTTL is the time-to-live for normal-priority nudges.
	DefaultNormalTTL = 30 * time.Minute

	// DefaultUrgentTTL is the time-to-live for urgent-priority nudges.
	DefaultUrgentTTL = 2 * time.Hour

	// MaxQueueDepth is the maximum number of pending nudges per session.
	MaxQueueDepth = 50

	// staleClaimThreshold is how long a .claimed file must be untouched
	// before Drain considers it orphaned (from a crashed drainer) and removes it.
	staleClaimThreshold = 5 * time.Minute
)

// Expired-nudge reporting (gt-oexm).
const (
	// expiredDirName is the subdirectory of a session queue that holds nudges
	// which reached ExpiresAt undelivered. A nudge is copied there before it
	// leaves the queue, so an expiry always leaves a record.
	expiredDirName = "expired"

	// expiredTraceRetention bounds that record's lifetime. The expiry is also
	// mailed out, so the file is a debugging aid rather than the only copy;
	// without a bound the directory would grow for the life of a session.
	expiredTraceRetention = 7 * 24 * time.Hour

	// expirySourceDrain and expirySourceRequeue are the ExpiryEvent.Source
	// values, naming the path that found the expiry.
	expirySourceDrain   = "drain"
	expirySourceRequeue = "requeue"
)

// nudgeConfig loads nudge-specific thresholds from town settings.
func nudgeConfig(townRoot string) *config.NudgeThresholds {
	return config.LoadOperationalConfig(townRoot).GetNudgeConfig()
}

// QueuedNudge represents a nudge message stored in the queue.
type QueuedNudge struct {
	Sender    string    `json:"sender"`
	Message   string    `json:"message"`
	Priority  string    `json:"priority"`
	Kind      string    `json:"kind,omitempty"`
	ThreadID  string    `json:"thread_id,omitempty"`
	Severity  string    `json:"severity,omitempty"`
	Timestamp time.Time `json:"timestamp"`
	ExpiresAt time.Time `json:"expires_at,omitempty"`
	// DeliverAfter, if non-zero, defers delivery until this time has passed.
	// Drain skips (but does not discard) the nudge until the deadline is met.
	DeliverAfter time.Time `json:"deliver_after,omitempty"`
	// Attempts counts how many times delivery of this nudge was attempted and
	// reported as failed, causing a requeue. Requeue increments it and drops
	// the nudge once it reaches the configured cap (gt-tmlu).
	Attempts int `json:"attempts,omitempty"`
	// ExpiredAt is stamped when the nudge reached ExpiresAt undelivered and was
	// recorded under the queue's expired/ directory (gt-oexm).
	ExpiredAt time.Time `json:"expired_at,omitempty"`
}

// ExpiryEvent reports one nudge that reached ExpiresAt without being delivered.
type ExpiryEvent struct {
	// TownRoot locates the workspace the queue belongs to.
	TownRoot string
	// Session is the tmux session the nudge was queued for.
	Session string
	// Nudge is the expired message, with ExpiredAt stamped.
	Nudge QueuedNudge
	// Source names the path that found the expiry: expirySourceDrain or
	// expirySourceRequeue.
	Source string
	// Trace is the file the message was preserved in, empty if that copy failed.
	Trace string
}

// ExpiryObserver receives every expiry event, installed by the command layer to
// deliver the message by mail. This package cannot reach the mail layer itself:
// internal/mail imports internal/nudge, not the reverse (gt-oexm).
var ExpiryObserver func(ExpiryEvent)

// queueDir returns the nudge queue directory for a given session.
// Path: <townRoot>/.runtime/nudge_queue/<session>/
func queueDir(townRoot, session string) string {
	// Sanitize session name for filesystem safety
	safe := strings.ReplaceAll(session, "/", "_")
	return filepath.Join(townRoot, constants.DirRuntime, "nudge_queue", safe)
}

// randomSuffix returns a short random hex string to disambiguate filenames
// when multiple processes enqueue within the same nanosecond.
func randomSuffix() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// Enqueue writes a nudge to the queue for the given session.
// The nudge will be picked up by the agent's hook at the next turn boundary.
// Returns an error if the queue is full (MaxQueueDepth reached).
func Enqueue(townRoot, session string, nudge QueuedNudge) error {
	dir := queueDir(townRoot, session)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("creating nudge queue dir: %w", err)
	}

	// Check queue depth before writing to prevent runaway senders.
	maxDepth := nudgeConfig(townRoot).MaxQueueDepthV()
	pending, _ := Pending(townRoot, session)
	if pending >= maxDepth {
		return fmt.Errorf("nudge queue for %s is full (%d/%d pending)", session, pending, maxDepth)
	}

	if nudge.Timestamp.IsZero() {
		nudge.Timestamp = time.Now()
	}
	if nudge.Priority == "" {
		nudge.Priority = PriorityNormal
	}

	// Set expiry if not already specified by the caller.
	if nudge.ExpiresAt.IsZero() {
		switch nudge.Priority {
		case PriorityUrgent:
			nudge.ExpiresAt = nudge.Timestamp.Add(DefaultUrgentTTL)
		default:
			nudge.ExpiresAt = nudge.Timestamp.Add(DefaultNormalTTL)
		}
	}

	data, err := json.MarshalIndent(nudge, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling nudge: %w", err)
	}

	// Use nanosecond timestamp + random suffix for unique, ordered filenames.
	// The random suffix prevents collisions when multiple agents enqueue
	// nudges for the same session within the same nanosecond.
	filename := fmt.Sprintf("%d-%s.json", nudge.Timestamp.UnixNano(), randomSuffix())
	path := filepath.Join(dir, filename)

	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("writing nudge to queue: %w", err)
	}

	return nil
}

// Requeue writes previously drained nudges back to the queue for later delivery.
// Existing timestamps are preserved so FIFO ordering remains stable relative to
// one another. A nudge that has expired meanwhile is not requeued, and is
// reported through reportExpiry rather than dropped in silence (gt-oexm).
//
// Requeue is bounded (gt-tmlu). A failed injection is not proof that the nudge
// was not delivered — the delivery verification can time out on a slow or busy
// session after the text has already reached the agent — so retrying forever
// re-injects the same message on every poll tick. One nudge was delivered 139
// times in 12 minutes this way. Each requeue therefore:
//
//   - increments the nudge's Attempts counter, and
//   - drops the nudge entirely once Attempts reaches MaxDeliveryAttempts.
//
// Retries are additionally spaced by RequeueBackoff so that even the bounded
// number of retries cannot repeat at the poll interval.
func Requeue(townRoot, session string, nudges []QueuedNudge) error {
	cfg := nudgeConfig(townRoot)
	maxAttempts := cfg.MaxDeliveryAttemptsV()
	backoff := cfg.RequeueBackoffD()
	now := time.Now()

	for _, n := range nudges {
		if !n.ExpiresAt.IsZero() && now.After(n.ExpiresAt) {
			reportExpiry(townRoot, session, n, expirySourceRequeue)
			continue
		}

		n.Attempts++
		if n.Attempts >= maxAttempts {
			fmt.Fprintf(os.Stderr,
				"Warning: dropping nudge from %s for %s after %d failed delivery attempts: %s\n",
				n.Sender, session, n.Attempts, firstLineExcerpt(n.Message))
			continue
		}

		// Space out retries: even a bounded number of retries would otherwise
		// repeat at the poll interval. Only ever pushed later, never earlier.
		if backoff > 0 {
			if due := now.Add(backoff); n.DeliverAfter.Before(due) {
				n.DeliverAfter = due
			}
		}

		if err := Enqueue(townRoot, session, n); err != nil {
			return err
		}
	}
	return nil
}

// Drain reads and removes all queued nudges for a session, returning them
// in FIFO order. This is called by the hook to pick up pending nudges.
//
// Uses rename-then-process to prevent concurrent Drain calls from delivering
// the same nudge twice: each file is atomically renamed to a .claimed suffix
// before reading, so only one caller can claim each nudge.
//
// Nudges past ExpiresAt are not returned; each one is reported through
// reportExpiry instead. Orphaned .claimed files from crashed drainers are swept
// if older than 5 minutes.
func Drain(townRoot, session string) ([]QueuedNudge, error) {
	dir := queueDir(townRoot, session)

	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading nudge queue: %w", err)
	}

	// Requeue orphaned .claimed files from crashed drainers.
	// A .claimed file older than staleClaimThreshold is certainly orphaned —
	// normal processing completes in milliseconds. We rename it back to .json
	// so it gets picked up on this or a future Drain call, rather than deleting
	// it (which would permanently drop the nudge).
	staleThreshold := nudgeConfig(townRoot).StaleClaimThresholdD()
	now := time.Now()
	for _, entry := range entries {
		if !strings.Contains(entry.Name(), ".claimed") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if now.Sub(info.ModTime()) > staleThreshold {
			orphanPath := filepath.Join(dir, entry.Name())
			// Strip everything from ".claimed" onward to restore original .json filename
			name := entry.Name()
			claimedIdx := strings.Index(name, ".claimed")
			restoredPath := filepath.Join(dir, name[:claimedIdx])
			if err := os.Rename(orphanPath, restoredPath); err != nil {
				// Rename failed — remove as last resort to prevent infinite accumulation
				fmt.Fprintf(os.Stderr, "Warning: failed to requeue orphaned claim %s: %v\n", entry.Name(), err)
				_ = os.Remove(orphanPath)
			}
		}
	}

	// Sort by name (timestamp-based) for FIFO ordering
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name() < entries[j].Name()
	})

	var nudges []QueuedNudge
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}

		path := filepath.Join(dir, entry.Name())

		// Atomically claim the file by renaming it. If another Drain call
		// is racing us, only one rename will succeed — the loser gets
		// ENOENT and moves on. This prevents double-delivery.
		//
		// Each drainer uses a unique claim suffix to avoid destination
		// collisions. On Windows, os.Rename to a shared destination is
		// not atomic — two goroutines can both "succeed" via
		// MOVEFILE_REPLACE_EXISTING, causing data loss. Unique suffixes
		// ensure each rename has a distinct target.
		claimPath := path + ".claimed." + randomSuffix()
		if err := os.Rename(path, claimPath); err != nil {
			// Another Drain got it first, or file was already removed
			continue
		}

		data, err := os.ReadFile(claimPath)
		if err != nil {
			if os.IsNotExist(err) {
				// File vanished between rename and read — treat as lost race
				continue
			}
			// Transient read error (e.g., Windows AV/indexer holding a share
			// lock) — unclaim so the nudge can be retried on a future Drain
			// call rather than permanently lost.
			_ = os.Rename(claimPath, path) // best-effort unclaim; orphan sweep catches failures
			continue
		}

		var n QueuedNudge
		if err := json.Unmarshal(data, &n); err != nil {
			// Malformed — clean up
			if rmErr := os.Remove(claimPath); rmErr != nil {
				fmt.Fprintf(os.Stderr, "Warning: failed to remove malformed claim %s: %v\n", entry.Name(), rmErr)
			}
			continue
		}

		// An expired nudge is not delivered — a stale message is noise. It is
		// reported rather than dropped, so the sender's intent survives the
		// queue entry (gt-oexm).
		if !n.ExpiresAt.IsZero() && now.After(n.ExpiresAt) {
			reportExpiry(townRoot, session, n, expirySourceDrain)
			if rmErr := os.Remove(claimPath); rmErr != nil {
				fmt.Fprintf(os.Stderr, "Warning: failed to remove expired nudge %s: %v\n", entry.Name(), rmErr)
			}
			continue
		}

		// Deferred nudge: not ready yet — unclaim and leave in queue.
		if !n.DeliverAfter.IsZero() && now.Before(n.DeliverAfter) {
			if renameErr := os.Rename(claimPath, path); renameErr != nil {
				fmt.Fprintf(os.Stderr, "Warning: failed to unclaim deferred nudge %s: %v\n", entry.Name(), renameErr)
			}
			continue
		}

		nudges = append(nudges, n)

		// Remove the claimed file after successful processing
		if rmErr := os.Remove(claimPath); rmErr != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to remove processed claim %s: %v\n", entry.Name(), rmErr)
		}
	}

	return nudges, nil
}

// reportExpiry announces a nudge that reached ExpiresAt undelivered, on three
// channels so no delivery plane can lose it: the message is copied under the
// queue's expired/ directory, the expiry is printed to stderr, and
// ExpiryObserver is called to carry it on to mail. The copy is attempted first
// so the record survives this process; the observer runs even when the copy
// fails, because mail is then the only durable notice left (gt-oexm).
func reportExpiry(townRoot, session string, n QueuedNudge, source string) {
	n.ExpiredAt = time.Now()
	trace, traceErr := writeExpiredTrace(townRoot, session, n)

	detail := fmt.Sprintf("sender=%s priority=%s aged=%s attempts=%d excerpt=%q",
		n.Sender, n.Priority, n.ExpiredAt.Sub(n.Timestamp).Round(time.Second), n.Attempts, firstLineExcerpt(n.Message))
	if traceErr != nil {
		fmt.Fprintf(os.Stderr, "EXPIRED NUDGE: %s for session %s (%s) could not be preserved: %v\n",
			detail, session, source, traceErr)
	} else {
		fmt.Fprintf(os.Stderr, "EXPIRED NUDGE: %s for session %s (%s) reached its TTL undelivered, kept at %s\n",
			detail, session, source, trace)
	}

	if ExpiryObserver != nil {
		ExpiryObserver(ExpiryEvent{
			TownRoot: townRoot,
			Session:  session,
			Nudge:    n,
			Source:   source,
			Trace:    trace,
		})
	}
}

// writeExpiredTrace copies an expired nudge into the session's expired/
// directory and returns that path, pruning traces past expiredTraceRetention in
// the same pass. The caller stamps ExpiredAt.
func writeExpiredTrace(townRoot, session string, n QueuedNudge) (string, error) {
	dir := filepath.Join(queueDir(townRoot, session), expiredDirName)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", fmt.Errorf("creating expired nudge dir: %w", err)
	}
	pruneExpiredTraces(dir)

	data, err := json.MarshalIndent(n, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshaling expired nudge: %w", err)
	}
	path := filepath.Join(dir, fmt.Sprintf("%d-%s.json", n.ExpiredAt.UnixNano(), randomSuffix()))
	if err := os.WriteFile(path, data, 0644); err != nil {
		return "", fmt.Errorf("writing expired nudge trace: %w", err)
	}
	return path, nil
}

// pruneExpiredTraces removes traces older than expiredTraceRetention. Failures
// are ignored: the next expiry retries, and a leftover file only costs disk.
func pruneExpiredTraces(dir string) {
	now := time.Now()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if now.Sub(info.ModTime()) > expiredTraceRetention {
			_ = os.Remove(filepath.Join(dir, entry.Name()))
		}
	}
}

// Pending returns the count of queued nudges for a session without draining.
// This is an approximate count — it does not check expiry or read file contents.
func Pending(townRoot, session string) (int, error) {
	dir := queueDir(townRoot, session)

	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("reading nudge queue: %w", err)
	}

	count := 0
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".json") {
			count++
		}
	}

	return count, nil
}

// QueueLen returns the number of pending nudges for a session without draining.
// Returns 0 on error — callers use this for quick checks. Missing queue
// directories are expected (no nudges yet) and silenced; other filesystem
// errors are logged to stderr so they don't go unnoticed.
func QueueLen(townRoot, session string) int {
	n, err := Pending(townRoot, session)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: nudge queue check failed for %s: %v\n", session, err)
	}
	return n
}

// RemoveKindByThread deletes queued nudges for a session that match both the
// provided kind and thread ID. It only removes queued .json files, leaving any
// in-flight claimed files alone so concurrent drainers can finish safely.
func RemoveKindByThread(townRoot, session, kind, threadID string) (int, error) {
	if kind == "" || threadID == "" {
		return 0, nil
	}

	dir := queueDir(townRoot, session)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("reading nudge queue: %w", err)
	}

	removed := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}

		path := filepath.Join(dir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return removed, fmt.Errorf("reading queued nudge %s: %w", entry.Name(), err)
		}

		var n QueuedNudge
		if err := json.Unmarshal(data, &n); err != nil {
			continue
		}
		if n.Kind != kind || n.ThreadID != threadID {
			continue
		}

		if err := os.Remove(path); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return removed, fmt.Errorf("removing queued nudge %s: %w", entry.Name(), err)
		}
		removed++
	}

	return removed, nil
}

// firstLineExcerpt returns the first non-empty line of a nudge message,
// truncated so a dropped nudge's warning stays one readable log line.
func firstLineExcerpt(message string) string {
	const maxLen = 120

	line := util.FirstLine(message)
	if len(line) <= maxLen {
		return line
	}
	// Cut on a rune boundary so multi-byte text is not mangled.
	cut := maxLen
	for cut > 0 && !utf8.RuneStart(line[cut]) {
		cut--
	}
	return line[:cut] + "…"
}

// FormatForInjection formats queued nudges as a system-reminder block
// suitable for Claude Code hook output.
func FormatForInjection(nudges []QueuedNudge) string {
	if len(nudges) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("<system-reminder>\n")

	// Separate urgent from normal
	var urgent, normal []QueuedNudge
	for _, n := range nudges {
		if n.Priority == PriorityUrgent {
			urgent = append(urgent, n)
		} else {
			normal = append(normal, n)
		}
	}

	if len(urgent) > 0 {
		b.WriteString(fmt.Sprintf("QUEUED NUDGE (%d urgent):\n\n", len(urgent)))
		for _, n := range urgent {
			b.WriteString(fmt.Sprintf("  [URGENT from %s] %s\n", n.Sender, n.Message))
		}
		if len(normal) > 0 {
			b.WriteString(fmt.Sprintf("\nPlus %d non-urgent nudge(s):\n", len(normal)))
			for _, n := range normal {
				b.WriteString(fmt.Sprintf("  [from %s] %s\n", n.Sender, n.Message))
			}
		}
		b.WriteString("\nHandle urgent nudges before continuing current work.\n")
	} else {
		b.WriteString(fmt.Sprintf("QUEUED NUDGE (%d message(s)):\n\n", len(normal)))
		for _, n := range normal {
			b.WriteString(fmt.Sprintf("  [from %s] %s\n", n.Sender, n.Message))
		}
		b.WriteString("\nThis is a background notification. Continue current work unless the nudge is higher priority.\n")
	}

	b.WriteString("</system-reminder>\n")
	return b.String()
}

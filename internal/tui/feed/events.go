package feed

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

	"github.com/steveyegge/gastown/internal/constants"
)

// EventSource represents a source of events
type EventSource interface {
	Events() <-chan Event
	Close() error
}

// GtEventsSource reads events from ~/gt/.events.jsonl (gt activity log)
type GtEventsSource struct {
	file   *os.File
	events chan Event
	cancel context.CancelFunc
}

// GtEvent is the structure of events in .events.jsonl
type GtEvent struct {
	Timestamp  string                 `json:"ts"`
	Source     string                 `json:"source"`
	Type       string                 `json:"type"`
	Actor      string                 `json:"actor"`
	Payload    map[string]interface{} `json:"payload"`
	Visibility string                 `json:"visibility"`
}

// NewGtEventsSource creates a source that tails ~/gt/.events.jsonl
func NewGtEventsSource(townRoot string) (*GtEventsSource, error) {
	eventsPath := filepath.Join(townRoot, ".events.jsonl")
	file, err := os.Open(eventsPath)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithCancel(context.Background())

	source := &GtEventsSource{
		file:   file,
		events: make(chan Event, 200),
		cancel: cancel,
	}

	go source.tail(ctx)

	return source, nil
}

// tail loads recent history then follows the file for new events.
func (s *GtEventsSource) tail(ctx context.Context) {
	defer close(s.events)

	// Load recent events (last 200 lines) for initial display, then resume
	// tailing from exactly where that scan stopped reading - NOT from
	// whatever the file's end happens to be by the time we get here.
	// Concurrent writers keep appending to .events.jsonl the whole time
	// loadRecentEvents is scanning + emitting; if we instead seeked to the
	// file's current end (`Seek(0, 2)`), any lines written during that
	// window would land strictly between "what the scan already consumed"
	// and "the new true EOF" - never scanned (backlog already finished)
	// and never tailed (we'd jump straight past them) - silently and
	// permanently dropped.
	_, _ = s.file.Seek(s.loadRecentEvents(), io.SeekStart)

	// Now tail for new events, polling every 100ms using a fresh scanner
	// each tick. bufio.Scanner latches an internal error (including
	// io.EOF) the first time Scan() returns false and never returns true
	// again on that instance - even after more data is appended to the
	// file. Reusing a single scanner across ticks would hit that EOF on
	// the very first tick (near-certain, since nothing has been appended
	// yet) and then silently stop seeing every event appended afterward.
	// os.File tracks the read offset independently of bufio.Scanner, so a
	// new scanner each tick resumes exactly where the last one left off.
	// Mirrors the --plain path in PrintGtEvents, which has the same fix.
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			scanner := bufio.NewScanner(s.file)
			for scanner.Scan() {
				line := scanner.Text()
				if event := parseGtEventLine(line); event != nil {
					select {
					case s.events <- *event:
					default:
					}
				}
			}
		}
	}
}

// loadRecentEvents reads the last N lines of the file and emits them as
// events. Uses a ring buffer so memory is O(maxLines) regardless of file
// size. Returns the byte offset where scanning stopped, so the caller can
// resume tailing from exactly there instead of re-querying the file's end
// (which may have advanced past lines written during this scan - see tail).
func (s *GtEventsSource) loadRecentEvents() int64 {
	const maxLines = 200

	if _, err := s.file.Seek(0, 0); err != nil {
		return 0
	}

	// Ring buffer: only keep the last maxLines lines in memory
	ring := make([]string, maxLines)
	idx := 0
	count := 0
	var consumed int64

	scanner := bufio.NewScanner(s.file)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	for scanner.Scan() {
		ring[idx%maxLines] = scanner.Text()
		consumed += int64(len(scanner.Bytes())) + 1 // +1 for the newline delimiter
		idx++
		count++
	}
	if scanner.Err() != nil {
		// Scanner failed (e.g. token too long) — seek to EOF so tail starts clean
		eof, _ := s.file.Seek(0, io.SeekEnd)
		return eof
	}

	// Emit lines in order (oldest first)
	n := count
	if n > maxLines {
		n = maxLines
	}
	start := idx - n
	for i := start; i < idx; i++ {
		line := ring[i%maxLines]
		if event := parseGtEventLine(line); event != nil {
			select {
			case s.events <- *event:
			default:
			}
		}
	}

	return consumed
}

// Events returns the event channel
func (s *GtEventsSource) Events() <-chan Event {
	return s.events
}

// Close stops the source
func (s *GtEventsSource) Close() error {
	s.cancel()
	return s.file.Close()
}

// parseGtEventLine parses a line from .events.jsonl
func parseGtEventLine(line string) *Event {
	if strings.TrimSpace(line) == "" {
		return nil
	}

	var ge GtEvent
	if err := json.Unmarshal([]byte(line), &ge); err != nil {
		return nil
	}

	// Only show feed-visible events
	if ge.Visibility != "feed" && ge.Visibility != "both" {
		return nil
	}

	t, err := time.Parse(time.RFC3339, ge.Timestamp)
	if err != nil {
		t = time.Now()
	}

	// Extract rig from payload or actor
	rig := ""
	if ge.Payload != nil {
		if r, ok := ge.Payload["rig"].(string); ok {
			rig = r
		}
	}
	if rig == "" && ge.Actor != "" {
		// Extract rig from actor like "gastown/witness"
		parts := strings.Split(ge.Actor, "/")
		if len(parts) > 0 && parts[0] != constants.RoleMayor && parts[0] != constants.RoleDeacon {
			rig = parts[0]
		}
	}

	// Extract role from actor
	role := ""
	if ge.Actor != "" {
		parts := strings.Split(ge.Actor, "/")
		if len(parts) >= 2 {
			role = parts[len(parts)-1]
			// Check for known roles
			switch parts[len(parts)-1] {
			case constants.RoleWitness:
				role = parts[len(parts)-1]
			default:
				// Could be polecat name - check second-to-last part
				if len(parts) >= 2 {
					switch parts[len(parts)-2] {
					case "polecats":
						role = constants.RolePolecat
					case constants.RoleCrew:
						role = constants.RoleCrew
					}
				}
			}
		} else if len(parts) == 1 {
			role = parts[0]
		}
	}

	// Build message from event type and payload
	message := buildEventMessage(ge.Type, ge.Payload)

	return &Event{
		Time:    t,
		Type:    ge.Type,
		Actor:   ge.Actor,
		Target:  getPayloadString(ge.Payload, "bead"),
		Message: message,
		Rig:     rig,
		Role:    role,
		Raw:     line,
	}
}

// buildEventMessage creates a human-readable message from event type and payload
func buildEventMessage(eventType string, payload map[string]interface{}) string {
	switch eventType {
	case "patrol_started":
		count := getPayloadInt(payload, "polecat_count")
		if msg := getPayloadString(payload, "message"); msg != "" {
			return msg
		}
		if count > 0 {
			return fmt.Sprintf("patrol started (%d polecats)", count)
		}
		return "patrol started"

	case "patrol_complete":
		count := getPayloadInt(payload, "polecat_count")
		if msg := getPayloadString(payload, "message"); msg != "" {
			return msg
		}
		if count > 0 {
			return fmt.Sprintf("patrol complete (%d polecats)", count)
		}
		return "patrol complete"

	case "polecat_checked":
		polecat := getPayloadString(payload, "polecat")
		status := getPayloadString(payload, "status")
		if polecat != "" {
			if status != "" {
				return fmt.Sprintf("checked %s (%s)", polecat, status)
			}
			return fmt.Sprintf("checked %s", polecat)
		}
		return "polecat checked"

	case "polecat_nudged":
		polecat := getPayloadString(payload, "polecat")
		reason := getPayloadString(payload, "reason")
		if polecat != "" {
			if reason != "" {
				return fmt.Sprintf("nudged %s: %s", polecat, reason)
			}
			return fmt.Sprintf("nudged %s", polecat)
		}
		return "polecat nudged"

	case "escalation_sent":
		target := getPayloadString(payload, "target")
		to := getPayloadString(payload, "to")
		reason := getPayloadString(payload, "reason")
		if target != "" && to != "" {
			if reason != "" {
				return fmt.Sprintf("escalated %s to %s: %s", target, to, reason)
			}
			return fmt.Sprintf("escalated %s to %s", target, to)
		}
		return "escalation sent"

	case "sling":
		bead := getPayloadString(payload, "bead")
		target := getPayloadString(payload, "target")
		if bead != "" && target != "" {
			return fmt.Sprintf("slung %s to %s", bead, target)
		}
		return "work slung"

	case "hook":
		bead := getPayloadString(payload, "bead")
		if bead != "" {
			return fmt.Sprintf("hooked %s", bead)
		}
		return "bead hooked"

	case "handoff":
		subject := getPayloadString(payload, "subject")
		if subject != "" {
			return fmt.Sprintf("handoff: %s", subject)
		}
		return "session handoff"

	case "done":
		bead := getPayloadString(payload, "bead")
		if bead != "" {
			return fmt.Sprintf("done: %s", bead)
		}
		return "work done"

	case "mail":
		subject := getPayloadString(payload, "subject")
		to := getPayloadString(payload, "to")
		if subject != "" {
			if to != "" {
				return fmt.Sprintf("→ %s: %s", to, subject)
			}
			return subject
		}
		return "mail sent"

	case "merged":
		worker := getPayloadString(payload, "worker")
		if worker != "" {
			return fmt.Sprintf("merged work from %s", worker)
		}
		return "merged"

	case "merge_failed":
		reason := getPayloadString(payload, "reason")
		if reason != "" {
			return fmt.Sprintf("merge failed: %s", reason)
		}
		return "merge failed"

	default:
		if msg := getPayloadString(payload, "message"); msg != "" {
			return msg
		}
		return eventType
	}
}

// getPayloadString extracts a string from payload
func getPayloadString(payload map[string]interface{}, key string) string {
	if payload == nil {
		return ""
	}
	if v, ok := payload[key].(string); ok {
		return v
	}
	return ""
}

// getPayloadInt extracts an int from payload
func getPayloadInt(payload map[string]interface{}, key string) int {
	if payload == nil {
		return 0
	}
	if v, ok := payload[key].(float64); ok {
		return int(v)
	}
	return 0
}

// CombinedSource merges events from multiple sources
type CombinedSource struct {
	sources []EventSource
	events  chan Event
	cancel  context.CancelFunc
}

// fanInTimeout is the maximum time a fan-in goroutine will wait for an event
// before checking if it should exit. This prevents goroutine leaks when a source
// channel blocks forever and the context is never canceled.
const fanInTimeout = 30 * time.Second

// NewCombinedSource creates a source that merges multiple event sources
func NewCombinedSource(sources ...EventSource) *CombinedSource {
	ctx, cancel := context.WithCancel(context.Background())

	combined := &CombinedSource{
		sources: sources,
		events:  make(chan Event, 100),
		cancel:  cancel,
	}

	// Fan-in from all sources with timeout to prevent goroutine leaks.
	// Each goroutine will exit if:
	// 1. Context is canceled (Close() called)
	// 2. Source channel is closed
	// 3. No event received for fanInTimeout (prevents indefinite blocking)
	for _, src := range sources {
		go func(s EventSource) {
			for {
				select {
				case <-ctx.Done():
					return
				case event, ok := <-s.Events():
					if !ok {
						return
					}
					select {
					case combined.events <- event:
					case <-ctx.Done():
						return
					default:
						// Drop if full
					}
				case <-time.After(fanInTimeout):
					// Timeout - check if we should exit
					select {
					case <-ctx.Done():
						return
					default:
						// Context still active, continue waiting
					}
				}
			}
		}(src)
	}

	return combined
}

// Events returns the combined event channel
func (c *CombinedSource) Events() <-chan Event {
	return c.events
}

// Close stops all sources
func (c *CombinedSource) Close() error {
	c.cancel()
	var lastErr error
	for _, src := range c.sources {
		if err := src.Close(); err != nil {
			lastErr = err
		}
	}
	return lastErr
}

// FindBeadsDir finds the beads directory for the given working directory
func FindBeadsDir(workDir string) (string, error) {
	// Walk up looking for .beads
	dir := workDir
	for {
		beadsPath := filepath.Join(dir, ".beads")
		if info, err := os.Stat(beadsPath); err == nil && info.IsDir() {
			return beadsPath, nil
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}

	return "", os.ErrNotExist
}

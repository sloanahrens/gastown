// Package channelevents provides file-based event emission for named channels.
//
// Channel events are JSON files written under ~/gt/events/<channel>/ and
// consumed by await-event subscribers (e.g., the refinery watching for
// MERGE_READY events). This is distinct from the activity feed events in
// the events package (~/gt/.events.jsonl).
//
// Channel scoping (gt-dsj): channels are single-consumer, but some channel
// names have one consumer PER RIG (every rig runs its own refinery and
// witness). Those channels are per-rig: their events live in
// events/<channel>/<rig>/ so one rig's consumer can never read or delete
// another rig's wake events. Town-global channels with a single consumer
// (e.g. "mayor") keep the flat events/<channel>/ layout. This package is
// the single source of truth for which channels are per-rig, so emitters
// and await-event always agree on the event directory.
package channelevents

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"time"
)

// ValidChannelName restricts channel names to safe characters (no path traversal).
// Rig names in event paths are held to the same charset.
var ValidChannelName = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// perRigChannels lists channels whose consumer runs once per rig. Events on
// these channels MUST be scoped to a rig; emitting or awaiting without a rig
// is an error (a global event here would be stolen by whichever rig's
// consumer polls first).
var perRigChannels = map[string]bool{
	"refinery": true,
	"witness":  true,
}

// emitSeq is an atomic counter to ensure unique event filenames even when
// time.Now().UnixNano() has low resolution.
var emitSeq atomic.Uint64

// IsPerRig reports whether events on the channel are scoped per rig.
func IsPerRig(channel string) bool {
	return perRigChannels[channel]
}

// Dir returns the directory holding pending events for a channel. Per-rig
// channels resolve to events/<channel>/<rig>/; town-global channels ignore
// rig and resolve to events/<channel>/.
func Dir(townRoot, channel, rig string) string {
	if IsPerRig(channel) {
		return filepath.Join(townRoot, "events", channel, rig)
	}
	return filepath.Join(townRoot, "events", channel)
}

// EmitToTown creates an event file for the channel under the given town root.
// rig scopes the event for per-rig channels and is required for them; it is
// ignored for town-global channels.
func EmitToTown(townRoot, channel, rig, eventType string, payloadPairs []string) (string, error) {
	if !ValidChannelName.MatchString(channel) {
		return "", fmt.Errorf("invalid channel name %q: must match [a-zA-Z0-9_-]", channel)
	}
	if IsPerRig(channel) {
		if rig == "" {
			return "", fmt.Errorf("channel %q is per-rig: a rig is required", channel)
		}
		if !ValidChannelName.MatchString(rig) {
			return "", fmt.Errorf("invalid rig name %q: must match [a-zA-Z0-9_-]", rig)
		}
	} else {
		rig = ""
	}

	eventDir := Dir(townRoot, channel, rig)
	if err := os.MkdirAll(eventDir, 0755); err != nil {
		return "", fmt.Errorf("creating event directory: %w", err)
	}
	return emitToDir(eventDir, channel, rig, eventType, payloadPairs)
}

// emitToDir writes an event file to the given directory.
func emitToDir(eventDir, channel, rig, eventType string, payloadPairs []string) (string, error) {
	payload := make(map[string]string)
	for _, pair := range payloadPairs {
		key, val, found := strings.Cut(pair, "=")
		if found {
			payload[key] = val
		}
	}

	now := time.Now()
	event := map[string]interface{}{
		"type":      eventType,
		"channel":   channel,
		"timestamp": now.Format(time.RFC3339),
		"payload":   payload,
	}
	if rig != "" {
		event["rig"] = rig
	}

	data, err := json.MarshalIndent(event, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshaling event: %w", err)
	}

	seq := emitSeq.Add(1)
	eventFile := filepath.Join(eventDir, fmt.Sprintf("%d-%d-%d.event", now.UnixNano(), seq, os.Getpid()))
	if err := os.WriteFile(eventFile, data, 0644); err != nil {
		return "", fmt.Errorf("writing event file: %w", err)
	}

	return eventFile, nil
}

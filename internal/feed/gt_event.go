package feed

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/constants"
)

// Event is one parsed event from ~/gt/.events.jsonl, as the plain feed prints
// it. Fields are the subset the printer and its filters read.
type Event struct {
	Time    time.Time
	Type    string
	Actor   string
	Target  string
	Message string
	Rig     string
	Role    string
	Raw     string
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
		// Extract rig from actor like "gastown/crew/max"
		parts := strings.Split(ge.Actor, "/")
		if len(parts) > 0 && parts[0] != constants.RoleMayor {
			rig = parts[0]
		}
	}

	// Extract role from actor
	role := ""
	if ge.Actor != "" {
		parts := strings.Split(ge.Actor, "/")
		if len(parts) >= 2 {
			role = parts[len(parts)-1]
			// Could be a polecat or crew name - check second-to-last part
			switch parts[len(parts)-2] {
			case "polecats":
				role = constants.RolePolecat
			case constants.RoleCrew:
				role = constants.RoleCrew
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

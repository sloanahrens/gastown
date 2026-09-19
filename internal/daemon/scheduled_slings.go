package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"
)

// ScheduledSlingsConfig is the opt-in scheduled_slings patrol: each entry is a
// formula slung onto a rig on an interval, one bead per run (gt-nj23).
type ScheduledSlingsConfig struct {
	Enabled bool                  `json:"enabled"`
	Entries []ScheduledSlingEntry `json:"entries,omitempty"`
}

// ScheduledSlingEntry is one scheduled formula. Name is the schedule's
// identity: runs are found by the label "scheduled:<name>".
type ScheduledSlingEntry struct {
	Name        string            `json:"name"`
	Rig         string            `json:"rig"`
	Formula     string            `json:"formula"`
	Agent       string            `json:"agent,omitempty"`
	IntervalStr string            `json:"interval"`
	Priority    int               `json:"priority,omitempty"`
	Vars        map[string]string `json:"vars,omitempty"`
}

const defaultScheduledSlingPriority = 3

var scheduledSlingNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

func (e ScheduledSlingEntry) validate() error {
	if !scheduledSlingNameRe.MatchString(e.Name) {
		return fmt.Errorf("scheduled_slings: name %q must match %s", e.Name, scheduledSlingNameRe)
	}
	if e.Rig == "" || e.Formula == "" {
		return fmt.Errorf("scheduled_slings[%s]: rig and formula are required", e.Name)
	}
	d, err := time.ParseDuration(e.IntervalStr)
	if err != nil || d <= 0 {
		return fmt.Errorf("scheduled_slings[%s]: interval %q must be a positive Go duration", e.Name, e.IntervalStr)
	}
	return nil
}

func (e ScheduledSlingEntry) interval() time.Duration {
	d, _ := time.ParseDuration(e.IntervalStr)
	return d
}

func (e ScheduledSlingEntry) label() string { return "scheduled:" + e.Name }

func (e ScheduledSlingEntry) priority() int {
	if e.Priority == 0 {
		return defaultScheduledSlingPriority
	}
	return e.Priority
}

// scheduledBead is the slice of a run bead the decision needs.
type scheduledBead struct {
	ID        string
	Status    string
	CreatedAt time.Time
}

type scheduledAction int

const (
	scheduledSkipOpen   scheduledAction = iota // a run is in flight or its MR is queued
	scheduledSkipRecent                        // the newest run started less than an interval ago
	scheduledDispatch
)

func (a scheduledAction) String() string {
	switch a {
	case scheduledSkipOpen:
		return "skip-open"
	case scheduledSkipRecent:
		return "skip-recent"
	default:
		return "dispatch"
	}
}

// decideScheduledSling is pure: the beads carrying the entry's label, the
// entry's interval, and now. Any open bead wins over recency so a run whose
// MR is still in the queue is never doubled.
func decideScheduledSling(beads []scheduledBead, interval time.Duration, now time.Time) scheduledAction {
	var newest time.Time
	for _, b := range beads {
		if b.Status != "closed" {
			return scheduledSkipOpen
		}
		if b.CreatedAt.After(newest) {
			newest = b.CreatedAt
		}
	}
	if !newest.IsZero() && now.Sub(newest) < interval {
		return scheduledSkipRecent
	}
	return scheduledDispatch
}

// parseScheduledBeads reads `bd list --json` output.
func parseScheduledBeads(data []byte) ([]scheduledBead, error) {
	var rows []struct {
		ID        string    `json:"id"`
		Status    string    `json:"status"`
		CreatedAt time.Time `json:"created_at"`
	}
	if err := json.Unmarshal(data, &rows); err != nil {
		return nil, fmt.Errorf("parsing bd list output: %w", err)
	}
	out := make([]scheduledBead, 0, len(rows))
	for _, r := range rows {
		out = append(out, scheduledBead{ID: r.ID, Status: r.Status, CreatedAt: r.CreatedAt})
	}
	return out, nil
}

// parseCreatedBeadID reads `bd create --json`, which some bd versions print
// as an object and others as a one-element array.
func parseCreatedBeadID(data []byte) (string, error) {
	var obj struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(data, &obj); err == nil && obj.ID != "" {
		return obj.ID, nil
	}
	var arr []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(data, &arr); err == nil && len(arr) > 0 && arr[0].ID != "" {
		return arr[0].ID, nil
	}
	return "", errors.New("bd create output has no id")
}

package steward

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Alert kinds: what the daemon raised to the overseer about the steward.
const (
	// AlertPro: a job started on the hard preset (one per job).
	AlertPro = "pro"
	// AlertStuck: a job outlived its timeout and its kill grace.
	AlertStuck = "stuck"
	// AlertErrorRate: the last hour's jobs broke at a rate over the threshold.
	AlertErrorRate = "error-rate"
)

// Alert is one escalation the daemon raised about the steward.
type Alert struct {
	// Key is the alert's identity: the same key is never raised twice.
	Key  string    `json:"key"`
	Kind string    `json:"kind"`
	Job  string    `json:"job,omitempty"`
	Bead string    `json:"bead,omitempty"`
	At   time.Time `json:"at"`
}

// AlertsPath is the town's steward alert record.
func AlertsPath(townRoot string) string {
	return filepath.Join(townRoot, ".runtime", "steward", "alerts.jsonl")
}

// Alerts is the append-only record of the escalations the daemon raised, so
// a condition escalates once however many scans see it and gt steward status
// can count them (gt-9bioi.3).
type Alerts struct {
	path string
}

// NewAlerts returns the record at path.
func NewAlerts(path string) *Alerts { return &Alerts{path: path} }

// Record appends a.
func (l *Alerts) Record(a Alert) error {
	data, err := json.Marshal(a)
	if err != nil {
		return fmt.Errorf("encoding alert %s: %w", a.Key, err)
	}
	if err := os.MkdirAll(filepath.Dir(l.path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(l.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(append(data, '\n')); err != nil {
		return err
	}
	return f.Sync()
}

// Read returns every alert, oldest first. A line that does not parse is
// skipped, as the ledger's are.
func (l *Alerts) Read() ([]Alert, error) {
	f, err := os.Open(l.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Alert
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var a Alert
		if json.Unmarshal([]byte(line), &a) != nil {
			continue
		}
		out = append(out, a)
	}
	return out, scanner.Err()
}

// Raised is the set of keys already raised.
func Raised(alerts []Alert) map[string]bool {
	out := make(map[string]bool, len(alerts))
	for _, a := range alerts {
		out[a.Key] = true
	}
	return out
}

// CountSince counts the alerts raised at or after since, by kind.
func CountSince(alerts []Alert, since time.Time) map[string]int {
	out := map[string]int{}
	for _, a := range alerts {
		if !a.At.Before(since) {
			out[a.Kind]++
		}
	}
	return out
}

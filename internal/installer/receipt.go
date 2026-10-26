package installer

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Receipt appends one line to the receipts file for event. A write that fails
// is a WARNING line and never changes the run's status: the receipt describes
// the install, it is not the install.
func (c *Contract) Receipt(event, commit, prev, reason string) {
	if c.Receipts == "" {
		return
	}
	rec := receipt{
		ts:       c.now(),
		event:    event,
		commit:   commit,
		prev:     prev,
		source:   c.Source,
		reason:   reason,
		mergedAt: c.MergedAt,
		start:    c.Start,
	}
	if err := appendReceipt(c.Receipts, rec.line()); err != nil {
		fmt.Fprintf(c.stderr(), "[install-gt] WARNING: could not append a receipt to %s\n", c.Receipts)
	}
}

func (c *Contract) now() time.Time {
	if c.Now == nil {
		return time.Now()
	}
	return c.Now()
}

// receipt is one line of <daemon>/install-receipts.jsonl. A field the run has
// nothing to say about is null, and consumers treat null and "" alike.
type receipt struct {
	ts       time.Time
	event    string
	commit   string
	prev     string
	source   string
	reason   string
	mergedAt time.Time
	start    time.Time
}

// line renders the record byte for byte as the script's python does: keys
// sorted, null for an absent field, whole seconds. The file has two writers
// until the script goes (gt-tpg0p.14), so a diff on it should mean a different
// install, not a different encoder.
func (r receipt) line() string {
	var duration any
	if !r.start.IsZero() {
		duration = int(r.ts.Sub(r.start).Seconds())
	}
	var mergedAt any
	if !r.mergedAt.IsZero() {
		mergedAt = r.mergedAt.UTC().Format(time.RFC3339)
	}
	fields := []string{
		`"commit": ` + jsonValue(nullable(r.commit)),
		`"duration_s": ` + jsonValue(duration),
		`"event": ` + jsonValue(nullable(r.event)),
		`"merged_at": ` + jsonValue(mergedAt),
		`"prev_commit": ` + jsonValue(nullable(r.prev)),
		`"reason": ` + jsonValue(nullable(r.reason)),
		`"source": ` + jsonValue(nullable(r.source)),
		`"ts": ` + jsonValue(r.ts.UTC().Format(time.RFC3339)),
	}
	return "{" + strings.Join(fields, ", ") + "}"
}

// nullable is an empty field as JSON null, which is how the script writes one.
func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// jsonValue encodes one value without json.Marshal's HTML escaping, which
// python does not do and would otherwise make the two writers' lines differ.
func jsonValue(v any) string {
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return "null"
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// appendReceipt writes line as one more line of path, creating the directory
// the script's mkdir -p makes. O_APPEND keeps concurrent writers (the daemon's
// own receipts) from interleaving inside a line.
func appendReceipt(path, line string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(line + "\n")
	return err
}

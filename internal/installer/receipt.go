package installer

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
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
		c.Warn("could not append a receipt to " + c.Receipts)
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
	duration, hasDuration := 0, false
	if !r.start.IsZero() {
		duration, hasDuration = int(r.ts.Sub(r.start).Seconds()), true
	}
	mergedAt := ""
	if !r.mergedAt.IsZero() {
		mergedAt = r.mergedAt.UTC().Format(time.RFC3339)
	}
	fields := []string{
		`"commit": ` + jsonOptional(r.commit),
		`"duration_s": ` + jsonOptionalInt(duration, hasDuration),
		`"event": ` + jsonOptional(r.event),
		`"merged_at": ` + jsonOptional(mergedAt),
		`"prev_commit": ` + jsonOptional(r.prev),
		`"reason": ` + jsonOptional(r.reason),
		`"source": ` + jsonOptional(r.source),
		`"ts": ` + jsonString(r.ts.UTC().Format(time.RFC3339)),
	}
	return "{" + strings.Join(fields, ", ") + "}"
}

// jsonOptional is s as a JSON string, or null when the run has nothing to say,
// which is how the script writes an absent field.
func jsonOptional(s string) string {
	if s == "" {
		return "null"
	}
	return jsonString(s)
}

// jsonOptionalInt is n as a JSON number, or null when the record does not hold
// one.
func jsonOptionalInt(n int, ok bool) string {
	if !ok {
		return "null"
	}
	return strconv.Itoa(n)
}

// jsonString quotes s the way python's json.dumps does at its defaults: every
// character outside printable ASCII becomes a \uXXXX escape, astral ones a
// surrogate pair, and \b and \f keep their short forms (gt-314j0).
func jsonString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			switch {
			case r >= 0x20 && r < 0x7f:
				b.WriteRune(r)
			case r > 0xffff:
				n := r - 0x10000
				fmt.Fprintf(&b, `\u%04x\u%04x`, 0xd800+(n>>10), 0xdc00+(n&0x3ff))
			default:
				fmt.Fprintf(&b, `\u%04x`, r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
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

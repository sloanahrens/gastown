package ominstaller

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// receiptName is the file under the daemon directory that each run appends its
// receipt to.
const receiptName = "install-om-receipts.jsonl"

// receipt is one line of the receipts file. An empty string field is written as
// JSON null; the event is always written.
type receipt struct {
	event  string
	commit string
	prev   string
	source string
	reason string
	// start is the run's start. The zero time writes duration_s as null.
	start time.Time
}

// writeReceipt appends the receipt for res to the receipts file. A failed write
// is logged and changes nothing else about the run.
func (c Config) writeReceipt(res Result, start time.Time) {
	now := c.now()
	path := filepath.Join(c.DaemonDir, receiptName)
	line := receiptLine(receipt{
		event:  res.Event,
		commit: res.Commit,
		prev:   res.Prev,
		source: c.Source,
		reason: res.Reason,
		start:  start,
	}, now)
	if err := appendLine(path, c.DaemonDir, line); err != nil {
		c.logf("[install-om] WARNING: could not append a receipt to %s: %v", path, err)
	}
}

// appendLine appends line to path, creating dir first.
func appendLine(path, dir, line string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644) //nolint:gosec // G304: a path built from the daemon directory
	if err != nil {
		return err
	}
	_, werr := f.WriteString(line)
	return errors.Join(werr, f.Close())
}

// receiptLine is the receipt as it is written: a JSON object with its
// keys sorted and Python's separators, ended by a newline. The timestamp is now
// in UTC, and duration_s is whole seconds from the start to now.
func receiptLine(r receipt, now time.Time) string {
	duration := "null"
	if !r.start.IsZero() {
		// The start is whole seconds, so the duration is measured from them.
		elapsed := now.Sub(time.Unix(r.start.Unix(), 0))
		duration = strconv.FormatInt(int64(elapsed/time.Second), 10)
	}
	fields := []string{
		`"commit": ` + nullOrString(r.commit),
		`"duration_s": ` + duration,
		`"event": ` + pyString(r.event),
		`"prev_commit": ` + nullOrString(r.prev),
		`"reason": ` + nullOrString(r.reason),
		`"source": ` + nullOrString(r.source),
		`"ts": ` + pyString(now.UTC().Format("2006-01-02T15:04:05Z")),
	}
	return "{" + strings.Join(fields, ", ") + "}\n"
}

func nullOrString(s string) string {
	if s == "" {
		return "null"
	}
	return pyString(s)
}

// pyString encodes s as Python's json.dumps does by default: quotes, backslashes
// and control characters are escaped, and every code point outside printable
// ASCII is written as \uXXXX, as a surrogate pair above U+FFFF.
func pyString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		default:
			switch {
			case r >= 0x20 && r <= 0x7e:
				b.WriteRune(r)
			case r < 0x10000:
				fmt.Fprintf(&b, `\u%04x`, r)
			default:
				v := r - 0x10000
				fmt.Fprintf(&b, `\u%04x\u%04x`, 0xd800|(v>>10)&0x3ff, 0xdc00|v&0x3ff)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

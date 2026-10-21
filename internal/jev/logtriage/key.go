package logtriage

import (
	"regexp"
	"strings"
)

// maxKeyRunes caps a pattern key. A daemon line can run long, and the key is
// stored in every log line.
const maxKeyRunes = 300

var (
	// reTime matches the daemon's own timestamp and ISO-8601 times.
	reTime = regexp.MustCompile(`\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2}(?:\.\d+)?|\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:?\d{2})?`)
	// reID matches a bead id: a rig prefix, a hyphen and the id's characters.
	// Only the prefixes the towns use are named, so a patrol such as jev-drift
	// keeps its words.
	reID = regexp.MustCompile(`\b(?:gt|hq|be|bd)-[a-z0-9]+(?:-[a-z0-9]+)*\b`)
	// reDuration matches a Go duration such as 306ms, 2.5s or 1h30m0s.
	reDuration = regexp.MustCompile(`\b\d+(?:\.\d+)?(?:ns|µs|us|ms|s|m|h)(?:\d+(?:\.\d+)?(?:ns|µs|us|ms|s|m|h))*\b`)
	// reHex matches a run of hex digits that could be a commit or content hash.
	// hexHash checks that it holds both a digit and a letter, so a word such
	// as effaced stays.
	reHex    = regexp.MustCompile(`\b[0-9a-f]{7,40}\b`)
	reNumber = regexp.MustCompile(`\d+`)
)

// PatternKey returns the line with its times, bead ids, hashes, durations and
// numbers replaced by placeholders, so lines that differ only in those values
// share one key. The key is capped at maxKeyRunes.
func PatternKey(line string) string {
	k := strings.TrimSpace(line)
	k = reTime.ReplaceAllString(k, "<time>")
	k = reID.ReplaceAllString(k, "<id>")
	k = reDuration.ReplaceAllString(k, "<dur>")
	k = reHex.ReplaceAllStringFunc(k, func(m string) string {
		if hexHash(m) {
			return "<hash>"
		}
		return m
	})
	k = reNumber.ReplaceAllString(k, "<n>")
	return truncate(k, maxKeyRunes)
}

// hexHash reports whether m holds both a digit and a letter from a to f.
func hexHash(m string) bool {
	return strings.ContainsAny(m, "0123456789") && strings.ContainsAny(m, "abcdef")
}

// truncate cuts s to at most n runes, marking a cut with an ellipsis.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

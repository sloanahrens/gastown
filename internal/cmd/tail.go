package cmd

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// tailLine is one line of the gt tail stream: when it happened, which rig it
// belongs to ("town" for the daemon, "hq" for the town store), which source
// produced it, and the text. Render turns it into
// "<local RFC3339 time> <rig> <kind> <text>".
type tailLine struct {
	At   time.Time
	Rig  string
	Kind string
	Text string
}

// tailSource is one read-only input to the stream. Poll returns the lines
// that appeared since the previous poll; the first poll returns the backlog
// from the source's cutoff. A source reports its own read failures as lines
// and never stops the stream.
type tailSource interface {
	Poll() []tailLine
}

// The kinds gt tail merges, in the order ties print.
const (
	tailKindEvents   = "events"
	tailKindLandings = "landings"
	tailKindDaemon   = "daemon"
)

var tailKinds = []string{tailKindEvents, tailKindLandings, tailKindDaemon}

// mergeTail concatenates the batches and orders them by time. The sort is
// stable, so lines with equal times keep batch order and, within a batch,
// source order.
func mergeTail(batches ...[]tailLine) []tailLine {
	var out []tailLine
	for _, b := range batches {
		out = append(out, b...)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out
}

// renderTailLine formats one line in loc. Rig and kind are single tokens and
// the text has no control characters, so every record is exactly one line
// and the first three fields can be cut or grepped by position.
func renderTailLine(l tailLine, loc *time.Location) string {
	return l.At.In(loc).Format(time.RFC3339) + " " + tailToken(l.Rig) + " " + tailToken(l.Kind) + " " + tailText(l.Text)
}

func tailToken(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return '_'
		}
		return r
	}, s)
	if s == "" {
		return "-"
	}
	return s
}

func tailText(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
}

var tailDaysRe = regexp.MustCompile(`^([0-9]+)d$`)

// tailMaxSinceDays bounds "--since Nd" well inside time.Duration's range.
const tailMaxSinceDays = 36500

// parseTailSince reads --since: a duration back from now ("15m", "2h", "1d",
// "0" for now), an RFC3339 timestamp, or a local time as
// "2006-01-02T15:04:05", "2006-01-02 15:04" or "2006-01-02".
func parseTailSince(s string, now time.Time, loc *time.Location) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "0" {
		return now, nil
	}
	if m := tailDaysRe.FindStringSubmatch(s); m != nil {
		n, err := strconv.Atoi(m[1])
		if err != nil || n > tailMaxSinceDays {
			return time.Time{}, fmt.Errorf("--since %q: at most %dd", s, tailMaxSinceDays)
		}
		return now.Add(-time.Duration(n) * 24 * time.Hour), nil
	}
	if d, err := time.ParseDuration(s); err == nil {
		if d < 0 {
			return time.Time{}, fmt.Errorf("--since %q: a duration back from now cannot be negative", s)
		}
		return now.Add(-d), nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	for _, layout := range []string{"2006-01-02T15:04:05", "2006-01-02 15:04:05", "2006-01-02 15:04", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, s, loc); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("--since %q: want a duration (15m, 2h, 1d) or a time (RFC3339, 2006-01-02T15:04:05, 2006-01-02 15:04, 2006-01-02)", s)
}

// parseTailKinds reads --kind: a comma-separated subset of tailKinds.
func parseTailKinds(s string) (map[string]bool, error) {
	kinds := map[string]bool{}
	for _, k := range strings.Split(s, ",") {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		valid := false
		for _, v := range tailKinds {
			if k == v {
				valid = true
			}
		}
		if !valid {
			return nil, fmt.Errorf("--kind %q: unknown kind (want any of %s)", k, strings.Join(tailKinds, ","))
		}
		kinds[k] = true
	}
	if len(kinds) == 0 {
		return nil, fmt.Errorf("--kind %q: name at least one of %s", s, strings.Join(tailKinds, ","))
	}
	return kinds, nil
}

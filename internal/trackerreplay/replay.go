// Package trackerreplay is the rollback replay: it applies the tracker's
// events since a seq onto Dolt through bd, so a rig flipped back to Dolt after
// a soak loses no writes. Each event becomes one bd call through an injected
// Apply. An event the feed does not carry enough to replay stops the run and
// names its kind and seq, because a write guessed from a partial event is worse
// than a stopped run.
package trackerreplay

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/steveyegge/gastown/tracker/api"
)

// Source reads the tracker's events feed: records with seq above after, at
// most limit of them (0 = the server's page). *client.Client satisfies it.
type Source interface {
	Events(ctx context.Context, after int64, limit int) (api.EventsPage, error)
}

// Apply runs one bd command given its arguments, without the bd binary.
type Apply func(argv []string) error

// Result is how far a replay got. Applied is the last seq whose bd call ran
// (or, on a dry run, would have run): a rerun with that as since starts after
// it. Commands counts the bd calls run or printed.
type Result struct {
	Applied  int64
	Commands int
}

// UnreplayableError is an event the replay refuses to guess at: its kind, or
// the field an update names, is not one the feed carries enough to replay.
type UnreplayableError struct {
	Seq    int64
	Kind   string
	Reason string
}

func (e *UnreplayableError) Error() string {
	return fmt.Sprintf("tracker event seq %d: cannot replay %s: %s", e.Seq, e.Kind, e.Reason)
}

// TruncatedError is a cursor below the oldest seq the feed still holds: the
// events in between were pruned, so a replay from there would leave a gap.
type TruncatedError struct {
	Since, Floor int64
}

func (e *TruncatedError) Error() string {
	return fmt.Sprintf("events feed pruned past seq %d (oldest retained %d): replaying from here would skip the pruned events", e.Since, e.Floor)
}

// ApplyError is a bd call that failed. It names the seq and the bd verb; the
// runner's own error stays behind Unwrap, out of the message, because bd's
// output can carry a secret.
type ApplyError struct {
	Seq  int64
	Verb string
	Err  error
}

func (e *ApplyError) Error() string {
	return fmt.Sprintf("tracker event seq %d: bd %s failed", e.Seq, e.Verb)
}

func (e *ApplyError) Unwrap() error { return e.Err }

// maxPages bounds one replay's reads: a feed that keeps producing events would
// otherwise hold the command forever, so the run stops and names the seq it
// reached instead.
const maxPages = 1000

// Replay reads the feed after since and applies each event in seq order. On a
// dry run it prints each bd call and runs none; apply is then not used and may
// be nil. The run stops at the first event it cannot replay or the first bd
// call that fails; the Result still says how far it got.
func Replay(ctx context.Context, src Source, apply Apply, since int64, dryRun bool, out io.Writer) (Result, error) {
	res := Result{Applied: since}
	after := since
	for pages := 0; ; pages++ {
		if pages == maxPages {
			return res, fmt.Errorf("tracker events feed did not end within %d pages: stopped at seq %d", maxPages, after)
		}
		page, err := src.Events(ctx, after, 0)
		if err != nil {
			return res, err
		}
		// Checked on every page, not only the first: a feed pruned while a long
		// replay runs opens a gap in the pages still to come.
		if page.FirstSeq > 0 && after < page.FirstSeq-1 {
			return res, &TruncatedError{Since: after, Floor: page.FirstSeq}
		}
		if len(page.Events) == 0 {
			if page.NextAfter > after {
				return res, fmt.Errorf("tracker events feed returned an empty page after seq %d but sends the next page to seq %d: refusing to read the feed as ended", after, page.NextAfter)
			}
			return res, nil
		}
		for _, ev := range page.Events {
			if ev.Seq <= after {
				return res, fmt.Errorf("tracker events feed is not ascending: seq %d follows seq %d", ev.Seq, after)
			}
			argv, err := Command(ev)
			if err != nil {
				return res, err
			}
			if dryRun {
				fmt.Fprintln(out, "bd "+display(argv))
			} else if err := apply(argv); err != nil {
				return res, &ApplyError{Seq: ev.Seq, Verb: argv[0], Err: err}
			}
			res.Applied, res.Commands = ev.Seq, res.Commands+1
			after = ev.Seq
		}
	}
}

// Command returns the bd arguments that replay ev. Every replayable event is
// one bd call; an event the feed cannot replay is an *UnreplayableError.
//
// The feed carries an issue's title at creation, not its type, priority or
// description, and an update to a text field names the field without its new
// value. Those are refused rather than filled in with bd's defaults.
func Command(ev api.Event) ([]string, error) {
	id, err := operand(ev, ev.IssueID, "issue id")
	if err != nil {
		return nil, err
	}
	switch ev.Op {
	case "created":
		title, err := value(ev, ev.NewValue, "title")
		if err != nil {
			return nil, err
		}
		argv := []string{"create", "--id=" + id, "--title=" + title}
		if ev.Actor != "" {
			argv = append(argv, "--actor="+ev.Actor)
		}
		return argv, nil
	case "updated":
		return updated(ev)
	case "status_changed", "reopened":
		status, err := value(ev, ev.NewValue, "status")
		if err != nil {
			return nil, err
		}
		return []string{"update", id, "--status=" + status}, nil
	case "label_added":
		label, err := value(ev, ev.NewValue, "label")
		if err != nil {
			return nil, err
		}
		return []string{"update", id, "--add-label=" + label}, nil
	case "label_removed":
		label, err := value(ev, ev.OldValue, "label")
		if err != nil {
			return nil, err
		}
		return []string{"update", id, "--remove-label=" + label}, nil
	case "closed":
		argv := []string{"close", id}
		if ev.Comment != nil && *ev.Comment != "" {
			argv = append(argv, "--reason="+*ev.Comment)
		}
		return argv, nil
	case "deleted":
		return []string{"delete", id, "--force"}, nil
	case "dependency_added":
		dep, err := operandValue(ev, ev.NewValue, "dependency")
		if err != nil {
			return nil, err
		}
		typ, err := value(ev, ev.Comment, "dependency type")
		if err != nil {
			return nil, err
		}
		return []string{"dep", "add", id, dep, "--type=" + typ}, nil
	case "dependency_removed":
		dep, err := operandValue(ev, ev.OldValue, "dependency")
		if err != nil {
			return nil, err
		}
		return []string{"dep", "remove", id, dep}, nil
	case "commented":
		return nil, unreplayable(ev, "commented", "the feed does not carry the comment text")
	default:
		return nil, unreplayable(ev, ev.Op, "no replay for this event kind")
	}
}

// updated replays the fields whose new value the feed carries. The field is
// the event's comment; the others are refused by name.
func updated(ev api.Event) ([]string, error) {
	field := ""
	if ev.Comment != nil {
		field = *ev.Comment
	}
	kind := "updated/" + field
	if ev.NewValue == nil {
		return nil, unreplayable(ev, kind, "the feed carries no new value")
	}
	val := *ev.NewValue
	switch field {
	case "title":
		return []string{"update", ev.IssueID, "--title=" + val}, nil
	case "assignee":
		return []string{"update", ev.IssueID, "--assignee=" + val}, nil
	case "priority":
		n, err := strconv.Atoi(val)
		if err != nil || n < 0 || n > 4 {
			return nil, unreplayable(ev, kind, fmt.Sprintf("priority %q is not 0-4", val))
		}
		return []string{"update", ev.IssueID, "--priority=" + strconv.Itoa(n)}, nil
	default:
		return nil, unreplayable(ev, kind, "the feed carries no new value for this field")
	}
}

// value returns the event's value for what, or refuses the event when the
// feed left it out.
func value(ev api.Event, v *string, what string) (string, error) {
	if v == nil || *v == "" {
		return "", unreplayable(ev, ev.Op, "the feed carries no "+what)
	}
	return *v, nil
}

// operandValue is value for an id bd takes as an operand rather than a flag.
func operandValue(ev api.Event, v *string, what string) (string, error) {
	s, err := value(ev, v, what)
	if err != nil {
		return "", err
	}
	return operand(ev, s, what)
}

// operand refuses an id bd would not read as an operand. The feed's ids are
// untrusted text: one beginning with "-" becomes a bd flag, and a space or
// control character makes the dry run's printed command ambiguous.
func operand(ev api.Event, s, what string) (string, error) {
	if !isBeadID(s) {
		return "", unreplayable(ev, ev.Op, fmt.Sprintf("%s %q is not a bead id bd takes as an operand", what, s))
	}
	return s, nil
}

// isBeadID reports whether s is a non-empty run of letters, digits, "-", "." and
// "_" that does not start with "-".
func isBeadID(s string) bool {
	if s == "" || s[0] == '-' {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-' || r == '.' || r == '_':
		default:
			return false
		}
	}
	return true
}

func unreplayable(ev api.Event, kind, reason string) error {
	return &UnreplayableError{Seq: ev.Seq, Kind: kind, Reason: reason}
}

// display prints argv the way a shell would take it, for a dry run.
func display(argv []string) string {
	parts := make([]string, len(argv))
	for i, a := range argv {
		parts[i] = quoteArg(a)
	}
	return strings.Join(parts, " ")
}

// quoteArg quotes an argument a shell would split or rewrite: one holding
// whitespace, a quote, a backslash, or a non-ASCII or control rune, any of
// which a title from the feed can carry.
func quoteArg(a string) string {
	for _, r := range a {
		if r <= ' ' || r >= 0x7f || r == '"' || r == '\'' || r == '\\' {
			return strconv.Quote(a)
		}
	}
	return a
}

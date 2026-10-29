// Package trackerreplay is the rollback replay: it applies one rig's tracker
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
	// Defaulted counts the creations replayed with bd's defaults for priority,
	// type and description, because the feed carries only the title at creation.
	// The command reports it: a replayed create is never a silent fill-in.
	Defaulted int
	// Skipped counts the feed's events that belong to another rig, which a
	// replay passes over so a foreign id never reaches bd.
	Skipped int
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

// Replay applies the events of the rig whose beads prefix is prefix, after since, in seq order.
//
// An event for another rig is skipped before it is turned into a bd call:
// bd run in this rig's directory routes a foreign id to that rig's own Dolt or
// refuses it, and either would apply the wrong rig's write or stop the run. A
// skipped event still advances the cursor, and Result.Skipped counts it. On a
// dry run Replay prints each bd call and runs none; apply is then not used and
// may be nil. The run stops at the first event it cannot replay or the first bd
// call that fails; the Result still says how far it got.
func Replay(ctx context.Context, src Source, apply Apply, since int64, prefix string, dryRun bool, out io.Writer) (Result, error) {
	res := Result{Applied: since}
	if strings.TrimSuffix(prefix, "-") == "" {
		return res, fmt.Errorf("replay needs a beads prefix to tell this rig's events from the others; got %q", prefix)
	}
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
			if err := feedEnded(page, after); err != nil {
				return res, err
			}
			return res, nil
		}
		for _, ev := range page.Events {
			if ev.Seq <= after {
				return res, fmt.Errorf("tracker events feed is not ascending: seq %d follows seq %d", ev.Seq, after)
			}
			// An event with no id names no rig, so it is not skipped as foreign:
			// passing it over would hide a write the replay cannot account for.
			if ev.IssueID == "" {
				return res, unreplayable(ev, ev.Op, "the feed carries no issue id, so the event cannot be told apart from another rig's")
			}
			if !belongsTo(ev, prefix) {
				res.Skipped++
				after = ev.Seq
				continue
			}
			argv, err := Command(ev, prefix)
			if err != nil {
				return res, err
			}
			if dryRun {
				fmt.Fprintln(out, "bd "+display(argv))
			} else if err := apply(argv); err != nil {
				return res, &ApplyError{Seq: ev.Seq, Verb: argv[0], Err: err}
			}
			res.Applied, res.Commands = ev.Seq, res.Commands+1
			if ev.Op == "created" {
				res.Defaulted++
			}
			after = ev.Seq
		}
	}
}

// feedEnded reports whether an empty page is the documented end of the feed,
// and otherwise refuses to read it as one. The store's contract says the end
// is a page that echoes the cursor it was asked from, and past seq 0 that comes
// from a log still holding events. A page that holds neither is a feed that
// stalled or lost its log, and a replay that read it as the end would report
// success having applied nothing — the loss the rollback exists to prevent.
func feedEnded(page api.EventsPage, after int64) error {
	if page.NextAfter > after {
		return fmt.Errorf("tracker events feed returned an empty page after seq %d but sends the next page to seq %d: refusing to read the feed as ended", after, page.NextAfter)
	}
	if page.NextAfter != after {
		return fmt.Errorf("tracker events feed returned an empty page after seq %d but holds the cursor at seq %d: refusing to read a stalled feed as ended", after, page.NextAfter)
	}
	if after > 0 && page.FirstSeq == 0 {
		return fmt.Errorf("tracker events feed returned an empty page after seq %d while holding no events at all: refusing to read a feed that lost its log as ended", after)
	}
	return nil
}

// belongsTo reports whether ev is an issue of the rig whose beads prefix is
// prefix. The prefix is compared without its trailing dash, and the id must
// carry it as a whole segment, so a "gt" rig does not claim "gtx-1"; a prefix
// that is the start of another rig's ("gt" and "gt-x") cannot be told apart
// here, so the caller refuses that route.
func belongsTo(ev api.Event, prefix string) bool {
	p := strings.TrimSuffix(prefix, "-")
	return strings.HasPrefix(ev.IssueID, p+"-")
}

// isWispID reports whether id is a wisp of the rig whose beads prefix is
// prefix: that prefix, "-wisp-" and the store's random characters, the form its
// newWispID builds (gt-wisp-c3w9x1). A normal issue id is the prefix and its
// own characters, so it never has "wisp-" after the prefix.
func isWispID(id, prefix string) bool {
	p := strings.TrimSuffix(prefix, "-")
	if p == "" {
		return false
	}
	rest, ok := strings.CutPrefix(id, p+"-")
	return ok && strings.HasPrefix(rest, "wisp-") && rest != "wisp-"
}

// Command returns the bd arguments that replay ev, one of the rig whose beads
// prefix is prefix. Every replayable event is one bd call; an event the feed
// cannot replay is an *UnreplayableError.
//
// The feed carries an issue's title at creation, not its type, priority or
// description, so a creation replays with bd's defaults for those: Result.Defaulted
// counts them and the command says so, so the fill-in is never silent. A
// creation whose id is a wisp id of prefix replays with --ephemeral: the feed
// carries no ephemeral field, so the id is the only mark it was a wisp, and
// without the flag a rollback would leave a persistent, versioned issue the rig
// then counts. An update whose field's new value the feed does not carry
// (defer_until, and the rows written before the store recorded values) is
// refused rather than filled in with the old one.
func Command(ev api.Event, prefix string) ([]string, error) {
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
		if isWispID(id, prefix) {
			argv = append(argv, "--ephemeral")
		}
		if ev.Actor != "" {
			argv = append(argv, "--actor="+ev.Actor)
		}
		return argv, nil
	case "updated":
		return updated(ev, id)
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
		text, err := value(ev, ev.NewValue, "comment text")
		if err != nil {
			return nil, err
		}
		// "--" so a comment that opens with "-" reaches bd as text, not a flag.
		return []string{"comment", id, "--", text}, nil
	default:
		return nil, unreplayable(ev, ev.Op, "no replay for this event kind")
	}
}

// updated replays the fields the feed names in the event's comment. A field
// whose new value the event does not carry is refused by name.
func updated(ev api.Event, id string) ([]string, error) {
	field := ""
	if ev.Comment != nil {
		field = *ev.Comment
	}
	kind := "updated/" + field
	switch field {
	case "promoted to persistent":
		// The field name is the whole change; there is no value to carry.
		return []string{"update", id, "--persistent"}, nil
	case "description":
		return textUpdate(ev, field, id, "--description=", ev.NewValue)
	case "acceptance_criteria":
		return textUpdate(ev, field, id, "--acceptance=", ev.NewValue)
	case "notes":
		// The event is the line the store appended, so replaying it appends
		// the same line rather than replacing the whole field.
		return textUpdate(ev, field, id, "--append-notes=", ev.NewValue)
	}
	if ev.NewValue == nil {
		return nil, unreplayable(ev, kind, "the feed carries no new value")
	}
	val := *ev.NewValue
	switch field {
	case "title":
		return []string{"update", id, "--title=" + val}, nil
	case "assignee":
		return []string{"update", id, "--assignee=" + val}, nil
	case "priority":
		n, err := strconv.Atoi(val)
		if err != nil || n < 0 || n > 4 {
			return nil, unreplayable(ev, kind, fmt.Sprintf("priority %q is not 0-4", val))
		}
		return []string{"update", id, "--priority=" + strconv.Itoa(n)}, nil
	default:
		return nil, unreplayable(ev, kind, "the feed carries no new value for this field")
	}
}

// textUpdate replays an update to a whole-text field. Unlike value it takes an
// empty new value: a cleared description is a change the feed carries.
func textUpdate(ev api.Event, field, id, flag string, v *string) ([]string, error) {
	if v == nil {
		return nil, unreplayable(ev, "updated/"+field, "the feed carries no new value")
	}
	return []string{"update", id, flag + *v}, nil
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

// quoteArg returns a as a POSIX shell must see it to pass it through
// unchanged: bare when every rune is one a shell never treats specially, and
// otherwise in single quotes. A dry run's output is pasted into a shell, so a
// value from the feed holding "$", "`", ";", "|", "&", a glob or a newline is
// as unsafe as one holding a space.
func quoteArg(a string) string {
	if a != "" && isBareInShell(a) {
		return a
	}
	return "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
}

// isBareInShell reports whether every rune of a is one a shell passes through
// as part of a word: letters, digits, and the punctuation that is never
// special. "=" is only an assignment when it leads the command, and bd's
// arguments never do.
func isBareInShell(a string) bool {
	for _, r := range a {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case strings.ContainsRune("-._/:@%+=,", r):
		default:
			return false
		}
	}
	return true
}

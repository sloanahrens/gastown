package daemon

import (
	"errors"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/steveyegge/gastown/internal/attention"
)

// clusterDriftResult is one answer from the drift compare: Differing holds one
// line per difference, as gt town cluster drift prints it, "<resource>: <JSON
// path ...>" (gt-wbt9pn).
type clusterDriftResult struct {
	Differing []string
}

// errNoRecordedRender is the compare's reason when no live render has recorded
// its flags: there is nothing to compare, and the reason names the command that
// records them (gt-wbt9pn).
var errNoRecordedRender = errors.New("no live render has recorded its flags, so there is nothing to compare; run gt town cluster render --record-flags")

// The drift summary names at most clusterDriftMaxObjects objects and no more
// than clusterDriftSummaryMax characters, since a queue row is one line and a
// compare can differ in hundreds of paths (gt-wbt9pn).
const (
	clusterDriftMaxObjects   = 10
	clusterDriftSummaryMax   = 400
	clusterDriftSummaryStart = "cluster differs from render: "
)

// clusterDriftItems turns one compare answer into the items it raises. A read
// error raises the unreadable item and never the drift one, even when the answer
// carries differences, so a collector built on it cannot clear an open drift
// item by failing to read. No error and no difference raises nothing, and
// an answer whose lines name no object is unreadable rather than in step
// (gt-wbt9pn).
func clusterDriftItems(res clusterDriftResult, err error) []attention.Item {
	if err != nil {
		return []attention.Item{clusterDriftUnreadableItem(clusterDriftErrorReason(err))}
	}
	if len(res.Differing) == 0 {
		return nil
	}
	names, ok := clusterDriftObjectNames(res.Differing)
	if !ok {
		return []attention.Item{clusterDriftUnreadableItem(
			"the compare's answer has a line that names no object, so no difference can be read")}
	}
	return []attention.Item{{
		Key:  attention.RuleClusterDrift,
		Kind: attention.KindClusterDrift,
		// attention's severities are low, medium and high; medium is the warn
		// loudness the cluster.drift rule asks for (gt-y705n1).
		Severity: attention.SeverityMedium,
		Summary:  clusterDriftSummary(names),
	}}
}

// clusterDriftErrorReason is the unreadable item's reason: the sentinel's words
// when the error wraps errNoRecordedRender, so the reader is told to record a
// render even when the parse wrapped it with the stderr line, and the error's
// own text otherwise (gt-wbt9pn).
func clusterDriftErrorReason(err error) string {
	if errors.Is(err, errNoRecordedRender) {
		return errNoRecordedRender.Error()
	}
	return err.Error()
}

// clusterDriftUnreadableItem is the item a compare that could not be read
// raises. It carries its own key, never the drift key, so an open drift item is
// not cleared by a failed read (gt-wbt9pn).
func clusterDriftUnreadableItem(reason string) attention.Item {
	return attention.Item{
		Key:      attention.RuleClusterDriftUnreadable,
		Kind:     attention.KindClusterDriftUnreadable,
		Severity: attention.SeverityMedium,
		Summary:  "cluster drift unreadable: " + reason,
	}
}

// clusterDriftObjectNames is each object the lines differ in, once each and in
// the order the lines first name them: the text before the first ": " of a
// line, never the JSON path or the fingerprint after it, since a path or a value
// can hold a secret. ok is false when a line is not the "<resource>: <...>"
// shape the command prints, since nothing in it can be named as drift
// (gt-wbt9pn).
func clusterDriftObjectNames(lines []string) (names []string, ok bool) {
	seen := make(map[string]struct{}, len(lines))
	for _, line := range lines {
		name, _, found := strings.Cut(line, ": ")
		if !found {
			return nil, false
		}
		name = strings.TrimSpace(name)
		if name == "" {
			return nil, false
		}
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		names = append(names, name)
	}
	return names, true
}

// clusterDriftSummary names the objects once each, at most
// clusterDriftMaxObjects, and ends with " and N more" when it names fewer than
// there are. It drops whole objects until the summary fits
// clusterDriftSummaryMax, so no name is half printed (gt-wbt9pn).
func clusterDriftSummary(names []string) string {
	shown := len(names)
	if shown > clusterDriftMaxObjects {
		shown = clusterDriftMaxObjects
	}
	for {
		var b strings.Builder
		b.WriteString(clusterDriftSummaryStart)
		b.WriteString(strings.Join(names[:shown], ", "))
		if more := len(names) - shown; more > 0 {
			if shown > 0 {
				b.WriteString(" and ")
			}
			b.WriteString(strconv.Itoa(more))
			b.WriteString(" more")
		}
		summary := b.String()
		if shown == 0 || utf8.RuneCountInString(summary) <= clusterDriftSummaryMax {
			return summary
		}
		shown--
	}
}

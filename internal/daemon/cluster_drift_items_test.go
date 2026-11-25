package daemon

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/steveyegge/gastown/internal/attention"
)

// TestClusterDriftItemsNamesEachObjectOnce: a compare that differs in three
// objects, two lines for one of them, raises one drift item naming each object
// once, in the order the lines first name them, and none of the JSON paths or
// fingerprints the lines carry (gt-wbt9pn).
func TestClusterDriftItemsNamesEachObjectOnce(t *testing.T) {
	t.Parallel()

	items := clusterDriftItems(clusterDriftResult{Differing: []string{
		"deployment/gt-daemon: spec.replicas fingerprint a1b2 != c3d4",
		"deployment/gt-daemon: spec.template.spec.containers[0].image fingerprint a1b2 != e5f6",
		"serviceaccount/gt-daemon: metadata.labels fingerprint a1b2 != c3d4",
		"cronjob/tracker-backup: spec.schedule fingerprint a1b2 != g7h8",
	}}, nil)
	if len(items) != 1 {
		t.Fatalf("clusterDriftItems returned %d items, want 1: %+v", len(items), items)
	}
	it := items[0]
	if it.Key != attention.RuleClusterDrift {
		t.Errorf("item key = %q, want %q", it.Key, attention.RuleClusterDrift)
	}
	if it.Kind != attention.KindClusterDrift {
		t.Errorf("item kind = %q, want %q", it.Kind, attention.KindClusterDrift)
	}
	if it.Severity != attention.SeverityMedium {
		t.Errorf("item severity = %q, want the warn loudness %q", it.Severity, attention.SeverityMedium)
	}
	want := "cluster differs from render: deployment/gt-daemon, serviceaccount/gt-daemon, cronjob/tracker-backup"
	if it.Summary != want {
		t.Errorf("summary = %q, want %q", it.Summary, want)
	}
	for _, leaked := range []string{"spec.", "fingerprint", "a1b2", "c3d4"} {
		if strings.Contains(it.Summary, leaked) {
			t.Errorf("summary %q carries %q, want object names only", it.Summary, leaked)
		}
	}
}

// TestClusterDriftItemsCapsNamesAtTen: fourteen differing objects raise one
// item that names the first ten and ends with the count it left out (gt-wbt9pn).
func TestClusterDriftItemsCapsNamesAtTen(t *testing.T) {
	t.Parallel()

	names := make([]string, 0, 14)
	lines := make([]string, 0, 14)
	for i := 0; i < 14; i++ {
		name := fmt.Sprintf("deployment/object-%02d", i)
		names = append(names, name)
		lines = append(lines, name+": spec.replicas fingerprint a1b2 != c3d4")
	}

	items := clusterDriftItems(clusterDriftResult{Differing: lines}, nil)
	if len(items) != 1 {
		t.Fatalf("clusterDriftItems returned %d items, want 1", len(items))
	}
	// The contract pins ten, so the test writes ten rather than reading the
	// builder's constant and following it if the cap moves.
	want := "cluster differs from render: " + strings.Join(names[:10], ", ") + " and 4 more"
	if got := items[0].Summary; got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
}

// TestClusterDriftItemsSummaryStaysUnderTheCap: when whole object names would
// push the summary past its cap, the builder drops whole objects and says how
// many it left out, instead of cutting a name or a character (gt-wbt9pn).
func TestClusterDriftItemsSummaryStaysUnderTheCap(t *testing.T) {
	t.Parallel()

	names := make([]string, 0, 14)
	lines := make([]string, 0, 14)
	for i := 0; i < 14; i++ {
		name := fmt.Sprintf("deployment/%s-%02d", strings.Repeat("x", 40), i)
		names = append(names, name)
		lines = append(lines, name+": spec.replicas fingerprint a1b2 != c3d4")
	}

	items := clusterDriftItems(clusterDriftResult{Differing: lines}, nil)
	if len(items) != 1 {
		t.Fatalf("clusterDriftItems returned %d items, want 1", len(items))
	}
	summary := items[0].Summary
	if n := utf8.RuneCountInString(summary); n > 400 {
		t.Fatalf("summary is %d characters, want at most 400: %q", n, summary)
	}

	body, ok := strings.CutPrefix(summary, "cluster differs from render: ")
	if !ok {
		t.Fatalf("summary %q does not start with the drift preamble", summary)
	}
	more := 0
	if rest, tail, found := strings.Cut(body, " and "); found {
		body = rest
		if _, err := fmt.Sscanf(tail, "%d more", &more); err != nil {
			t.Fatalf("summary tail %q is not \"N more\": %v", tail, err)
		}
	}
	var listed []string
	if body != "" {
		listed = strings.Split(body, ", ")
	}
	if len(listed) > 10 {
		t.Errorf("summary names %d objects, want at most 10", len(listed))
	}
	if more == 0 {
		t.Errorf("summary %q names every one of %d long objects, want the cap to drop some", summary, len(names))
	}
	if got, want := len(listed)+more, len(names); got != want {
		t.Errorf("summary names %d objects and says %d more, which is %d of %d; want every object accounted for", len(listed), more, got, want)
	}
	for i, name := range listed {
		if name != names[i] {
			t.Errorf("summary object %d = %q, want the whole name %q in order", i, name, names[i])
		}
	}
}

// TestClusterDriftItemsUnreadable: a read error raises one unreadable item and
// no drift item even when the answer carries differences; an error wrapping
// errNoRecordedRender says there is no recorded render in words, naming the
// command that records one (gt-wbt9pn).
func TestClusterDriftItemsUnreadable(t *testing.T) {
	t.Parallel()

	plain := errors.New("kubectl get deployment/gt-daemon: forbidden")
	recorded := fmt.Errorf("the drift compare's stderr: %w", errNoRecordedRender)
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"plain", plain, "cluster drift unreadable: " + plain.Error()},
		{"no recorded render", recorded, "cluster drift unreadable: " + errNoRecordedRender.Error()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// The answer carries a difference, which the error must outrank: an
			// unreadable compare is not a cluster in step.
			items := clusterDriftItems(clusterDriftResult{Differing: []string{
				"deployment/gt-daemon: spec.replicas fingerprint a1b2 != c3d4",
			}}, tc.err)
			if len(items) != 1 {
				t.Fatalf("clusterDriftItems returned %d items, want 1: %+v", len(items), items)
			}
			it := items[0]
			if it.Key != attention.RuleClusterDriftUnreadable {
				t.Errorf("item key = %q, want %q", it.Key, attention.RuleClusterDriftUnreadable)
			}
			if it.Kind != attention.KindClusterDriftUnreadable {
				t.Errorf("item kind = %q, want %q", it.Kind, attention.KindClusterDriftUnreadable)
			}
			if it.Key == attention.RuleClusterDrift || it.Kind == attention.KindClusterDrift {
				t.Errorf("item %+v carries the drift key or kind beside the unreadable one", it)
			}
			if it.Summary != tc.want {
				t.Errorf("summary = %q, want %q", it.Summary, tc.want)
			}
			if errors.Is(tc.err, errNoRecordedRender) {
				for _, want := range []string{
					"no live render has recorded its flags",
					"gt town cluster render --record-flags",
				} {
					if !strings.Contains(it.Summary, want) {
						t.Errorf("summary %q does not carry %q", it.Summary, want)
					}
				}
			}
		})
	}
}

// TestClusterDriftItemsNothing: a compare that read fine and differed in
// nothing raises no item, so a collector clears the drift item it held
// (gt-wbt9pn).
func TestClusterDriftItemsNothing(t *testing.T) {
	t.Parallel()

	for _, res := range []clusterDriftResult{{}, {Differing: []string{}}} {
		if items := clusterDriftItems(res, nil); len(items) != 0 {
			t.Errorf("clusterDriftItems(%+v, nil) = %+v, want no items", res, items)
		}
	}
}

// TestClusterDriftItemsRefusesAnAnswerThatNamesNoObject: a line without the
// command's "<resource>: " shape cannot be read as a difference, so the answer
// is unreadable rather than a clear (gt-wbt9pn).
func TestClusterDriftItemsRefusesAnAnswerThatNamesNoObject(t *testing.T) {
	t.Parallel()

	for _, line := range []string{"", "   ", "no separator here", ": spec.replicas fingerprint a1b2 != c3d4"} {
		t.Run(fmt.Sprintf("%q", line), func(t *testing.T) {
			t.Parallel()
			items := clusterDriftItems(clusterDriftResult{Differing: []string{
				"deployment/gt-daemon: spec.replicas fingerprint a1b2 != c3d4",
				line,
			}}, nil)
			if len(items) != 1 {
				t.Fatalf("clusterDriftItems returned %d items, want 1: %+v", len(items), items)
			}
			if items[0].Key != attention.RuleClusterDriftUnreadable {
				t.Errorf("item key = %q, want %q", items[0].Key, attention.RuleClusterDriftUnreadable)
			}
			if items[0].Summary == "" {
				t.Error("unreadable item has no summary")
			}
		})
	}
}

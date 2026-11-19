package cmd

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/deploy/k8s"
)

// driftExceptionsNow is the instant the exceptions tests judge expiry at.
func driftExceptionsNow() time.Time {
	return time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
}

// driftExceptionEntryYAML is one entry as the file spells it. %q quotes every
// value, so a case can leave one empty or hold text the format refuses.
func driftExceptionEntryYAML(object, path, reason, owner, expires string) string {
	return fmt.Sprintf("- object: %q\n  path: %q\n  reason: %q\n  owner: %q\n  expires: %q\n",
		object, path, reason, owner, expires)
}

// driftExceptionsYAML is a file holding entries.
func driftExceptionsYAML(entries ...string) string {
	return "exceptions:\n" + strings.Join(entries, "")
}

// TestClusterDriftExceptionsLoad holds the reader to the entries it accepts and
// to the shipped file: the embedded deploy/k8s/drift-exceptions.yaml parses with
// no exceptions, so the file the binary carries is one the reader can read.
func TestClusterDriftExceptionsLoad(t *testing.T) {
	t.Parallel()
	t.Run("the shipped file", func(t *testing.T) {
		t.Parallel()
		exceptions, err := loadDriftExceptions(k8s.DriftExceptions, driftExceptionsNow())
		if err != nil {
			t.Fatalf("loadDriftExceptions(DriftExceptions): %v", err)
		}
		if len(exceptions) != 0 {
			t.Errorf("exceptions = %+v, want the shipped file to explain nothing yet", exceptions)
		}
	})

	t.Run("a namespaced and a cluster-scoped entry", func(t *testing.T) {
		t.Parallel()
		data := driftExceptionsYAML(
			driftExceptionEntryYAML("Role/gt/gt-update", "rules[0].verbs[1]", "the operator keeps list on the live Role", "overseer", "2026-12-01"),
			// An entry expiring today holds through the day.
			driftExceptionEntryYAML("ClusterRole//gt-sweep-reader", "rules[0].apiGroups[0]", "hand-applied before the role was embedded", "overseer", "2026-11-19"),
		)
		exceptions, err := loadDriftExceptions([]byte(data), driftExceptionsNow())
		if err != nil {
			t.Fatalf("loadDriftExceptions(): %v", err)
		}
		want := []driftException{
			{Object: "Role/gt/gt-update", Path: "rules[0].verbs[1]", Reason: "the operator keeps list on the live Role", Owner: "overseer", Expires: time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)},
			{Object: "ClusterRole//gt-sweep-reader", Path: "rules[0].apiGroups[0]", Reason: "hand-applied before the role was embedded", Owner: "overseer", Expires: time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC)},
		}
		if !slices.Equal(exceptions, want) {
			t.Errorf("exceptions = %+v, want %+v", exceptions, want)
		}
	})
}

// TestClusterDriftExceptionsRefuse holds every entry the reader cannot apply to
// a failed read: an exception it drops silently would leave a difference it was
// meant to explain, and one it applies wrongly would hide a difference nobody
// explained (gt-5gtb1x).
func TestClusterDriftExceptionsRefuse(t *testing.T) {
	t.Parallel()
	valid := driftExceptionEntryYAML("Role/gt/gt-update", "rules[0].verbs[1]", "the operator keeps list on the live Role", "overseer", "2026-12-01")
	cases := []struct {
		name string
		data string
		want string
	}{
		{
			name: "an empty reason",
			data: driftExceptionsYAML(driftExceptionEntryYAML("Role/gt/gt-update", "rules[0].verbs[1]", "", "overseer", "2026-12-01")),
			want: "gives no reason",
		},
		{
			name: "an empty owner",
			data: driftExceptionsYAML(driftExceptionEntryYAML("Role/gt/gt-update", "rules[0].verbs[1]", "the operator keeps list", "", "2026-12-01")),
			want: "names no owner",
		},
		{
			name: "an entry with no object",
			data: driftExceptionsYAML(driftExceptionEntryYAML("", "rules[0].verbs[1]", "the operator keeps list", "overseer", "2026-12-01")),
			want: "names no object",
		},
		{
			name: "a whole-object exception",
			data: driftExceptionsYAML(driftExceptionEntryYAML("Role/gt/gt-update", "", "the live Role is older", "overseer", "2026-12-01")),
			want: "whole-object exception",
		},
		{
			name: "an object that is not <Kind>/<namespace>/<name>",
			data: driftExceptionsYAML(driftExceptionEntryYAML("Role/gt-update", "rules[0].verbs[1]", "the operator keeps list", "overseer", "2026-12-01")),
			want: `not "<Kind>/<namespace>/<name>"`,
		},
		{
			name: "a date the format does not hold",
			data: driftExceptionsYAML(driftExceptionEntryYAML("Role/gt/gt-update", "rules[0].verbs[1]", "the operator keeps list", "overseer", "first deployer apply")),
			want: "is not a 2006-01-02 date",
		},
		{
			name: "an expired entry",
			data: driftExceptionsYAML(driftExceptionEntryYAML("Role/gt/gt-update", "rules[0].verbs[1]", "the operator keeps list", "overseer", "2026-11-18")),
			want: "expired after 2026-11-18",
		},
		{
			name: "an empty file",
			data: "",
			want: "is empty",
		},
		{
			name: "a file with no exceptions key",
			data: "{}\n",
			want: "no exceptions: key",
		},
		{
			name: "a key the format does not name",
			data: "exceptions: []\nhand_waved: true\n",
			want: "hand_waved",
		},
		{
			name: "an entry key the format does not name",
			data: "exceptions:\n- object: Role/gt/gt-update\n  path: rules[0].verbs[1]\n  reason: kept\n  owner: overseer\n  expires: 2026-12-01\n  reasn: kept\n",
			want: "reasn",
		},
		{
			name: "a second document",
			data: driftExceptionsYAML(valid) + "---\nexceptions: []\n",
			want: "more than one YAML document",
		},
		{
			name: "a malformed second document",
			data: driftExceptionsYAML(valid) + "---\na: b: c\n",
			want: "reading the drift exceptions",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			exceptions, err := loadDriftExceptions([]byte(tc.data), driftExceptionsNow())
			if err == nil {
				t.Fatalf("loadDriftExceptions() = %+v with no error, want %s refused", exceptions, tc.name)
			}
			if exceptions != nil {
				t.Errorf("got %+v beside the error, want no exceptions", exceptions)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not say %q", err, tc.want)
			}
		})
	}
}

// TestClusterDriftExceptionsFilter holds the match to one object and one path: a
// matching line moves to explained, and a line of another path, another object,
// or no compare path stays unexplained so an exception never hides it.
func TestClusterDriftExceptionsFilter(t *testing.T) {
	t.Parallel()
	const (
		matching  = "rules[0].verbs[1]: rendered sha256:aaaaaaaaaaaa, observed sha256:bbbbbbbbbbbb"
		otherPath = "rules[0].verbs[0]: rendered sha256:aaaaaaaaaaaa, observed sha256:cccccccccccc"
		shapeless = "the live Role could not be read"
	)
	exceptions := []driftException{{
		Object:  "Role/gt/gt-update",
		Path:    "rules[0].verbs[1]",
		Reason:  "the operator keeps list on the live Role",
		Owner:   "overseer",
		Expires: time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC),
	}}
	cases := []struct {
		name    string
		object  string
		line    string
		explain bool
	}{
		{name: "the object and path the entry names", object: "Role/gt/gt-update", line: matching, explain: true},
		{name: "another path of the same object", object: "Role/gt/gt-update", line: otherPath},
		{name: "another object at the same path", object: "Role/gt/gt-update-launch", line: matching},
		{name: "another kind at the same name", object: "ClusterRole//gt-update", line: matching},
		{name: "a line with no compare path", object: "Role/gt/gt-update", line: shapeless},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			unexplained, explained := filterExplained([]string{tc.line}, tc.object, exceptions)
			if tc.explain {
				if !slices.Equal(explained, []string{tc.line}) || unexplained != nil {
					t.Fatalf("unexplained = %q, explained = %q, want the line explained", unexplained, explained)
				}
				return
			}
			if !slices.Equal(unexplained, []string{tc.line}) || explained != nil {
				t.Fatalf("unexplained = %q, explained = %q, want the line unexplained", unexplained, explained)
			}
		})
	}

	t.Run("one call splits the lines and keeps their order", func(t *testing.T) {
		t.Parallel()
		unexplained, explained := filterExplained([]string{matching, otherPath, shapeless}, "Role/gt/gt-update", exceptions)
		if !slices.Equal(explained, []string{matching}) {
			t.Errorf("explained = %q, want the matching line", explained)
		}
		if !slices.Equal(unexplained, []string{otherPath, shapeless}) {
			t.Errorf("unexplained = %q, want the other two in order", unexplained)
		}
	})
}

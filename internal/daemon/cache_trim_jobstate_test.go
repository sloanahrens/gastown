package daemon

import (
	"context"
	"strings"
	"testing"
)

const cacheTrimJobStateVerb = "get job " + cacheTrimJob + " -n " + cacheTrimNamespace + " -o json"

// TestCacheTrimJobStateReadsTheThreeStates pins the three states, complete and
// failed winning over active, and that the read is made exactly once.
func TestCacheTrimJobStateReadsTheThreeStates(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, status string
		want         cacheTrimJobStateKind
	}{
		{"Complete condition", `{"status":{"conditions":[{"type":"Complete","status":"True"}]}}`, cacheTrimJobStateComplete},
		{"failed count", `{"status":{"failed":1}}`, cacheTrimJobStateFailed},
		{"Failed condition", `{"status":{"conditions":[{"type":"Failed","status":"True"}]}}`, cacheTrimJobStateFailed},
		{"active alone", `{"status":{"active":2}}`, cacheTrimJobStateActive},
		{"failed beats active", `{"status":{"active":2,"failed":1}}`, cacheTrimJobStateFailed},
		{"complete beats active", `{"status":{"active":2,"conditions":[{"type":"Complete","status":"True"}]}}`, cacheTrimJobStateComplete},
	} {
		f := newCacheTrimFixture(t, "", map[string]cliReply{cacheTrimJobStateVerb: {stdout: tc.status}})
		got, err := f.d.cacheTrimJobState(context.Background(), "/kubeconfig", cacheTrimNamespace, cacheTrimJob)
		if err != nil || got != tc.want {
			t.Errorf("%s: cacheTrimJobState = %v, %v; want %v, nil", tc.name, got, err, tc.want)
		}
		if calls := f.verbs(); len(calls) != 1 {
			t.Errorf("%s: made calls %v; want one Job read", tc.name, calls)
		}
	}
}

// TestCacheTrimJobStateUnreadableIsAnError pins that an unreadable status is an
// error, never active, and that only the first stderr line reaches the error.
func TestCacheTrimJobStateUnreadableIsAnError(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		reply cliReply
		has   string
	}{
		{"a failed kubectl read", cliReply{stderr: "Error from server (NotFound): jobs.batch \"x\" not found\nthe rest", code: 1}, "Error from server (NotFound)"},
		{"garbled JSON", cliReply{stdout: `{"status":`}, ""},
		{"active 0 and no condition", cliReply{stdout: `{"status":{"active":0}}`}, ""},
		{"a Complete condition that is not true", cliReply{stdout: `{"status":{"conditions":[{"type":"Complete","status":"False"}]}}`}, ""},
	} {
		f := newCacheTrimFixture(t, "", map[string]cliReply{cacheTrimJobStateVerb: tc.reply})
		got, err := f.d.cacheTrimJobState(context.Background(), "/kubeconfig", cacheTrimNamespace, cacheTrimJob)
		if err == nil {
			t.Errorf("%s: cacheTrimJobState = %v, nil; want an error", tc.name, got)
			continue
		}
		if got == cacheTrimJobStateActive {
			t.Errorf("%s: read an unreadable status as active", tc.name)
		}
		if tc.has != "" && !strings.Contains(err.Error(), tc.has) {
			t.Errorf("%s: error %q does not name the first stderr line", tc.name, err)
		}
		if strings.Contains(err.Error(), "the rest") {
			t.Errorf("%s: error %q carries more than the first stderr line", tc.name, err)
		}
	}
}

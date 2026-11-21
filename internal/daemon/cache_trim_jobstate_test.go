package daemon

import (
	"context"
	"errors"
	"strings"
	"testing"
)

const cacheTrimJobStateVerb = "get job " + cacheTrimJob + " -n " + cacheTrimNamespace + " -o json"

// TestCacheTrimJobStateCancelledContextIsAnError pins that a read whose context
// ended is an error wrapping context.Canceled, never active. The fixture's
// runner ignores the context and answers a reply that reads active.
func TestCacheTrimJobStateCancelledContextIsAnError(t *testing.T) {
	t.Parallel()
	f := newCacheTrimFixture(t, "", map[string]cliReply{cacheTrimJobStateVerb: {stdout: `{"status":{"active":2}}`}})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got, err := f.d.cacheTrimJobState(ctx, "/kubeconfig", cacheTrimNamespace, cacheTrimJob)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cacheTrimJobState = %v, %v; want an error wrapping context.Canceled", got, err)
	}
}

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
		{"Failed condition that is not true beside an active pod", `{"status":{"active":1,"conditions":[{"type":"Failed","status":"False"}]}}`, cacheTrimJobStateActive},
		{"complete beside a Failed condition", `{"status":{"conditions":[{"type":"Complete","status":"True"},{"type":"Failed","status":"True"}]}}`, cacheTrimJobStateComplete},
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
		{"a Failed condition that is not true alone", cliReply{stdout: `{"status":{"conditions":[{"type":"Failed","status":"False"}]}}`}, ""},
		{"an empty body", cliReply{}, ""},
		{"a null body", cliReply{stdout: `null`}, ""},
		{"an empty object", cliReply{stdout: `{}`}, ""},
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

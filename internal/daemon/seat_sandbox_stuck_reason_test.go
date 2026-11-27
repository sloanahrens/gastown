package daemon

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/sandbox"
)

// stuckItem is the item the stuck-sandbox tracker raises for name.
func stuckItem(name string) attention.Item {
	return attention.Item{Key: seatSandboxStaleKeyPrefix + name, Kind: attention.KindSeatSandboxStale, Severity: attention.SeverityHigh, Summary: "sandbox " + name + " is Provisioning"}
}

func TestAppendStuckSandboxReasons(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		st   sandbox.Status
		want string
	}{
		{"reason and message", sandbox.Status{ReadyKnown: true, ReadyReason: "DependenciesNotReady", ReadyMessage: "sandbox-runtime enforcement dependencies are not ready"}, " Gateway Ready reason: DependenciesNotReady (sandbox-runtime enforcement dependencies are not ready)."},
		{"one line each", sandbox.Status{ReadyKnown: true, ReadyReason: "Supervisor\nNotConnected", ReadyMessage: "no session\r\n\tfor the supervisor"}, " Gateway Ready reason: Supervisor NotConnected (no session for the supervisor)."},
		{"long message cut on a character boundary", sandbox.Status{ReadyKnown: true, ReadyReason: "DependenciesNotReady", ReadyMessage: strings.Repeat("a", 199) + "é"}, " Gateway Ready reason: DependenciesNotReady (" + strings.Repeat("a", 199) + ")."},
		{"no Ready condition", sandbox.Status{}, " Gateway Ready reason: none given."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			it, reads := stuckItem("gt-dag"), 0
			got := appendStuckSandboxReasons([]attention.Item{it}, func(string) bool { return false }, func(name string) (sandbox.Status, error) {
				reads++
				if name != "gt-dag" {
					t.Errorf("read(%q), want the sandbox name without the key prefix", name)
				}
				return tc.st, nil
			})
			if len(got) != 1 || reads != 1 {
				t.Fatalf("got %d items after %d reads, want 1 and 1", len(got), reads)
			}
			if !strings.HasSuffix(got[0].Summary, tc.want) || got[0].Key != it.Key || got[0].Kind != it.Kind {
				t.Errorf("item = %+v, want the input's other fields and a Summary ending %q", got[0], tc.want)
			}
		})
	}
}

func TestAppendStuckSandboxReasonsReadFailure(t *testing.T) {
	t.Parallel()
	for _, err := range []error{
		errors.New("openshell sandbox get gt-dag: exit status 1"),
		errors.New("Gateway Ready reason: SupervisorNotConnected"),
	} {
		t.Run(err.Error(), func(t *testing.T) {
			t.Parallel()
			got := appendStuckSandboxReasons([]attention.Item{stuckItem("gt-dag")}, func(string) bool { return false }, func(string) (sandbox.Status, error) { return sandbox.Status{}, err })
			if len(got) != 1 || !strings.HasSuffix(got[0].Summary, " Gateway Ready reason: not read.") || strings.Contains(got[0].Summary, err.Error()) {
				t.Errorf("got %+v, want the kept item ending in the not-read clause and carrying no error text %q", got, err.Error())
			}
		})
	}
}

func TestAppendStuckSandboxReasonsSkipsFailed(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		failed map[string]bool
		reads  int
	}{
		{"one failed", map[string]bool{"gt-gone": true}, 1},
		{"both failed", map[string]bool{"gt-gone": true, "gt-dag": true}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			items := []attention.Item{stuckItem("gt-gone"), stuckItem("gt-dag")}
			before := append([]attention.Item(nil), items...)
			reads := 0
			got := appendStuckSandboxReasons(items, func(name string) bool { return tc.failed[name] }, func(string) (sandbox.Status, error) {
				reads++
				return sandbox.Status{ReadyKnown: true, ReadyReason: "SupervisorNotConnected", ReadyMessage: "no session"}, nil
			})
			if reads != tc.reads || !reflect.DeepEqual(items, before) {
				t.Errorf("read %d times from input %+v, want %d reads and the input unmodified", reads, items, tc.reads)
			}
			if len(got) != 2 || got[0].Key != items[0].Key || got[1].Key != items[1].Key {
				t.Fatalf("got %+v, want both items in the input's order", got)
			}
			for i, name := range []string{"gt-gone", "gt-dag"} {
				if tc.failed[name] {
					if !reflect.DeepEqual(got[i], before[i]) {
						t.Errorf("failed sandbox %s: item = %+v, want it unchanged", name, got[i])
					}
				} else if !strings.HasSuffix(got[i].Summary, " Gateway Ready reason: SupervisorNotConnected (no session).") {
					t.Errorf("%s: Summary = %q, want the reason appended", name, got[i].Summary)
				}
			}
		})
	}
}

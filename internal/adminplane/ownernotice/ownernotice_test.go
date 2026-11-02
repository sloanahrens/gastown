package ownernotice

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// utcTime is a fixed UTC instant on the given day of November 2026.
func utcTime(day, hour, minute int) time.Time {
	return time.Date(2026, time.November, day, hour, minute, 0, 0, time.UTC)
}

// TestComposeEachCause is the acceptance test for the five causes: each yields
// a subject and a body naming the workspace and the one action open to the
// owner, and an unknown cause is an error.
func TestComposeEachCause(t *testing.T) {
	t.Parallel()
	const workspace = "acme"
	cases := []struct {
		name   string
		ev     Event
		action string
	}{
		{
			name:   "key refused",
			ev:     Event{Workspace: workspace, Cause: CauseKeyRefused, At: utcTime(2, 9, 30)},
			action: "replace the key",
		},
		{
			name:   "daily spend cap",
			ev:     Event{Workspace: workspace, Cause: CauseSpendCap, At: utcTime(2, 9, 30), Spent: 42.75, Cap: 40},
			action: "wait for the next utc day",
		},
		{
			name:   "rotation failed",
			ev:     Event{Workspace: workspace, Cause: CauseRotationFailed, At: utcTime(2, 9, 30)},
			action: "replace the key",
		},
		{
			name:   "suspended",
			ev:     Event{Workspace: workspace, Cause: CauseSuspended, At: utcTime(2, 9, 30)},
			action: "ask the operator",
		},
		{
			name:   "closed",
			ev:     Event{Workspace: workspace, Cause: CauseClosed, At: utcTime(2, 9, 30), Days: 30},
			action: "request an export",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			n, err := Compose(tc.ev)
			if err != nil {
				t.Fatalf("Compose(%s): %v", tc.name, err)
			}
			if n.Subject == "" || n.Body == "" {
				t.Fatalf("Compose(%s) = %+v, want a subject and a body", tc.name, n)
			}
			if !strings.Contains(n.Subject, workspace) {
				t.Errorf("subject %q does not name the workspace", n.Subject)
			}
			if !strings.Contains(n.Body, workspace) {
				t.Errorf("body %q does not name the workspace", n.Body)
			}
			if !strings.Contains(strings.ToLower(n.Body), tc.action) {
				t.Errorf("body %q does not state the action %q", n.Body, tc.action)
			}
			wantKey := workspace + ":" + string(tc.ev.Cause) + ":2026-11-02"
			if n.DedupeKey != wantKey {
				t.Errorf("DedupeKey = %q, want %q", n.DedupeKey, wantKey)
			}
		})
	}

	t.Run("unknown cause", func(t *testing.T) {
		t.Parallel()
		n, err := Compose(Event{Workspace: workspace, Cause: Cause("provider-outage"), At: utcTime(2, 9, 30)})
		if !errors.Is(err, ErrUnknownCause) {
			t.Fatalf("Compose(unknown cause) error = %v, want ErrUnknownCause", err)
		}
		if n != (Notice{}) {
			t.Errorf("Compose(unknown cause) = %+v, want no notice", n)
		}
	})
}

// TestComposeLeaksNothing is the acceptance test for the package's text
// boundary: numbers an event carries for one cause never reach another cause's
// notice, the workspace id (the one string field a notice may print) is the
// only sentinel that appears, and no notice carries a key-shaped string.
func TestComposeLeaksNothing(t *testing.T) {
	t.Parallel()
	const workspace = "ws-4f8c2a1e"
	const (
		spentSentinel = 81726354.42
		capSentinel   = 71625344.31
		daysSentinel  = 61234567
	)
	spentText := fmt.Sprintf("%.2f", spentSentinel)
	capText := fmt.Sprintf("%.2f", capSentinel)
	daysText := strconv.Itoa(daysSentinel)

	events := []Event{
		{Workspace: workspace, Cause: CauseKeyRefused, At: utcTime(2, 9, 30), Spent: spentSentinel, Cap: capSentinel, Days: daysSentinel},
		{Workspace: workspace, Cause: CauseSpendCap, At: utcTime(2, 9, 30), Spent: spentSentinel, Cap: capSentinel, Days: daysSentinel},
		{Workspace: workspace, Cause: CauseRotationFailed, At: utcTime(2, 9, 30), Spent: spentSentinel, Cap: capSentinel, Days: daysSentinel},
		{Workspace: workspace, Cause: CauseSuspended, At: utcTime(2, 9, 30), Spent: spentSentinel, Cap: capSentinel, Days: daysSentinel},
		{Workspace: workspace, Cause: CauseClosed, At: utcTime(2, 9, 30), Spent: spentSentinel, Cap: capSentinel, Days: daysSentinel},
	}
	for _, ev := range events {
		t.Run(string(ev.Cause), func(t *testing.T) {
			t.Parallel()
			n, err := Compose(ev)
			if err != nil {
				t.Fatalf("Compose(%s): %v", ev.Cause, err)
			}
			text := n.Subject + "\n" + n.Body
			if !strings.Contains(text, workspace) {
				t.Errorf("notice does not name the workspace: %q", text)
			}

			switch ev.Cause {
			case CauseSpendCap:
				for _, want := range []string{spentText, capText} {
					if !strings.Contains(text, want) {
						t.Errorf("spend cap notice does not state %s: %q", want, text)
					}
				}
				if strings.Contains(text, daysText) {
					t.Errorf("spend cap notice carries the closed event's days: %q", text)
				}
			case CauseClosed:
				if !strings.Contains(text, daysText) {
					t.Errorf("closed notice does not state its export window: %q", text)
				}
				for _, unwanted := range []string{spentText, capText} {
					if strings.Contains(text, unwanted) {
						t.Errorf("closed notice carries the spend cap event's number %s: %q", unwanted, text)
					}
				}
			default:
				for _, unwanted := range []string{spentText, capText, daysText} {
					if strings.Contains(text, unwanted) {
						t.Errorf("notice carries a number its cause does not use (%s): %q", unwanted, text)
					}
				}
			}

			for _, shape := range keyShapes {
				if shape.MatchString(text) {
					t.Errorf("notice text %q matches key shape %s", text, shape)
				}
			}
		})
	}

	t.Run("unknown cause text is not echoed", func(t *testing.T) {
		t.Parallel()
		const sentinel = "sk-ant-provider-text-SENTINEL"
		_, err := Compose(Event{Workspace: workspace, Cause: Cause(sentinel), At: utcTime(2, 9, 30)})
		if err == nil {
			t.Fatal("Compose(unknown cause) = nil error, want one")
		}
		if strings.Contains(err.Error(), sentinel) {
			t.Errorf("error %q echoes the caller's cause text", err)
		}
	})
}

// keyShapes are the shapes of a credential that must never reach a notice.
var keyShapes = []*regexp.Regexp{
	regexp.MustCompile(`sk-`),
	regexp.MustCompile(`(?i)bearer\s`),
	regexp.MustCompile(`(?i)api[_-]?key`),
	regexp.MustCompile(`[A-Za-z0-9+/=_-]{32,}`),
}

// TestComposeRefusals drives every branch that composes no notice.
func TestComposeRefusals(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		ev   Event
		want error
	}{
		{
			name: "empty workspace",
			ev:   Event{Cause: CauseKeyRefused, At: utcTime(2, 9, 30)},
			want: ErrNoWorkspace,
		},
		{
			name: "workspace with a space",
			ev:   Event{Workspace: "ac me", Cause: CauseKeyRefused, At: utcTime(2, 9, 30)},
			want: ErrBadWorkspace,
		},
		{
			name: "workspace with a colon",
			ev:   Event{Workspace: "acme:prod", Cause: CauseKeyRefused, At: utcTime(2, 9, 30)},
			want: ErrBadWorkspace,
		},
		{
			name: "workspace with a newline",
			ev:   Event{Workspace: "acme\nbcc", Cause: CauseKeyRefused, At: utcTime(2, 9, 30)},
			want: ErrBadWorkspace,
		},
		{
			name: "zero time",
			ev:   Event{Workspace: "acme", Cause: CauseKeyRefused},
			want: ErrNoTime,
		},
		{
			name: "unknown cause",
			ev:   Event{Workspace: "acme", Cause: Cause("outage"), At: utcTime(2, 9, 30)},
			want: ErrUnknownCause,
		},
		{
			name: "closed with no export window",
			ev:   Event{Workspace: "acme", Cause: CauseClosed, At: utcTime(2, 9, 30), Days: 0},
			want: ErrExportWindow,
		},
		{
			name: "closed with a negative export window",
			ev:   Event{Workspace: "acme", Cause: CauseClosed, At: utcTime(2, 9, 30), Days: -30},
			want: ErrExportWindow,
		},
		{
			name: "spend cap with no cap",
			ev:   Event{Workspace: "acme", Cause: CauseSpendCap, At: utcTime(2, 9, 30), Spent: 10, Cap: 0},
			want: ErrSpend,
		},
		{
			name: "spend cap with negative spend",
			ev:   Event{Workspace: "acme", Cause: CauseSpendCap, At: utcTime(2, 9, 30), Spent: -1, Cap: 10},
			want: ErrSpend,
		},
		{
			name: "spend cap with a NaN spend",
			ev:   Event{Workspace: "acme", Cause: CauseSpendCap, At: utcTime(2, 9, 30), Spent: math.NaN(), Cap: 10},
			want: ErrSpend,
		},
		{
			name: "spend cap with an infinite cap",
			ev:   Event{Workspace: "acme", Cause: CauseSpendCap, At: utcTime(2, 9, 30), Spent: 10, Cap: math.Inf(1)},
			want: ErrSpend,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			n, err := Compose(tc.ev)
			if !errors.Is(err, tc.want) {
				t.Fatalf("Compose(%s) error = %v, want %v", tc.name, err, tc.want)
			}
			if n != (Notice{}) {
				t.Errorf("Compose(%s) = %+v, want no notice", tc.name, n)
			}
		})
	}
}

// TestDedupeOncePerCausePerDay is the acceptance test for the deduper: a
// second notice for one cause and workspace on one UTC date is suppressed, and
// the same cause on the next UTC date and another cause on the same date are
// not.
func TestDedupeOncePerCausePerDay(t *testing.T) {
	t.Parallel()
	var d Memory
	lastMinute := utcTime(2, 23, 59)
	nextMidnight := utcTime(3, 0, 0)

	if !d.Claim("acme", CauseKeyRefused, utcTime(2, 9, 30)) {
		t.Fatal("first claim was suppressed, want it sent")
	}
	if d.Claim("acme", CauseKeyRefused, lastMinute) {
		t.Error("second claim on the same UTC day was not suppressed")
	}
	if !d.Claim("acme", CauseKeyRefused, nextMidnight) {
		t.Error("claim on the next UTC day was suppressed, want it sent")
	}
	if !d.Claim("acme", CauseSpendCap, utcTime(2, 9, 30)) {
		t.Error("another cause on the same UTC day was suppressed, want it sent")
	}
	if !d.Claim("other", CauseKeyRefused, utcTime(2, 9, 30)) {
		t.Error("another workspace's same cause and day was suppressed, want it sent")
	}
}

// TestDedupeConcurrent holds Memory to its concurrency contract: of many
// callers claiming the same workspace, cause and UTC day at once, exactly one
// is granted.
func TestDedupeConcurrent(t *testing.T) {
	t.Parallel()
	var d Memory
	const callers = 64
	at := utcTime(2, 9, 30)

	var granted atomic.Int64
	start := make(chan struct{})
	var wg sync.WaitGroup
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if d.Claim("acme", CauseKeyRefused, at) {
				granted.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()

	if got := granted.Load(); got != 1 {
		t.Fatalf("%d concurrent claims were granted, want exactly 1", got)
	}
}

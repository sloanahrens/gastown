package session

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

// notReadyReply is a sandbox still Provisioning whose Ready condition is False
// with the given reason and message. Empty ones are left out, as a condition
// that carries none would be.
func notReadyReply(reason, message string) string {
	return fmt.Sprintf(`{"name":%q,"phase":"Provisioning","conditions":[{"type":"Ready","status":"False","reason":%q,"message":%q}]}`,
		openshellTestSession, reason, message)
}

// TestAwaitReadyTimeoutNamesTheReadyReason drives the wait through Prepare, so
// the timeout it returns is the one a sling reaches.
func TestAwaitReadyTimeoutNamesTheReadyReason(t *testing.T) {
	t.Parallel()
	timeout := fmt.Sprintf("not Ready after %d polls (phase \"Provisioning\")", readyPolls)
	tests := []struct {
		name    string
		reply   string
		want    []string
		notWant []string
	}{
		{
			name:  "the reason and message reach the error",
			reply: notReadyReply("DependenciesNotReady", "Pod is pending: 0/3 nodes are available: Insufficient memory."),
			want:  []string{timeout, "Ready condition: DependenciesNotReady: Pod is pending: 0/3 nodes are available: Insufficient memory."},
		},
		{
			name:    "no reason or message keeps today's error text",
			reply:   notReadyReply("", ""),
			want:    []string{timeout},
			notWant: []string{"Ready condition"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			town, work := openshellTown(t)
			base := time.Date(2026, 11, 13, 2, 0, 0, 0, time.UTC)
			fake := openshellPrepareFake(map[string]string{"get": tt.reply})
			b, _ := openshellTestBackend(town, openshellTestProfile, fake)
			setReadyClock(b, &readyClock{now: base})

			_, err := b.Prepare(context.Background(), openshellSpawn(town, work))
			if err == nil {
				t.Fatal("Prepare succeeded, want the readiness timeout")
			}
			for _, s := range tt.want {
				if !strings.Contains(err.Error(), s) {
					t.Errorf("err = %v, want it to contain %q", err, s)
				}
			}
			for _, s := range tt.notWant {
				if strings.Contains(err.Error(), s) {
					t.Errorf("err = %v, want it to leave out %q", err, s)
				}
			}
		})
	}
}

// A wait that polled again names the Ready reason on the trace's "ready" phase,
// because that is what the operator reads to tell a seat that is waiting on
// the scheduler from one that stalled. A sandbox Ready on its first poll has
// no reason to name.
func TestOpenShellPrepareTraceNotesTheReadyReasonOfALongWait(t *testing.T) {
	t.Parallel()
	const reason, message = "DependenciesNotReady", "Pod is pending"
	tests := []struct {
		name     string
		replies  []string // the sandbox get replies, in poll order; the last repeats
		wantNote bool
	}{
		{
			name:     "a wait that polled again carries the reason of its last not-Ready poll",
			replies:  []string{notReadyReply(reason, message), openshellReadyReply},
			wantNote: true,
		},
		{
			name:    "a sandbox Ready on the first poll carries no note",
			replies: []string{openshellReadyReply},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			town, work := openshellTown(t)
			fake := openshellPrepareFake(map[string]string{"get": openshellReadyReply})
			b, _ := openshellTestBackend(town, openshellTestProfile, fake)
			clock := &readyClock{now: time.Date(2026, 11, 13, 2, 0, 0, 0, time.UTC)}
			setReadyClock(b, clock)
			run := b.cli.Run
			gets := 0
			b.cli.Run = func(ctx context.Context, argv []string, stdin []byte, env []string) ([]byte, []byte, error) {
				if slices.Contains(argv, "get") {
					reply := tt.replies[min(gets, len(tt.replies)-1)]
					gets++
					return []byte(reply), nil, nil
				}
				return run(ctx, argv, stdin, env)
			}
			spec := openshellSpawn(town, work)
			spec.Trace = NewSandboxTrace(func() time.Time { return clock.now })

			if _, err := b.Prepare(context.Background(), spec); err != nil {
				t.Fatalf("Prepare: %v", err)
			}

			summary := spec.Trace.Summary()
			note := "(" + reason + ": " + message + ")"
			if tt.wantNote && !strings.Contains(summary, note) {
				t.Errorf("summary = %q, want the ready phase to carry %q", summary, note)
			}
			if !tt.wantNote && strings.Contains(summary, "(") {
				t.Errorf("summary = %q, want no note on a sandbox Ready on its first poll", summary)
			}
		})
	}
}

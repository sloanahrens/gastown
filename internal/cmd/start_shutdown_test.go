package cmd

import (
	"bytes"
	"errors"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/supervisor"
)

// liveSessions is the set of sessions a shutdown test's fake tmux still has.
type liveSessions map[string]bool

func (l liveSessions) HasSession(name string) (bool, error) { return l[name], nil }

// recordingKiller is a supervisor's tmux that records every session it kills.
type recordingKiller struct{ killed []string }

func (r *recordingKiller) KillSessionWithProcesses(name string) error {
	r.killed = append(r.killed, name)
	return nil
}

// A shutdown deletes a sandboxed session's sandbox once its session is gone,
// and only then: a session that survived the kill keeps its sandbox, and one
// that was already gone was not this shutdown's to clean.
func TestKillSessionsInOrderCleansUpAfterTheKill(t *testing.T) {
	t.Parallel()
	live := liveSessions{"gt-a": true, "gt-stuck": true, "gt-b": true}
	var events []string
	st := shutdownStopper{
		stop: func(sess string) error {
			events = append(events, "stop "+sess)
			if sess != "gt-stuck" {
				delete(live, sess)
			}
			return nil
		},
		cleanup: func(sess string) error {
			events = append(events, "cleanup "+sess)
			if sess == "gt-b" {
				return errors.New("gateway unreachable")
			}
			return nil
		},
	}

	stopped := killSessionsInOrder(live, []string{"gt-a", "gt-gone", "gt-stuck", "gt-b"}, st, io.Discard)

	want := []string{"stop gt-a", "cleanup gt-a", "stop gt-stuck", "stop gt-b", "cleanup gt-b"}
	if !reflect.DeepEqual(events, want) {
		t.Errorf("events = %v, want %v", events, want)
	}
	if stopped != 2 {
		t.Errorf("stopped = %d, want 2 (gt-a, and gt-b whose sandbox cleanup failed); gt-stuck survived, gt-gone was already gone", stopped)
	}
}

// A session that survives its stop is reported with the stop's error, is not
// counted as stopped, and keeps its sandbox. A stop that returned no error
// still says the session is running.
func TestKillSessionsInOrderReportsASessionThatSurvivesItsStop(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		stopErr error
		want    string
	}{
		{name: "stop refused", stopErr: errors.New("killing gt-a: lock busy"), want: "gt-a not stopped: killing gt-a: lock busy"},
		{name: "stop returned nil", stopErr: nil, want: "gt-a not stopped: still running after the stop"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			live := liveSessions{"gt-a": true}
			cleaned := false
			st := shutdownStopper{
				stop: func(string) error { return tt.stopErr },
				cleanup: func(string) error {
					cleaned = true
					return nil
				},
			}
			var out bytes.Buffer

			stopped := killSessionsInOrder(live, []string{"gt-a"}, st, &out)

			if stopped != 0 {
				t.Errorf("stopped = %d, want 0", stopped)
			}
			if cleaned {
				t.Error("cleanup ran for a session that survived its stop")
			}
			if !strings.Contains(out.String(), tt.want) {
				t.Errorf("output = %q, want it to contain %q", out.String(), tt.want)
			}
		})
	}
}

// The town-root stopper ends a session through its supervisor: the kill
// reaches the supervisor's tmux and the stop is recorded in the town's action
// log. A session with no sandbox record cleans up without error.
func TestSupervisedShutdownStopperKillsThroughTheSupervisor(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	tm := &recordingKiller{}
	sup := supervisor.New(supervisor.Options{TownRoot: town, Tmux: tm, Logf: func(string, ...any) {}})
	st := supervisedShutdownStopper(town, sup)

	if err := st.stop("scratch-session"); err != nil {
		t.Fatalf("stop(scratch-session) = %v, want nil", err)
	}
	if want := []string{"scratch-session"}; !reflect.DeepEqual(tm.killed, want) {
		t.Errorf("killed = %v, want %v", tm.killed, want)
	}
	log, err := os.ReadFile(supervisor.ActionLogPath(town))
	if err != nil {
		t.Fatalf("reading action log: %v", err)
	}
	if !strings.Contains(string(log), `"verb":"stop-stray"`) {
		t.Errorf("action log = %q, want a stop-stray line", log)
	}
	if err := st.cleanup("scratch-session"); err != nil {
		t.Errorf("cleanup(scratch-session) = %v, want nil for a session with no sandbox", err)
	}
}

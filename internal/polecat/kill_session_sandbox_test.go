package polecat

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/steveyegge/gastown/internal/rig"
	"github.com/steveyegge/gastown/internal/session"
)

// The failures the tests inject, matched by identity: a caller branches on the
// cause, so the wrapped wording stays the code's to reword (gt-5rdtv).
var (
	errKillFailed   = errors.New("tmux: server gone")
	errPushRefused  = errors.New("rejected by origin")
	errDeleteFailed = errors.New("gateway unreachable")
)

// linkedPusherReached is the panic a kill test gets when it reaches the sandbox
// push without a stub of its own.
const linkedPusherReached = "a polecat test reached the linked sandbox pusher; stub Manager.sandboxPusher"

// newKillTestManager is newTestManager with the sandbox pusher armed, so a kill
// test that forgets to stub Manager.sandboxPusher fails at this guard instead
// of reaching the linked openshell pusher, which runs a sandbox CLI on the
// host (gt-5rdtv). Each test still injects its own pusher to record the push.
func newKillTestManager(r *rig.Rig, w *world, tm sessionProbe, db *polecatDB) *Manager {
	m := newTestManager(r, w, tm, db)
	m.sandboxPusher = func(string, session.SandboxRecord) error {
		panic(linkedPusherReached)
	}
	return m
}

// A failing probe: KillSessionWithProcesses errors, so the test shows the
// sandbox cleanup still runs.
type failingKillProbe struct{ *fakeProbe }

func (p failingKillProbe) KillSessionWithProcesses(string) error {
	return errKillFailed
}

// A kill test that never stubs the pusher fails at the guard rather than
// shelling out to the linked one (gt-5rdtv).
func TestNewKillTestManagerArmsThePusherGuard(t *testing.T) {
	t.Parallel()
	m := newKillTestManager(&rig.Rig{Name: "gastown", Path: t.TempDir()}, nil, newFakeProbe(), nil)
	defer func() {
		if got := recover(); got != linkedPusherReached {
			t.Fatalf("the pusher guard = %v, want %q", got, linkedPusherReached)
		}
	}()
	_ = m.sandboxPusher("", session.SandboxRecord{})
}

//testpolicy:allow parallel — points the process-wide sandbox routing at a temp town, which another test must not see mid-run
func TestKillSessionCleansUpTheSandboxEvenWhenTheKillFails(t *testing.T) {
	town := t.TempDir()
	session.ResetSandboxRoutingForTest(town)
	t.Cleanup(func() { session.ResetSandboxRoutingForTest("") })
	r := &rig.Rig{Name: "gastown", Path: filepath.Join(town, "gastown")}
	m := newKillTestManager(r, nil, failingKillProbe{newFakeProbe()}, nil)
	// session's init links the openshell deleter; stub it on the manager so
	// the test never shells out to a real sandbox CLI (a failed delete keeps
	// the record).
	var deleted []string
	m.sandboxDeleter = func(_ string, rec session.SandboxRecord) error {
		deleted = append(deleted, rec.Session)
		return nil
	}
	m.sandboxPusher = func(string, session.SandboxRecord) error { return nil }
	sessionName := m.sessionName("box")
	if err := session.WriteSandboxRecord(town, session.SandboxRecord{Session: sessionName, Rig: "gastown", Polecat: "box"}); err != nil {
		t.Fatal(err)
	}

	err := m.killSession("box", "test")
	if !errors.Is(err, errKillFailed) {
		t.Fatalf("the kill error must surface: %v", err)
	}
	if len(deleted) != 1 || deleted[0] != sessionName {
		t.Fatalf("the sandbox must be deleted once after a failed kill: %v", deleted)
	}
	if still, serr := session.IsSandboxed(town, sessionName); serr != nil || still {
		t.Fatalf("the sandbox record survived a failed kill (still=%v err=%v); the sandbox must still be cleaned up", still, serr)
	}
}

// A kill that deletes a sandboxed session's sandbox pushes the commits its
// clone holds to origin first, since the sandbox is the only copy of what the
// seat never pushed (gt-fxwtw). A refused push keeps the sandbox and its
// record, so those commits survive.
//
//testpolicy:allow parallel — points the process-wide sandbox routing at a temp town, which another test must not see mid-run
func TestKillSessionPushesTheSandboxWorkBeforeDeletingIt(t *testing.T) {
	tests := []struct {
		name string
		// badRecord writes a record that does not parse, as a registry
		// damaged under the town does.
		badRecord  bool
		pushErr    error
		deleteErr  error
		wantCalls  []string
		wantRecord bool
		wantErr    bool
		// wantCauses are the errors the returned chain must carry, checked by
		// cause identity rather than by the wrapper's wording: the wording is
		// free, the %w that keeps the cause is the contract (gt-5rdtv).
		wantCauses []error
		// wantUnreadable says the failure is the registry that does not parse:
		// the chain must carry the JSON parse error, not just any error.
		wantUnreadable bool
	}{
		{name: "push_succeeds_then_the_sandbox_is_deleted", wantCalls: []string{"push polecat/Box/gt-x", "delete"}},
		{name: "a_gone_sandbox_has_nothing_to_push_so_the_kill_goes_on", pushErr: session.ErrNothingToPush, wantCalls: []string{"push polecat/Box/gt-x", "delete"}},
		{name: "refused_push_keeps_the_sandbox", pushErr: errPushRefused, wantCalls: []string{"push polecat/Box/gt-x"}, wantRecord: true, wantErr: true, wantCauses: []error{errPushRefused}},
		{name: "a_failed_deletion_is_reported_and_keeps_the_record", deleteErr: errDeleteFailed, wantCalls: []string{"push polecat/Box/gt-x", "delete"}, wantRecord: true, wantErr: true, wantCauses: []error{errDeleteFailed}},
		{name: "an_unreadable_registry_is_not_a_host_session", badRecord: true, wantRecord: true, wantErr: true, wantUnreadable: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			town := t.TempDir()
			session.ResetSandboxRoutingForTest(town)
			t.Cleanup(func() { session.ResetSandboxRoutingForTest("") })
			r := &rig.Rig{Name: "gastown", Path: filepath.Join(town, "gastown")}
			m := newKillTestManager(r, nil, newFakeProbe(), nil)
			sessionName := m.sessionName("box")
			recordPath := filepath.Join(session.SandboxRegistryDir(town), sessionName+".json")
			if tt.badRecord {
				if err := os.MkdirAll(filepath.Dir(recordPath), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(recordPath, []byte("{not json"), 0o644); err != nil {
					t.Fatal(err)
				}
			} else if err := session.WriteSandboxRecord(town, session.SandboxRecord{Session: sessionName, Rig: "gastown", Polecat: "box", Branch: "polecat/Box/gt-x"}); err != nil {
				t.Fatal(err)
			}
			var calls []string
			m.sandboxPusher = func(_ string, rec session.SandboxRecord) error {
				calls = append(calls, "push "+rec.Branch)
				return tt.pushErr
			}
			m.sandboxDeleter = func(_ string, rec session.SandboxRecord) error {
				calls = append(calls, "delete")
				return tt.deleteErr
			}

			err := m.killSession("box", "test")

			if !tt.wantErr {
				if err != nil {
					t.Fatalf("killSession = %v, want nil", err)
				}
			} else {
				if err == nil {
					t.Fatal("killSession succeeded; the sandbox failure must surface")
				}
				for _, cause := range tt.wantCauses {
					if !errors.Is(err, cause) {
						t.Errorf("killSession = %v, want it to carry %v", err, cause)
					}
				}
				if tt.wantUnreadable {
					var syntax *json.SyntaxError
					if !errors.As(err, &syntax) {
						t.Errorf("killSession = %v, want it to carry the registry's JSON parse error", err)
					}
				}
			}
			if len(calls) != len(tt.wantCalls) {
				t.Fatalf("sandbox calls = %v, want %v", calls, tt.wantCalls)
			}
			for i := range calls {
				if calls[i] != tt.wantCalls[i] {
					t.Fatalf("sandbox calls = %v, want %v", calls, tt.wantCalls)
				}
			}
			_, statErr := os.Stat(recordPath)
			if kept := statErr == nil; kept != tt.wantRecord {
				t.Errorf("registry record kept = %v, want %v", kept, tt.wantRecord)
			}
		})
	}
}

package doctor

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/events"
	"github.com/steveyegge/gastown/internal/runtime"
	"github.com/steveyegge/gastown/internal/session"
)

type fakeHooksLister struct {
	sessions []string
	err      error
}

func (f fakeHooksLister) ListSessions() ([]string, error) { return f.sessions, f.err }

// sessionHooksCtx is a CheckContext for a town whose events log holds evs.
func sessionHooksCtx(t *testing.T, evs ...events.Event) *CheckContext {
	t.Helper()
	townRoot := t.TempDir()
	var b strings.Builder
	b.WriteString("{torn\n")
	for _, ev := range evs {
		line, err := json.Marshal(ev)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	if err := os.WriteFile(filepath.Join(townRoot, events.EventsFile), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	reg := session.NewPrefixRegistry()
	reg.Register("gt", "gastown")
	return &CheckContext{TownRoot: townRoot, sessionPrefixes: reg}
}

func hooksEv(typ, sess, reason string) events.Event {
	return events.Event{
		Timestamp: "2026-10-01T10:00:00Z",
		Type:      typ,
		Payload:   runtime.HooksStatus{Present: typ == runtime.EventHooksPresent, Reason: reason}.Payload(sess),
	}
}

func TestSessionHooksCheck(t *testing.T) {
	t.Parallel()
	ctx := sessionHooksCtx(t,
		hooksEv(runtime.EventHooksAbsent, "gt-crew-max", "old"),
		hooksEv(runtime.EventHooksPresent, "gt-crew-max", ""), // a later start fixed it
		hooksEv(runtime.EventHooksPresent, "gt-witness", ""),
		hooksEv(runtime.EventHooksPresent, "gt-refinery", ""),
		hooksEv(runtime.EventHooksAbsent, "gt-refinery", "no Claude hooks"), // a respawn lost them
		hooksEv(runtime.EventHooksAbsent, "gt-gone", "dead session"),
		events.Event{Type: events.TypeHandoff, Payload: map[string]interface{}{"session": "hq-mayor"}},
	)
	c := NewSessionHooksCheck()
	c.listerForTest = fakeHooksLister{sessions: []string{"gt-crew-max", "gt-witness", "gt-refinery", "hq-mayor", "scratch"}}

	res := c.Run(ctx)
	if res.Status != StatusWarning {
		t.Fatalf("status = %v, want warning: %+v", res.Status, res)
	}
	got := strings.Join(res.Details, "\n")
	for _, want := range []string{"gt-refinery: hooks:absent at 2026-10-01T10:00:00Z: no Claude hooks", "hq-mayor: no hooks event"} {
		if !strings.Contains(got, want) {
			t.Errorf("details missing %q:\n%s", want, got)
		}
	}
	for _, not := range []string{"gt-crew-max", "gt-witness", "gt-gone", "scratch"} {
		if strings.Contains(got, not) {
			t.Errorf("details name %q, which is present, dead or not a Gas Town session:\n%s", not, got)
		}
	}
	if len(res.Details) != 2 {
		t.Errorf("details = %q, want 2", res.Details)
	}
}

func TestSessionHooksCheck_AllPresent(t *testing.T) {
	t.Parallel()
	ctx := sessionHooksCtx(t, hooksEv(runtime.EventHooksPresent, "hq-mayor", ""))
	c := NewSessionHooksCheck()
	c.listerForTest = fakeHooksLister{sessions: []string{"hq-mayor"}}
	if res := c.Run(ctx); res.Status != StatusOK {
		t.Errorf("status = %v, want OK: %+v", res.Status, res)
	}
}

func TestSessionHooksCheck_ListFailureIsSkipped(t *testing.T) {
	t.Parallel()
	c := NewSessionHooksCheck()
	c.listerForTest = fakeHooksLister{err: errors.New("no server")}
	if res := c.Run(sessionHooksCtx(t)); res.Status != StatusSkipped {
		t.Errorf("status = %v, want skipped", res.Status)
	}
}

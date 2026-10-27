package mail

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// bdNotFoundAnswer is how machine mode reports an issue that does not exist:
// the seam classifies that envelope as ErrNotFound, which mail reads as a
// missing message.
const bdNotFoundAnswer = `{"error":{"kind":"not_found"}}`

// messageAnswer scripts bd for a mailbox whose bead id exists and carries no
// delivery labels: show answers it, update succeeds and is recorded, and any
// other argv fails. A test reads the update calls back to see what mail wrote.
func messageAnswer(t *testing.T, id string) func(c bdCall) (string, string, int) {
	t.Helper()
	return func(c bdCall) (string, string, int) {
		switch c.Args[0] {
		case "show":
			return `[{"id":"` + id + `","title":"A message","description":"","status":"open","priority":2,"assignee":"gastown/max","created_at":"2026-10-01T00:00:00Z","labels":["gt:message"]}]`, "", 0
		case "update":
			return "", "", 0
		}
		return "", "unexpected bd args: " + strings.Join(c.Args, " "), 1
	}
}

// updateArgvs returns each update call's arguments joined by spaces.
func updateArgvs(bd *bdScript) []string {
	var out []string
	for _, c := range bd.recorded() {
		if c.Args[0] == "update" {
			out = append(out, strings.Join(c.Args, " "))
		}
	}
	return out
}

// TestMarkReadOnlyAddsTheReadLabel pins the write behind marking a message
// read without closing it: one update adding the read label in the mutation
// environment a write needs. The write names no actor, so bd records the
// ambient identity, as the label command it replaces did.
func TestMarkReadOnlyAddsTheReadLabel(t *testing.T) {
	t.Parallel()
	const id = "gt-wisp-read1"
	bd := &bdScript{answer: messageAnswer(t, id)}
	m := NewMailboxWithBeadsDir("gastown/crew/max", t.TempDir(), t.TempDir())
	m.bd = bd.run

	if err := m.MarkReadOnly(id); err != nil {
		t.Fatalf("MarkReadOnly: %v", err)
	}

	if got := updateArgvs(bd); len(got) != 1 || got[0] != "update "+id+" --add-label=read" {
		t.Fatalf("update calls = %v, want one adding the read label", got)
	}
	for _, c := range bd.recorded() {
		if c.Args[0] != "update" {
			continue
		}
		if got, _ := envValue(c, "BD_DOLT_AUTO_COMMIT"); got != "on" {
			t.Errorf("BD_DOLT_AUTO_COMMIT = %q, want on (the write environment)", got)
		}
		if strings.Contains(strings.Join(c.Args, " "), "--actor=") {
			t.Errorf("update names an actor: %v; a label write runs as the ambient identity", c.Args)
		}
	}
}

// TestMarkReadOnlyReportsAnAddFailureWhateverItSays: bd's "does not have
// label" text means success only when removing a label. An add that fails —
// with that text included — is a failed write, and mail must say so rather
// than report the message as read.
func TestMarkReadOnlyReportsAnAddFailureWhateverItSays(t *testing.T) {
	t.Parallel()
	const id = "gt-wisp-read2"
	bd := &bdScript{answer: func(c bdCall) (string, string, int) {
		if c.Args[0] == "update" {
			return "", "Issue " + id + " does not have label read", 1
		}
		return messageAnswer(t, id)(c)
	}}
	m := NewMailboxWithBeadsDir("gastown/crew/max", t.TempDir(), t.TempDir())
	m.bd = bd.run

	err := m.MarkReadOnly(id)
	if err == nil {
		t.Fatal("MarkReadOnly reported success after bd refused the add")
	}
	if errors.Is(err, ErrMessageNotFound) {
		t.Errorf("a refused add read as a missing message: %v", err)
	}
}

// TestMarkUnreadOnlyRemovesTheReadLabel covers the removal bd accepts and the
// one it refuses because the label is already gone: the second is the state
// the caller asked for, so it is not a failure.
func TestMarkUnreadOnlyRemovesTheReadLabel(t *testing.T) {
	t.Parallel()
	const id = "gt-wisp-unread1"
	tests := []struct {
		name    string
		answer  func(c bdCall) (string, string, int)
		wantErr bool
	}{
		{
			name:   "label present",
			answer: messageAnswer(t, id),
		},
		{
			name: "label already gone",
			answer: func(c bdCall) (string, string, int) {
				if c.Args[0] == "update" {
					return "", "Issue " + id + " does not have label read", 1
				}
				return messageAnswer(t, id)(c)
			},
		},
		{
			name: "write failed for another reason",
			answer: func(c bdCall) (string, string, int) {
				if c.Args[0] == "update" {
					return "", "Error: database is unavailable", 1
				}
				return messageAnswer(t, id)(c)
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			bd := &bdScript{answer: tt.answer}
			m := NewMailboxWithBeadsDir("gastown/crew/max", t.TempDir(), t.TempDir())
			m.bd = bd.run

			err := m.MarkUnreadOnly(id)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("MarkUnreadOnly succeeded, want the write failure")
				}
				return
			}
			if err != nil {
				t.Fatalf("MarkUnreadOnly: %v", err)
			}
			if got := updateArgvs(bd); len(got) != 1 || got[0] != "update "+id+" --remove-label=read" {
				t.Fatalf("update calls = %v, want one removing the read label", got)
			}
		})
	}
}

// TestMarkUnreadReopensByStatus pins how a message is put back in the inbox:
// an update setting status open, which is the seam's reopen (see the comment
// in markUnreadBeads).
func TestMarkUnreadReopensByStatus(t *testing.T) {
	t.Parallel()
	const id = "gt-wisp-reopen1"
	bd := &bdScript{answer: messageAnswer(t, id)}
	m := NewMailboxWithBeadsDir("gastown/crew/max", t.TempDir(), t.TempDir())
	m.bd = bd.run

	if err := m.MarkUnread(id); err != nil {
		t.Fatalf("MarkUnread: %v", err)
	}

	if got := updateArgvs(bd); len(got) != 1 || got[0] != "update "+id+" --status=open" {
		t.Fatalf("update calls = %v, want one setting status open", got)
	}
}

// TestMailboxWritesFallBackToTheMailboxDir: a cross-rig id (ne-*) may live in
// the home database rather than the one its prefix routes to (ne-bgr), so a
// write bd answers "no such issue" to in the routed directory is retried in
// the mailbox's own — and an id neither has is ErrMessageNotFound.
func TestMailboxWritesFallBackToTheMailboxDir(t *testing.T) {
	t.Parallel()
	const id = "ne-msg-1"

	townRoot := t.TempDir()
	townBeads := filepath.Join(townRoot, ".beads")
	rigBeads := filepath.Join(townRoot, "other", ".beads")
	for _, dir := range []string{townBeads, rigBeads} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	routes := []byte(`{"prefix":"ne-","path":"other"}` + "\n")
	if err := os.WriteFile(filepath.Join(townBeads, "routes.jsonl"), routes, 0o644); err != nil {
		t.Fatalf("write routes: %v", err)
	}

	tests := []struct {
		name      string
		foundIn   string
		wantErr   error
		wantWrite string
	}{
		{name: "home database has it", foundIn: townBeads, wantWrite: townBeads},
		{name: "neither has it", foundIn: "", wantErr: ErrMessageNotFound},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var writeDirs []string
			bd := &bdScript{answer: func(c bdCall) (string, string, int) {
				beadsDir, _ := envValue(c, "BEADS_DIR")
				if c.Args[0] != "update" {
					return "", "unexpected bd args: " + strings.Join(c.Args, " "), 1
				}
				if tt.foundIn != "" && beadsDir == tt.foundIn {
					writeDirs = append(writeDirs, beadsDir)
					return "", "", 0
				}
				return bdNotFoundAnswer, "Issue " + id + " not found", 1
			}}
			m := NewMailboxWithBeadsDir("gastown/crew/max", t.TempDir(), townBeads)
			m.bd = bd.run

			err := m.MarkUnread(id)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("MarkUnread = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("MarkUnread: %v", err)
			}
			if len(writeDirs) != 1 || writeDirs[0] != tt.wantWrite {
				t.Fatalf("writes ran against %v, want one against %s", writeDirs, tt.wantWrite)
			}
			if n := len(updateArgvs(bd)); n != 2 {
				t.Fatalf("got %d update calls, want the routed attempt and the retry", n)
			}
		})
	}
}

package mail

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
)

func TestNewMailbox(t *testing.T) {
	t.Parallel()
	m := NewMailbox("/tmp/test")
	if filepath.ToSlash(m.path) != "/tmp/test/inbox.jsonl" {
		t.Errorf("NewMailbox path = %q, want %q", m.path, "/tmp/test/inbox.jsonl")
	}
	if !m.legacy {
		t.Error("NewMailbox should create legacy mailbox")
	}
}

func TestNewMailboxBeads(t *testing.T) {
	t.Parallel()
	m := NewMailboxBeads("gastown/Toast", "/work/dir")
	if m.identity != "gastown/Toast" {
		t.Errorf("identity = %q, want %q", m.identity, "gastown/Toast")
	}
	if m.legacy {
		t.Error("NewMailboxBeads should not create legacy mailbox")
	}
}

func TestMailboxLegacyAppend(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	m := NewMailbox(tmpDir)

	msg := &Message{
		ID:        "msg-001",
		From:      "mayor/",
		To:        "gastown/Toast",
		Subject:   "Test message",
		Body:      "Hello world",
		Timestamp: time.Now(),
	}

	if err := m.Append(msg); err != nil {
		t.Fatalf("Append error: %v", err)
	}

	// Verify file exists
	if _, err := os.Stat(m.path); os.IsNotExist(err) {
		t.Fatal("inbox.jsonl was not created")
	}

	// Verify content
	content, err := os.ReadFile(m.path)
	if err != nil {
		t.Fatalf("ReadFile error: %v", err)
	}

	var readMsg Message
	if err := json.Unmarshal(content[:len(content)-1], &readMsg); err != nil { // -1 for newline
		t.Fatalf("Unmarshal error: %v", err)
	}

	if readMsg.ID != msg.ID {
		t.Errorf("ID = %q, want %q", readMsg.ID, msg.ID)
	}
}

func TestMailboxLegacyList(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	m := NewMailbox(tmpDir)

	// Append multiple messages
	msgs := []*Message{
		{ID: "msg-001", Subject: "First", Timestamp: time.Now().Add(-2 * time.Hour)},
		{ID: "msg-002", Subject: "Second", Timestamp: time.Now().Add(-1 * time.Hour)},
		{ID: "msg-003", Subject: "Third", Timestamp: time.Now()},
	}

	for _, msg := range msgs {
		if err := m.Append(msg); err != nil {
			t.Fatalf("Append error: %v", err)
		}
	}

	// List should return newest first
	listed, err := m.List()
	if err != nil {
		t.Fatalf("List error: %v", err)
	}

	if len(listed) != 3 {
		t.Fatalf("List returned %d messages, want 3", len(listed))
	}

	// Verify order (newest first)
	if listed[0].ID != "msg-003" {
		t.Errorf("First message ID = %q, want msg-003 (newest)", listed[0].ID)
	}
	if listed[2].ID != "msg-001" {
		t.Errorf("Last message ID = %q, want msg-001 (oldest)", listed[2].ID)
	}
}

func TestMailboxLegacyGet(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	m := NewMailbox(tmpDir)

	msg := &Message{
		ID:      "msg-001",
		Subject: "Test",
		Body:    "Content",
	}
	if err := m.Append(msg); err != nil {
		t.Fatalf("Append error: %v", err)
	}

	// Get existing message
	got, err := m.Get("msg-001")
	if err != nil {
		t.Fatalf("Get error: %v", err)
	}
	if got.Subject != "Test" {
		t.Errorf("Subject = %q, want %q", got.Subject, "Test")
	}

	// Get non-existent message
	_, err = m.Get("msg-nonexistent")
	if err != ErrMessageNotFound {
		t.Errorf("Get non-existent = %v, want ErrMessageNotFound", err)
	}
}

func TestMailboxLegacyMarkRead(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	m := NewMailbox(tmpDir)

	msg := &Message{
		ID:   "msg-001",
		Read: false,
	}
	if err := m.Append(msg); err != nil {
		t.Fatalf("Append error: %v", err)
	}

	// Mark as read
	if err := m.MarkRead("msg-001"); err != nil {
		t.Fatalf("MarkRead error: %v", err)
	}

	// Verify it's now read
	got, err := m.Get("msg-001")
	if err != nil {
		t.Fatalf("Get error: %v", err)
	}
	if !got.Read {
		t.Error("Message should be marked as read")
	}

	// Mark non-existent
	err = m.MarkRead("msg-nonexistent")
	if err != ErrMessageNotFound {
		t.Errorf("MarkRead non-existent = %v, want ErrMessageNotFound", err)
	}
}

func TestMailboxLegacyDelete(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	m := NewMailbox(tmpDir)

	msgs := []*Message{
		{ID: "msg-001", Subject: "First"},
		{ID: "msg-002", Subject: "Second"},
	}
	for _, msg := range msgs {
		if err := m.Append(msg); err != nil {
			t.Fatalf("Append error: %v", err)
		}
	}

	// Delete one
	if err := m.Delete("msg-001"); err != nil {
		t.Fatalf("Delete error: %v", err)
	}

	// Verify only one remains
	listed, err := m.List()
	if err != nil {
		t.Fatalf("List error: %v", err)
	}
	if len(listed) != 1 {
		t.Fatalf("List returned %d messages, want 1", len(listed))
	}
	if listed[0].ID != "msg-002" {
		t.Errorf("Remaining message ID = %q, want msg-002", listed[0].ID)
	}

	// Delete non-existent
	err = m.Delete("msg-nonexistent")
	if err != ErrMessageNotFound {
		t.Errorf("Delete non-existent = %v, want ErrMessageNotFound", err)
	}
}

func TestMailboxLegacyCount(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	m := NewMailbox(tmpDir)

	// Empty inbox
	total, unread, err := m.Count()
	if err != nil {
		t.Fatalf("Count error: %v", err)
	}
	if total != 0 || unread != 0 {
		t.Errorf("Empty inbox count = (%d, %d), want (0, 0)", total, unread)
	}

	// Add messages
	msgs := []*Message{
		{ID: "msg-001", Read: false},
		{ID: "msg-002", Read: true},
		{ID: "msg-003", Read: false},
	}
	for _, msg := range msgs {
		if err := m.Append(msg); err != nil {
			t.Fatalf("Append error: %v", err)
		}
	}

	total, unread, err = m.Count()
	if err != nil {
		t.Fatalf("Count error: %v", err)
	}
	if total != 3 {
		t.Errorf("total = %d, want 3", total)
	}
	if unread != 2 {
		t.Errorf("unread = %d, want 2", unread)
	}
}

func TestMailboxLegacyListUnread(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	m := NewMailbox(tmpDir)

	msgs := []*Message{
		{ID: "msg-001", Read: false},
		{ID: "msg-002", Read: true},
		{ID: "msg-003", Read: false},
	}
	for _, msg := range msgs {
		if err := m.Append(msg); err != nil {
			t.Fatalf("Append error: %v", err)
		}
	}

	unread, err := m.ListUnread()
	if err != nil {
		t.Fatalf("ListUnread error: %v", err)
	}
	if len(unread) != 2 {
		t.Errorf("ListUnread returned %d, want 2", len(unread))
	}
}

func TestMailboxMarkReadOnlyExcludesFromUnread(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	m := NewMailbox(tmpDir)

	msgs := []*Message{
		{ID: "msg-001", Read: false, Subject: "First"},
		{ID: "msg-002", Read: false, Subject: "Second"},
	}
	for _, msg := range msgs {
		if err := m.Append(msg); err != nil {
			t.Fatalf("Append error: %v", err)
		}
	}

	// Both should be unread initially
	unread, err := m.ListUnread()
	if err != nil {
		t.Fatalf("ListUnread error: %v", err)
	}
	if len(unread) != 2 {
		t.Errorf("ListUnread returned %d, want 2", len(unread))
	}

	// Mark one as read-only (simulates gt mail read behavior)
	if err := m.MarkReadOnly("msg-001"); err != nil {
		t.Fatalf("MarkReadOnly error: %v", err)
	}

	// Should only have 1 unread now
	unread, err = m.ListUnread()
	if err != nil {
		t.Fatalf("ListUnread error: %v", err)
	}
	if len(unread) != 1 {
		t.Errorf("ListUnread returned %d after MarkReadOnly, want 1", len(unread))
	}
	if len(unread) == 1 && unread[0].ID != "msg-002" {
		t.Errorf("Expected msg-002 to be unread, got %s", unread[0].ID)
	}

	// The marked message should still be in full list
	all, err := m.List()
	if err != nil {
		t.Fatalf("List error: %v", err)
	}
	if len(all) != 2 {
		t.Errorf("List returned %d, want 2 (MarkReadOnly should not remove)", len(all))
	}
}

func TestMailboxLegacyListByThread(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	m := NewMailbox(tmpDir)

	msgs := []*Message{
		{ID: "msg-001", ThreadID: "thread-A", Timestamp: time.Now().Add(-2 * time.Hour)},
		{ID: "msg-002", ThreadID: "thread-B", Timestamp: time.Now().Add(-1 * time.Hour)},
		{ID: "msg-003", ThreadID: "thread-A", Timestamp: time.Now()},
	}
	for _, msg := range msgs {
		if err := m.Append(msg); err != nil {
			t.Fatalf("Append error: %v", err)
		}
	}

	// Get thread A
	thread, err := m.ListByThread("thread-A")
	if err != nil {
		t.Fatalf("ListByThread error: %v", err)
	}
	if len(thread) != 2 {
		t.Fatalf("thread-A has %d messages, want 2", len(thread))
	}

	// Verify oldest first
	if thread[0].ID != "msg-001" {
		t.Errorf("First thread message = %q, want msg-001 (oldest)", thread[0].ID)
	}

	// Non-existent thread
	empty, err := m.ListByThread("thread-nonexistent")
	if err != nil {
		t.Fatalf("ListByThread error: %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("Non-existent thread has %d messages, want 0", len(empty))
	}
}

func TestMailboxLegacyEmptyInbox(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	m := NewMailbox(tmpDir)

	// List on non-existent file should return empty, not error
	msgs, err := m.List()
	if err != nil {
		t.Fatalf("List on empty inbox error: %v", err)
	}
	if len(msgs) != 0 {
		t.Errorf("Empty inbox returned %d messages, want 0", len(msgs))
	}
}

func TestMailboxBeadsAppendError(t *testing.T) {
	t.Parallel()
	m := NewMailboxBeads("gastown/Toast", "/work/dir")

	err := m.Append(&Message{})
	if err == nil {
		t.Error("Append on beads mailbox should error")
	}
}

func TestMailboxIdentityAndPath(t *testing.T) {
	t.Parallel()
	// Legacy mailbox
	legacy := NewMailbox("/tmp/test")
	if legacy.Identity() != "" {
		t.Errorf("Legacy mailbox identity = %q, want empty", legacy.Identity())
	}
	if filepath.ToSlash(legacy.Path()) != "/tmp/test/inbox.jsonl" {
		t.Errorf("Legacy mailbox path = %q, want /tmp/test/inbox.jsonl", legacy.Path())
	}

	// Beads mailbox
	beads := NewMailboxBeads("gastown/Toast", "/work/dir")
	if beads.Identity() != "gastown/Toast" {
		t.Errorf("Beads mailbox identity = %q, want gastown/Toast", beads.Identity())
	}
	if beads.Path() != "" {
		t.Errorf("Beads mailbox path = %q, want empty", beads.Path())
	}
}

func TestMailboxPersistence(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()

	// Create mailbox and add message
	m1 := NewMailbox(tmpDir)
	msg := &Message{
		ID:      "persist-001",
		Subject: "Persistent message",
		Body:    "Should survive reload",
	}
	if err := m1.Append(msg); err != nil {
		t.Fatalf("Append error: %v", err)
	}

	// Create new mailbox pointing to same location
	m2 := NewMailbox(tmpDir)
	msgs, err := m2.List()
	if err != nil {
		t.Fatalf("List error: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("Reloaded mailbox has %d messages, want 1", len(msgs))
	}
	if msgs[0].Subject != "Persistent message" {
		t.Errorf("Subject = %q, want 'Persistent message'", msgs[0].Subject)
	}
}

func TestNewMailboxWithBeadsDir(t *testing.T) {
	t.Parallel()
	m := NewMailboxWithBeadsDir("gastown/Toast", "/work/dir", "/custom/.beads")
	if m.identity != "gastown/Toast" {
		t.Errorf("identity = %q, want 'gastown/Toast'", m.identity)
	}
	if filepath.ToSlash(m.beadsDir) != "/custom/.beads" {
		t.Errorf("beadsDir = %q, want '/custom/.beads'", m.beadsDir)
	}
}

// TestCloseInDirPassesActorMatchingAssignee guards against gt-ovem: bd's
// assignee==actor close guard rejects a close whose actor differs from the
// bead's assignee, so `gt mail archive` fails when the ambient actor bd would
// otherwise fall back to (BD_ACTOR/git user.name) differs from the mailbox's
// own identity. closeInDir must pass --actor explicitly using that identity so
// the close always matches the assignee the message was filed under.
func TestCloseInDirPassesActorMatchingAssignee(t *testing.T) {
	t.Parallel()
	bd := &bdScript{answer: func(c bdCall) (string, string, int) {
		if c.Args[0] == "close" {
			return "", "", 0
		}
		return "", "unexpected bd args: " + strings.Join(c.Args, " "), 1
	}}
	m := NewMailboxWithBeadsDir("gastown/crew/max", t.TempDir(), t.TempDir())
	m.bd = bd.run
	if err := m.closeInDir("gt-wisp-xv525", t.TempDir()); err != nil {
		t.Fatalf("closeInDir: %v", err)
	}

	if log := strings.Join(bd.argvs(), "\n"); !strings.Contains(log, "--actor=gastown/max") {
		t.Fatalf("bd close missing --actor=gastown/max matching assignee identity; calls:\n%s", log)
	}
}

func TestMailboxListFromDirConvergesWispQueryAndFiltersStatuses(t *testing.T) {
	t.Parallel()
	beadsDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(beadsDir, ".gt-types-configured"), []byte(beads.TypeConfigSentinelValue()+"\n"), 0644); err != nil {
		t.Fatalf("write types sentinel: %v", err)
	}

	bd := &bdScript{answer: func(c bdCall) (string, string, int) {
		// The argv is matched whole: a substring would also accept the wrong
		// filter (a label where an assignee is asked for), and a fixture that
		// answers a broken call proves nothing about the call.
		switch argv := strings.Join(c.Args, " "); argv {
		case "list --json --label=gt:message --assignee=gastown/synth --limit=0 --flat":
			return `[{"id":"issue-direct-open","title":"Direct open","description":"","status":"open","priority":2,"assignee":"gastown/synth","created_at":"2026-06-12T12:00:05Z","labels":["gt:message","from:mayor/"]},{"id":"issue-direct-hooked","title":"Direct hooked","description":"","status":"hooked","priority":2,"assignee":"gastown/synth","created_at":"2026-06-12T12:00:04Z","labels":["gt:message","from:mayor/"]},{"id":"issue-direct-closed","title":"Direct closed","description":"","status":"closed","priority":2,"assignee":"gastown/synth","created_at":"2026-06-12T12:00:03Z","labels":["gt:message","from:mayor/"]}]`, "", 0
		case "list --json --label=gt:message --label=cc:gastown/synth --limit=0 --flat":
			return `[{"id":"issue-cc-open","title":"CC open","description":"","status":"open","priority":2,"assignee":"mayor/","created_at":"2026-06-12T12:00:02Z","labels":["gt:message","cc:gastown/synth","from:mayor/"]},{"id":"issue-cc-hooked","title":"CC hooked","description":"","status":"hooked","priority":2,"assignee":"mayor/","created_at":"2026-06-12T12:00:01Z","labels":["gt:message","cc:gastown/synth","from:mayor/"]}]`, "", 0
		case `query --json ephemeral=true AND label="gt:message" AND (status="open" OR status="hooked") --limit=0`:
			return `[{"id":"wisp-direct-open","title":"Wisp direct open","description":"","status":"open","priority":2,"assignee":"gastown/synth","created_at":"2026-06-12T12:00:00Z","labels":["gt:message","from:mayor/"]},{"id":"wisp-direct-hooked","title":"Wisp direct hooked","description":"","status":"hooked","priority":2,"assignee":"gastown/synth","created_at":"2026-06-12T11:59:59Z","labels":["gt:message","from:mayor/"]},{"id":"wisp-cc-open","title":"Wisp CC open","description":"","status":"open","priority":2,"assignee":"mayor/","created_at":"2026-06-12T11:59:58Z","labels":["gt:message","cc:gastown/synth","from:mayor/"]},{"id":"wisp-cc-hooked","title":"Wisp CC hooked","description":"","status":"hooked","priority":2,"assignee":"mayor/","created_at":"2026-06-12T11:59:57Z","labels":["gt:message","cc:gastown/synth","from:mayor/"]},{"id":"issue-direct-open","title":"Duplicate wisp","description":"","status":"open","priority":2,"assignee":"gastown/synth","created_at":"2026-06-12T11:59:56Z","labels":["gt:message","from:mayor/"]},{"id":"wisp-unrelated","title":"Someone else's wisp","description":"","status":"open","priority":2,"assignee":"other/agent","created_at":"2026-06-12T11:59:55Z","labels":["gt:message"]}]`, "", 0
		default:
			return "", "unexpected bd args: " + argv, 1
		}
	}}

	m := NewMailboxWithBeadsDir("gastown/synth", t.TempDir(), beadsDir)
	m.bd = bd.run
	msgs, err := m.listFromDir(beadsDir)
	if err != nil {
		t.Fatalf("listFromDir: %v", err)
	}

	byID := make(map[string]*Message)
	for _, msg := range msgs {
		byID[msg.ID] = msg
	}
	wantPresent := []string{
		"issue-direct-open",
		"issue-direct-hooked",
		"issue-cc-open",
		"wisp-direct-open",
		"wisp-direct-hooked",
		"wisp-cc-open",
	}
	for _, id := range wantPresent {
		if byID[id] == nil {
			t.Fatalf("missing message %s in %#v", id, byID)
		}
	}
	wantAbsent := []string{
		"issue-direct-closed",
		"issue-cc-hooked",
		"wisp-cc-hooked",
		"wisp-unrelated",
	}
	for _, id := range wantAbsent {
		if byID[id] != nil {
			t.Fatalf("unexpected message %s in inbox", id)
		}
	}
	if byID["issue-direct-open"].Wisp {
		t.Fatal("issue duplicate should keep issue result, not later wisp result")
	}
	for _, id := range []string{"wisp-direct-open", "wisp-direct-hooked", "wisp-cc-open"} {
		if !byID[id].Wisp {
			t.Fatalf("%s should be marked as wisp", id)
		}
	}

	// Three reads: assignee list, cc list, one wisp query. The wisp read stays
	// one call whatever the identity list: the match flags are read off each
	// record, and its status set is pushed into the query.
	calls := bd.argvs()
	if len(calls) != 3 {
		t.Fatalf("bd calls = %d, want 3 (assignee, cc, wisps); calls:\n%s", len(calls), strings.Join(calls, "\n"))
	}
	queries := 0
	for _, c := range bd.recorded() {
		if c.Args[0] == "query" {
			queries++
		}
	}
	if queries != 1 {
		t.Fatalf("bd query calls = %d, want 1; calls:\n%s", queries, strings.Join(calls, "\n"))
	}
}

// TestMailboxGetBeads: the show a Get runs returns the message, and bd's
// absence reaches the caller as ErrMessageNotFound — the error every mail path
// matches on — whether bd spells it as its typed not-found exit status or only
// in the text an older bd prints. A read that failed for any other reason is
// not an absence: it must surface as the failure it is, not as a missing
// message.
func TestMailboxGetBeads(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		stdout      string
		stderr      string
		code        int
		wantErr     error // nil with wantFailure: any error but ErrMessageNotFound
		wantFailure bool
		wantSub     string
	}{
		{
			name:    "record",
			stdout:  `[{"id":"gt-msg","title":"Subject line","description":"Body text","status":"open","labels":["gt:message"]}]`,
			wantSub: "Subject line",
		},
		{
			// The typed status alone: bd's stderr says nothing to read.
			name:    "typed absence",
			stderr:  "Error: exit status 20\n",
			code:    20,
			wantErr: ErrMessageNotFound,
		},
		{name: "text absence", stderr: "Error: no issue found\n", code: 1, wantErr: ErrMessageNotFound},
		{
			// An older bd names the id it could not find.
			name:    "text absence naming the id",
			stderr:  "Error: issue gt-msg not found\n",
			code:    1,
			wantErr: ErrMessageNotFound,
		},
		{
			// A failure, not an answer: the store was unreachable.
			name:        "store failure",
			stderr:      "dial tcp 127.0.0.1:3307: connect: connection refused\n",
			code:        1,
			wantFailure: true,
		},
		{
			// "not found" about something else must not read as a missing
			// message.
			name:        "unrelated not-found text",
			stderr:      "Error: config file not found: /nonexistent/config.yaml\n",
			code:        1,
			wantFailure: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			bd := &bdScript{answer: func(c bdCall) (string, string, int) {
				if c.Args[0] != "show" {
					return "", "unexpected bd args: " + strings.Join(c.Args, " "), 1
				}
				return tt.stdout, tt.stderr, tt.code
			}}
			m := NewMailboxWithBeadsDir("gastown/synth", t.TempDir(), t.TempDir())
			m.bd = bd.run

			msg, err := m.Get("gt-msg")
			if tt.wantFailure {
				if err == nil {
					t.Fatalf("Get error = nil, want the read's failure")
				}
				if errors.Is(err, ErrMessageNotFound) {
					t.Fatalf("Get error = %v, want the failure, not a missing message", err)
				}
				return
			}
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Get error = %v, want %v", err, tt.wantErr)
			}
			if tt.wantSub == "" {
				return
			}
			if msg.Subject != tt.wantSub {
				t.Fatalf("Subject = %q, want %q", msg.Subject, tt.wantSub)
			}
			if msg.Body != "Body text" {
				t.Fatalf("Body = %q, want %q", msg.Body, "Body text")
			}
		})
	}
}

// A wisp assigned to one identity and cc'ing another must carry both match
// flags — the read serves every identity at once, so neither match may be lost
// because the record arrived from the other one's point of view — and a wisp
// naming none of them must not come back at all. The read is one query whose
// status set is pushed down, whatever the identity list.
func TestQueryWispMessagesKeepsBothMatchFlags(t *testing.T) {
	t.Parallel()
	bd := &bdScript{answer: func(c bdCall) (string, string, int) {
		if argv := strings.Join(c.Args, " "); argv == `query --json ephemeral=true AND label="gt:message" AND (status="open" OR status="hooked") --limit=0` {
			return `[{"id":"wisp-both","title":"Both","status":"open","priority":2,"assignee":"mayor/","created_at":"2026-06-12T12:00:00Z","labels":["gt:message","cc:mayor"]},{"id":"wisp-unrelated","title":"Unrelated","status":"open","priority":2,"assignee":"rig/other","created_at":"2026-06-12T12:00:00Z","labels":["gt:message"]}]`, "", 0
		}
		return "", "unexpected bd args: " + strings.Join(c.Args, " "), 1
	}}

	m := NewMailboxWithBeadsDir("mayor/", t.TempDir(), t.TempDir())
	m.bd = bd.run
	wisps, err := m.queryWispMessages(t.TempDir(), []string{"mayor", "mayor/"})
	if err != nil {
		t.Fatalf("queryWispMessages: %v", err)
	}
	if len(wisps) != 1 || wisps[0].message.ID != "wisp-both" {
		t.Fatalf("wisps = %+v, want only the record naming an asked identity", wisps)
	}
	if !wisps[0].assigneeMatch || !wisps[0].ccMatch {
		t.Fatalf("match flags = assignee:%v cc:%v, want both set", wisps[0].assigneeMatch, wisps[0].ccMatch)
	}
	if calls := bd.argvs(); len(calls) != 1 {
		t.Fatalf("bd calls = %d, want one query whatever the identity list; calls:\n%s", len(calls), strings.Join(calls, "\n"))
	}
}

// TestQueryIssueMessagesBatchKeepsOnlyTheAskedIdentities: the batch read covers
// every open gt:message record, so the rows it hands back must be exactly the
// ones assigned to, or cc'ing, an identity the caller asked about.
func TestQueryIssueMessagesBatchKeepsOnlyTheAskedIdentities(t *testing.T) {
	t.Parallel()
	bd := &bdScript{answer: func(c bdCall) (string, string, int) {
		if c.Args[0] != "list" {
			return "", "unexpected bd args: " + strings.Join(c.Args, " "), 1
		}
		return `[{"id":"assigned","title":"Assigned","status":"open","assignee":"mayor","labels":["gt:message"]},{"id":"cc","title":"CC","status":"open","assignee":"someone/else","labels":["gt:message","cc:mayor"]},{"id":"unrelated","title":"Unrelated","status":"open","assignee":"rig/other","labels":["gt:message"]}]`, "", 0
	}}

	rows, err := queryIssueMessagesBatch(bd.run, t.TempDir(), t.TempDir(), []string{"mayor"})
	if err != nil {
		t.Fatalf("queryIssueMessagesBatch: %v", err)
	}

	var ids []string
	for _, row := range rows {
		ids = append(ids, row.ID)
	}
	if len(ids) != 2 || ids[0] != "assigned" || ids[1] != "cc" {
		t.Fatalf("rows = %q, want [assigned cc] — a message to nobody the caller asked about is dropped", ids)
	}
	if rows[1].CCLabelsCSV != "cc:mayor" {
		t.Fatalf("CCLabelsCSV = %q, want cc:mayor", rows[1].CCLabelsCSV)
	}
}

func TestParseWispTimestamp(t *testing.T) {
	t.Parallel()
	want := time.Date(2026, 6, 12, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name  string
		value string
		want  time.Time
		ok    bool
	}{
		{name: "rfc3339", value: "2026-06-12T12:00:00Z", want: want, ok: true},
		{name: "rfc3339 offset", value: "2026-06-12T08:00:00-04:00", want: want, ok: true},
		{name: "go utc", value: "2026-06-12 12:00:00 +0000 UTC", want: want, ok: true},
		{name: "sql datetime", value: "2026-06-12 12:00:00", want: want, ok: true},
		{name: "empty"},
		{name: "malformed", value: "not a timestamp"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parseWispTimestamp(tt.value)
			if ok != tt.ok {
				t.Fatalf("parseWispTimestamp(%q) ok = %v, want %v", tt.value, ok, tt.ok)
			}
			if ok && !got.Equal(tt.want) {
				t.Fatalf("parseWispTimestamp(%q) = %v, want %v", tt.value, got, tt.want)
			}
			if ok && got.Location() != time.UTC {
				t.Fatalf("parseWispTimestamp(%q) location = %v, want UTC", tt.value, got.Location())
			}
		})
	}
}

func TestMailboxLegacyMultipleOperations(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	m := NewMailbox(tmpDir)

	// Append multiple messages
	for i := 0; i < 5; i++ {
		msg := &Message{
			ID:        fmt.Sprintf("msg-%03d", i),
			Subject:   fmt.Sprintf("Subject %d", i),
			Body:      fmt.Sprintf("Body %d", i),
			Read:      i%2 == 0, // Alternate read/unread
			Timestamp: time.Now().Add(time.Duration(i) * time.Minute),
		}
		if err := m.Append(msg); err != nil {
			t.Fatalf("Append error: %v", err)
		}
	}

	// Delete middle message
	if err := m.Delete("msg-002"); err != nil {
		t.Fatalf("Delete error: %v", err)
	}

	// Mark one as read
	if err := m.MarkRead("msg-001"); err != nil {
		t.Fatalf("MarkRead error: %v", err)
	}

	// Verify counts
	total, unread, err := m.Count()
	if err != nil {
		t.Fatalf("Count error: %v", err)
	}
	if total != 4 {
		t.Errorf("total = %d, want 4", total)
	}
	// After marking msg-001 as read, we have: msg-000 (read), msg-001 (read), msg-003 (unread), msg-004 (read)
	// So unread = 1
	if unread != 1 {
		t.Errorf("unread = %d, want 1", unread)
	}
}

func TestMailboxLegacyAppendWithMissingDir(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	deepPath := filepath.Join(tmpDir, "deep", "nested", "inbox")
	m := NewMailbox(deepPath)

	msg := &Message{
		ID:      "msg-001",
		Subject: "Test",
	}

	// Should create directories
	if err := m.Append(msg); err != nil {
		t.Fatalf("Append error: %v", err)
	}

	// Verify file exists
	if _, err := os.Stat(m.path); os.IsNotExist(err) {
		t.Fatal("inbox.jsonl was not created")
	}
}

func TestMailboxLegacyDeleteAll(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	m := NewMailbox(tmpDir)

	// Add messages
	msgs := []*Message{
		{ID: "msg-001"},
		{ID: "msg-002"},
	}
	for _, msg := range msgs {
		if err := m.Append(msg); err != nil {
			t.Fatalf("Append error: %v", err)
		}
	}

	// Delete all
	for _, msg := range msgs {
		if err := m.Delete(msg.ID); err != nil {
			t.Fatalf("Delete error: %v", err)
		}
	}

	// Should be empty
	total, _, err := m.Count()
	if err != nil {
		t.Fatalf("Count error: %v", err)
	}
	if total != 0 {
		t.Errorf("total = %d, want 0", total)
	}
}

func TestMailboxLegacyMarkReadTwice(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	m := NewMailbox(tmpDir)

	msg := &Message{ID: "msg-001", Read: false}
	if err := m.Append(msg); err != nil {
		t.Fatalf("Append error: %v", err)
	}

	// Mark as read twice
	if err := m.MarkRead("msg-001"); err != nil {
		t.Fatalf("First MarkRead error: %v", err)
	}
	if err := m.MarkRead("msg-001"); err != nil {
		t.Fatalf("Second MarkRead error: %v", err)
	}

	// Should still be read
	got, err := m.Get("msg-001")
	if err != nil {
		t.Fatalf("Get error: %v", err)
	}
	if !got.Read {
		t.Error("Message should be marked as read")
	}
}

func TestMailboxLegacyCorruptionDetection(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	m := NewMailbox(tmpDir)

	// Write a valid message followed by a corrupt line
	msg := &Message{ID: "msg-001", Subject: "Valid"}
	if err := m.Append(msg); err != nil {
		t.Fatalf("Append error: %v", err)
	}

	// Manually append a corrupt line
	f, err := os.OpenFile(m.path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatalf("OpenFile error: %v", err)
	}
	if _, err := f.WriteString("this is not valid json\n"); err != nil {
		t.Fatalf("WriteString error: %v", err)
	}
	f.Close()

	// List should return error mentioning corruption
	_, err = m.List()
	if err == nil {
		t.Fatal("List should return error for corrupt mailbox")
	}
	if !strings.Contains(err.Error(), "corrupt mailbox") {
		t.Errorf("error should mention corruption, got: %v", err)
	}
	if !strings.Contains(err.Error(), "line 2") {
		t.Errorf("error should mention line number, got: %v", err)
	}
}

func TestMailboxLegacyArchiveCorruptionDetection(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	m := NewMailbox(tmpDir)

	// Create a corrupt archive file
	archivePath := m.ArchivePath()
	if err := os.WriteFile(archivePath, []byte("{bad json\n"), 0644); err != nil {
		t.Fatalf("WriteFile error: %v", err)
	}

	_, err := m.ListArchived()
	if err == nil {
		t.Fatal("ListArchived should return error for corrupt archive")
	}
	if !strings.Contains(err.Error(), "corrupt archive") {
		t.Errorf("error should mention corruption, got: %v", err)
	}
}

func TestMailboxLegacyConcurrentMarkRead(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	m := NewMailbox(tmpDir)

	// Add messages
	for i := 0; i < 10; i++ {
		msg := &Message{
			ID:        fmt.Sprintf("msg-%03d", i),
			Subject:   fmt.Sprintf("Subject %d", i),
			Read:      false,
			Timestamp: time.Now().Add(time.Duration(i) * time.Minute),
		}
		if err := m.Append(msg); err != nil {
			t.Fatalf("Append error: %v", err)
		}
	}

	// Concurrently mark different messages as read
	var wg sync.WaitGroup
	errs := make([]error, 10)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			errs[idx] = m.MarkRead(fmt.Sprintf("msg-%03d", idx))
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("MarkRead msg-%03d error: %v", i, err)
		}
	}

	// All messages should be marked as read
	messages, err := m.List()
	if err != nil {
		t.Fatalf("List error: %v", err)
	}
	if len(messages) != 10 {
		t.Fatalf("Expected 10 messages, got %d", len(messages))
	}
	for _, msg := range messages {
		if !msg.Read {
			t.Errorf("Message %s should be marked as read", msg.ID)
		}
	}
}

func TestMailboxLegacyAtomicArchive(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	m := NewMailbox(tmpDir)

	// Add messages
	msgs := []*Message{
		{ID: "msg-001", Subject: "First", Timestamp: time.Now().Add(-2 * time.Hour)},
		{ID: "msg-002", Subject: "Second", Timestamp: time.Now().Add(-1 * time.Hour)},
		{ID: "msg-003", Subject: "Third", Timestamp: time.Now()},
	}
	for _, msg := range msgs {
		if err := m.Append(msg); err != nil {
			t.Fatalf("Append error: %v", err)
		}
	}

	// Archive the middle message
	if err := m.Archive("msg-002"); err != nil {
		t.Fatalf("Archive error: %v", err)
	}

	// Inbox should have 2 messages
	inbox, err := m.List()
	if err != nil {
		t.Fatalf("List error: %v", err)
	}
	if len(inbox) != 2 {
		t.Fatalf("Expected 2 inbox messages, got %d", len(inbox))
	}

	// Archive should have 1 message
	archived, err := m.ListArchived()
	if err != nil {
		t.Fatalf("ListArchived error: %v", err)
	}
	if len(archived) != 1 {
		t.Fatalf("Expected 1 archived message, got %d", len(archived))
	}
	if archived[0].ID != "msg-002" {
		t.Errorf("Archived message ID = %q, want msg-002", archived[0].ID)
	}
}

// writeTypesSentinel marks beadsDir as already configured so listFromDir does
// not spawn bd list for the custom-type setup check.
func writeTypesSentinel(t *testing.T, beadsDir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(beadsDir, ".gt-types-configured"), []byte(beads.TypeConfigSentinelValue()+"\n"), 0644); err != nil {
		t.Fatalf("write types sentinel: %v", err)
	}
}

// A failed wisp query must surface: wisp-stored mail lives only in the wisps
// table, so swallowing the error shows an empty inbox with a nil error.
func TestListFromDirWispError(t *testing.T) {
	t.Parallel()
	beadsDir := t.TempDir()
	writeTypesSentinel(t, beadsDir)

	bd := &bdScript{answer: func(c bdCall) (string, string, int) {
		switch c.Args[0] {
		case "list":
			return "[]", "", 0
		case "query":
			return "", "Error: context deadline exceeded: timed out waiting for bd query\n", 1
		}
		return "", "unexpected bd args: " + strings.Join(c.Args, " "), 1
	}}

	m := NewMailboxWithBeadsDir("gastown/synth", t.TempDir(), beadsDir)
	m.bd = bd.run

	if _, err := m.List(); err == nil {
		t.Fatal("List returned nil error for a failed wisp query; wisp mail would read as an empty inbox")
	} else if !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("List error = %v, want the wisp query failure", err)
	}
}

// A database without a wisps table is normal (no wisp has been written yet),
// and must keep listing as empty rather than failing.
func TestListFromDirMissingWispTableIsEmpty(t *testing.T) {
	t.Parallel()
	beadsDir := t.TempDir()
	writeTypesSentinel(t, beadsDir)

	bd := &bdScript{answer: func(c bdCall) (string, string, int) {
		switch c.Args[0] {
		case "list":
			return "[]", "", 0
		case "query":
			return "", "Error 1146 (42S02): Table 'beads.wisps' doesn't exist\n", 1
		}
		return "", "unexpected bd args: " + strings.Join(c.Args, " "), 1
	}}

	m := NewMailboxWithBeadsDir("gastown/synth", t.TempDir(), beadsDir)
	m.bd = bd.run

	msgs, err := m.List()
	if err != nil {
		t.Fatalf("List with no wisps table: %v", err)
	}
	if len(msgs) != 0 {
		t.Fatalf("messages = %d, want 0", len(msgs))
	}
}

// One oversized legacy line must not poison the inbox: List used to fail with
// bufio.ErrTooLong for good, which also left Delete unable to remove it.
func TestListLegacyLongLine(t *testing.T) {
	t.Parallel()
	m := NewMailbox(t.TempDir())

	msg := &Message{
		ID:        "msg-long",
		Subject:   "large body",
		Body:      strings.Repeat("x", 100*1024),
		Timestamp: time.Now(),
	}
	if err := m.Append(msg); err != nil {
		t.Fatalf("Append: %v", err)
	}

	listed, err := m.List()
	if err != nil {
		t.Fatalf("List with a 100KB line: %v", err)
	}
	if len(listed) != 1 || listed[0].ID != msg.ID {
		t.Fatalf("List = %#v, want the 100KB message", listed)
	}

	if err := m.Delete(msg.ID); err != nil {
		t.Fatalf("Delete of the 100KB message: %v", err)
	}
	after, err := m.List()
	if err != nil {
		t.Fatalf("List after delete: %v", err)
	}
	if len(after) != 0 {
		t.Fatalf("messages after delete = %d, want 0", len(after))
	}
}

// A failed CC query returns the mail the other queries found and names the
// failure in a warning, instead of failing the whole list or hiding it.
func TestListFromDirCCErrorKeepsOtherMail(t *testing.T) {
	t.Parallel()
	beadsDir := t.TempDir()
	writeTypesSentinel(t, beadsDir)

	bd := &bdScript{answer: func(c bdCall) (string, string, int) {
		switch c.Args[0] {
		case "list":
			if strings.Contains(strings.Join(c.Args, " "), "cc:") {
				return "", "Error: context deadline exceeded: timed out waiting for bd list\n", 1
			}
			return `[{"id":"direct-1","title":"Direct","description":"","status":"open","priority":2,"assignee":"gastown/synth","created_at":"2026-06-12T12:00:00Z","labels":["gt:message","from:mayor/"]}]`, "", 0
		case "query":
			return "[]", "", 0
		}
		return "", "unexpected bd args: " + strings.Join(c.Args, " "), 1
	}}

	var warn bytes.Buffer
	m := NewMailboxWithBeadsDir("gastown/synth", t.TempDir(), beadsDir)
	m.bd = bd.run
	m.warn = &warn

	msgs, err := m.List()
	if err != nil {
		t.Fatalf("List error = %v, want the assignee mail with a warning", err)
	}
	if len(msgs) != 1 || msgs[0].ID != "direct-1" {
		t.Fatalf("List = %v, want the one assignee message", msgs)
	}
	got := warn.String()
	if !strings.Contains(got, "CC mail query failed") || !strings.Contains(got, "timed out waiting for bd list") {
		t.Fatalf("warning = %q, want it to name the CC query failure", got)
	}
}

// Archive appends and PurgeArchive rewrites share one archive file. Appends
// racing a purge must all survive it: every fresh message is still archived
// once the writers and the purges have finished.
func TestPurgeArchiveConcurrentArchive(t *testing.T) {
	t.Parallel()
	m := NewMailboxWithBeadsDir("gastown/synth", t.TempDir(), t.TempDir())

	old := time.Now().AddDate(0, 0, -30)
	for i := 0; i < 20; i++ {
		if err := m.appendToArchive(&Message{ID: fmt.Sprintf("old-%d", i), Timestamp: old}); err != nil {
			t.Fatalf("seed archive: %v", err)
		}
	}

	const writers, perWriter = 4, 60
	var wg, purger sync.WaitGroup
	done := make(chan struct{})
	errs := make(chan error, writers*perWriter+1024)
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				msg := &Message{ID: fmt.Sprintf("fresh-%d-%d", w, i), Timestamp: time.Now()}
				if err := m.appendToArchive(msg); err != nil {
					errs <- err
				}
			}
		}(w)
	}
	// The purger rewrites the archive until every writer has finished.
	purger.Add(1)
	go func() {
		defer purger.Done()
		for {
			select {
			case <-done:
				return
			default:
			}
			if _, err := m.PurgeArchive(1); err != nil {
				errs <- err
			}
		}
	}()
	wg.Wait()
	close(done)
	purger.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent archive or purge: %v", err)
	}

	left, err := m.ListArchived()
	if err != nil {
		t.Fatalf("ListArchived: %v", err)
	}
	fresh := 0
	for _, msg := range left {
		if strings.HasPrefix(msg.ID, "fresh-") {
			fresh++
		}
	}
	if fresh != writers*perWriter {
		t.Fatalf("archived fresh messages = %d, want %d: a purge dropped appended mail", fresh, writers*perWriter)
	}
}

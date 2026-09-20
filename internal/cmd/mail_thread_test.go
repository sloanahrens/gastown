package cmd

import (
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/mail"
)

// recoveredBeadProtocolBody is the dead-worker RECOVERED_BEAD payload a
// refinery sends the deacon (internal/refinery/dead_worker_recovery.go).
// gt-8sex: replying to a thread carrying this body sent the template itself
// instead of the caller's -m prose, and a deacon may read the template as a
// fresh redispatch request for the same bead.
const recoveredBeadProtocolBody = `Merge rejection with no live worker (transient polecat).

Bead: gt-ntqf
Polecat: gastown/polecats/flint
MR: gt-wisp-c12
Branch: polecat/flint/gt-ntqf
Failure-Type: tests
Attempt: 1

The source bead has been reopened with merge-rejection notes.
Please re-dispatch. The branch survives on origin, so the next polecat
can check it out and make a targeted fix instead of starting over.`

const proseAck = "Ack — flint is still on it, so no redispatch needed.\n" +
	"I re-verified the worker before replying; standing down."

// TestNewReplyMessageSendsCallerBodyNotParentPayload is the regression test
// gt-8sex asked for: on a thread whose original body carries protocol fields,
// the reply body is exactly the caller's text and contains none of the
// original's protocol block.
func TestNewReplyMessageSendsCallerBodyNotParentPayload(t *testing.T) {
	t.Parallel()
	original := &mail.Message{
		ID:       "hq-wisp-z6l09",
		From:     "gastown/refinery",
		To:       "deacon/",
		Subject:  "RECOVERED_BEAD gt-ntqf",
		Body:     recoveredBeadProtocolBody,
		ThreadID: "thread-fcf046cd0263",
	}

	reply := newReplyMessage("gastown/refinery", original, "Re: "+original.Subject, proseAck)

	if reply.Body != proseAck {
		t.Errorf("reply body is not the caller's text\n got: %q\nwant: %q", reply.Body, proseAck)
	}
	for _, field := range []string{"Bead:", "Polecat:", "MR:", "Branch:", "Failure-Type:", "Attempt:", "Please re-dispatch"} {
		if strings.Contains(reply.Body, field) {
			t.Errorf("reply body leaked the parent's protocol field %q:\n%s", field, reply.Body)
		}
	}
	if reply.Body == original.Body {
		t.Error("reply body is a verbatim copy of the original's payload")
	}

	// Threading comes from the original; content never does.
	if reply.ReplyTo != original.ID {
		t.Errorf("ReplyTo = %q, want %q", reply.ReplyTo, original.ID)
	}
	if reply.ThreadID != original.ThreadID {
		t.Errorf("ThreadID = %q, want %q", reply.ThreadID, original.ThreadID)
	}
	if reply.To != original.From {
		t.Errorf("To = %q, want %q (reply to sender)", reply.To, original.From)
	}
	if reply.Type != mail.TypeReply {
		t.Errorf("Type = %q, want %q", reply.Type, mail.TypeReply)
	}
}

// TestNewReplyMessageKeepsUntheadedReplyUntheaded pins that the pure builder
// does not invent a thread either — runMailReply generates one after the
// builder returns, so the builder must report "no thread" as "no thread".
func TestNewReplyMessageKeepsUntheadedReplyUntheaded(t *testing.T) {
	t.Parallel()
	original := &mail.Message{ID: "hq-wisp-abc", From: "mayor/", Subject: "Status"}

	reply := newReplyMessage("gastown/refinery", original, "Re: Status", "on it")

	if reply.ThreadID != "" {
		t.Errorf("ThreadID = %q, want empty (runMailReply assigns one)", reply.ThreadID)
	}
}

func TestReplyDuplicatesProtocolPayload(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		parentBody string
		replyBody  string
		want       bool
	}{
		{"verbatim protocol payload", recoveredBeadProtocolBody, recoveredBeadProtocolBody, true},
		{"payload re-sent with stray whitespace", recoveredBeadProtocolBody, "\n" + recoveredBeadProtocolBody + "\n\n", true},
		{"prose reply to a protocol thread", recoveredBeadProtocolBody, proseAck, false},
		{"prose reply that quotes the payload", recoveredBeadProtocolBody, "> Bead: gt-ntqf\n> Attempt: 1\n\nAgreed, standing down.", false},
		{"prose reply to prose", "Status check please", "Status check please", false},
		{"empty reply", recoveredBeadProtocolBody, "", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := replyDuplicatesProtocolPayload(tc.parentBody, tc.replyBody); got != tc.want {
				t.Errorf("replyDuplicatesProtocolPayload() = %v, want %v", got, tc.want)
			}
		})
	}
}

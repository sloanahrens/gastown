package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/mail"
	"github.com/steveyegge/gastown/internal/protocol"
	"github.com/steveyegge/gastown/internal/style"
)

func runMailThread(cmd *cobra.Command, args []string) error {
	threadID := args[0]

	// All mail uses town beads (two-level architecture)
	workDir, err := findMailWorkDir()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}

	// Determine which inbox
	address := detectSender()

	// Get mailbox and thread messages
	router := mail.NewRouter(workDir)
	mailbox, err := router.GetMailbox(address)
	if err != nil {
		return fmt.Errorf("getting mailbox: %w", err)
	}

	messages, err := mailbox.ListByThread(threadID)
	if err != nil {
		return fmt.Errorf("getting thread: %w", err)
	}

	// JSON output
	if mailThreadJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(messages)
	}

	// Human-readable output
	fmt.Printf("%s Thread: %s (%d messages)\n\n",
		style.Bold.Render("🧵"), threadID, len(messages))

	if len(messages) == 0 {
		fmt.Printf("  %s\n", style.Dim.Render("(no messages in thread)"))
		return nil
	}

	for i, msg := range messages {
		typeMarker := ""
		if msg.Type != "" && msg.Type != mail.TypeNotification {
			typeMarker = fmt.Sprintf(" [%s]", msg.Type)
		}
		priorityMarker := ""
		if msg.Priority == mail.PriorityHigh || msg.Priority == mail.PriorityUrgent {
			priorityMarker = " " + style.Bold.Render("!")
		}

		if i > 0 {
			fmt.Printf("  %s\n", style.Dim.Render("│"))
		}
		fmt.Printf("  %s %s%s%s\n", style.Bold.Render("●"), msg.Subject, typeMarker, priorityMarker)
		fmt.Printf("    %s from %s to %s\n",
			style.Dim.Render(msg.ID),
			msg.From, msg.To)
		fmt.Printf("    %s\n",
			style.Dim.Render(msg.Timestamp.Local().Format("2006-01-02 15:04")))

		if msg.Body != "" {
			fmt.Printf("    %s\n", msg.Body)
		}
	}

	return nil
}

func runMailReply(cmd *cobra.Command, args []string) error {
	msgID := args[0]

	// Get message body from positional arg or flag (positional takes precedence)
	messageBody := mailReplyMessage
	if len(args) > 1 {
		messageBody = args[1]
	}

	// Validate message is provided
	if messageBody == "" {
		return fmt.Errorf("message body required: provide as second argument or use -m flag")
	}

	// All mail uses town beads (two-level architecture)
	workDir, err := findMailWorkDir()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}

	// Determine current address
	from := detectSender()

	// Get the original message
	router := mail.NewRouter(workDir)
	mailbox, err := router.GetMailbox(from)
	if err != nil {
		return fmt.Errorf("getting mailbox: %w", err)
	}

	original, err := mailbox.Get(msgID)
	if err != nil {
		return fmt.Errorf("getting message: %w", err)
	}

	// Refuse to mint a duplicate protocol payload. A reply body that is a
	// verbatim copy of the original's protocol payload is never the prose the
	// caller meant to send — it means -m was dropped, substituted, or mangled
	// upstream — and the original was a dispatch/redispatch request that
	// automation acts on. See gt-8sex: a prose ack of a RECOVERED_BEAD thread
	// went out as the RECOVERED_BEAD template, which a deacon could have
	// parsed as a second redispatch request for the same bead.
	if replyDuplicatesProtocolPayload(original.Body, messageBody) {
		return fmt.Errorf("refusing to send: reply body is a verbatim copy of the protocol payload in %s (%q)\n"+
			"Protocol payloads are acted on by automation, so replaying one mints a duplicate request;\n"+
			"-m was probably dropped or substituted before reaching gt.\n"+
			"To acknowledge or comment on that message, send prose instead — a plain body, or "+
			"`gt mail send %s -s %q -m <text> --reply-to %s` if you really mean to re-send the payload",
			msgID, original.Subject, original.From, "Re: "+original.Subject, msgID)
	}

	// Build the reply. The body is exactly the caller's text: the original is
	// consulted for threading metadata (sender, subject, thread) only, never
	// for content.
	subject := mailReplySubject
	if subject == "" {
		if strings.HasPrefix(original.Subject, "Re: ") {
			subject = original.Subject
		} else {
			subject = "Re: " + original.Subject
		}
	}

	reply := newReplyMessage(from, original, subject, messageBody)

	// If original has no thread ID, create one
	if reply.ThreadID == "" {
		reply.ThreadID = generateThreadID()
	}

	// Send the reply (defer drains async notification goroutines before CLI exits)
	defer router.WaitPendingNotifications()
	if err := router.Send(reply); err != nil {
		return fmt.Errorf("sending reply: %w", err)
	}
	if err := router.ClearReplyReminders(from, reply.ThreadID); err != nil {
		style.PrintWarning("could not clear satisfied reply reminders: %v", err)
	}

	fmt.Printf("%s Reply sent to %s\n", style.Bold.Render("✓"), original.From)
	fmt.Printf("  Subject: %s\n", subject)
	if original.ThreadID != "" {
		fmt.Printf("  Thread: %s\n", style.Dim.Render(original.ThreadID))
	}

	return nil
}

// newReplyMessage builds the reply to original.
//
// Contract: the reply carries exactly the caller's body. The original is read
// for threading metadata only (sender, id, thread) — its body is never
// copied, quoted, or inherited. Anything else would let a reply on a protocol
// thread re-emit that protocol's payload (gt-8sex).
func newReplyMessage(from string, original *mail.Message, subject, body string) *mail.Message {
	return &mail.Message{
		From:     from,
		To:       original.From, // Reply to sender
		Subject:  subject,
		Body:     body,
		Type:     mail.TypeReply,
		Priority: mail.PriorityNormal,
		ReplyTo:  original.ID,
		ThreadID: original.ThreadID,
	}
}

// replyDuplicatesProtocolPayload reports whether sending replyBody as a reply
// to a message whose body is parentBody would re-mint a protocol payload
// verbatim. Both sides must be the same text modulo surrounding whitespace,
// and the parent must be shaped like a payload — so ordinary prose replies,
// and replies that merely quote a payload, are unaffected.
func replyDuplicatesProtocolPayload(parentBody, replyBody string) bool {
	if strings.TrimSpace(replyBody) == "" {
		return false
	}
	if strings.TrimSpace(parentBody) != strings.TrimSpace(replyBody) {
		return false
	}
	return protocol.LooksLikeProtocolPayload(parentBody)
}

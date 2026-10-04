package cmd

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/mail"
	"github.com/steveyegge/gastown/internal/style"
	"github.com/steveyegge/gastown/internal/workspace"
)

func runMailSend(cmd *cobra.Command, args []string) error {
	// Handle --stdin: read message body from stdin (avoids shell quoting issues)
	if mailStdin {
		if mailBody != "" {
			return fmt.Errorf("cannot use --stdin with --message/-m")
		}
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return fmt.Errorf("reading stdin: %w", err)
		}
		mailBody = strings.TrimRight(string(data), "\n")
	}

	to, err := mailSendRecipient(args)
	if err != nil {
		return err
	}

	// All mail uses town beads (two-level architecture)
	workDir, err := findMailWorkDir()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}

	// Determine sender (--from overrides auto-detection, for relay/bridge use)
	from := mailFrom
	if from == "" {
		from = detectSender()
	}

	// If subject looks like a reply ("Re: ...") but the user didn't pass
	// --reply-to, try to infer the original message from the sender's inbox.
	// When exactly one unambiguous match exists, populate the reply-to so the
	// thread lookup and reply-reminder clearing in mail.SendMessage work as
	// designed. hq-k382x: without this, every "gt mail send <addr> -s
	// 'Re: ...'" leaves the queued reply-reminder in place.
	replyTo := mailReplyTo
	if replyTo == "" && hasReplyPrefix(mailSubject) {
		replyTo = inferReplyTo(workDir, from, to, mailSubject)
	}

	router := mail.NewRouter(workDir, townRegistry())
	receipt, err := router.SendMessage(mail.SendRequest{
		From:           from,
		To:             to,
		Subject:        mailSubject,
		Body:           mailBody,
		Priority:       mailSendPriority(mailUrgent, mailPriority, mailNotify),
		Type:           mail.ParseMessageType(mailType),
		CC:             mailCC,
		Pinned:         mailPinned,
		Wisp:           mailWisp && !mailPermanent,
		SuppressNotify: mailNoNotify,
		ReplyTo:        replyTo,
	})
	if err != nil {
		return err
	}

	printSendReceipt(to, mailSubject, receipt)
	return nil
}

// mailSendRecipient is the address the message goes to: --self resolved from
// cwd, --to, or the positional argument.
func mailSendRecipient(args []string) (string, error) {
	if mailSendSelf {
		// Auto-detect identity from cwd
		cwd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("getting current directory: %w", err)
		}
		townRoot, err := workspace.FindFromCwd()
		if err != nil || townRoot == "" {
			return "", fmt.Errorf("not in a Gas Town workspace")
		}
		roleInfo, err := GetRoleWithContext(cwd, townRoot)
		if err != nil {
			return "", fmt.Errorf("detecting role: %w", err)
		}
		ctx := RoleContext{
			Role:     roleInfo.Role,
			Rig:      roleInfo.Rig,
			Polecat:  roleInfo.Polecat,
			TownRoot: townRoot,
			WorkDir:  cwd,
		}
		to := buildAgentIdentity(ctx)
		if to == "" {
			return "", fmt.Errorf("cannot determine identity (role: %s)", ctx.Role)
		}
		return to, nil
	}
	if mailTo != "" {
		return mailTo, nil
	}
	if len(args) > 0 {
		return args[0], nil
	}
	return "", fmt.Errorf("address required (use positional arg, --to, or --self)")
}

// mailSendPriority is the priority the flags describe: --urgent wins, and
// --notify bumps a normal message to high (the notification itself is
// automatic; --no-notify is what suppresses it).
func mailSendPriority(urgent bool, priority int, notify bool) mail.Priority {
	if urgent {
		return mail.PriorityUrgent
	}
	p := mail.PriorityFromInt(priority)
	if notify && p == mail.PriorityNormal {
		p = mail.PriorityHigh
	}
	return p
}

// printSendReceipt reports what the send did.
func printSendReceipt(to, subject string, receipt mail.Receipt) {
	for _, w := range receipt.Warnings {
		style.PrintWarning("%s", w)
	}
	if len(receipt.Failed) > 0 {
		fmt.Fprintf(os.Stderr, "⚠ Some deliveries failed: %s\n", strings.Join(receipt.Failed, "; "))
	}

	fmt.Printf("%s Message sent to %s\n", style.Bold.Render("✓"), to)
	fmt.Printf("  Subject: %s\n", subject)

	// Show resolved recipients if fan-out occurred
	if addrs := receipt.Recipients; len(addrs) > 1 || (len(addrs) == 1 && addrs[0] != to) {
		fmt.Printf("  Recipients: %s\n", strings.Join(addrs, ", "))
	}

	msg := receipt.Message
	if msg == nil {
		return
	}
	if len(msg.CC) > 0 {
		fmt.Printf("  CC: %s\n", strings.Join(msg.CC, ", "))
	}
	if msg.Type != mail.TypeNotification {
		fmt.Printf("  Type: %s\n", msg.Type)
	}
}

// generateThreadID creates a random thread ID for new message threads.
func generateThreadID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b) // crypto/rand.Read only fails on broken system
	return "thread-" + hex.EncodeToString(b)
}

// hasReplyPrefix reports whether subject begins with a "Re:" prefix
// (case-insensitive, tolerating arbitrary whitespace after the colon).
func hasReplyPrefix(subject string) bool {
	s := strings.TrimSpace(subject)
	if len(s) < 3 {
		return false
	}
	return strings.EqualFold(s[:3], "re:")
}

// normalizeReplySubject strips leading "Re: " prefixes (case-insensitive,
// possibly nested) and surrounding whitespace, so that two subjects with
// different reply nesting compare equal.
func normalizeReplySubject(subject string) string {
	s := strings.TrimSpace(subject)
	for hasReplyPrefix(s) {
		s = strings.TrimSpace(s[3:])
	}
	return strings.ToLower(s)
}

// normalizeAddress lowercases an address and trims a trailing slash so that
// "Gastown/Toast/" and "gastown/toast" compare equal. Matches identityVariants
// behavior in mail.Mailbox without depending on its internals.
func normalizeAddress(addr string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(addr)), "/")
}

// inferReplyTo searches the sender's mailbox for a single unambiguous message
// FROM `to` whose subject (after stripping "Re:" prefixes) matches `subject`.
// Returns the matching message ID when exactly one match exists; returns "" on
// no-match, ambiguity, or any error. Best-effort — used only as a convenience
// to make `gt mail send <to> -s "Re: ..."` clear queued reply-reminders.
func inferReplyTo(workDir, from, to, subject string) string {
	router := mail.NewRouter(workDir, townRegistry())
	mailbox, err := router.GetMailbox(from)
	if err != nil {
		return ""
	}
	messages, err := mailbox.List()
	if err != nil {
		return ""
	}
	return pickReplyTo(messages, to, subject)
}

// pickReplyTo is the pure matching logic for inferReplyTo: given a list of
// candidate messages, returns the single matching message's ID, or "" if there
// is no match or more than one match. Pure to keep it unit-testable.
func pickReplyTo(messages []*mail.Message, to, subject string) string {
	wantSubject := normalizeReplySubject(subject)
	if wantSubject == "" {
		return ""
	}
	wantFrom := normalizeAddress(to)

	var matchID string
	matches := 0
	for _, m := range messages {
		if normalizeAddress(m.From) != wantFrom {
			continue
		}
		if normalizeReplySubject(m.Subject) != wantSubject {
			continue
		}
		matches++
		matchID = m.ID
		if matches > 1 {
			return ""
		}
	}
	if matches != 1 {
		return ""
	}
	return matchID
}

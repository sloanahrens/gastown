package mail

import (
	"errors"
	"fmt"
	"strings"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/events"
)

// SendRequest is one message to send: what `gt mail send` has once the CLI has
// resolved the sender and the recipient. SendMessage decides the routing.
type SendRequest struct {
	// From is the sender address.
	From string
	// To is the recipient address: SendMessage resolves it and drives one
	// delivery per recipient.
	To string
	// Subject and Body are the message's text.
	Subject string
	Body    string

	// Priority is the message priority.
	Priority Priority
	// Type is the message type (task, notification, reply, ...).
	Type MessageType
	// CC lists addresses that get a copy without being the recipient.
	CC []string
	// Pinned keeps the message from auto-archiving and Wisp makes it
	// transient (not synced to git).
	Pinned bool
	Wisp   bool
	// SuppressNotify suppresses the router's own notification to each
	// recipient, which is `gt mail send --no-notify`.
	SuppressNotify bool
	// ReplyTo is the ID of the message this one replies to. The reply
	// inherits that message's thread and satisfies the reply reminders it
	// was about.
	ReplyTo string
}

// Receipt is what a send delivered.
type Receipt struct {
	// Message is the message as it was sent, stamped with its ID and thread.
	Message *Message
	// Recipients are the addresses a copy reached.
	Recipients []string
	// Failed names the deliveries that failed, as "<address>: <error>". A
	// send whose failures are all of them is an error instead, and the
	// receipt is empty.
	Failed []string
	// Warnings are things the delivery could not do after the message was
	// sent, such as clearing the reply reminders a reply satisfied. They do
	// not make the send fail; the caller reports them.
	Warnings []string
}

// SendMessage sends req the way `gt mail send` does: it resolves req.To,
// delivers one copy per resolved recipient, and records the send in the event
// feed. The router's pending notifications are done when it returns.
func (r *Router) SendMessage(req SendRequest) (Receipt, error) {
	defer r.WaitPendingNotifications()

	msg := NewMessage(req.From, req.To, req.Subject, req.Body)
	msg.Priority = req.Priority
	msg.Type = req.Type
	msg.CC = req.CC
	msg.Pinned = req.Pinned
	msg.Wisp = req.Wisp
	msg.SuppressNotify = req.SuppressNotify

	var warnings []string
	if req.ReplyTo != "" {
		msg.ReplyTo = req.ReplyTo
		if msg.Type == TypeNotification {
			msg.Type = TypeReply
		}
		// The message being replied to is in the sender's own inbox: read
		// its thread from there. A lookup that fails costs the thread only,
		// and the reply goes out on a new one.
		if original, err := r.originalMessage(req.From, req.ReplyTo); err == nil {
			msg.ThreadID = original.ThreadID
		} else {
			warnings = append(warnings, fmt.Sprintf(
				"could not find %s for threading (new thread will be created): %v", req.ReplyTo, err))
		}
	}

	recipients, err := r.resolvedRecipients(req.To)
	if err != nil {
		// A rejection is definitive — legacy routing would deliver to a dead
		// inbox (steveyegge/gastown#2038) — but an infrastructure failure
		// (beads down, say) is no verdict on the address, so the message
		// goes the legacy way.
		if errors.Is(err, ErrUnknownRecipient) {
			return Receipt{}, err
		}
		return r.sendLegacy(msg, req, warnings)
	}

	addrs, failed, err := r.fanOut(msg, recipients)
	if err != nil {
		return Receipt{}, err
	}

	receipt := Receipt{Message: msg, Recipients: addrs, Failed: failed, Warnings: warnings}
	if req.ReplyTo != "" {
		if err := r.ClearReplyReminders(req.From, msg.ThreadID); err != nil {
			receipt.Warnings = append(receipt.Warnings,
				fmt.Sprintf("could not clear satisfied reply reminders: %v", err))
		}
	}
	r.logSent(req)
	return receipt, nil
}

// sendLegacy delivers msg without the address resolver, for a resolver that
// failed for a reason other than an unknown recipient.
func (r *Router) sendLegacy(msg *Message, req SendRequest, warnings []string) (Receipt, error) {
	if err := r.Send(msg); err != nil {
		return Receipt{}, fmt.Errorf("sending message: %w", err)
	}
	r.logSent(req)
	return Receipt{Message: msg, Recipients: []string{req.To}, Warnings: warnings}, nil
}

// fanOut delivers one copy of msg per recipient, returning the addresses a
// copy reached and the failures, as "<address>: <error>".
func (r *Router) fanOut(msg *Message, recipients []Recipient) (addrs, failed []string, err error) {
	// A queue or a channel is one shared message; only a direct recipient
	// needs a copy of their own, with an ID of its own.
	for _, rec := range recipients {
		switch rec.Type {
		case RecipientQueue:
			msg.To = rec.Address
			if err := r.Send(msg); err != nil {
				failed = append(failed, fmt.Sprintf("queue %s: %v", rec.Address, err))
				continue
			}
			addrs = append(addrs, rec.Address)

		case RecipientChannel:
			msg.To = rec.Address
			if err := r.Send(msg); err != nil {
				failed = append(failed, fmt.Sprintf("channel %s: %v", rec.Address, err))
				continue
			}
			addrs = append(addrs, rec.Address)

		default:
			copy := *msg
			copy.To = rec.Address
			copy.ID = ""
			if err := r.Send(&copy); err != nil {
				failed = append(failed, fmt.Sprintf("%s: %v", rec.Address, err))
				continue
			}
			addrs = append(addrs, rec.Address)
		}
	}
	if len(failed) > 0 && len(addrs) == 0 {
		return nil, nil, fmt.Errorf("all sends failed: %s", strings.Join(failed, "; "))
	}
	return addrs, failed, nil
}

// resolvedRecipients resolves address through the address resolver over the
// town's beads.
func (r *Router) resolvedRecipients(address string) ([]Recipient, error) {
	if r.resolve != nil {
		return r.resolve(address)
	}
	return NewResolver(beads.New(r.townRoot), r.townRoot).Resolve(address)
}

// originalMessage reads the message a reply names from the sender's own
// inbox, which is where the message being replied to lives.
func (r *Router) originalMessage(address, id string) (*Message, error) {
	mailbox, err := r.GetMailbox(address)
	if err != nil {
		return nil, err
	}
	return mailbox.Get(id)
}

// logSent records a sent message in the event feed.
func (r *Router) logSent(req SendRequest) {
	_ = events.LogFeedTo(r.townRoot, events.TypeMail, req.From, events.MailPayload(req.To, req.Subject))
}

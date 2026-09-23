package cmd

import (
	"fmt"
	"os"
	"strings"

	"github.com/steveyegge/gastown/internal/channelevents"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/tmux"
)

// emitChannelEventAndWake writes a channel event and then delivers the session
// half of the wake for channels whose consumer is an agent session
// (channelevents.SessionConsumer). It returns the event path even when
// delivery fails, so the caller can report where the durable record landed.
//
// Such a channel needs both halves. The file is the record a later await-event
// could replay; the nudge is the only thing that reaches a session parked at
// its prompt, which is where an idle witness sits (gt-wpf0).
func emitChannelEventAndWake(townRoot, channel, rigName, eventType string, payload []string) (string, error) {
	path, err := channelevents.EmitToTown(townRoot, channel, rigName, eventType, payload)
	if err != nil {
		return "", err
	}
	if err := wakeChannelSession(townRoot, channel, rigName, channelWakeMessage(eventType, payload)); err != nil {
		return path, err
	}
	return path, nil
}

// channelWakeMessage renders an event as the line the consuming session
// receives. Payload pairs are kept verbatim: a POLECAT_DONE wake has to arrive
// as the "POLECAT_DONE <polecat> exit=<status>" text the witness acts on.
func channelWakeMessage(eventType string, payload []string) string {
	if len(payload) == 0 {
		return eventType
	}
	return eventType + " " + strings.Join(payload, " ")
}

// wakeChannelSession delivers a wake to the agent session that consumes the
// channel, or returns an error when the wake did not land. A channel with no
// session consumer is a no-op: its await-event subscriber polls the event
// directory, so the file emitted alongside is already the delivery.
//
// A missing session, or a nudge whose submission tmux could not verify, is an
// error rather than a warning — the caller asked for a wake, and reporting
// success for a wake that did not reach the pane is how an unwoken witness
// looks healthy (gt-eigw).
func wakeChannelSession(townRoot, channel, rigName, message string) error {
	role := channelevents.SessionConsumer(channel)
	if role == "" {
		return nil
	}

	sessionName, err := channelConsumerSessionName(role, rigName)
	if err != nil {
		return err
	}

	// Test hook: log the wake instead of delivering it, mirroring the nudge
	// paths in sling_helpers.go. Tests assert on this log rather than on a
	// live tmux server.
	if logPath := os.Getenv("GT_TEST_NUDGE_LOG"); logPath != "" {
		entry := fmt.Sprintf("nudge:%s:%s\n", sessionName, message)
		f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			return fmt.Errorf("logging wake for %s: %w", sessionName, err)
		}
		_, _ = f.WriteString(entry)
		_ = f.Close()
		return nil
	}

	t := tmux.NewTmux()
	exists, err := t.HasSession(sessionName)
	if err != nil {
		return fmt.Errorf("checking %s session %q: %w", role, sessionName, err)
	}
	if !exists {
		return fmt.Errorf("%s session %q is not running, so nothing consumed the wake", role, sessionName)
	}
	if err := t.NudgeSessionWithOpts(sessionName, message, tmux.NudgeOpts{TownRoot: townRoot}); err != nil {
		return fmt.Errorf("waking %s session %q: %w", role, sessionName, err)
	}
	return nil
}

// channelConsumerSessionName resolves the tmux session that consumes a
// channel's events for one rig.
func channelConsumerSessionName(role, rigName string) (string, error) {
	if rigName == "" {
		return "", fmt.Errorf("channel consumer role %q needs a rig to name its session", role)
	}
	prefix := session.PrefixFor(rigName)
	switch role {
	case constants.RoleWitness:
		return session.WitnessSessionName(prefix), nil
	case constants.RoleRefinery:
		return session.RefinerySessionName(prefix), nil
	default:
		return "", fmt.Errorf("no session name is known for channel consumer role %q", role)
	}
}

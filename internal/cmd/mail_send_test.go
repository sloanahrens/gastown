package cmd

import (
	"testing"

	"github.com/steveyegge/gastown/internal/mail"
)

// TestMailSendPriority pins the flag precedence: --urgent wins outright, and
// --notify raises only a normal message (the notification is automatic either
// way; --no-notify is what suppresses it).
func TestMailSendPriority(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		urgent   bool
		priority int
		notify   bool
		want     mail.Priority
	}{
		{"default is normal", false, 2, false, mail.PriorityNormal},
		{"--priority 0 is urgent", false, 0, false, mail.PriorityUrgent},
		{"--priority 3 is low", false, 3, false, mail.PriorityLow},
		{"--notify raises normal to high", false, 2, true, mail.PriorityHigh},
		{"--notify leaves a set priority alone", false, 3, true, mail.PriorityLow},
		{"--urgent beats a set priority", true, 4, false, mail.PriorityUrgent},
		{"--urgent beats --notify", true, 2, true, mail.PriorityUrgent},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := mailSendPriority(tc.urgent, tc.priority, tc.notify); got != tc.want {
				t.Errorf("mailSendPriority(%v, %d, %v) = %v, want %v",
					tc.urgent, tc.priority, tc.notify, got, tc.want)
			}
		})
	}
}

func TestHasReplyPrefix(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want bool
	}{
		{"Re: Status", true},
		{"re: status", true},
		{"RE:status", true},
		{"  Re: leading ws", true},
		{"Reply: not a Re prefix", false},
		{"Status", false},
		{"", false},
		{"Re", false},
		{":empty", false},
	}
	for _, c := range cases {
		if got := hasReplyPrefix(c.in); got != c.want {
			t.Errorf("hasReplyPrefix(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestNormalizeReplySubject(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in, want string
	}{
		{"Re: Hello", "hello"},
		{"re:hello", "hello"},
		{"Re: Re: Re: nested", "nested"},
		{"  Re:  spaced  ", "spaced"},
		{"plain subject", "plain subject"},
		{"", ""},
		{"Re: ", ""},
	}
	for _, c := range cases {
		if got := normalizeReplySubject(c.in); got != c.want {
			t.Errorf("normalizeReplySubject(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestNormalizeAddress(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in, want string
	}{
		{"mayor/", "mayor"},
		{"Mayor", "mayor"},
		{"  gastown/Toast/  ", "gastown/toast"},
		{"", ""},
	}
	for _, c := range cases {
		if got := normalizeAddress(c.in); got != c.want {
			t.Errorf("normalizeAddress(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestPickReplyTo(t *testing.T) {
	t.Parallel()
	msg := func(id, from, subj string) *mail.Message {
		return &mail.Message{ID: id, From: from, Subject: subj}
	}

	t.Run("exact match returns id", func(t *testing.T) {
		msgs := []*mail.Message{
			msg("bd-1", "deacon/", "Build broken"),
			msg("bd-2", "witness/", "Build broken"),
		}
		if got := pickReplyTo(msgs, "deacon/", "Re: Build broken"); got != "bd-1" {
			t.Errorf("got %q, want bd-1", got)
		}
	})

	t.Run("address normalization", func(t *testing.T) {
		msgs := []*mail.Message{msg("bd-3", "Mayor", "[HIGH] alert")}
		if got := pickReplyTo(msgs, "mayor/", "Re: [HIGH] alert"); got != "bd-3" {
			t.Errorf("got %q, want bd-3", got)
		}
	})

	t.Run("nested Re prefixes normalize", func(t *testing.T) {
		msgs := []*mail.Message{msg("bd-4", "deacon/", "Re: original")}
		if got := pickReplyTo(msgs, "deacon/", "Re: Re: original"); got != "bd-4" {
			t.Errorf("got %q, want bd-4", got)
		}
	})

	t.Run("ambiguous match returns empty", func(t *testing.T) {
		msgs := []*mail.Message{
			msg("bd-5", "deacon/", "stuck"),
			msg("bd-6", "deacon/", "stuck"),
		}
		if got := pickReplyTo(msgs, "deacon/", "Re: stuck"); got != "" {
			t.Errorf("got %q, want empty (ambiguous)", got)
		}
	})

	t.Run("wrong sender returns empty", func(t *testing.T) {
		msgs := []*mail.Message{msg("bd-7", "witness/", "important")}
		if got := pickReplyTo(msgs, "deacon/", "Re: important"); got != "" {
			t.Errorf("got %q, want empty (wrong sender)", got)
		}
	})

	t.Run("wrong subject returns empty", func(t *testing.T) {
		msgs := []*mail.Message{msg("bd-8", "deacon/", "alpha")}
		if got := pickReplyTo(msgs, "deacon/", "Re: beta"); got != "" {
			t.Errorf("got %q, want empty (wrong subject)", got)
		}
	})

	t.Run("empty subject after strip returns empty", func(t *testing.T) {
		msgs := []*mail.Message{msg("bd-9", "deacon/", "")}
		if got := pickReplyTo(msgs, "deacon/", "Re: "); got != "" {
			t.Errorf("got %q, want empty (degenerate)", got)
		}
	})

	t.Run("empty message list returns empty", func(t *testing.T) {
		if got := pickReplyTo(nil, "deacon/", "Re: anything"); got != "" {
			t.Errorf("got %q, want empty", got)
		}
	})
}

package specdispatch

import (
	"errors"
	"math/rand"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/config"
)

func TestClassifyAgent(t *testing.T) {
	agents := map[string]*config.RuntimeConfig{
		"claude-sonnet":  {Provider: "claude", Command: "claude"},
		"deepseek-flash": {Provider: "claude", Command: "claude", Env: map[string]string{"ANTHROPIC_BASE_URL": "https://api.deepseek.com/anthropic"}},
		"local-coder":    {Provider: "claude", Command: "claude", Env: map[string]string{"ANTHROPIC_BASE_URL": "http://127.0.0.1:8080"}},
		"codex":          {Provider: "codex", Command: "codex"},
		"bare-claude":    {Command: "claude"},
	}
	cases := map[string]Class{
		"claude-sonnet":  ClassHooked,
		"deepseek-flash": ClassHookless,
		"local-coder":    ClassHookless,
		"codex":          ClassHookless,
		"bare-claude":    ClassHooked,
		"claude-opus":    ClassHooked, // built-in preset, not in the table
		"claude":         ClassHooked,
		"gemini":         ClassHookless,
		"":               ClassHookless,
	}
	for name, want := range cases {
		if got := ClassifyAgent(name, agents); got != want {
			t.Errorf("ClassifyAgent(%q) = %s, want %s", name, got, want)
		}
	}
}

func TestHostSafetyTerm(t *testing.T) {
	cases := []struct {
		edit func(*Spec)
		want string
	}{
		{func(s *Spec) {}, ""},
		{func(s *Spec) { s.Title = "Fix gt uninstall" }, "install"},
		{func(s *Spec) { s.Description += "\nrun make install" }, "install"},
		{func(s *Spec) { s.Notes = "writes to ~/.local/bin/gt" }, "~/.local/bin"},
		{func(s *Spec) { s.Description += "\nhonor INSTALL_DIR" }, "install"},
		{func(s *Spec) { s.Design = "then gt dolt cleanup" }, "dolt cleanup"},
		{func(s *Spec) { s.Acceptance = "- [ ] rm -rf the cache" }, "rm -rf"},
	}
	for i, tc := range cases {
		s := goodSpec()
		tc.edit(&s)
		if got := HostSafetyTerm(s); got != tc.want {
			t.Errorf("case %d: HostSafetyTerm = %q, want %q", i, got, tc.want)
		}
	}
}

func TestEligible(t *testing.T) {
	cases := []struct {
		edit func(*Spec)
		ok   bool
		why  string
	}{
		{func(s *Spec) {}, true, ""},
		{func(s *Spec) { s.Status = "in_progress" }, false, "status in_progress"},
		{func(s *Spec) { s.Status = "deferred" }, false, "deferred"},
		{func(s *Spec) { s.Assignee = "gastown/polecats/ruby" }, false, "assigned"},
		{func(s *Spec) { s.Labels = nil }, false, "no spec label"},
		{func(s *Spec) { s.Type = "task" }, false, "type task"},
		{func(s *Spec) { s.Labels = append(s.Labels, "gt:ready-to-land") }, false, "label gt:ready-to-land"},
		{func(s *Spec) { s.Labels = append(s.Labels, "needs-human") }, false, "label needs-human"},
		{func(s *Spec) { s.Labels = append(s.Labels, "Needs-Mayor-Review") }, false, "label needs-mayor-review"},
	}
	for i, tc := range cases {
		s := goodSpec()
		tc.edit(&s)
		ok, why := Eligible(s)
		if ok != tc.ok || !strings.Contains(why, tc.why) {
			t.Errorf("case %d: Eligible = %v %q, want %v %q", i, ok, why, tc.ok, tc.why)
		}
	}
}

func TestOrderIsPriorityThenCreatedThenID(t *testing.T) {
	specs := []Spec{
		{ID: "gt-c", Priority: 2, CreatedAt: "2026-09-29T10:00:00Z"},
		{ID: "gt-b", Priority: 1, CreatedAt: "2026-09-29T12:00:00Z"},
		{ID: "gt-a", Priority: 2, CreatedAt: "2026-09-29T10:00:00Z"},
		{ID: "gt-d", Priority: 1, CreatedAt: "2026-09-29T11:00:00Z"},
		{ID: "gt-e", Priority: 2, CreatedAt: "garbage"},
		{ID: "gt-f", Priority: 2, CreatedAt: "2026-09-28T10:00:00Z"},
	}
	Order(specs)
	var got []string
	for _, s := range specs {
		got = append(got, s.ID)
	}
	if want := "gt-d gt-b gt-f gt-a gt-c gt-e"; strings.Join(got, " ") != want {
		t.Fatalf("order = %v, want %s", got, want)
	}
}

func baseBudget() Budget {
	return Budget{
		HookedAgent: "claude-sonnet", HookedCap: 2,
		HooklessAgent: "deepseek-flash", HooklessCap: 2,
		Now: time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC),
	}
}

func TestChooseSeatClassesAndCaps(t *testing.T) {
	cases := []struct {
		name   string
		edit   func(*Budget)
		spec   func(*Spec)
		agent  string
		skip   bool
		reason string
	}{
		{"hookless first by default", func(b *Budget) {}, nil, "deepseek-flash", false, "hookless seat 1/2"},
		{"prefer hooked", func(b *Budget) { b.PreferHooked = true }, nil, "claude-sonnet", false, "hooked seat 1/2"},
		{"hookless full falls to hooked", func(b *Budget) { b.HooklessLive = 2 }, nil, "claude-sonnet", false, "hooked seat 1/2"},
		{"hooked full falls to hookless", func(b *Budget) { b.PreferHooked = true; b.HookedLive = 2 }, nil, "deepseek-flash", false, "hookless seat 1/2"},
		{"everything full", func(b *Budget) { b.HooklessLive = 2; b.HookedLive = 2 }, nil, "", true, "seats full: hooked 2/2, hookless 2/2"},
		{"over cap counts as full", func(b *Budget) { b.HooklessLive = 4; b.HookedLive = 3 }, nil, "", true, "seats full"},
		{"hookless cap zero", func(b *Budget) { b.HooklessCap = 0; b.HookedLive = 2 }, nil, "", true, "seats full"},
		{"no hookless agent", func(b *Budget) { b.HooklessAgent = "" }, nil, "claude-sonnet", false, "hooked"},
		{"min spawn gap", func(b *Budget) { b.MinSpawnGap = 4 * time.Minute; b.NewestSpawn = b.Now.Add(-time.Minute) }, nil, "", true, "min_spawn_gap"},
		{"gap elapsed", func(b *Budget) { b.MinSpawnGap = 4 * time.Minute; b.NewestSpawn = b.Now.Add(-5 * time.Minute) }, nil, "deepseek-flash", false, ""},
		{"host safety goes hooked", func(b *Budget) {}, func(s *Spec) { s.Title = "uninstall gt" }, "claude-sonnet", false, "host-safety"},
		{"host safety never hookless", func(b *Budget) { b.HookedLive = 2 }, func(s *Spec) { s.Title = "uninstall gt" }, "", true, "needs a hooked seat: 2/2"},
		{"host safety hooked cap zero", func(b *Budget) { b.HookedCap = 0 }, func(s *Spec) { s.Notes = "rm -rf ~/.local/bin" }, "", true, "needs a hooked seat"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := baseBudget()
			tc.edit(&b)
			s := goodSpec()
			if tc.spec != nil {
				tc.spec(&s)
			}
			got := ChooseSeat(s, b)
			if got.Skip != tc.skip || got.Agent != tc.agent || !strings.Contains(got.Reason, tc.reason) {
				t.Fatalf("ChooseSeat = %+v, want agent %q skip %v reason ~%q", got, tc.agent, tc.skip, tc.reason)
			}
			if !got.Skip && got.Class == ClassHookless && HostSafetyTerm(s) != "" {
				t.Fatal("host-safety spec placed on a hookless seat")
			}
		})
	}
}

func TestRetryOnContention(t *testing.T) {
	contention := errors.New("bd mol bond: Error 1213 (40001): serialization failure: this transaction conflicts")
	var slept []time.Duration
	sleep := func(d time.Duration) { slept = append(slept, d) }
	jitter := rand.New(rand.NewSource(1))

	t.Run("succeeds after contention", func(t *testing.T) {
		slept = nil
		calls := 0
		n, err := RetryOnContention(RetryAttempts, sleep, jitter, func() error {
			calls++
			if calls < 3 {
				return contention
			}
			return nil
		})
		if err != nil || n != 3 || len(slept) != 2 {
			t.Fatalf("n=%d err=%v slept=%v", n, err, slept)
		}
		for _, d := range slept {
			if d < RetryBackoffMin/2 || d > RetryBackoffMax {
				t.Errorf("backoff %v outside bounds", d)
			}
		}
	})
	t.Run("gives up after attempts", func(t *testing.T) {
		slept = nil
		calls := 0
		n, err := RetryOnContention(RetryAttempts, sleep, jitter, func() error { calls++; return contention })
		if !IsSerializationFailure(err) || n != RetryAttempts || calls != RetryAttempts || len(slept) != RetryAttempts-1 {
			t.Fatalf("n=%d calls=%d err=%v slept=%d", n, calls, err, len(slept))
		}
	})
	t.Run("other errors do not retry", func(t *testing.T) {
		slept = nil
		calls := 0
		n, err := RetryOnContention(RetryAttempts, sleep, jitter, func() error { calls++; return errors.New("connection refused") })
		if err == nil || n != 1 || calls != 1 || len(slept) != 0 {
			t.Fatalf("n=%d calls=%d err=%v", n, calls, err)
		}
	})
}

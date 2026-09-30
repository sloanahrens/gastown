package specdispatch

import (
	"errors"
	"math/rand"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/config"
)

func TestClassifyAgentKeysOnProvider(t *testing.T) {
	t.Parallel()
	agents := map[string]*config.RuntimeConfig{
		"claude-sonnet":   {Provider: "claude", Command: "claude"},
		"deepseek-flash":  {Provider: "claude", Command: "claude", Env: map[string]string{"ANTHROPIC_BASE_URL": "https://api.deepseek.com/anthropic"}},
		"local-coder":     {Provider: "openai", Command: "claude"},
		"deepseek-dog":    {Provider: "deepseek", Command: "claude"},
		"codex":           {Provider: "codex", Command: "codex"},
		"bare-claude":     {Command: "claude"},
		"bare-other":      {Command: "aider"},
		"claude-imposter": {Provider: "gemini", Command: "gemini"},
	}
	cases := map[string]Class{
		"claude-sonnet":   ClassHooked,
		"deepseek-flash":  ClassHooked, // provider claude: same hooks, whatever the backend
		"local-coder":     ClassHookless,
		"deepseek-dog":    ClassHookless,
		"codex":           ClassHookless,
		"bare-claude":     ClassHooked,
		"bare-other":      ClassHookless,
		"claude-imposter": ClassHookless, // the name never decides
		"claude":          ClassHookless, // not in the table: provider unknown, fail closed
		"claude-opus":     ClassHookless, // not in the table: fail closed
		"gemini":          ClassHookless,
		"":                ClassHookless,
	}
	for name, want := range cases {
		if got := ClassifyAgent(name, agents); got != want {
			t.Errorf("ClassifyAgent(%q) = %s, want %s", name, got, want)
		}
	}
}

func TestHostSafetyTerm(t *testing.T) {
	t.Parallel()
	cases := []struct {
		text string
		want string
	}{
		{"", ""},
		{"Write the installer docs and an installation guide", ""},
		{"Fix gt uninstall", "uninstall"},
		{"then run `make install` locally", "make install"},
		{"writes to ~/.local/bin/gt", ".local/bin"},
		{"honor INSTALL_DIR when set", "install_dir"},
		{"then gt dolt cleanup", "dolt cleanup"},
		{"rm -rf the cache", "rm -r"},
		{"rm -fr build/", "rm -r"},
		{"rm -Rf build/", "rm -r"},
		{"rm --recursive x", "rm -r"},
		{"rm -f one-file", ""},
		{"shred the key", "shred"},
		{"use dd to copy", "dd"},
		{"chmod -R 755 .", "chmod -R"},
		{"chown -R me .", "chown -R"},
		{"echo x > /etc/hosts", "> host path"},
		{"echo x >/etc/hosts", "> host path"},
		{"echo alias >> ~/.zshrc", ">> host path"},
		{"echo alias >>~/.zshrc", ">> host path"},
		{"append to .bashrc", ".bashrc"},
		{"Terms like reinstalling matter", "reinstalling"},
		{"a directory named firm-rf", ""},
	}
	for _, tc := range cases {
		s := goodSpec()
		s.Description += "\n" + tc.text
		if got := HostSafetyTerm(s); got != tc.want {
			t.Errorf("HostSafetyTerm(%q) = %q, want %q", tc.text, got, tc.want)
		}
	}
	// Every text field is read.
	for _, edit := range []func(*Spec){
		func(s *Spec) { s.Title = "uninstall" },
		func(s *Spec) { s.Notes = "uninstall" },
		func(s *Spec) { s.Design = "uninstall" },
		func(s *Spec) { s.Acceptance = "- [ ] uninstall" },
	} {
		s := goodSpec()
		edit(&s)
		if HostSafetyTerm(s) == "" {
			t.Errorf("field not scanned: %+v", s)
		}
	}
}

func TestEligible(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
		Seats: []Seat{
			{Agent: "deepseek-flash", Class: ClassHooked, Cap: 2},
			{Agent: "local-coder", Class: ClassHookless, Cap: 2},
			{Agent: "claude-sonnet", Class: ClassHooked, Cap: 2},
		},
		Now: time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC),
	}
}

func hostSafe(s *Spec) { s.Labels = append(s.Labels, HostSafeLabel) }

func TestChooseSeatFailsClosed(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		live     map[string]int
		edit     func(*Budget)
		spec     func(*Spec)
		agent    string
		skip     bool
		overrode bool
		reason   string
	}{
		{name: "default spec takes the first hooked seat", agent: "deepseek-flash", reason: "no host-safe label"},
		{name: "default spec skips a free hookless seat", live: map[string]int{"deepseek-flash": 2}, agent: "claude-sonnet"},
		{name: "default spec waits when hooked seats are full", live: map[string]int{"deepseek-flash": 2, "claude-sonnet": 2}, skip: true, reason: "hooked seats only"},
		{name: "installer docs still dispatches hooked", spec: func(s *Spec) { s.Title = "Write the installer docs" }, agent: "deepseek-flash"},
		{name: "host-safe spec may use the hookless seat", spec: hostSafe, live: map[string]int{"deepseek-flash": 2}, agent: "local-coder", reason: "host-safe"},
		{name: "host-safe spec in order takes first free", spec: hostSafe, agent: "deepseek-flash"},
		{name: "host-safe with rm -fr is forced hooked", live: map[string]int{"deepseek-flash": 2},
			spec: func(s *Spec) { hostSafe(s); s.Description += "\nrm -fr build" }, agent: "claude-sonnet", overrode: true, reason: "label overridden"},
		{name: "host-safe with rm -fr never goes hookless", live: map[string]int{"deepseek-flash": 2, "claude-sonnet": 2},
			spec: func(s *Spec) { hostSafe(s); s.Description += "\nrm -fr build" }, skip: true, overrode: true},
		{name: "over cap counts as full", live: map[string]int{"deepseek-flash": 5, "local-coder": 3, "claude-sonnet": 9}, spec: hostSafe, skip: true, reason: "seats full"},
		{name: "cap zero is closed", edit: func(b *Budget) { b.Seats[0].Cap = 0 }, agent: "claude-sonnet"},
		{name: "min spawn gap", edit: func(b *Budget) { b.MinSpawnGap = 4 * time.Minute; b.NewestSpawn = b.Now.Add(-time.Minute) }, skip: true, reason: "min_spawn_gap"},
		{name: "gap elapsed", edit: func(b *Budget) { b.MinSpawnGap = 4 * time.Minute; b.NewestSpawn = b.Now.Add(-5 * time.Minute) }, agent: "deepseek-flash"},
		{name: "no seats", edit: func(b *Budget) { b.Seats = nil }, skip: true, reason: "no seats"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := baseBudget()
			b.SetLive(tc.live)
			if tc.edit != nil {
				tc.edit(&b)
			}
			s := goodSpec()
			if tc.spec != nil {
				tc.spec(&s)
			}
			got := ChooseSeat(s, b)
			if got.Skip != tc.skip || got.Agent != tc.agent || got.OverrodeHostSafe != tc.overrode || !strings.Contains(got.Reason, tc.reason) {
				t.Fatalf("ChooseSeat = %+v, want agent %q skip %v overrode %v reason ~%q", got, tc.agent, tc.skip, tc.overrode, tc.reason)
			}
			if !got.Skip && got.Class == ClassHookless && (!s.HasLabel(HostSafeLabel) || HostSafetyTerm(s) != "") {
				t.Fatal("a spec without a clean host-safe label reached a hookless seat")
			}
		})
	}
}

func TestBudgetBumpAndPicture(t *testing.T) {
	t.Parallel()
	b := baseBudget()
	b.SetLive(map[string]int{"claude-sonnet": 1})
	b.Bump("claude-sonnet", b.Now)
	if b.Seats[2].Live != 2 || !b.NewestSpawn.Equal(b.Now) {
		t.Fatalf("bump: %+v", b)
	}
	if got := b.Picture(); got != "deepseek-flash 0/2 hooked, local-coder 0/2 hookless, claude-sonnet 2/2 hooked" {
		t.Errorf("picture = %q", got)
	}
}

func TestRetryOnContention(t *testing.T) {
	t.Parallel()
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

package tmux

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/config"
)

// TestAdaptiveTextDelay verifies the delay scaling logic for post-text delivery.
func TestAdaptiveTextDelay(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		msgLen  int
		wantMin time.Duration
		wantMax time.Duration
	}{
		{"empty", 0, 500 * time.Millisecond, 500 * time.Millisecond},
		{"small single chunk", 100, 500 * time.Millisecond, 500 * time.Millisecond},
		{"exactly one chunk", 512, 500 * time.Millisecond, 500 * time.Millisecond},
		{"two chunks", 513, 525 * time.Millisecond, 525 * time.Millisecond},
		{"five chunks", 2048 + 1, 600 * time.Millisecond, 600 * time.Millisecond},
		{"huge message capped", 100000, 2 * time.Second, 2 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := adaptiveTextDelay(tt.msgLen)
			if got < tt.wantMin || got > tt.wantMax {
				t.Errorf("adaptiveTextDelay(%d) = %v, want [%v, %v]", tt.msgLen, got, tt.wantMin, tt.wantMax)
			}
		})
	}
}

// TestMatchesPromptPrefix verifies that prompt matching handles non-breaking
// spaces (NBSP, U+00A0) correctly. Claude Code uses NBSP after its > prompt
// character, but the default ReadyPromptPrefix uses a regular space.
// Regression test for https://github.com/steveyegge/gastown/issues/1387.
func TestMatchesPromptPrefix(t *testing.T) {
	t.Parallel()
	const (
		nbsp          = "\u00a0" // non-breaking space
		regularPrefix = "❯ "     // default: ❯ + regular space
	)

	tests := []struct {
		name   string
		line   string
		prefix string
		want   bool
	}{
		// Regular space in both line and prefix (baseline)
		{"regular space matches", "❯ ", regularPrefix, true},
		{"regular space with trailing content", "❯ some input", regularPrefix, true},

		// NBSP in line, regular space in prefix (the bug scenario)
		{"NBSP bare prompt matches", "❯" + nbsp, regularPrefix, true},
		{"NBSP with content matches", "❯" + nbsp + "claude --help", regularPrefix, true},
		{"NBSP with leading whitespace", "  ❯" + nbsp, regularPrefix, true},

		// NBSP in prefix (defensive: user could configure it either way)
		{"NBSP prefix matches NBSP line", "❯" + nbsp + "hello", "❯" + nbsp, true},
		{"NBSP prefix matches regular space line", "❯ hello", "❯" + nbsp, true},

		// Empty prefix never matches
		{"empty prefix", "❯ ", "", false},

		// No prompt character at all
		{"no prompt", "hello world", regularPrefix, false},
		{"empty line", "", regularPrefix, false},
		{"whitespace only", "   ", regularPrefix, false},

		// Bare prompt character without any space
		{"bare prompt no space", "❯", regularPrefix, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := matchesPromptPrefix(tt.line, tt.prefix)
			if got != tt.want {
				t.Errorf("matchesPromptPrefix(%q, %q) = %v, want %v",
					tt.line, tt.prefix, got, tt.want)
			}
		})
	}
}

func TestHasBusyIndicator(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		line string
		want bool
	}{
		{"claude status busy (legacy marker)", "⏵⏵ bypass permissions on ... · esc to interrupt", true},
		{"codex status busy", "• Working (2m 18s • esc to interrupt)", true},
		// Current Claude Code TUI (verified live 2026-09-09, gastownhall/gastown#4240):
		// the spinner verb rotates ("Leavening", "Gesticulating", "Skedaddling", ...) so
		// these fixtures use several distinct verbs captured from real running panes to
		// prove detection does not depend on any one of them.
		{"claude spinner busy - Leavening", "✽ Leavening… (3m 17s · ↓ 14.1k tokens)", true},
		{"claude spinner busy - Gesticulating", "✱ Gesticulating... (4m 26s · ↓ 1.6k tokens)", true},
		{"claude spinner busy - no k suffix", "· Processing… (2s · ↓ 129 tokens)", true},
		{"claude subagent busy", "◯ Explore  Grepping runEscalate and BdCli definitions   48s · ↓ 38.0k tokens", true},
		{"claude tool-running hint busy", "  (ctrl+b ctrl+b (twice) to run in background)", true},
		{"claude spinner busy - indented", "   ✽ Leavening… (3m 17s · ↓ 14.1k tokens)", true},
		{"claude idle done line", "✻ Cooked for 4m 23s · done 10:29 PM", false},
		{"idle line", "› Review ready notification", false},
		{"idle footer with context pct, no arrow", "  Sonnet 5 | Context: 21%", false},
		// gt-dq6pi: a relayed nudge quoting another pane's live spinner carries
		// the same token-count substring but is prose, not the status line
		// itself — a session read this as BUSY on every idle poll because
		// hasBusyIndicator matched the substring anywhere in the line.
		{"transcript quoting another pane's spinner", `live turn "Sautéing… 8m16s · ↓14.6k tokens"`, false},
		{"prose mentioning tokens without leading glyph", "the deacon nudge showed ↓14.6k tokens in the transcript", false},
		{"blank", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := hasBusyIndicator(tt.line); got != tt.want {
				t.Errorf("hasBusyIndicator(%q) = %v, want %v", tt.line, got, tt.want)
			}
		})
	}
}

// TestShouldSendEscapeForLines guards against the regression where a nudge
// sends the vim-mode Escape keystroke while the agent is actively generating,
// interrupting its current turn. When the pane shows the busy
// indicator ("esc to interrupt"), the Escape must be suppressed.
func TestShouldSendEscapeForLines(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		lines []string
		want  bool
	}{
		{
			name:  "claude generating - suppress escape",
			lines: []string{"✻ Cogitating… (12s · ↑ 2.1k tokens · esc to interrupt)"},
			want:  false,
		},
		{
			name:  "codex working - suppress escape",
			lines: []string{"• Working (2m 18s • esc to interrupt)"},
			want:  false,
		},
		{
			name:  "busy indicator among multiple lines - suppress escape",
			lines: []string{"tool output", "more output", "⏵⏵ bypass permissions on · esc to interrupt"},
			want:  false,
		},
		{
			// Reproduces the exact shape of the second, independent defect in
			// gt-8bh: the spinner line sits several lines above the footer, with
			// the busy marker nowhere near the bottom of the captured window.
			// Captured live 2026-09-09 from a real running Claude Code pane
			// (gastownhall/gastown#4240).
			name: "current claude code TUI - spinner above footer - suppress escape",
			lines: []string{
				"⏺ Running 1 shell command…",
				"",
				"✳ Skedaddling… (1m 12s · ↓ 4.2k tokens)",
				"  ⎿  Tip: Use /btw to ask a quick side question without interrupting Claude's",
				"     current work",
				"",
				"────────────────────────────────────────────────────────────────────────────────",
				"❯ ",
				"────────────────────────────────────────────────────────────────────────────────",
				"  Sonnet 5 | Context: 9%                                                   /rc",
				"  /Users/sloan/gt/gastown/polecats/jasper/gastown",
				"  [polecat/jasper/gt-ftt+mttjg5m4]",
				"  ⏵⏵ bypass permissions on (shift+tab to cycle) · ← for agents",
			},
			want: false,
		},
		{
			// Same shape, but busy on a long-running tool call rather than pure
			// generation: the "run in background" hint replaces the token-count
			// spinner as the visible marker. Captured live 2026-09-09.
			name: "current claude code TUI - tool running with background hint - suppress escape",
			lines: []string{
				"⏺ Executing dolt-archive plugin script · 4s",
				"  ⎿  $ cd /Users/sloan/gt/plugins/dolt-archive && bash run.sh (4s · 20 lines)",
				"     (ctrl+b ctrl+b (twice) to run in background)",
				"",
				"✶ Nebulizing… (31s · ↓ 732 tokens)",
				"────────────────────────────────────────────────────────────────────────────────",
				"❯ ",
				"────────────────────────────────────────────────────────────────────────────────",
				"  Haiku 4.5 | Context: 24%                                                 /rc",
				"  ⏵⏵ bypass permissions on (shift+tab to cycle) · ← for agents",
			},
			want: false,
		},
		{
			// The dead-marker regression itself: only footer lines, no busy
			// marker anywhere, no "esc to interrupt" — this is what every busy
			// Claude Code pane looked like once the TUI stopped rendering the
			// old marker. Must NOT suppress a genuinely idle agent.
			name: "current claude code TUI - idle, footer only - allow escape",
			lines: []string{
				"────────────────────────────────────────────────────────────────────────────────",
				"❯ ",
				"────────────────────────────────────────────────────────────────────────────────",
				"  Sonnet 5 | Context: 13%                                                  /rc",
				"  /Users/sloan/gt/gastown/polecats/quartz/gastown",
				"  [polecat/quartz/gt-8bh+mttjdaf5]",
				"  ⏵⏵ bypass permissions on (shift+tab to cycle) · ← for agents",
			},
			want: true,
		},
		{
			name:  "idle ready prompt - allow escape",
			lines: []string{"❯ "},
			want:  true,
		},
		{
			name:  "idle with typed nudge text - allow escape",
			lines: []string{"❯ HEALTH_CHECK: heartbeat stale, respond to confirm"},
			want:  true,
		},
		{
			name:  "no lines captured - allow escape (not busy)",
			lines: nil,
			want:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldSendEscapeForLines(tt.lines); got != tt.want {
				t.Errorf("shouldSendEscapeForLines(%q) = %v, want %v", tt.lines, got, tt.want)
			}
		})
	}
}

// TestEscapeAllowed guards gt-cyyg and claude-9a8: an agent whose harness
// cancels on Escape (Claude Code, Gemini, Copilot), or whose harness cannot
// be identified, never receives the vim-mode Escape, whatever the busy
// scrape says.
func TestEscapeAllowed(t *testing.T) {
	t.Parallel()
	claude := config.GetAgentPresetByName("claude")
	// A registry preset may clear escape_cancels_request; only then does the
	// busy scrape gate the Escape keystroke.
	scrapeGated := &config.AgentPresetInfo{Name: "scrape-gated", Command: "claude"}
	tests := []struct {
		name   string
		agent  string
		preset *config.AgentPresetInfo
		ok     bool
		want   bool
	}{
		{"claude", "claude", claude, true, false},
		{"custom agent resolved to claude", "deepseek-flash", claude, true, false},
		{"preset without escape_cancels_request keeps scrape-gated escape", "scrape-gated", scrapeGated, true, true},
		{"unresolved agent fails safe", "not-a-real-agent", nil, false, false},
		{"no GT_AGENT fails safe", "", nil, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := escapeAllowed(tt.preset, tt.ok); got != tt.want {
				t.Errorf("escapeAllowed(%q) = %v, want %v", tt.agent, got, tt.want)
			}
		})
	}
}

// TestBusyIndicators pins the centralized busy-indicator source of truth so a
// change to the upstream-coupled status string is intentional and reviewed
// rather than accidental (gastownhall/gastown#4240).
func TestBusyIndicators(t *testing.T) {
	t.Parallel()

	if len(busyIndicators) == 0 {
		t.Fatal("busyIndicators must not be empty — busy/idle detection would silently break")
	}

	// "esc to interrupt" is the legacy marker.
	// "to run in background" is the current Claude Code hint. If either
	// assertion fails, the change must be deliberate.
	wantMarkers := []string{"esc to interrupt", "to run in background"}
	for _, want := range wantMarkers {
		found := false
		for _, m := range busyIndicators {
			if m == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("busyIndicators = %q, want it to contain the known %q marker", busyIndicators, want)
		}
	}

	// Every marker must be matched by hasBusyIndicator (guards against an empty
	// or whitespace-only entry slipping in).
	for _, m := range busyIndicators {
		if strings.TrimSpace(m) == "" {
			t.Fatal("busyIndicators must not contain empty or whitespace-only markers")
		}
		if !hasBusyIndicator("⏵⏵ status · " + m) {
			t.Errorf("hasBusyIndicator did not match a registered busy indicator %q", m)
		}
	}

	// The structural token-spinner pattern must match the current Claude Code
	// busy line regardless of which verb word is spinning (verified live
	// 2026-09-09, gastownhall/gastown#4240) and must not match plain idle text.
	spinnerCases := []string{
		"✽ Leavening… (3m 17s · ↓ 14.1k tokens)",
		"✱ Gesticulating... (4m 26s · ↓ 1.6k tokens)",
		"◯ Explore  Grepping runEscalate and BdCli definitions   48s · ↓ 38.0k tokens",
	}
	for _, line := range spinnerCases {
		if !busyTokenSpinnerPattern.MatchString(line) {
			t.Errorf("busyTokenSpinnerPattern did not match live spinner line %q", line)
		}
		if !hasBusyIndicator(line) {
			t.Errorf("hasBusyIndicator did not match live spinner line %q", line)
		}
	}
	idleCases := []string{
		"  Sonnet 5 | Context: 21%",
		"✻ Cooked for 4m 23s · done 10:29 PM",
		"❯ ",
	}
	for _, line := range idleCases {
		if hasBusyIndicator(line) {
			t.Errorf("hasBusyIndicator incorrectly matched idle line %q", line)
		}
	}
}

func TestDefaultReadyPromptPrefix(t *testing.T) {
	t.Parallel()
	// Verify the constant is set correctly
	if DefaultReadyPromptPrefix == "" {
		t.Error("DefaultReadyPromptPrefix should not be empty")
	}
	if !strings.Contains(DefaultReadyPromptPrefix, "❯") {
		t.Errorf("DefaultReadyPromptPrefix = %q, want to contain ❯", DefaultReadyPromptPrefix)
	}
}

func TestNewSessionSet(t *testing.T) {
	t.Parallel()
	// Test creating SessionSet from names
	names := []string{"session-a", "session-b", "session-c"}
	set := NewSessionSet(names)

	if set == nil {
		t.Fatal("NewSessionSet returned nil")
	}

	// Test Has() for existing sessions
	for _, name := range names {
		if !set.Has(name) {
			t.Errorf("SessionSet.Has(%q) = false, want true", name)
		}
	}

	// Test Has() for non-existing session
	if set.Has("nonexistent") {
		t.Error("SessionSet.Has(nonexistent) = true, want false")
	}

	// Test Names() returns all sessions
	gotNames := set.Names()
	if len(gotNames) != len(names) {
		t.Errorf("SessionSet.Names() returned %d names, want %d", len(gotNames), len(names))
	}

	// Verify all names are present (order may differ)
	nameSet := make(map[string]bool)
	for _, n := range gotNames {
		nameSet[n] = true
	}
	for _, n := range names {
		if !nameSet[n] {
			t.Errorf("SessionSet.Names() missing %q", n)
		}
	}
}

func TestNewSessionSet_Empty(t *testing.T) {
	t.Parallel()
	set := NewSessionSet([]string{})

	if set == nil {
		t.Fatal("NewSessionSet returned nil for empty input")
	}

	if set.Has("anything") {
		t.Error("Empty SessionSet.Has() = true, want false")
	}

	names := set.Names()
	if len(names) != 0 {
		t.Errorf("Empty SessionSet.Names() returned %d names, want 0", len(names))
	}
}

func TestNewSessionSet_Nil(t *testing.T) {
	t.Parallel()
	set := NewSessionSet(nil)

	if set == nil {
		t.Fatal("NewSessionSet returned nil for nil input")
	}

	if set.Has("anything") {
		t.Error("Nil-input SessionSet.Has() = true, want false")
	}
}

func TestFindBindingLine(t *testing.T) {
	t.Parallel()
	// Representative list-keys -T prefix output (tmux 3.7c formatting:
	// aligned columns, backslash-escaped special keys, -r repeat flags).
	output := "bind-key    -T prefix Space   next-layout\n" +
		"bind-key    -T prefix \\\"      split-window\n" +
		"bind-key -r -T prefix n       next-window\n" +
		"bind-key    -T prefix s       choose-tree -Zs\n" +
		"bind-key    -T prefix g       if-shell \"true\" \"run-shell 'gt agents menu'\" \":\""

	tests := []struct {
		name  string
		table string
		key   string
		want  string
	}{
		{"plain key", "prefix", "s", "bind-key    -T prefix s       choose-tree -Zs"},
		{"repeat flag", "prefix", "n", "bind-key -r -T prefix n       next-window"},
		{"escaped key", "prefix", `"`, "bind-key    -T prefix \\\"      split-window"},
		{"named key", "prefix", "Space", "bind-key    -T prefix Space   next-layout"},
		{"missing key", "prefix", "F12", ""},
		{"wrong table", "root", "n", ""},
		// "n" appears inside commands (next-layout, next-window) — must not match there.
		{"no substring match", "prefix", "next-window", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := findBindingLine(output, tt.table, tt.key)
			if got != tt.want {
				t.Errorf("findBindingLine(%q, %q) = %q, want %q", tt.table, tt.key, got, tt.want)
			}
		})
	}
}

func TestZombieStatusString(t *testing.T) {
	t.Parallel()
	tests := []struct {
		status   ZombieStatus
		expected string
		zombie   bool
	}{
		{SessionHealthy, "healthy", false},
		{SessionDead, "session-dead", false},
		{AgentDead, "agent-dead", true},
		{AgentHung, "agent-hung", true},
	}

	for _, tc := range tests {
		if got := tc.status.String(); got != tc.expected {
			t.Errorf("ZombieStatus(%d).String() = %q, want %q", tc.status, got, tc.expected)
		}
		if got := tc.status.IsZombie(); got != tc.zombie {
			t.Errorf("ZombieStatus(%d).IsZombie() = %v, want %v", tc.status, got, tc.zombie)
		}
	}
}

func TestValidateCommandBinary(t *testing.T) {
	t.Parallel()

	// Any absolute path that exists will do; the test binary always does.
	absoluteShell, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		cmd     string
		wantErr bool
	}{
		{"empty", "", false},
		{"relative binary", "echo hello", false},
		{"valid absolute", absoluteShell + " -c 'echo hi'", false},
		{"missing absolute", "/nonexistent/binary --flag", true},
		{"exec env missing", "exec env GT_TEST=1 /nonexistent/claude-code --settings /tmp", true},
		{"exec env valid", "exec env GT_TEST=1 " + absoluteShell + " -c 'echo hi'", false},
		{"env vars only", "exec env FOO=bar BAZ=1", false},
		{"bare exec", "exec " + absoluteShell, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateCommandBinary(tc.cmd)
			if (err != nil) != tc.wantErr {
				t.Errorf("validateCommandBinary(%q) error = %v, wantErr = %v", tc.cmd, err, tc.wantErr)
			}
		})
	}
}

// bannerKeys returns the text SendNotificationBanner handed to `send-keys -l`,
// the literal paste that ends up in the overseer's shell.
func bannerKeys(t *testing.T, sc *scripted) string {
	t.Helper()
	for _, c := range sc.find("send-keys") {
		if c.has("-l") {
			return c.last()
		}
	}
	t.Fatal("SendNotificationBanner sent no send-keys -l call")
	return ""
}

// TestSendNotificationBanner_QuotesSubject feeds the banner a subject that
// closes the echo argument and starts a command. The keystrokes must still
// split into exactly one echo command, with the subject intact inside its
// argument: a third word would be a command the shell runs.
func TestSendNotificationBanner_QuotesSubject(t *testing.T) {
	t.Parallel()
	sc := newScripted(nil)
	tm := newTmuxForTest("gt-test-unit", sc.exec, nil)

	subject := "x'; touch /tmp/p; echo '"
	if err := tm.SendNotificationBanner("gt-overseer", "a/b", subject); err != nil {
		t.Fatalf("SendNotificationBanner: %v", err)
	}

	keys := bannerKeys(t, sc)
	words, err := shellWords(keys)
	if err != nil {
		t.Fatalf("keystrokes are not a complete command: %v\nkeys: %q", err, keys)
	}
	if len(words) != 2 || words[0] != "echo" {
		t.Fatalf("keystrokes are not one echo command, they run %q\nkeys: %q", words, keys)
	}
	if !strings.Contains(words[1], subject) {
		t.Errorf("echo argument %q does not carry the subject %q", words[1], subject)
	}
	if !strings.Contains(words[1], "NEW MAIL from a/b") || !strings.Contains(words[1], "Run: gt mail inbox") {
		t.Errorf("echo argument %q lost the banner text", words[1])
	}

	// The unescaped interpolation the banner used before the fix must split
	// into more words, or this test asserts nothing about the escaping.
	raw := "echo '\n📬 NEW MAIL from a/b\nSubject: " + subject + "\nRun: gt mail inbox\n'"
	if rawWords, err := shellWords(raw); err != nil || len(rawWords) == 2 {
		t.Fatalf("unquoted interpolation parses as one command (words %q, err %v): "+
			"the parser no longer detects the injection", rawWords, err)
	}
}

// shellWords resolves a command string into the words a POSIX shell passes to
// the command: unquoted whitespace separates words, a single-quoted run is
// literal, and a backslash outside single quotes escapes the next character.
// An unterminated quote is the state a closing quote in a value leaves the
// shell in, so it is reported rather than swallowed.
func shellWords(s string) ([]string, error) {
	var words []string
	var cur strings.Builder
	started, quoted, escaped := false, false, false
	for _, r := range s {
		switch {
		case escaped:
			cur.WriteRune(r)
			escaped = false
		case quoted:
			if r == '\'' {
				quoted = false
				continue
			}
			cur.WriteRune(r)
		case r == '\\':
			escaped, started = true, true
		case r == '\'':
			quoted, started = true, true
		case r == ' ' || r == '\t' || r == '\n':
			if started {
				words = append(words, cur.String())
				cur.Reset()
				started = false
			}
		default:
			cur.WriteRune(r)
			started = true
		}
	}
	switch {
	case quoted:
		return words, errors.New("unterminated single quote")
	case escaped:
		return words, errors.New("trailing backslash")
	}
	if started {
		words = append(words, cur.String())
	}
	return words, nil
}

// TestSendNotificationBanner_DropsControlCharacters keeps ESC and Ctrl-C out of
// the keystrokes: tmux -l delivers their bytes to the shell as terminal input.
func TestSendNotificationBanner_DropsControlCharacters(t *testing.T) {
	t.Parallel()
	sc := newScripted(nil)
	tm := newTmuxForTest("gt-test-unit", sc.exec, nil)

	if err := tm.SendNotificationBanner("gt-overseer", "a/b", "hi\x1bthere\x03now"); err != nil {
		t.Fatalf("SendNotificationBanner: %v", err)
	}

	arg := bannerKeys(t, sc)
	if i := strings.IndexAny(arg, "\x1b\x03"); i >= 0 {
		t.Errorf("keystrokes carry control byte %q at %d: %q", arg[i], i, arg)
	}
	if !strings.Contains(arg, "hitherenow") {
		t.Errorf("keystrokes lost the subject text: %q", arg)
	}
}

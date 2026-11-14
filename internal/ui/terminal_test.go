package ui

import (
	"testing"

	"github.com/charmbracelet/lipgloss"
)

// fakeEnv is a process with environment kv and a stdout that is (tty) or is
// not a terminal.
func fakeEnv(tty bool, kv map[string]string) uiEnv {
	return uiEnv{
		lookup: func(k string) (string, bool) {
			v, ok := kv[k]
			return v, ok
		},
		stdoutTTY: func() bool { return tty },
	}
}

func TestShouldUseColor_Default(t *testing.T) {
	t.Parallel()
	// With no overrides, color follows whether stdout is a terminal.
	if !fakeEnv(true, nil).shouldUseColor() {
		t.Error("ShouldUseColor() = false on a TTY with no overrides, want true")
	}
	if fakeEnv(false, nil).shouldUseColor() {
		t.Error("ShouldUseColor() = true on a non-TTY with no overrides, want false")
	}
}

func TestShouldUseColor_NO_COLOR(t *testing.T) {
	t.Parallel()
	if fakeEnv(true, map[string]string{"NO_COLOR": "1"}).shouldUseColor() {
		t.Error("ShouldUseColor() should return false when NO_COLOR is set")
	}
}

func TestShouldUseColor_NO_COLOR_AnyValue(t *testing.T) {
	t.Parallel()
	// NO_COLOR with any value (even "0" or empty) should disable color.
	for _, v := range []string{"0", ""} {
		if fakeEnv(true, map[string]string{"NO_COLOR": v}).shouldUseColor() {
			t.Errorf("ShouldUseColor() should return false when NO_COLOR=%q", v)
		}
	}
	// ...even over CLICOLOR_FORCE.
	if fakeEnv(true, map[string]string{"NO_COLOR": "", "CLICOLOR_FORCE": "1"}).shouldUseColor() {
		t.Error("NO_COLOR must take precedence over CLICOLOR_FORCE")
	}
}

func TestShouldUseColor_CLICOLOR_0(t *testing.T) {
	t.Parallel()
	if fakeEnv(true, map[string]string{"CLICOLOR": "0"}).shouldUseColor() {
		t.Error("ShouldUseColor() should return false when CLICOLOR=0")
	}
	if !fakeEnv(true, map[string]string{"CLICOLOR": "1"}).shouldUseColor() {
		t.Error("CLICOLOR=1 on a TTY should leave color on")
	}
}

func TestShouldUseColor_CLICOLOR_FORCE(t *testing.T) {
	t.Parallel()
	if !fakeEnv(false, map[string]string{"CLICOLOR_FORCE": "1"}).shouldUseColor() {
		t.Error("ShouldUseColor() should return true when CLICOLOR_FORCE is set, even without a TTY")
	}
}

func TestShouldUseEmoji_Default(t *testing.T) {
	t.Parallel()
	if !fakeEnv(true, nil).shouldUseEmoji() {
		t.Error("ShouldUseEmoji() = false on a TTY, want true")
	}
	if fakeEnv(false, nil).shouldUseEmoji() {
		t.Error("ShouldUseEmoji() = true on a non-TTY, want false")
	}
}

func TestShouldUseEmoji_GT_NO_EMOJI(t *testing.T) {
	t.Parallel()
	if fakeEnv(true, map[string]string{"GT_NO_EMOJI": "1"}).shouldUseEmoji() {
		t.Error("ShouldUseEmoji() should return false when GT_NO_EMOJI is set")
	}
}

func TestIsAgentMode_Default(t *testing.T) {
	t.Parallel()
	if fakeEnv(true, nil).isAgentMode() {
		t.Error("IsAgentMode() should return false by default")
	}
}

func TestIsAgentMode_GT_AGENT_MODE(t *testing.T) {
	t.Parallel()
	if !fakeEnv(false, map[string]string{"GT_AGENT_MODE": "1"}).isAgentMode() {
		t.Error("IsAgentMode() should return true when GT_AGENT_MODE=1")
	}
	if fakeEnv(false, map[string]string{"GT_AGENT_MODE": "0"}).isAgentMode() {
		t.Error("IsAgentMode() should return false when GT_AGENT_MODE=0")
	}
}

func TestIsAgentMode_CLAUDE_CODE(t *testing.T) {
	t.Parallel()
	if !fakeEnv(false, map[string]string{"CLAUDE_CODE": "1"}).isAgentMode() {
		t.Error("IsAgentMode() should return true when CLAUDE_CODE is set")
	}
}

func TestIsAgentMode_CLAUDE_CODE_AnyValue(t *testing.T) {
	t.Parallel()
	if !fakeEnv(false, map[string]string{"CLAUDE_CODE": "any-value"}).isAgentMode() {
		t.Error("IsAgentMode() should return true when CLAUDE_CODE is set to any value")
	}
	if fakeEnv(false, map[string]string{"CLAUDE_CODE": ""}).isAgentMode() {
		t.Error("IsAgentMode() should return false when CLAUDE_CODE is set but empty")
	}
}

func TestInitTheme_EnvOverridesConfig(t *testing.T) {
	t.Parallel()
	if got := fakeEnv(false, map[string]string{"GT_THEME": "dark"}).resolveThemeMode("light"); got != ThemeModeDark {
		t.Errorf("Expected dark mode from env var, got %s", got)
	}
	if got := fakeEnv(false, map[string]string{"GT_THEME": "LIGHT"}).resolveThemeMode("dark"); got != ThemeModeLight {
		t.Errorf("Expected light mode from env var, got %s", got)
	}
	if got := fakeEnv(false, map[string]string{"GT_THEME": "auto"}).resolveThemeMode("dark"); got != ThemeModeAuto {
		t.Errorf("Expected auto mode from env var, got %s", got)
	}
	// An invalid env value falls through to the config.
	if got := fakeEnv(false, map[string]string{"GT_THEME": "sepia"}).resolveThemeMode("dark"); got != ThemeModeDark {
		t.Errorf("Expected an invalid GT_THEME to fall through to config, got %s", got)
	}
}

func TestInitTheme_ConfigUsedWhenNoEnv(t *testing.T) {
	t.Parallel()
	e := fakeEnv(false, nil)
	for config, want := range map[string]ThemeMode{"dark": ThemeModeDark, "Light": ThemeModeLight, "auto": ThemeModeAuto} {
		if got := e.resolveThemeMode(config); got != want {
			t.Errorf("resolveThemeMode(%q) = %s, want %s", config, got, want)
		}
	}
}

func TestInitTheme_DefaultsToAuto(t *testing.T) {
	t.Parallel()
	e := fakeEnv(false, nil)
	for _, config := range []string{"", "bogus"} {
		if got := e.resolveThemeMode(config); got != ThemeModeAuto {
			t.Errorf("resolveThemeMode(%q) = %s, want auto", config, got)
		}
	}
}

func TestHasDarkBackground_ForcedModes(t *testing.T) {
	t.Parallel()
	if !fakeEnv(false, nil).detectDarkBackground(ThemeModeDark) {
		t.Error("Expected a dark background when mode is dark")
	}
	if fakeEnv(false, nil).detectDarkBackground(ThemeModeLight) {
		t.Error("Expected a light background when mode is light")
	}
}

// TestDetectDarkBackground pins how auto mode reads COLORFGBG and that the
// theme from GT_THEME or the config wins over it.
func TestDetectDarkBackground(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		config string
		env    map[string]string
		want   bool
	}{
		{"GT_THEME dark over a light COLORFGBG", "", map[string]string{"GT_THEME": "dark", "COLORFGBG": "0;15"}, true},
		{"GT_THEME light over a dark COLORFGBG", "", map[string]string{"GT_THEME": "light", "COLORFGBG": "15;0"}, false},
		{"configured dark over a light COLORFGBG", "dark", map[string]string{"COLORFGBG": "0;15"}, true},
		{"configured light over a dark COLORFGBG", "light", map[string]string{"COLORFGBG": "15;0"}, false},
		{"COLORFGBG dark background", "", map[string]string{"COLORFGBG": "15;0"}, true},
		{"COLORFGBG light background", "", map[string]string{"COLORFGBG": "0;15"}, false},
		{"COLORFGBG grey background is light", "", map[string]string{"COLORFGBG": "0;7"}, false},
		{"COLORFGBG unparseable", "", map[string]string{"COLORFGBG": "0;bogus"}, true},
		{"COLORFGBG empty background", "", map[string]string{"COLORFGBG": "0;"}, true},
		{"COLORFGBG without a separator", "", map[string]string{"COLORFGBG": "0"}, true},
		{"nothing set", "", nil, true},
	}
	for _, tc := range cases {
		e := fakeEnv(false, tc.env)
		if got := e.detectDarkBackground(e.resolveThemeMode(tc.config)); got != tc.want {
			t.Errorf("%s: detectDarkBackground = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestApplyThemeSetsLipglossBackground: applyTheme must leave lipgloss's
// default renderer on the resolved background even with no terminal, since
// that is the case ApplyThemeMode skips and lipgloss would then query the
// terminal for the answer (gt-xvekee).
func TestApplyThemeSetsLipglossBackground(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		config string
		env    map[string]string
		want   bool
	}{
		{"COLORFGBG dark background", "", map[string]string{"COLORFGBG": "15;0"}, true},
		{"COLORFGBG light background", "", map[string]string{"COLORFGBG": "0;15"}, false},
		{"GT_THEME light over a dark COLORFGBG", "", map[string]string{"GT_THEME": "light", "COLORFGBG": "15;0"}, false},
	}
	for _, tc := range cases {
		if _, got := fakeEnv(false, tc.env).applyTheme(tc.config); got != tc.want {
			t.Errorf("%s: applyTheme dark = %v, want %v", tc.name, got, tc.want)
		}
		if got := lipgloss.HasDarkBackground(); got != tc.want {
			t.Errorf("%s: lipgloss.HasDarkBackground = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestShouldUsePager(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		e    uiEnv
		opts PagerOptions
		want bool
	}{
		{"tty", fakeEnv(true, nil), PagerOptions{}, true},
		{"--no-pager", fakeEnv(true, nil), PagerOptions{NoPager: true}, false},
		{"GT_NO_PAGER", fakeEnv(true, map[string]string{"GT_NO_PAGER": "1"}), PagerOptions{}, false},
		{"not a tty", fakeEnv(false, nil), PagerOptions{}, false},
	}
	for _, tc := range cases {
		if got := tc.e.shouldUsePager(tc.opts); got != tc.want {
			t.Errorf("%s: shouldUsePager = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestPagerCommand(t *testing.T) {
	t.Parallel()
	cases := []struct {
		env  map[string]string
		want string
	}{
		{map[string]string{"GT_PAGER": "most", "PAGER": "more"}, "most"},
		{map[string]string{"PAGER": "more"}, "more"},
		{nil, "less"},
	}
	for _, tc := range cases {
		if got := fakeEnv(true, tc.env).pagerCommand(); got != tc.want {
			t.Errorf("pagerCommand(%v) = %q, want %q", tc.env, got, tc.want)
		}
	}
}

func TestContentHeight(t *testing.T) {
	t.Parallel()
	for content, want := range map[string]int{"": 0, "one": 1, "a\nb": 2, "a\nb\n": 3} {
		if got := contentHeight(content); got != want {
			t.Errorf("contentHeight(%q) = %d, want %d", content, got, want)
		}
	}
}

// The exported predicates are the process-environment versions: each must
// answer what processEnv computes, never a test's fake.
func TestExportedPredicatesReadProcessEnv(t *testing.T) {
	t.Parallel()
	if ShouldUseColor() != processEnv.shouldUseColor() {
		t.Error("ShouldUseColor() disagrees with the process environment")
	}
	if ShouldUseEmoji() != processEnv.shouldUseEmoji() {
		t.Error("ShouldUseEmoji() disagrees with the process environment")
	}
	if IsAgentMode() != processEnv.isAgentMode() {
		t.Error("IsAgentMode() disagrees with the process environment")
	}
}

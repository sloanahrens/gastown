package ui

import (
	"os"
	"strings"

	"github.com/muesli/termenv"
	"golang.org/x/term"
)

// ThemeMode represents the CLI color scheme mode.
type ThemeMode string

const (
	// ThemeModeAuto lets the terminal background guide color selection.
	ThemeModeAuto ThemeMode = "auto"
	// ThemeModeDark forces dark mode colors (light text on dark background).
	ThemeModeDark ThemeMode = "dark"
	// ThemeModeLight forces light mode colors (dark text on light background).
	ThemeModeLight ThemeMode = "light"
)

// uiEnv is what the output decisions read from their process: environment
// variables and whether stdout is a terminal. The exported functions use
// processEnv; tests build their own.
type uiEnv struct {
	lookup    func(string) (string, bool)
	stdoutTTY func() bool
}

// processEnv reads the running process's environment and stdout.
var processEnv = uiEnv{lookup: os.LookupEnv, stdoutTTY: IsTerminal}

func (e uiEnv) getenv(key string) string {
	v, _ := e.lookup(key)
	return v
}

func (e uiEnv) isSet(key string) bool {
	_, ok := e.lookup(key)
	return ok
}

// themeMode is the cached theme mode, set during init.
var themeMode ThemeMode

// hasDarkBackground caches whether we're in dark mode.
var hasDarkBackground bool

// InitTheme initializes the theme mode. Call this early in main.
// configTheme is the value from TownSettings.CLITheme (may be empty).
func InitTheme(configTheme string) {
	themeMode = processEnv.resolveThemeMode(configTheme)
	hasDarkBackground = detectDarkBackground(themeMode)
}

// GetThemeMode returns the current CLI color scheme mode.
// Priority order:
//  1. GT_THEME environment variable ("dark", "light", "auto")
//  2. Configured value from settings (passed to InitTheme)
//  3. Default: "auto"
func GetThemeMode() ThemeMode {
	return themeMode
}

// HasDarkBackground returns true if we're displaying on a dark background.
// This is used by lipgloss AdaptiveColor to select appropriate colors.
func HasDarkBackground() bool {
	return hasDarkBackground
}

// resolveThemeMode determines the theme mode from env and config.
func (e uiEnv) resolveThemeMode(configTheme string) ThemeMode {
	// Priority 1: GT_THEME environment variable
	if envTheme := e.getenv("GT_THEME"); envTheme != "" {
		switch strings.ToLower(envTheme) {
		case "dark":
			return ThemeModeDark
		case "light":
			return ThemeModeLight
		case "auto":
			return ThemeModeAuto
		}
		// Invalid value - fall through to config
	}

	// Priority 2: Config value
	if configTheme != "" {
		switch strings.ToLower(configTheme) {
		case "dark":
			return ThemeModeDark
		case "light":
			return ThemeModeLight
		case "auto":
			return ThemeModeAuto
		}
	}

	// Default: auto
	return ThemeModeAuto
}

// detectDarkBackground determines if we're on a dark background.
func detectDarkBackground(mode ThemeMode) bool {
	switch mode {
	case ThemeModeDark:
		return true
	case ThemeModeLight:
		return false
	default:
		// Auto mode - use termenv detection
		return termenv.HasDarkBackground()
	}
}

// IsTerminal returns true if stdout is connected to a terminal (TTY).
func IsTerminal() bool {
	return term.IsTerminal(int(os.Stdout.Fd()))
}

// ShouldUseColor determines if ANSI color codes should be used.
// Respects NO_COLOR (https://no-color.org/), CLICOLOR, and CLICOLOR_FORCE conventions.
func ShouldUseColor() bool {
	return processEnv.shouldUseColor()
}

func (e uiEnv) shouldUseColor() bool {
	// NO_COLOR takes precedence - any value disables color
	if e.isSet("NO_COLOR") {
		return false
	}

	// CLICOLOR=0 disables color
	if e.getenv("CLICOLOR") == "0" {
		return false
	}

	// CLICOLOR_FORCE enables color even in non-TTY
	if e.isSet("CLICOLOR_FORCE") {
		return true
	}

	// default: use color only if stdout is a TTY
	return e.stdoutTTY()
}

// ShouldUseEmoji determines if emoji decorations should be used.
// Disabled in non-TTY mode to keep output machine-readable.
func ShouldUseEmoji() bool {
	return processEnv.shouldUseEmoji()
}

func (e uiEnv) shouldUseEmoji() bool {
	// GT_NO_EMOJI disables emoji output
	if e.isSet("GT_NO_EMOJI") {
		return false
	}

	// default: use emoji only if stdout is a TTY
	return e.stdoutTTY()
}

// IsAgentMode returns true if the CLI is running in agent-optimized mode.
// This is triggered by:
//   - GT_AGENT_MODE=1 environment variable (explicit)
//   - CLAUDE_CODE environment variable (auto-detect Claude Code)
//
// Agent mode provides ultra-compact output optimized for LLM context windows.
func IsAgentMode() bool {
	return processEnv.isAgentMode()
}

func (e uiEnv) isAgentMode() bool {
	if e.getenv("GT_AGENT_MODE") == "1" {
		return true
	}
	// auto-detect Claude Code environment
	if e.getenv("CLAUDE_CODE") != "" {
		return true
	}
	return false
}

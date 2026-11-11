package storypoints

import "strings"

// SeatClass names the seat class of an agent: haiku, flash or sonnet. An
// agent outside those classes has the class "". Names match exactly, except
// that any name starting with deepseek is the flash class.
func SeatClass(agent string) string {
	switch {
	case agent == "sbx-haiku" || agent == "claude-haiku":
		return "haiku"
	case agent == "sbx-flash" || strings.HasPrefix(agent, "deepseek"):
		return "flash"
	case agent == "claude-sonnet":
		return "sonnet"
	}
	return ""
}

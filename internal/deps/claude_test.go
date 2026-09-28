package deps

import (
	"errors"
	"testing"
)

func TestParseClaudeCodeVersion(t *testing.T) {
	t.Parallel()
	tests := []struct {
		input    string
		expected string
	}{
		{"2.1.101 (Claude Code)", "2.1.101"},
		{"2.1.101 (Claude Code)\n", "2.1.101"},
		{"2.1.101", "2.1.101"},
		{"2.0.20", "2.0.20"},
		{"1.0.128", "1.0.128"},
		{"10.20.30 (Claude Code)", "10.20.30"},
		{"some other output", ""},
		{"", ""},
	}

	for _, tt := range tests {
		result := parseClaudeCodeVersion(tt.input)
		if result != tt.expected {
			t.Errorf("parseClaudeCodeVersion(%q) = %q, want %q", tt.input, result, tt.expected)
		}
	}
}

func TestClaudeCodeStatusFromOutput(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		output      string
		err         error
		wantStatus  ClaudeCodeStatus
		wantVersion string
	}{
		{"exec error", "2.1.101 (Claude Code)", errors.New("exit status 1"), ClaudeCodeExecFailed, ""},
		{"unparseable", "garbage", nil, ClaudeCodeUnknown, ""},
		{"too old", "2.0.19 (Claude Code)", nil, ClaudeCodeTooOld, "2.0.19"},
		{"at minimum, below recommended", MinClaudeCodeVersion + " (Claude Code)\n", nil, ClaudeCodeOldButOK, MinClaudeCodeVersion},
		{"at recommended", RecommendedClaudeCodeVersion, nil, ClaudeCodeOK, RecommendedClaudeCodeVersion},
		{"newer", "2.1.101 (Claude Code)", nil, ClaudeCodeOK, "2.1.101"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			status, version := claudeCodeStatusFromOutput([]byte(tt.output), tt.err)
			if status != tt.wantStatus || version != tt.wantVersion {
				t.Errorf("claudeCodeStatusFromOutput(%q, %v) = %d, %q; want %d, %q", tt.output, tt.err, status, version, tt.wantStatus, tt.wantVersion)
			}
		})
	}
}

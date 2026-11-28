package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestTreeRoot(t *testing.T) {
	t.Parallel()
	cases := []struct {
		label string
		args  []string
		work  string
		ok    bool
	}{
		{"sweepArgs", sweepArgs("/work", []string{"shell"}), "/work", true},
		{"no arguments", nil, "", false},
		{"first argument not root", []string{"shell", "/work"}, "", false},
		{"--root alone", []string{"--root"}, "", false},
		{"empty value", []string{"--root", ""}, "", false},
	}
	for _, c := range cases {
		t.Run(c.label, func(t *testing.T) {
			t.Parallel()
			var stderr bytes.Buffer
			work, ok := treeRoot(c.args, &stderr)
			if work != c.work || ok != c.ok {
				t.Fatalf("treeRoot(%q) = %q, %v, want %q, %v", c.args, work, ok, c.work, c.ok)
			}
			line := stderr.String()
			if c.ok && line != "" {
				t.Fatalf("stderr = %q on success, want none", line)
			}
			if !c.ok && (!strings.HasPrefix(line, "sweep-run: ") || !strings.Contains(line, "--root") || strings.Count(line, "\n") != 1) {
				t.Fatalf("stderr = %q, want one sweep-run: line naming the missing --root", line)
			}
		})
	}
}

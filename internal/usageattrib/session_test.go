package usageattrib

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/dashboard"
)

const assistantLineFormat = `{"type":"assistant","timestamp":"2026-11-13T10:05:00Z","message":{"id":"%s","model":"claude-sonnet-5-5","usage":{"input_tokens":%d,"output_tokens":%d}}}` + "\n"

func writeLines(t *testing.T, path string, lines ...string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	var data []byte
	for _, l := range lines {
		data = append(data, l...)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestUsageAttribReadSessionAddsSubagentMessages(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	main := filepath.Join(dir, "proj", "sess-1.jsonl")
	writeLines(t, main, sessionLine("own", 10, 20))
	writeLines(t, filepath.Join(dir, "proj", "sess-1", "subagents", "agent-x.jsonl"), sessionLine("sub", 5, 6))
	writeLines(t, filepath.Join(dir, "proj", "sess-1", "subagents", "notes.jsonl"), sessionLine("ignored", 99, 99))

	s, mtime, err := ReadSession(main)
	if err != nil {
		t.Fatalf("ReadSession: %v", err)
	}
	if s.ID != "sess-1" || !s.Complete || len(s.Main) != 1 || len(s.Subagent) != 1 {
		t.Fatalf("session = %+v, want sess-1 complete with one own and one subagent message", s)
	}
	if s.Subagent[0].Input != 5 {
		t.Errorf("subagent input = %d, want 5", s.Subagent[0].Input)
	}
	info, err := os.Stat(main)
	if err != nil {
		t.Fatal(err)
	}
	if !mtime.Equal(info.ModTime()) {
		t.Errorf("mtime = %v, want the transcript's last write %v", mtime, info.ModTime())
	}
}

func TestUsageAttribReadSessionFlagsUnparsableLine(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	main := filepath.Join(dir, "sess-2.jsonl")
	writeLines(t, main, sessionLine("own", 10, 20), "{not json\n")

	s, _, err := ReadSession(main)
	if err == nil || s.Complete {
		t.Fatalf("ReadSession complete=%t err=%v, want an incomplete session with an error", s.Complete, err)
	}
	if len(s.Main) != 1 {
		t.Errorf("messages read before the bad line = %d, want 1 kept", len(s.Main))
	}
}

func TestUsageAttribReplaceDoesNotDoubleCount(t *testing.T) {
	t.Parallel()
	usageDir := t.TempDir()
	rec := dashboard.UsageRecord{
		Bead: "b-1", Kind: dashboard.UsageKindWork, Class: dashboard.UsageClassPolecat,
		Rig: "gastown", Workspace: "customer-zero", Complete: true,
		Start: t0, End: t0.Add(time.Hour),
		Totals: dashboard.UsageTotals{Calls: 1, Input: 10, Uncached: 10},
		Models: map[string]dashboard.UsageModel{"claude-sonnet-5-5": {Calls: 1, Input: 10}},
	}
	for i := 0; i < 2; i++ {
		if err := Replace(usageDir, rec, []string{"s1"}, ""); err != nil {
			t.Fatalf("Replace %d: %v", i, err)
		}
	}
	st, err := ReadWork(usageDir, "b-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Sessions) != 1 || st.Record.Totals.Input != 10 {
		t.Errorf("after two replaces sessions=%v input=%d, want [s1] and input 10", st.Sessions, st.Record.Totals.Input)
	}
}

// sessionLine is one assistant line of a transcript: its message id and token counts.
func sessionLine(id string, input, output int) string {
	return fmt.Sprintf(assistantLineFormat, id, input, output)
}

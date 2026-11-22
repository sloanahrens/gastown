package session

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/sandbox"
)

func TestTranscriptTailReadsThePath(t *testing.T) {
	t.Parallel()
	fake := &sandbox.Fake{Reply: map[string]string{"exec": "the tail"}}
	b, _ := openshellTestBackend(t.TempDir(), openshellTestProfile, fake)
	got, err := b.transcriptTail(context.Background(), openshellTestSession, sandboxProjectsDir+"/p/a.jsonl")
	if err != nil || string(got) != "the tail" {
		t.Fatalf("transcriptTail = %q, %v, want the output", got, err)
	}
	if calls := fake.Calls(); len(calls) != 1 || strings.Join(calls[0], " ") != "openshell sandbox exec -n "+openshellTestSession+" --no-tty --no-login-shell -- tail -c 16384 -- "+sandboxProjectsDir+"/p/a.jsonl" {
		t.Fatalf("openshell calls = %v, want the one tail command", calls)
	}
	sentinel := errors.New("exec failed")
	fake.Err = map[string]error{"exec": sentinel}
	_, err = b.transcriptTail(context.Background(), openshellTestSession, sandboxProjectsDir+"/p/a.jsonl")
	if !errors.Is(err, sentinel) || !strings.Contains(err.Error(), openshellTestSession) {
		t.Fatalf("transcriptTail err = %v, want the exec error naming the session", err)
	}
}

func TestTranscriptTailRefusesOtherPaths(t *testing.T) {
	t.Parallel()
	fake := &sandbox.Fake{}
	b, _ := openshellTestBackend(t.TempDir(), openshellTestProfile, fake)
	for _, path := range []string{sandboxHome + "/.claude/other/a.jsonl", sandboxProjectsDir + "/../a.jsonl", sandboxProjectsDir + "/p/a.txt"} {
		if _, err := b.transcriptTail(context.Background(), openshellTestSession, path); err == nil || !strings.Contains(err.Error(), sandboxProjectsDir) {
			t.Errorf("transcriptTail(%q) err = %v, want an error naming the rule", path, err)
		}
	}
	if calls := fake.Calls(); len(calls) != 0 {
		t.Fatalf("openshell calls = %v, want none", calls)
	}
}

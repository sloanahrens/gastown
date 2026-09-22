package beads

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stubBdCapturingArgs writes each arg passed to bd, one per line, into
// args.txt under stubDir, and makes the stub emit a minimal valid issue
// JSON so callers that unmarshal the result succeed. Returns the args.txt
// path.
func stubBdCapturingArgs(t *testing.T, stubDir string, issueJSON string) string {
	t.Helper()
	argsPath := filepath.Join(stubDir, "args.txt")
	stubScript := `#!/bin/sh
for a in "$@"; do
  printf '%s\n' "$a" >> "` + argsPath + `"
done
echo '` + issueJSON + `'
exit 0
`
	stubPath := filepath.Join(stubDir, "bd")
	if err := os.WriteFile(stubPath, []byte(stubScript), 0755); err != nil {
		t.Fatalf("write bd stub: %v", err)
	}
	t.Setenv("PATH", stubDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	ResetBdAllowStaleCacheForTest()
	return argsPath
}

func TestCreateProbeBead_StructuralGuards(t *testing.T) {
	stubDir := t.TempDir()
	argsPath := stubBdCapturingArgs(t, stubDir,
		`{"id":"gt-probe1","title":"TEST-ROUTING-PROBE","status":"open","priority":4,"type":"chore","labels":["gt:probe"]}`)

	b := New(t.TempDir())
	issue, err := b.CreateProbeBead("TEST-ROUTING-PROBE: does --repo route to gastown?", "answers gt-eje7 follow-up")
	if err != nil {
		t.Fatalf("CreateProbeBead: %v", err)
	}
	if issue.ID != "gt-probe1" {
		t.Errorf("issue.ID = %q, want gt-probe1", issue.ID)
	}

	argsData, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatalf("read stub args: %v", err)
	}
	args := string(argsData)

	// A probe must never be created dispatchable: never bug/task at a
	// pageable priority. It must be ephemeral (invisible to bd list/ready)
	// and, as defense-in-depth against a forgotten probe surviving TTL
	// promotion (gt-eje7), type=chore at the lowest priority.
	for _, want := range []string{
		"--ephemeral",
		"--wisp-type=probe",
		"--type=chore",
		"--priority=4",
		"--labels=gt:probe",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("expected %q in bd args, got:\n%s", want, args)
		}
	}
	for _, forbid := range []string{"--type=bug", "--priority=0", "--priority=1", "--priority=2"} {
		if strings.Contains(args, forbid) {
			t.Errorf("probe bead must never be dispatchable; found %q in bd args:\n%s", forbid, args)
		}
	}
}

func TestCreateProbeBead_RejectsFlagLikeTitle(t *testing.T) {
	b := New(t.TempDir())
	if _, err := b.CreateProbeBead("--help", ""); err == nil {
		t.Fatal("expected error for flag-like title, got nil")
	}
}

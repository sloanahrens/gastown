package formula

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

// pushRuleName is the contract paragraph that states the push-after-commit
// rule. The committing steps point at it by this name instead of copying the
// command (R2, gt-6096pf).
const pushRuleName = "Push as you go"

// pushCommand is the command the contract states once.
const pushCommand = "git push origin HEAD"

// decodedPolecatWork is mol-polecat-work decoded to the parts the push tests
// read.
type decodedPolecatWork struct {
	Description string `toml:"description"`
	Steps       []struct {
		ID          string `toml:"id"`
		Description string `toml:"description"`
	} `toml:"steps"`
}

func decodePolecatWork(t *testing.T) decodedPolecatWork {
	t.Helper()
	raw, err := GetEmbeddedFormulaContent("mol-polecat-work")
	if err != nil {
		t.Fatalf("GetEmbeddedFormulaContent(mol-polecat-work): %v", err)
	}
	var f decodedPolecatWork
	if _, err := toml.Decode(string(raw), &f); err != nil {
		t.Fatalf("decode mol-polecat-work: %v", err)
	}
	return f
}

// step returns one step's description, or fails when the formula has no such
// step.
func (f decodedPolecatWork) step(t *testing.T, id string) string {
	t.Helper()
	for _, s := range f.Steps {
		if s.ID == id {
			return s.Description
		}
	}
	t.Fatalf("mol-polecat-work has no step %q", id)
	return ""
}

// committingSteps names each step that commits before `gt done` and the text
// in it that commits, so each step's pointer can be required after that
// step's own last commit. submit-and-exit is not here: every commit it makes
// is followed immediately by `gt done`, which the rule says pushes.
var committingSteps = []struct {
	step, commit string
}{
	{"branch-setup", `git commit -m "WIP:`},
	{"implement", `git commit -m "<type>:`},
	{"commit-changes", `git commit -m "<type>:`},
	{"self-review", "amend or add commits"},
	{"pre-verify", "commit, and re-run"},
}

// TestPolecatWorkPushesAfterCommit pins gt-6096pf's lever 1: a seat stopped at
// the overrun stop loses no finished work because every committing step
// pushes the seat's own branch. The contract states the rule once, under the
// name Push as you go; each committing step points at it after that step's
// last commit and none restates the command.
func TestPolecatWorkPushesAfterCommit(t *testing.T) {
	t.Parallel()
	f := decodePolecatWork(t)

	t.Run("states the rule once in the contract", func(t *testing.T) {
		t.Parallel()
		if got := strings.Count(f.Description, pushRuleName); got != 1 {
			t.Errorf("the contract names %q %d times, want 1", pushRuleName, got)
		}
		if got := strings.Count(f.Description, "`"+pushCommand+"`"); got != 1 {
			t.Errorf("the contract states %q %d times, want 1", pushCommand, got)
		}
	})

	t.Run("names the form a replaced commit needs", func(t *testing.T) {
		t.Parallel()
		if !strings.Contains(f.Description, "git push --force-with-lease origin HEAD") {
			t.Error("the contract lacks the force-with-lease form an amend, rebase or squash needs")
		}
	})

	t.Run("a failed push is noted, not fatal", func(t *testing.T) {
		t.Parallel()
		if !strings.Contains(f.Description, "A push that fails") || !strings.Contains(f.Description, "`gt done` pushes again") {
			t.Error("the contract does not say a failed push is noted and gt done pushes again")
		}
	})

	t.Run("refuses a push toward main", func(t *testing.T) {
		t.Parallel()
		for _, clause := range []string{"starts `polecat/`", "`git branch --show-current` prints `main`"} {
			if !strings.Contains(f.Description, clause) {
				t.Errorf("the contract lacks the branch guard %q", clause)
			}
		}
	})

	t.Run("every committing step points at the rule after its last commit", func(t *testing.T) {
		t.Parallel()
		for _, tc := range committingSteps {
			t.Run(tc.step, func(t *testing.T) {
				t.Parallel()
				desc := f.step(t, tc.step)
				lastCommit := strings.LastIndex(desc, tc.commit)
				if lastCommit < 0 {
					t.Fatalf("step %s no longer carries the committing text %q; update the table", tc.step, tc.commit)
				}
				pointer := strings.LastIndex(desc, pushRuleName)
				if pointer < 0 {
					t.Fatalf("step %s does not point at %q", tc.step, pushRuleName)
				}
				if got := strings.Count(desc, pushRuleName); got != 1 {
					t.Errorf("step %s carries %d pointers, want 1", tc.step, got)
				}
				if pointer < lastCommit {
					t.Errorf("step %s points at %q at byte %d, before its commit at byte %d", tc.step, pushRuleName, pointer, lastCommit)
				}
				if strings.Contains(desc, "git push") {
					t.Errorf("step %s restates the push command instead of pointing at %q", tc.step, pushRuleName)
				}
			})
		}
	})

	t.Run("no committing step is missing from the table", func(t *testing.T) {
		t.Parallel()
		inTable := map[string]bool{}
		for _, tc := range committingSteps {
			inTable[tc.step] = true
		}
		for _, s := range f.Steps {
			if strings.Contains(s.Description, "git commit") && !inTable[s.ID] {
				t.Errorf("step %s commits but is not in committingSteps; add its row and its pointer", s.ID)
			}
		}
	})
}

// TestDirectivePolecatPushesAfterCommit pins the directive's half of
// gt-6096pf: one pointer line, at most 160 bytes, naming the formula's rule
// and restating neither its command nor a flag.
func TestDirectivePolecatPushesAfterCommit(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Join("..", "..", "directives", "polecat.md"))
	if err != nil {
		t.Fatalf("read directives/polecat.md: %v", err)
	}
	var pointers []string
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.Contains(line, pushRuleName) {
			pointers = append(pointers, line)
		}
	}
	if len(pointers) != 1 {
		t.Fatalf("directives/polecat.md has %d lines naming %q, want 1", len(pointers), pushRuleName)
	}
	line := pointers[0]

	t.Run("names the formula whose rule it points at", func(t *testing.T) {
		t.Parallel()
		if !strings.Contains(line, "mol-polecat-work") {
			t.Errorf("the pointer line does not name the formula: %q", line)
		}
	})

	t.Run("restates no command", func(t *testing.T) {
		t.Parallel()
		if strings.Contains(line, "git") {
			t.Errorf("the pointer line carries a git command instead of pointing: %q", line)
		}
	})

	t.Run("stays within its 160-byte cap", func(t *testing.T) {
		t.Parallel()
		if n := len(line); n > 160 {
			t.Errorf("the pointer line is %d bytes, over its 160-byte cap: %q", n, line)
		}
	})
}

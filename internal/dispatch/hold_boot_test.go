package dispatch

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/ring/ringtest"
)

// writeBootHealth writes the daemon's boot-health file under townRoot with
// content, creating the daemon directory the way the daemon does.
func writeBootHealth(t *testing.T, townRoot, content string) {
	t.Helper()
	path := BootHealthPath(townRoot)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestBootGateHoldReadsTheFile: the file's line holds dispatch while it exists,
// no file allows it, and a file that cannot be read holds with the unreadable
// reason. A Valid Ring does not override the file.
func TestBootGateHoldReadsTheFile(t *testing.T) {
	t.Parallel()
	t.Run("file present holds with its line", func(t *testing.T) {
		t.Parallel()
		townRoot := t.TempDir()
		writeBootHealth(t, townRoot, "waiting for infrastructure\n")
		reason := OperatorHold(townRoot)
		if !strings.Contains(reason, "waiting for infrastructure") {
			t.Fatalf("OperatorHold with the boot file = %q, want the file's line", reason)
		}
	})
	t.Run("file absent allows", func(t *testing.T) {
		t.Parallel()
		townRoot := t.TempDir()
		if reason := OperatorHold(townRoot); reason != "" {
			t.Fatalf("OperatorHold with no boot file = %q, want \"\"", reason)
		}
	})
	t.Run("file unreadable holds", func(t *testing.T) {
		t.Parallel()
		// A daemon path that is a regular file makes the read fail with
		// ENOTDIR, which is not "does not exist".
		townRoot := t.TempDir()
		if err := os.WriteFile(filepath.Join(townRoot, "daemon"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		reason := OperatorHold(townRoot)
		if !strings.Contains(reason, "treating as held") {
			t.Fatalf("OperatorHold with an unreadable boot file = %q, want an unreadable-hold refusal", reason)
		}
	})
	t.Run("valid Ring does not override the file", func(t *testing.T) {
		t.Parallel()
		townRoot := t.TempDir()
		ringtest.Valid(t, townRoot, ringKey, time.Now())
		writeBootHealth(t, townRoot, "waiting for infrastructure\n")
		reason := operatorHold(townRoot, testKey, time.Now())
		if !strings.Contains(reason, "waiting for infrastructure") {
			t.Fatalf("operatorHold under a valid Ring and the boot file = %q, want the file's line", reason)
		}
	})
}

// TestBootGateHoldEmptyFileHolds: a file with no first line still holds. An
// empty reason must not read as the file being absent (fail closed).
func TestBootGateHoldEmptyFileHolds(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	writeBootHealth(t, townRoot, "")
	reason := BootHold(townRoot)
	if reason == "" {
		t.Fatal("BootHold on an empty boot file = \"\", want it held")
	}
	if !strings.Contains(reason, BootHealthPath(townRoot)) {
		t.Errorf("BootHold on an empty boot file = %q, want it to name the file", reason)
	}
}

// TestBootGateHoldEmptyTownRootAllows: with no town there is no file to read,
// so BootHold must not read daemon/boot-health from the working directory.
func TestBootGateHoldEmptyTownRootAllows(t *testing.T) {
	t.Parallel()
	if reason := BootHold(""); reason != "" {
		t.Fatalf("BootHold(\"\") = %q, want \"\"", reason)
	}
}

// TestBootGateHoldReasonNamesTheFile: the reason names the file, and says the
// file is removed by hand if the daemon is retired, so an operator who reads a
// refusal can find and clear it.
func TestBootGateHoldReasonNamesTheFile(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	writeBootHealth(t, townRoot, "waiting for infrastructure\n")
	reason := BootHold(townRoot)
	if !strings.Contains(reason, BootHealthPath(townRoot)) {
		t.Errorf("BootHold = %q, want it to name %s", reason, BootHealthPath(townRoot))
	}
	if !strings.Contains(reason, "remove the file by hand") {
		t.Errorf("BootHold = %q, want the instruction to remove the file by hand", reason)
	}
}

// TestBootGateHoldDocStatesFailClosedAndTheClearingStep: the BootHold comment
// states that a file left by a dead daemon holds on purpose, and names the step
// that clears it. That contract lives in the comment, so the test reads the doc
// comment of BootHold from this package's hold.go rather than a runtime value.
func TestBootGateHoldDocStatesFailClosedAndTheClearingStep(t *testing.T) {
	t.Parallel()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "hold.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parsing hold.go: %v", err)
	}
	var doc string
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == "BootHold" && fn.Doc != nil {
			doc = fn.Doc.Text()
		}
	}
	if doc == "" {
		t.Fatal("BootHold has no doc comment")
	}
	for _, want := range []string{"fail closed", "daemon/boot-health"} {
		if !strings.Contains(doc, want) {
			t.Errorf("BootHold doc comment does not contain %q:\n%s", want, doc)
		}
	}
}

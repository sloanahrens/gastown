package bdgate

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

//testpolicy:allow parallel — sets the package-wide gate that the next test also sets
func TestRequireIsANoOpUntilSet(t *testing.T) {
	Set(nil)
	if err := Require(); err != nil {
		t.Fatalf("Require() with no gate = %v", err)
	}
}

//testpolicy:allow parallel — sets the package-wide gate that the previous test also sets
func TestRequireReturnsTheGateVerdict(t *testing.T) {
	refusal := errors.New("bd handshake failed")
	Set(func() error { return refusal })
	t.Cleanup(func() { Set(nil) })
	if err := Require(); !errors.Is(err, refusal) {
		t.Fatalf("Require() = %v, want the gate's refusal", err)
	}
}

// TestEverySessionStartCallsTheGate pins the call sites: each function that
// starts an agent session asks the gate first, so a command path the CLI
// gate list does not name still cannot start a session on an unknown bd.
func TestEverySessionStartCallsTheGate(t *testing.T) {
	t.Parallel()
	root := filepath.Join("..", "..")
	for file, fn := range map[string]string{
		"internal/session/lifecycle.go":       "func StartSession(",
		"internal/polecat/session_manager.go": "func (m *SessionManager) Start(",
		"internal/crew/manager.go":            "func (m *Manager) Start(",
		"internal/witness/manager.go":         "func (m *Manager) Start(",
		"internal/refinery/manager.go":        "func (m *Manager) start(",
		"internal/deacon/manager.go":          "func (m *Manager) Start(",
		"internal/mayor/manager.go":           "func (m *Manager) Start(",
	} {
		data, err := os.ReadFile(filepath.Join(root, file))
		if err != nil {
			t.Fatal(err)
		}
		src := string(data)
		i := strings.Index(src, fn)
		if i < 0 {
			t.Errorf("%s: %s not found", file, fn)
			continue
		}
		body := src[i:]
		if j := strings.Index(body[1:], "\nfunc "); j >= 0 {
			body = body[:j+1]
		}
		if !strings.Contains(body, "bdgate.Require()") {
			t.Errorf("%s: %s... does not call bdgate.Require()", file, fn)
		}
	}
}

package doctor

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/deps"
)

type fakeExitStatus int

func (e fakeExitStatus) Error() string { return fmt.Sprintf("exit status %d", int(e)) }
func (e fakeExitStatus) ExitCode() int { return int(e) }

const (
	forkVersionJSON      = `{"build":"da4983e","build_id":"da4983e","commit":"da4983e","contract_version":1,"db_schema_version":66,"version":"1.2.2"}`
	installedVersionJSON = `{"build":"537accb","commit":"537accbea9ca","schema_version":1,"version":"1.2.2"}`
)

func handshakeCheck(version string, versionErr error, dbLevel int) *BeadsBinaryCheck {
	c := NewBeadsBinaryCheck()
	c.lookPath = func(string) (string, error) { return "/opt/bd", nil }
	c.run = func(_ context.Context, _ []string, args ...string) ([]byte, []byte, error) {
		if args[0] == "version" {
			return []byte(version), nil, versionErr
		}
		return []byte(fmt.Sprintf(`[{"version": %d}]`, dbLevel)), nil, nil
	}
	return c
}

func TestBeadsBinaryCheck_Metadata(t *testing.T) {
	t.Parallel()
	check := NewBeadsBinaryCheck()
	if check.Name() != "beads-binary" {
		t.Errorf("Name() = %q, want %q", check.Name(), "beads-binary")
	}
	if check.Category() != CategoryInfrastructure {
		t.Errorf("Category() = %q, want %q", check.Category(), CategoryInfrastructure)
	}
	if check.CanFix() {
		t.Error("CanFix() should return false: gastown never installs bd")
	}
}

func TestBeadsBinaryCheck_HandshakePasses(t *testing.T) {
	t.Parallel()
	result := handshakeCheck(forkVersionJSON, nil, 66).Run(&CheckContext{TownRoot: t.TempDir()})
	if result.Status != StatusOK {
		t.Fatalf("status = %v: %s %v", result.Status, result.Message, result.Details)
	}
	for _, want := range []string{"da4983e", "schema<=66", "contract 1", "database at 66"} {
		if !strings.Contains(result.Message, want) {
			t.Errorf("message %q lacks %q", result.Message, want)
		}
	}
}

func TestBeadsBinaryCheck_Refusals(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		check *BeadsBinaryCheck
		want  string
	}{
		{"installed pre-D1 build", handshakeCheck(installedVersionJSON, nil, 66), "no contract_version"},
		{"schema mismatch", handshakeCheck(forkVersionJSON, nil, 65), "database is at 65"},
		{"not on PATH", handshakeCheck("", &exec.Error{Name: "bd", Err: exec.ErrNotFound}, 0), "not found on PATH"},
		{"unparseable version", handshakeCheck("some garbage output", nil, 0), "not JSON"},
		{"version fails", handshakeCheck("", fakeExitStatus(1), 0), "bd version --json failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			result := tc.check.Run(&CheckContext{TownRoot: t.TempDir()})
			if result.Status != StatusError {
				t.Fatalf("status = %v, want StatusError: %s", result.Status, result.Message)
			}
			if !strings.Contains(result.Message, tc.want) {
				t.Errorf("message %q lacks %q", result.Message, tc.want)
			}
			details := strings.Join(result.Details, "\n")
			if !strings.Contains(details, "found:") || !strings.Contains(details, "required:") {
				t.Errorf("details must name found vs required:\n%s", details)
			}
			if result.FixHint != deps.BDInstallHint {
				t.Errorf("fix hint = %q, want %q byte for byte", result.FixHint, deps.BDInstallHint)
			}
		})
	}
}

// TestBeadsBinaryCheck_DoltUnreachableNamesDolt is the refusal when bd passed
// its version check and the database did not answer: the report names Dolt and
// its commands, and never sends the operator to install bd (gt-j572r).
func TestBeadsBinaryCheck_DoltUnreachableNamesDolt(t *testing.T) {
	t.Parallel()
	c := NewBeadsBinaryCheck()
	c.lookPath = func(string) (string, error) { return "/opt/bd", nil }
	c.handshakeTimeout = 50 * time.Millisecond
	c.run = func(_ context.Context, _ []string, args ...string) ([]byte, []byte, error) {
		if args[0] == "version" {
			return []byte(forkVersionJSON), nil, nil
		}
		return nil, []byte("Error: store unavailable"), fakeExitStatus(25)
	}
	result := c.Run(&CheckContext{TownRoot: t.TempDir()})
	if result.Status != StatusError {
		t.Fatalf("status = %v, want StatusError: %s", result.Status, result.Message)
	}
	if !strings.Contains(result.Message, "Dolt is not reachable") {
		t.Errorf("message %q does not name Dolt", result.Message)
	}
	for _, want := range []string{"gt dolt status", "gt dolt start"} {
		if !strings.Contains(result.FixHint, want) {
			t.Errorf("fix hint %q lacks %q", result.FixHint, want)
		}
	}
	if strings.Contains(result.FixHint, "make safe-install") {
		t.Errorf("fix hint %q sends the operator to install bd", result.FixHint)
	}
	details := strings.Join(result.Details, "\n")
	if !strings.Contains(details, "passed its version check") {
		t.Errorf("details must say the bd on PATH is not the problem:\n%s", details)
	}
	for _, unwanted := range []string{"make safe-install", "required:"} {
		if strings.Contains(details, unwanted) {
			t.Errorf("details must not blame bd with %q:\n%s", unwanted, details)
		}
	}
}

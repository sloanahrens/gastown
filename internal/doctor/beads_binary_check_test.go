package doctor

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"
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
			if !strings.Contains(result.FixHint, "make safe-install") || strings.Contains(result.FixHint, "go install") {
				t.Errorf("fix hint = %q, want make safe-install and never go install", result.FixHint)
			}
		})
	}
}

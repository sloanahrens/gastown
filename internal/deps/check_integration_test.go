//go:build integration

package deps

import (
	"os"
	"os/exec"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil"
)

// TestMain runs the integration tier under the hermetic harness (gt-lwi):
// CheckBeads runs the real bd binary, which could otherwise reach live town
// state.
func TestMain(m *testing.M) {
	os.Exit(testutil.HermeticMain(m))
}

// The Check* tests run the real tools. A host without the tool must report
// NotFound and exec.LookPath must agree; a host with it must report a
// version whenever the status is OK.

func TestIntegrationCheckBeads(t *testing.T) {
	status, version := CheckBeads()
	_, lookErr := exec.LookPath("bd")
	if (status == BeadsNotFound) != (lookErr != nil) {
		t.Fatalf("CheckBeads status = %d, but LookPath(bd) err = %v", status, lookErr)
	}
	if status == BeadsOK && version == "" {
		t.Error("CheckBeads returned BeadsOK but empty version")
	}
	t.Logf("CheckBeads: status=%d, version=%s", status, version)
}

func TestIntegrationCheckClaudeCode(t *testing.T) {
	status, version := CheckClaudeCode()
	_, lookErr := exec.LookPath("claude")
	if (status == ClaudeCodeNotFound) != (lookErr != nil) {
		t.Fatalf("CheckClaudeCode status = %d, but LookPath(claude) err = %v", status, lookErr)
	}
	if status == ClaudeCodeOK && version == "" {
		t.Error("CheckClaudeCode returned ClaudeCodeOK but empty version")
	}
	t.Logf("CheckClaudeCode: status=%d, version=%s", status, version)
}

func TestIntegrationCheckDolt(t *testing.T) {
	status, version, detail := CheckDolt()
	_, lookErr := exec.LookPath("dolt")
	if (status == DoltNotFound) != (lookErr != nil) {
		t.Fatalf("CheckDolt status = %d, but LookPath(dolt) err = %v", status, lookErr)
	}
	if status == DoltOK && version == "" {
		t.Error("CheckDolt returned DoltOK but empty version")
	}
	t.Logf("CheckDolt: status=%d, version=%s, detail=%s", status, version, detail)
}

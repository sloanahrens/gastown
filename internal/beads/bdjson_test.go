package beads

import (
	"os"
	"runtime"
	"strings"
	"testing"
)

func TestRunBdJSON_FailureCarriesBdsStderr(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell stub")
	}
	binDir := t.TempDir()
	writeBDStub(t, binDir, "#!/bin/sh\necho 'database locked' >&2\nexit 1\n", "")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	_, err := RunBdJSON(t.TempDir(), "show", "gt-x", "--json")
	if err == nil {
		t.Fatal("RunBdJSON succeeded, want bd's failure")
	}
	if !strings.Contains(err.Error(), "bd show: database locked") {
		t.Errorf("err = %q, want it to carry bd's stderr", err)
	}
}

func TestRunBdJSONWith_StartsFromTheSuppliedEnv(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell stub")
	}
	binDir := t.TempDir()
	writeBDStub(t, binDir, "#!/bin/sh\nprintf '%s' \"$GT_TEST_MARKER\"\n", "")

	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	env := append(os.Environ(), "GT_TEST_MARKER=from-daemon-env")
	out, err := RunBdJSONWith(BdJSONOptions{Env: env}, t.TempDir(), "list", "--json")
	if err != nil {
		t.Fatalf("RunBdJSONWith: %v", err)
	}
	if string(out) != "from-daemon-env" {
		t.Errorf("stdout = %q, want the marker from the supplied env", out)
	}
}

package callercwd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestGetwd pins the seam: GT_CALLER_CWD wins when it names an absolute, clean
// directory; a value that cannot name one is refused by name; unset falls back
// to the process directory.
//
//testpolicy:allow parallel — every case sets GT_CALLER_CWD, which t.Setenv shares with the whole process
func TestGetwd(t *testing.T) {
	processDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("process directory: %v", err)
	}

	t.Run("unset falls back to the process directory", func(t *testing.T) {
		// The hermetic harness scrubs GT_* variables before the tests run.
		got, err := Getwd()
		if err != nil {
			t.Fatalf("Getwd: %v", err)
		}
		if got != processDir {
			t.Fatalf("Getwd = %q, want the process directory %q", got, processDir)
		}
	})

	t.Run("the variable wins over the process directory", func(t *testing.T) {
		callerDir := t.TempDir()
		//testpolicy:allow no-env — GT_CALLER_CWD is the input under test
		t.Setenv(EnvCallerCwd, callerDir)

		got, err := Getwd()
		if err != nil {
			t.Fatalf("Getwd: %v", err)
		}
		if got != callerDir {
			t.Fatalf("Getwd = %q, want the caller's directory %q", got, callerDir)
		}
	})

	t.Run("an absolute path that does not exist is trusted", func(t *testing.T) {
		// Resolution must answer for a caller whose directory is not in this
		// process; Require is where presence is checked.
		callerDir := filepath.Join(t.TempDir(), "crew", "eagle")
		//testpolicy:allow no-env — GT_CALLER_CWD is the input under test
		t.Setenv(EnvCallerCwd, callerDir)

		got, err := Getwd()
		if err != nil {
			t.Fatalf("Getwd: %v", err)
		}
		if got != callerDir {
			t.Fatalf("Getwd = %q, want the absent caller's directory %q", got, callerDir)
		}
	})

	refused := []struct {
		name  string
		value string
	}{
		{name: "empty", value: ""},
		{name: "whitespace only", value: "   "},
		{name: "whitespace padded", value: " /tmp/crew"},
		{name: "relative", value: "gastown/crew/eagle"},
		{name: "parent segment", value: "/tmp/crew/../eagle"},
		{name: "trailing separator", value: "/tmp/crew/eagle/"},
		{name: "control character", value: "/tmp/crew\neagle"},
	}
	for _, tc := range refused {
		t.Run("refuses "+tc.name, func(t *testing.T) {
			//testpolicy:allow no-env — GT_CALLER_CWD is the input under test
			t.Setenv(EnvCallerCwd, tc.value)

			got, err := Getwd()
			if err == nil {
				t.Fatalf("Getwd(%q) = %q, want a refusal", tc.value, got)
			}
			if !strings.Contains(err.Error(), EnvCallerCwd) {
				t.Fatalf("Getwd(%q) error %q does not name %s", tc.value, err, EnvCallerCwd)
			}
			if got != "" {
				t.Fatalf("Getwd(%q) returned %q beside the refusal", tc.value, got)
			}
		})
	}
}

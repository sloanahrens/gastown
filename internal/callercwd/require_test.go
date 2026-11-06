package callercwd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRequire pins the guard commands use before running git or writing in the
// caller's directory: a present directory is returned, and an absent one is
// refused with the caller's path, never replaced by the process directory.
//
//testpolicy:allow parallel — every case sets GT_CALLER_CWD, which t.Setenv shares with the whole process
func TestRequire(t *testing.T) {
	t.Run("a present directory is returned", func(t *testing.T) {
		callerDir := t.TempDir()
		//testpolicy:allow no-env — GT_CALLER_CWD is the input under test
		t.Setenv(EnvCallerCwd, callerDir)

		got, err := Require()
		if err != nil {
			t.Fatalf("Require: %v", err)
		}
		if got != callerDir {
			t.Fatalf("Require = %q, want %q", got, callerDir)
		}
	})

	t.Run("an absent directory is refused by path", func(t *testing.T) {
		processDir, err := os.Getwd()
		if err != nil {
			t.Fatalf("process directory: %v", err)
		}
		callerDir := filepath.Join(t.TempDir(), "crew", "eagle")
		//testpolicy:allow no-env — GT_CALLER_CWD is the input under test
		t.Setenv(EnvCallerCwd, callerDir)

		got, err := Require()
		if err == nil {
			t.Fatalf("Require = %q, want the not-present refusal", got)
		}
		want := fmt.Sprintf("the caller's directory %s is not present here", callerDir)
		if err.Error() != want {
			t.Fatalf("Require error = %q, want %q", err, want)
		}
		if got != "" {
			t.Fatalf("Require returned %q beside the refusal", got)
		}
		if got == processDir {
			t.Fatalf("Require fell back to the process directory %q", processDir)
		}
	})

	t.Run("a file where the directory should be is refused", func(t *testing.T) {
		callerFile := filepath.Join(t.TempDir(), "eagle")
		if err := os.WriteFile(callerFile, []byte("not a directory"), 0644); err != nil {
			t.Fatalf("write file: %v", err)
		}
		//testpolicy:allow no-env — GT_CALLER_CWD is the input under test
		t.Setenv(EnvCallerCwd, callerFile)

		got, err := Require()
		if err == nil {
			t.Fatalf("Require = %q, want the not-present refusal", got)
		}
		want := fmt.Sprintf("the caller's directory %s is not present here", callerFile)
		if err.Error() != want {
			t.Fatalf("Require error = %q, want %q", err, want)
		}
	})

	t.Run("an unusable variable is refused by name", func(t *testing.T) {
		processDir, err := os.Getwd()
		if err != nil {
			t.Fatalf("process directory: %v", err)
		}
		//testpolicy:allow no-env — GT_CALLER_CWD is the input under test
		t.Setenv(EnvCallerCwd, "gastown/crew/eagle")

		got, err := Require()
		if err == nil {
			t.Fatalf("Require = %q, want a refusal", got)
		}
		if got == processDir {
			t.Fatalf("Require fell back to the process directory %q", processDir)
		}
	})

	t.Run("an unreadable path is an error, not absent", func(t *testing.T) {
		parent := filepath.Join(t.TempDir(), "eagle")
		if err := os.WriteFile(parent, []byte("not a directory"), 0644); err != nil {
			t.Fatalf("write file: %v", err)
		}
		callerPath := filepath.Join(parent, "gastown")
		//testpolicy:allow no-env — GT_CALLER_CWD is the input under test
		t.Setenv(EnvCallerCwd, callerPath)

		got, err := Require()
		if err == nil {
			t.Fatalf("Require = %q, want the stat failure", got)
		}
		if !strings.Contains(err.Error(), "checking the caller's directory") {
			t.Fatalf("Require error = %q, want the stat failure named", err)
		}
		if strings.Contains(err.Error(), "is not present here") {
			t.Fatalf("Require error = %q, read an unreadable path as absent", err)
		}
	})
}

// TestExists pins the presence question prime asks before it touches a
// worktree: a present directory is true, an absent one false without an error,
// and an unusable variable is an error rather than "absent".
//
//testpolicy:allow parallel — GT_CALLER_CWD is the input under test, which t.Setenv shares with the whole process
func TestExists(t *testing.T) {
	t.Run("present", func(t *testing.T) {
		//testpolicy:allow no-env — GT_CALLER_CWD is the input under test
		t.Setenv(EnvCallerCwd, t.TempDir())

		ok, err := Exists()
		if err != nil {
			t.Fatalf("Exists: %v", err)
		}
		if !ok {
			t.Fatalf("Exists = false, want true")
		}
	})

	t.Run("absent", func(t *testing.T) {
		//testpolicy:allow no-env — GT_CALLER_CWD is the input under test
		t.Setenv(EnvCallerCwd, filepath.Join(t.TempDir(), "crew", "eagle"))

		ok, err := Exists()
		if err != nil {
			t.Fatalf("Exists: %v", err)
		}
		if ok {
			t.Fatalf("Exists = true, want false")
		}
	})

	t.Run("unusable", func(t *testing.T) {
		//testpolicy:allow no-env — GT_CALLER_CWD is the input under test
		t.Setenv(EnvCallerCwd, "relative/path")

		ok, err := Exists()
		if err == nil {
			t.Fatalf("Exists = %v, want the variable's refusal", ok)
		}
		if ok {
			t.Fatalf("Exists = true beside the refusal")
		}
	})

	t.Run("unreadable", func(t *testing.T) {
		parent := filepath.Join(t.TempDir(), "eagle")
		if err := os.WriteFile(parent, []byte("not a directory"), 0644); err != nil {
			t.Fatalf("write file: %v", err)
		}
		//testpolicy:allow no-env — GT_CALLER_CWD is the input under test
		t.Setenv(EnvCallerCwd, filepath.Join(parent, "gastown"))

		ok, err := Exists()
		if err == nil {
			t.Fatalf("Exists = %v, want the stat failure", ok)
		}
		if ok {
			t.Fatalf("Exists = true beside the stat failure")
		}
	})
}

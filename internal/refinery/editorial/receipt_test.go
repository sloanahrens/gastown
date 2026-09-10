package editorial

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/plugin"
)

// fakeBD writes a bd stand-in onto PATH that logs its args and returns a
// canned bead id, mirroring internal/plugin's own RecordRun test.
func fakeBD(t *testing.T) (logPath string) {
	t.Helper()
	binDir := t.TempDir()
	logPath = filepath.Join(t.TempDir(), "bd-args.log")
	bdPath := filepath.Join(binDir, "bd")
	script := "#!/usr/bin/env bash\n" +
		"printf '%s\\n' \"$*\" >> \"$BD_ARGS_LOG\"\n" +
		"case \"$1\" in\n" +
		"  create) printf '{\\\"id\\\":\\\"gt-test-receipt\\\"}\\n' ;;\n" +
		"  close) exit 0 ;;\n" +
		"  *) exit 2 ;;\n" +
		"esac\n"
	if err := os.WriteFile(bdPath, []byte(script), 0755); err != nil {
		t.Fatalf("write fake bd: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("BD_ARGS_LOG", logPath)
	return logPath
}

func TestRecordReceipt_LabelSetExact(t *testing.T) {
	logPath := fakeBD(t)
	rec := plugin.NewRecorder(t.TempDir())

	n := Note{
		OMVersion: "1.4.0",
		Rig:       "gastown",
		Worker:    "marble",
		PatchID:   "patchxyz",
		HeadSHA:   "headxyz",
		Score:     0.72,
		Verdict:   "approve",
		Attempt:   2,
	}
	id, err := RecordReceipt(rec, n)
	if err != nil {
		t.Fatalf("RecordReceipt: %v", err)
	}
	if id != "gt-test-receipt" {
		t.Fatalf("RecordReceipt id = %q, want gt-test-receipt", id)
	}

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read fake bd log: %v", err)
	}
	log := string(data)
	wantLabels := []string{
		"-l type:plugin-run",
		"-l plugin:quality-review-result",
		"-l result:success",
		"-l rig:gastown",
		"-l worker:marble",
		"-l score:0.72",
		"-l recommendation:approve",
		"-l patch_id:patchxyz",
		"-l head_sha:headxyz",
		"-l om_version:1.4.0",
		"-l attempt:2",
	}
	for _, want := range wantLabels {
		if !strings.Contains(log, want) {
			t.Fatalf("fake bd log missing %q in:\n%s", want, log)
		}
	}
	// No label beyond the ones the spec calls for.
	if strings.Count(log, "-l ") != len(wantLabels) {
		t.Fatalf("expected exactly %d labels, log had a different count:\n%s", len(wantLabels), log)
	}
}

func TestRecordFailure_ResultAndFailureClassAndDescription(t *testing.T) {
	logPath := fakeBD(t)
	rec := plugin.NewRecorder(t.TempDir())

	stderr := "om: execution error: backend timed out"
	id, err := RecordFailure(rec, "gastown", "marble", "gt-wisp-x", BackendTimeout, stderr, 1)
	if err != nil {
		t.Fatalf("RecordFailure: %v", err)
	}
	if id != "gt-test-receipt" {
		t.Fatalf("RecordFailure id = %q, want gt-test-receipt", id)
	}

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read fake bd log: %v", err)
	}
	log := string(data)
	for _, want := range []string{
		"-l result:failure",
		"-l failure_class:backend_timeout",
		"-l retries:1",
		"--description=" + stderr,
	} {
		if !strings.Contains(log, want) {
			t.Fatalf("fake bd log missing %q in:\n%s", want, log)
		}
	}
}

func TestRecordFailure_TruncatesDescriptionAt4KiB(t *testing.T) {
	logPath := fakeBD(t)
	rec := plugin.NewRecorder(t.TempDir())

	stderr := strings.Repeat("x", maxFailureDescription+500)
	if _, err := RecordFailure(rec, "gastown", "marble", "gt-wisp-x", Tooling, stderr, 0); err != nil {
		t.Fatalf("RecordFailure: %v", err)
	}

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read fake bd log: %v", err)
	}
	log := string(data)
	if strings.Contains(log, strings.Repeat("x", maxFailureDescription+1)) {
		t.Fatalf("description was not truncated to %d bytes", maxFailureDescription)
	}
	if !strings.Contains(log, "--description="+strings.Repeat("x", maxFailureDescription)) {
		t.Fatalf("description was not truncated to exactly %d bytes:\n%s", maxFailureDescription, log)
	}
}

func TestFailureClass_Retryable(t *testing.T) {
	cases := []struct {
		fc   FailureClass
		want bool
	}{
		{BinaryMissing, false},
		{VersionMismatch, false},
		{ConfigError, false},
		{BackendTimeout, true},
		{MalformedVerdict, true},
		{Tooling, false},
		{RecordFailed, false},
		{Precondition, false},
	}
	for _, tc := range cases {
		if got := tc.fc.Retryable(); got != tc.want {
			t.Errorf("FailureClass(%q).Retryable() = %v, want %v", tc.fc, got, tc.want)
		}
	}
}

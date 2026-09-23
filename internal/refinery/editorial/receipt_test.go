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
		"-l outcome:verdict",
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
		"-l outcome:infra_failure",
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

// recordedCreates returns the fake bd log's bead-creating invocations, one per
// recorded receipt. The recorder also closes each bead it creates, and those
// lines carry no labels, so a shape assertion that kept them would fail on
// every well-formed receipt.
func recordedCreates(t *testing.T, logPath string) []string {
	t.Helper()
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read fake bd log: %v", err)
	}
	var creates []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.HasPrefix(line, "create ") {
			creates = append(creates, line)
		}
	}
	return creates
}

// assertNothingRecorded fails when the fake bd stand-in was invoked at all —
// the refusal guards must return before shelling out, so a refusal that still
// writes a wisp is the bug, not the error return.
func assertNothingRecorded(t *testing.T, logPath string) {
	t.Helper()
	data, err := os.ReadFile(logPath)
	if err == nil && len(data) > 0 {
		t.Fatalf("a refused receipt still reached bd:\n%s", data)
	}
}

func TestRecordReceipt_RefusesVerdictOutsideTheSchema(t *testing.T) {
	for _, verdict := range []string{"", "maybe", "Approve", "approved", "reject"} {
		t.Run("verdict="+verdict, func(t *testing.T) {
			logPath := fakeBD(t)
			rec := plugin.NewRecorder(t.TempDir())

			id, err := RecordReceipt(rec, Note{Worker: "marble", Score: 0.72, Verdict: verdict})
			if err == nil {
				t.Fatalf("RecordReceipt(verdict %q) succeeded with id %q; a receipt whose "+
					"recommendation: no consumer can read must be refused", verdict, id)
			}
			if !strings.Contains(err.Error(), "refusing to record a receipt") {
				t.Fatalf("RecordReceipt(verdict %q) error = %v, want a refusal naming the verdict", verdict, err)
			}
			assertNothingRecorded(t, logPath)
		})
	}
}

func TestRecordReceipt_AcceptsBothSchemaVerdicts(t *testing.T) {
	for _, verdict := range []string{VerdictApprove, VerdictRequestChanges} {
		t.Run(verdict, func(t *testing.T) {
			logPath := fakeBD(t)
			rec := plugin.NewRecorder(t.TempDir())

			if _, err := RecordReceipt(rec, Note{Worker: "marble", Score: 0.5, Verdict: verdict}); err != nil {
				t.Fatalf("RecordReceipt(%s): %v", verdict, err)
			}
			data, err := os.ReadFile(logPath)
			if err != nil {
				t.Fatalf("read fake bd log: %v", err)
			}
			log := string(data)
			for _, want := range []string{
				"-l outcome:verdict",
				"-l result:success",
				"-l score:0.5",
				"-l recommendation:" + verdict,
			} {
				if !strings.Contains(log, want) {
					t.Fatalf("fake bd log missing %q in:\n%s", want, log)
				}
			}
		})
	}
}

func TestRecordFailure_RefusesClassThatIsNotAReason(t *testing.T) {
	for _, fc := range []FailureClass{"", "not_a_class", "VERSION_MISMATCH"} {
		t.Run("class="+string(fc), func(t *testing.T) {
			logPath := fakeBD(t)
			rec := plugin.NewRecorder(t.TempDir())

			id, err := RecordFailure(rec, "gastown", "marble", "gt-wisp-x", fc, "boom", 0)
			if err == nil {
				t.Fatalf("RecordFailure(class %q) succeeded with id %q; a failure receipt's "+
					"class is its only quality signal and must be one bd can be routed on", fc, id)
			}
			if !strings.Contains(err.Error(), "refusing to record an untyped failure receipt") {
				t.Fatalf("RecordFailure(class %q) error = %v, want a refusal naming the class", fc, err)
			}
			assertNothingRecorded(t, logPath)
		})
	}
}

// TestReceiptShapes_EveryResultIsScoredOrTyped is the invariant the bead asks
// for, checked across every shape the writers can produce: a receipt carries a
// score or a typed failure reason, never neither, and the outcome: label says
// which of the two it is.
func TestReceiptShapes_EveryResultIsScoredOrTyped(t *testing.T) {
	shapes := []struct {
		name string
		// write records one shape through the real writer.
		write   func(rec *plugin.Recorder) (string, error)
		outcome ReceiptOutcome
	}{
		{
			name: "verdict approve",
			write: func(rec *plugin.Recorder) (string, error) {
				return RecordReceipt(rec, Note{Worker: "m", Score: 0.9, Verdict: VerdictApprove})
			},
			outcome: OutcomeVerdict,
		},
		{
			name: "verdict request_changes",
			write: func(rec *plugin.Recorder) (string, error) {
				return RecordReceipt(rec, Note{Worker: "m", Score: 0.2, Verdict: VerdictRequestChanges})
			},
			outcome: OutcomeVerdict,
		},
		{
			name: "infra failure every class",
			write: func(rec *plugin.Recorder) (string, error) {
				for _, fc := range []FailureClass{BinaryMissing, VersionMismatch, ConfigError, BackendTimeout, MalformedVerdict, Tooling, RecordFailed, Precondition, RubricRegression, ReviewInFlight} {
					if _, err := RecordFailure(rec, "gastown", "m", "gt-wisp-x", fc, "boom", 0); err != nil {
						return "", err
					}
				}
				return "recorded", nil
			},
			outcome: OutcomeInfraFailure,
		},
	}
	for _, tc := range shapes {
		t.Run(tc.name, func(t *testing.T) {
			logPath := fakeBD(t)
			rec := plugin.NewRecorder(t.TempDir())

			if _, err := tc.write(rec); err != nil {
				t.Fatalf("write: %v", err)
			}
			creates := recordedCreates(t, logPath)
			if len(creates) == 0 {
				t.Fatalf("no receipt was recorded")
			}
			for _, line := range creates {
				hasScore := strings.Contains(line, " -l score:")
				hasClass := strings.Contains(line, " -l failure_class:")
				if hasScore && hasClass {
					t.Fatalf("receipt is both scored and typed:\n%s", line)
				}
				if !hasScore && !hasClass {
					t.Fatalf("scoreless and untyped — the shape the bead exists to remove:\n%s", line)
				}
				if !strings.Contains(line, " -l outcome:"+string(tc.outcome)) {
					t.Fatalf("receipt missing outcome:%s — a consumer reading result: alone cannot tell a "+
						"review verdict from an unreviewed merge:\n%s", tc.outcome, line)
				}
			}
		})
	}
}

// TestReceiptOutcomes_DistinguishVerdictFromRejection pins the reading the
// bead's 14% came from: a request_changes rejection is a verdict (scored), not
// an infra failure, and result: alone says neither.
func TestReceiptOutcomes_DistinguishVerdictFromRejection(t *testing.T) {
	logPath := fakeBD(t)
	rec := plugin.NewRecorder(t.TempDir())

	// A rejection and an unreviewed merge side by side.
	if _, err := RecordReceipt(rec, Note{Worker: "m", Score: 0.2, Verdict: VerdictRequestChanges}); err != nil {
		t.Fatalf("RecordReceipt: %v", err)
	}
	if _, err := RecordFailure(rec, "gastown", "m", "gt-wisp-x", VersionMismatch, "boom", 0); err != nil {
		t.Fatalf("RecordFailure: %v", err)
	}

	lines := recordedCreates(t, logPath)
	if len(lines) != 2 {
		t.Fatalf("expected 2 recorded runs, got %d:\n%s", len(lines), strings.Join(lines, "\n"))
	}
	// Both are result:success / result:failure respectively, but exactly one
	// is a verdict and exactly one is an infra failure.
	if !strings.Contains(lines[0], "-l outcome:verdict") || !strings.Contains(lines[0], "-l recommendation:request_changes") {
		t.Fatalf("the rejection was not recorded as a scored verdict:\n%s", lines[0])
	}
	if !strings.Contains(lines[1], "-l outcome:infra_failure") || !strings.Contains(lines[1], "-l failure_class:version_mismatch") {
		t.Fatalf("the version_mismatch was not recorded as a typed infra failure:\n%s", lines[1])
	}
}

func TestFailureClass_Valid(t *testing.T) {
	valid := []FailureClass{
		BinaryMissing, VersionMismatch, ConfigError, BackendTimeout,
		MalformedVerdict, Tooling, RecordFailed, Precondition,
		RubricRegression, ReviewInFlight,
	}
	for _, fc := range valid {
		if !fc.Valid() {
			t.Errorf("FailureClass(%q).Valid() = false; every class the writers may name must be valid", fc)
		}
	}
	for _, fc := range []FailureClass{"", "nope", "VERSION_MISMATCH", "infra"} {
		if fc.Valid() {
			t.Errorf("FailureClass(%q).Valid() = true, want false — Valid is what RecordFailure refuses on", fc)
		}
	}
}

func TestValidVerdict(t *testing.T) {
	for _, v := range []string{VerdictApprove, VerdictRequestChanges} {
		if !ValidVerdict(v) {
			t.Errorf("ValidVerdict(%q) = false, want true", v)
		}
	}
	for _, v := range []string{"", "maybe", "Approve", "approved", "request changes", "reject"} {
		if ValidVerdict(v) {
			t.Errorf("ValidVerdict(%q) = true, want false", v)
		}
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

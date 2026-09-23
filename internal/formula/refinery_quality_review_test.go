package formula

import (
	"strings"
	"testing"
)

func TestRefineryPatrolVerifyAndReviewUsesMQVerify(t *testing.T) {
	contentBytes, err := formulasFS.ReadFile("formulas/mol-refinery-patrol.formula.toml")
	if err != nil {
		t.Fatalf("reading refinery formula: %v", err)
	}
	content := string(contentBytes)

	forbidden := []string{"om review", "om-gate.sh", "fail-open"}
	for _, pattern := range forbidden {
		if strings.Contains(content, pattern) {
			t.Fatalf("refinery formula must not invoke om directly or describe it as fail-open; found %q", pattern)
		}
	}

	f := loadRefineryPatrolFormula(t)

	verifyAndReview := requireFormulaStep(t, f, "verify-and-review")
	if !strings.Contains(verifyAndReview.Description, "gt mq verify") {
		t.Fatal("verify-and-review must invoke gt mq verify")
	}

	batchScan := requireFormulaStep(t, f, "batch-scan")
	if strings.Contains(batchScan.Description, "fail-open") {
		t.Fatal("batch-scan must not describe gt mq batch run's per-member review as fail-open")
	}
	if strings.Contains(batchScan.Description, "om review") {
		t.Fatal("batch-scan must not invoke om review directly; gt mq batch run reviews members itself")
	}
}

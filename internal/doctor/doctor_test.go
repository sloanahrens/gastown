package doctor

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/ui"
)

// mockCheck is a test check that can be configured to return any status.
type mockCheck struct {
	BaseCheck
	status   CheckStatus
	fixable  bool
	fixError error
	fixCount int
}

func newMockCheck(name string, status CheckStatus) *mockCheck {
	return &mockCheck{
		BaseCheck: BaseCheck{
			CheckName:        name,
			CheckDescription: "Test check: " + name,
		},
		status: status,
	}
}

func (m *mockCheck) Run(ctx *CheckContext) *CheckResult {
	return &CheckResult{
		Name:    m.CheckName,
		Status:  m.status,
		Message: "mock result",
	}
}

func (m *mockCheck) CanFix() bool {
	return m.fixable
}

func (m *mockCheck) Fix(ctx *CheckContext) error {
	m.fixCount++
	if m.fixError != nil {
		return m.fixError
	}
	// Simulate successful fix by changing status
	m.status = StatusOK
	return nil
}

// destructiveMockCheck is a fixable check whose repair is destructive, so it
// exercises the authorize hook.
type destructiveMockCheck struct {
	mockCheck
}

func newDestructiveMockCheck(name string, status CheckStatus) *destructiveMockCheck {
	c := &destructiveMockCheck{mockCheck: *newMockCheck(name, status)}
	c.mockCheck.fixable = true
	return c
}

func (m *destructiveMockCheck) DestructiveFix() bool { return true }

func TestCheckStatus_String(t *testing.T) {
	t.Parallel()
	tests := []struct {
		status CheckStatus
		want   string
	}{
		{StatusOK, "OK"},
		{StatusWarning, "Warning"},
		{StatusError, "Error"},
		{CheckStatus(99), "Unknown"},
	}

	for _, tt := range tests {
		got := tt.status.String()
		if got != tt.want {
			t.Errorf("CheckStatus(%d).String() = %q, want %q", tt.status, got, tt.want)
		}
	}
}

func TestCheckContext_RigPath(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		ctx      CheckContext
		wantPath string
	}{
		{
			name:     "empty rig name",
			ctx:      CheckContext{TownRoot: "/town"},
			wantPath: "",
		},
		{
			name:     "with rig name",
			ctx:      CheckContext{TownRoot: "/town", RigName: "myrig"},
			wantPath: "/town/myrig",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.ctx.RigPath()
			if got != tt.wantPath {
				t.Errorf("RigPath() = %q, want %q", got, tt.wantPath)
			}
		})
	}
}

func TestNewReport(t *testing.T) {
	t.Parallel()
	r := NewReport()

	if r.Timestamp.IsZero() {
		t.Error("NewReport() should set Timestamp")
	}
	if len(r.Checks) != 0 {
		t.Error("NewReport() should have empty Checks slice")
	}
	if r.Summary.Total != 0 {
		t.Error("NewReport() should have zero Total")
	}
}

func TestReport_Add(t *testing.T) {
	t.Parallel()
	r := NewReport()

	// Add an OK result
	r.Add(&CheckResult{Name: "test1", Status: StatusOK})
	if r.Summary.Total != 1 || r.Summary.OK != 1 {
		t.Errorf("After adding OK: Total=%d, OK=%d", r.Summary.Total, r.Summary.OK)
	}

	// Add a warning
	r.Add(&CheckResult{Name: "test2", Status: StatusWarning})
	if r.Summary.Total != 2 || r.Summary.Warnings != 1 {
		t.Errorf("After adding Warning: Total=%d, Warnings=%d", r.Summary.Total, r.Summary.Warnings)
	}

	// Add an error
	r.Add(&CheckResult{Name: "test3", Status: StatusError})
	if r.Summary.Total != 3 || r.Summary.Errors != 1 {
		t.Errorf("After adding Error: Total=%d, Errors=%d", r.Summary.Total, r.Summary.Errors)
	}

	// Add a skipped result
	r.Add(&CheckResult{Name: "test4", Status: StatusSkipped})
	if r.Summary.Total != 4 || r.Summary.Skipped != 1 {
		t.Errorf("After adding Skipped: Total=%d, Skipped=%d", r.Summary.Total, r.Summary.Skipped)
	}
}

func TestReport_HasErrors(t *testing.T) {
	t.Parallel()
	r := NewReport()
	if r.HasErrors() {
		t.Error("Empty report should not have errors")
	}

	r.Add(&CheckResult{Status: StatusOK})
	if r.HasErrors() {
		t.Error("Report with only OK should not have errors")
	}

	r.Add(&CheckResult{Status: StatusWarning})
	if r.HasErrors() {
		t.Error("Report with only OK/Warning should not have errors")
	}

	r.Add(&CheckResult{Status: StatusError})
	if !r.HasErrors() {
		t.Error("Report with Error should have errors")
	}
}

func TestReport_HasWarnings(t *testing.T) {
	t.Parallel()
	r := NewReport()
	if r.HasWarnings() {
		t.Error("Empty report should not have warnings")
	}

	r.Add(&CheckResult{Status: StatusOK})
	if r.HasWarnings() {
		t.Error("Report with only OK should not have warnings")
	}

	r.Add(&CheckResult{Status: StatusWarning})
	if !r.HasWarnings() {
		t.Error("Report with Warning should have warnings")
	}
}

func TestReport_HasSkipped(t *testing.T) {
	t.Parallel()
	r := NewReport()
	if r.HasSkipped() {
		t.Error("Empty report should not have skipped checks")
	}

	r.Add(&CheckResult{Status: StatusOK})
	if r.HasSkipped() {
		t.Error("Report with only OK should not have skipped checks")
	}

	r.Add(&CheckResult{Status: StatusSkipped})
	if !r.HasSkipped() {
		t.Error("Report with Skipped should have skipped checks")
	}
}

func TestReport_IsHealthy(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		results []CheckStatus
		want    bool
	}{
		{"empty", nil, true},
		{"all OK", []CheckStatus{StatusOK, StatusOK}, true},
		{"has warning", []CheckStatus{StatusOK, StatusWarning}, false},
		{"has error", []CheckStatus{StatusOK, StatusError}, false},
		{"has skipped", []CheckStatus{StatusOK, StatusSkipped}, false},
		{"mixed", []CheckStatus{StatusOK, StatusWarning, StatusError}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := NewReport()
			for _, status := range tt.results {
				r.Add(&CheckResult{Status: status})
			}
			if got := r.IsHealthy(); got != tt.want {
				t.Errorf("IsHealthy() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestReport_Print(t *testing.T) {
	t.Parallel()
	r := NewReport()
	r.Add(&CheckResult{
		Name:    "TestCheck",
		Status:  StatusOK,
		Message: "All good",
	})
	r.Add(&CheckResult{
		Name:    "WarningCheck",
		Status:  StatusWarning,
		Message: "Minor issue",
		FixHint: "Run fix command",
	})

	var buf bytes.Buffer
	r.Print(&buf, false, 0)

	output := buf.String()
	if output == "" {
		t.Error("Print() should produce output")
	}
	// Basic checks that key elements are present
	if !bytes.Contains(buf.Bytes(), []byte("TestCheck")) {
		t.Error("Output should contain check name")
	}
	// New summary format: "✓ N passed  ⚠ N warnings  ✖ N failed"
	if !bytes.Contains(buf.Bytes(), []byte("1 passed")) {
		t.Error("Output should contain summary with passed count")
	}
	if !bytes.Contains(buf.Bytes(), []byte("1 warnings")) {
		t.Error("Output should contain summary with warnings count")
	}
}

func TestReport_Print_SkippedSection(t *testing.T) {
	t.Parallel()
	r := NewReport()
	r.Add(&CheckResult{
		Name:    "OKCheck",
		Status:  StatusOK,
		Message: "All good",
	})
	r.Add(&CheckResult{
		Name:    "SkippedCheck",
		Status:  StatusSkipped,
		Message: "Could not determine",
	})

	var buf bytes.Buffer
	r.Print(&buf, false, 0)

	output := buf.String()
	if !bytes.Contains(buf.Bytes(), []byte("SKIPPED")) {
		t.Error("Output should contain a SKIPPED section for a skipped check")
	}
	if !bytes.Contains(buf.Bytes(), []byte("1 skipped")) {
		t.Error("Output should contain summary with skipped count")
	}
	// A skipped check has not proven the workspace healthy, so the report
	// must not claim everything passed (gt-whvu).
	if bytes.Contains(buf.Bytes(), []byte("All checks passed")) {
		t.Errorf("Output should not claim all checks passed when a check was skipped, got: %s", output)
	}
}

func TestNewDoctor(t *testing.T) {
	t.Parallel()
	d := NewDoctor()
	if d == nil {
		t.Fatal("NewDoctor() returned nil")
	}
	if len(d.Checks()) != 0 {
		t.Error("NewDoctor() should have no checks registered")
	}
}

func TestDoctor_Register(t *testing.T) {
	t.Parallel()
	d := NewDoctor()

	check1 := newMockCheck("check1", StatusOK)
	check2 := newMockCheck("check2", StatusOK)

	d.Register(check1)
	if len(d.Checks()) != 1 {
		t.Error("Register() should add one check")
	}

	d.Register(check2)
	if len(d.Checks()) != 2 {
		t.Error("Register() should add another check")
	}
}

func TestDoctor_RegisterAll(t *testing.T) {
	t.Parallel()
	d := NewDoctor()

	check1 := newMockCheck("check1", StatusOK)
	check2 := newMockCheck("check2", StatusOK)
	check3 := newMockCheck("check3", StatusOK)

	d.RegisterAll(check1, check2, check3)
	if len(d.Checks()) != 3 {
		t.Errorf("RegisterAll() should add 3 checks, got %d", len(d.Checks()))
	}
}

func TestDoctor_Run(t *testing.T) {
	t.Parallel()
	d := NewDoctor()
	d.Register(newMockCheck("ok", StatusOK))
	d.Register(newMockCheck("warn", StatusWarning))
	d.Register(newMockCheck("error", StatusError))

	ctx := &CheckContext{TownRoot: "/test"}
	report := d.Run(ctx)

	if report.Summary.Total != 3 {
		t.Errorf("Run() Total = %d, want 3", report.Summary.Total)
	}
	if report.Summary.OK != 1 {
		t.Errorf("Run() OK = %d, want 1", report.Summary.OK)
	}
	if report.Summary.Warnings != 1 {
		t.Errorf("Run() Warnings = %d, want 1", report.Summary.Warnings)
	}
	if report.Summary.Errors != 1 {
		t.Errorf("Run() Errors = %d, want 1", report.Summary.Errors)
	}
}

// TestDoctor_RunStreaming_SkippedIcon locks in the fix for gt-whvu: the
// streaming runner used by `gt doctor` must render a real icon for a skipped
// check, not an empty string (which misaligned the line and left placeholder
// remnants on screen).
func TestDoctor_RunStreaming_SkippedIcon(t *testing.T) {
	t.Parallel()
	d := NewDoctor()
	d.Register(newMockCheck("skipped", StatusSkipped))

	var buf bytes.Buffer
	ctx := &CheckContext{TownRoot: "/test"}
	d.RunStreaming(ctx, &buf, 0)

	// With no icon, the "\r  %s%s%s" format collapses to four bare spaces
	// before the name instead of icon + two-space gutter — that's the
	// misalignment this test guards against.
	if bytes.Contains(buf.Bytes(), []byte("\r    skipped")) {
		t.Errorf("Skipped check rendered with no status icon (misaligned line), got: %s", buf.String())
	}
	if !bytes.Contains(buf.Bytes(), []byte(ui.IconSkip)) {
		t.Errorf("Skipped check should render the skip icon, got: %s", buf.String())
	}
}

// fixRegisteredForTest repairs every registered check, one FixOne call at a
// time, and returns their results. It exists for tests of several fixers at
// once; production has no blanket path — `gt doctor fix <check>` names one.
func fixRegisteredForTest(t *testing.T, d *Doctor, ctx *CheckContext) *Report {
	t.Helper()
	report := NewReport()
	for _, check := range d.Checks() {
		result, _ := d.FixOne(ctx, check.Name(), nil, nil)
		if result != nil {
			report.Add(result)
		}
	}
	return report
}

func TestDoctor_FixOne(t *testing.T) {
	t.Parallel()
	d := NewDoctor()

	okCheck := newMockCheck("ok", StatusOK)
	d.Register(okCheck)

	fixableCheck := newMockCheck("fixable", StatusError)
	fixableCheck.fixable = true
	d.Register(fixableCheck)

	unfixableCheck := newMockCheck("unfixable", StatusError)
	unfixableCheck.fixable = false
	d.Register(unfixableCheck)

	ctx := &CheckContext{TownRoot: "/test"}

	// A passing check is already the goal: no repair, no error.
	result, err := d.FixOne(ctx, "ok", nil, nil)
	if err != nil {
		t.Fatalf("FixOne(ok) returned error: %v", err)
	}
	if result.Status != StatusOK {
		t.Errorf("ok check should stay OK, got %s", result.Status)
	}
	if okCheck.fixCount != 0 {
		t.Error("a passing check must not have Fix() called")
	}

	// A fixable problem is repaired and verified.
	result, err = d.FixOne(ctx, "fixable", nil, nil)
	if err != nil {
		t.Fatalf("FixOne(fixable) returned error: %v", err)
	}
	if fixableCheck.fixCount != 1 {
		t.Errorf("fixable check should have Fix() called once, got %d", fixableCheck.fixCount)
	}
	if result.Status != StatusOK || !result.Fixed {
		t.Errorf("fixable check should be OK and marked fixed, got status=%s fixed=%v", result.Status, result.Fixed)
	}

	// A report-only check is refused, not repaired.
	result, err = d.FixOne(ctx, "unfixable", nil, nil)
	if !errors.Is(err, ErrNotFixable) {
		t.Errorf("FixOne(unfixable) error = %v, want ErrNotFixable", err)
	}
	if unfixableCheck.fixCount != 0 {
		t.Error("unfixable check must not have Fix() called")
	}
	if result.Status != StatusError {
		t.Errorf("unfixable check should stay Error, got %s", result.Status)
	}
}

// TestDoctor_FixOne_RefusesUnknownResult locks in the fix for gt-whvu and the
// gt-fcxe9.1 rule: a check that could not determine a result is UNKNOWN, and a
// fixer must never be invoked on it even when the check is otherwise fixable.
func TestDoctor_FixOne_RefusesUnknownResult(t *testing.T) {
	t.Parallel()
	d := NewDoctor()

	skippedCheck := newMockCheck("skipped", StatusSkipped)
	skippedCheck.fixable = true
	d.Register(skippedCheck)

	ctx := &CheckContext{TownRoot: "/test"}
	result, err := d.FixOne(ctx, "skipped", nil, nil)
	if !errors.Is(err, ErrUnknownResult) {
		t.Errorf("FixOne(skipped) error = %v, want ErrUnknownResult", err)
	}
	if skippedCheck.fixCount != 0 {
		t.Error("Skipped check should not have Fix() called")
	}
	if result.Status != StatusSkipped {
		t.Errorf("Skipped check should remain Skipped, got %s", result.Status)
	}
	if result.Fixed {
		t.Error("Skipped check should not be marked Fixed")
	}
}

// TestDoctor_FixOne_UnknownCheck: the fixer names the check, so a typo is an
// error rather than a silent no-op.
func TestDoctor_FixOne_UnknownCheck(t *testing.T) {
	t.Parallel()
	d := NewDoctor()
	d.Register(newMockCheck("known", StatusOK))

	_, err := d.FixOne(&CheckContext{TownRoot: "/test"}, "nope", nil, nil)
	if !errors.Is(err, ErrUnknownCheck) {
		t.Errorf("FixOne(unknown) error = %v, want ErrUnknownCheck", err)
	}
}

// TestDoctor_FixOne_DestructiveNeedsAuthorization: a destructive check consults
// the authorize hook before its Fix runs, and a refusal leaves the check alone.
func TestDoctor_FixOne_DestructiveNeedsAuthorization(t *testing.T) {
	t.Parallel()
	d := NewDoctor()

	destructive := newDestructiveMockCheck("killer", StatusError)
	d.Register(destructive)

	ctx := &CheckContext{TownRoot: "/test"}
	refused := errors.New("no authorization")
	_, err := d.FixOne(ctx, "killer", nil, func(Check) error { return refused })
	if !errors.Is(err, refused) {
		t.Fatalf("FixOne(destructive) error = %v, want the authorizer's refusal", err)
	}
	if destructive.fixCount != 0 {
		t.Error("a refused destructive check must not have Fix() called")
	}

	// With authorization, the repair runs.
	result, err := d.FixOne(ctx, "killer", nil, func(Check) error { return nil })
	if err != nil {
		t.Fatalf("authorized FixOne(destructive) returned error: %v", err)
	}
	if destructive.fixCount != 1 {
		t.Errorf("authorized destructive check should have Fix() called once, got %d", destructive.fixCount)
	}
	if !result.Fixed {
		t.Error("authorized destructive repair should be marked fixed")
	}
}

// TestDoctor_FixOne_NonDestructiveSkipsAuthorizer: the authorizer is a
// destructive-only gate, so a benign repair never calls it.
func TestDoctor_FixOne_NonDestructiveSkipsAuthorizer(t *testing.T) {
	t.Parallel()
	d := NewDoctor()

	benign := newMockCheck("benign", StatusError)
	benign.fixable = true
	d.Register(benign)

	consulted := false
	_, err := d.FixOne(&CheckContext{TownRoot: "/test"}, "benign", nil, func(Check) error {
		consulted = true
		return errors.New("must not be called")
	})
	if err != nil {
		t.Fatalf("FixOne(benign) returned error: %v", err)
	}
	if consulted {
		t.Error("authorizer must not be consulted for a non-destructive check")
	}
}

func TestBaseCheck(t *testing.T) {
	t.Parallel()
	b := &BaseCheck{
		CheckName:        "test",
		CheckDescription: "Test description",
	}

	if b.Name() != "test" {
		t.Errorf("Name() = %q, want %q", b.Name(), "test")
	}
	if b.Description() != "Test description" {
		t.Errorf("Description() = %q, want %q", b.Description(), "Test description")
	}
	if b.CanFix() {
		t.Error("BaseCheck.CanFix() should return false")
	}
	if err := b.Fix(nil); err != ErrCannotFix {
		t.Errorf("BaseCheck.Fix() should return ErrCannotFix, got %v", err)
	}
}

// panicCheck is a test check whose Fix panics.
type panicCheck struct {
	FixableCheck
}

func (p *panicCheck) Run(ctx *CheckContext) *CheckResult {
	return &CheckResult{Name: p.CheckName, Status: StatusError, Message: "needs fix"}
}

func (p *panicCheck) Fix(ctx *CheckContext) error {
	panic("dolt nil pointer dereference")
}

func TestSafeFixCheck_RecoversPanic(t *testing.T) {
	t.Parallel()
	check := &panicCheck{
		FixableCheck: FixableCheck{
			BaseCheck: BaseCheck{
				CheckName:        "panic-check",
				CheckDescription: "A check that panics during fix",
			},
		},
	}

	err := safeFixCheck(check, &CheckContext{TownRoot: "/test"})
	if err == nil {
		t.Fatal("safeFixCheck should return error when Fix panics")
	}
	if !strings.Contains(err.Error(), "fix panicked") {
		t.Errorf("error should mention 'fix panicked', got: %v", err)
	}
	if !strings.Contains(err.Error(), "nil pointer") {
		t.Errorf("error should contain panic message, got: %v", err)
	}
}

func TestSafeFixCheck_NormalError(t *testing.T) {
	t.Parallel()
	check := newMockCheck("failing-fix", StatusError)
	check.fixable = true
	check.fixError = fmt.Errorf("some fix error")

	err := safeFixCheck(check, &CheckContext{TownRoot: "/test"})
	if err == nil {
		t.Fatal("safeFixCheck should return the Fix error")
	}
	if err.Error() != "some fix error" {
		t.Errorf("expected 'some fix error', got: %v", err)
	}
}

func TestSafeFixCheck_Success(t *testing.T) {
	t.Parallel()
	check := newMockCheck("good-fix", StatusError)
	check.fixable = true

	err := safeFixCheck(check, &CheckContext{TownRoot: "/test"})
	if err != nil {
		t.Fatalf("safeFixCheck should return nil on success, got: %v", err)
	}
}

func TestFixableCheck(t *testing.T) {
	t.Parallel()
	f := &FixableCheck{
		BaseCheck: BaseCheck{
			CheckName:        "fixable",
			CheckDescription: "Fixable check",
		},
	}

	if !f.CanFix() {
		t.Error("FixableCheck.CanFix() should return true")
	}
}

// TestPrintSummaryOnly_FixFailedDetailsAlwaysVisible verifies that "Fix
// failed:" details print even WITHOUT --verbose. Fix errors that only render
// under --verbose make a doctor repair look like a silent no-op and hide the
// real error (gt-8po).
func TestPrintSummaryOnly_FixFailedDetailsAlwaysVisible(t *testing.T) {
	t.Parallel()
	r := NewReport()
	r.Add(&CheckResult{
		Name:    "broken-check",
		Status:  StatusError,
		Message: "2 thing(s) missing",
		Details: []string{
			"thing-one",
			"Fix failed: creating thing-one: database not initialized",
		},
	})

	var buf bytes.Buffer
	r.PrintSummaryOnly(&buf, false, 0)
	out := buf.String()

	if !bytes.Contains(buf.Bytes(), []byte("Fix failed: creating thing-one: database not initialized")) {
		t.Errorf("non-verbose summary must show Fix failed details, got:\n%s", out)
	}
	if bytes.Contains(buf.Bytes(), []byte("thing-one\n")) && !bytes.Contains(buf.Bytes(), []byte("Fix failed")) {
		t.Errorf("unexpected output:\n%s", out)
	}

	// Ordinary details stay verbose-only.
	if idx := bytes.Index(buf.Bytes(), []byte("thing-one")); idx >= 0 {
		// "thing-one" appears inside the Fix failed line; ensure the bare
		// detail line was not printed separately.
		if bytes.Count(buf.Bytes(), []byte("thing-one")) != 1 {
			t.Errorf("bare detail should be hidden without verbose, got:\n%s", out)
		}
	}

	// Verbose shows both.
	buf.Reset()
	r.PrintSummaryOnly(&buf, true, 0)
	if bytes.Count(buf.Bytes(), []byte("thing-one")) < 2 {
		t.Errorf("verbose summary must show all details, got:\n%s", buf.String())
	}
}

// TestPrintSummaryOnly_VerboseDetailsLabeledByCheck verifies that --verbose
// prints each check's details under that check's name instead of one
// unlabeled block after the SKIPPED list (gt-l2alj).
func TestPrintSummaryOnly_VerboseDetailsLabeledByCheck(t *testing.T) {
	t.Parallel()
	r := NewReport()
	r.Add(&CheckResult{Name: "alpha-check", Status: StatusWarning, Message: "a", Details: []string{"alpha-detail"}})
	r.Add(&CheckResult{Name: "no-details", Status: StatusError, Message: "b"})
	r.Add(&CheckResult{Name: "beta-check", Status: StatusSkipped, Message: "c", Details: []string{"beta-detail-1", "beta-detail-2"}})
	r.Add(&CheckResult{Name: "ok-check", Status: StatusOK, Details: []string{"ok-detail"}})

	var buf bytes.Buffer
	r.PrintSummaryOnly(&buf, true, 0)
	out := buf.String()

	_, section, ok := strings.Cut(out, "DETAILS\n")
	if !ok {
		t.Fatalf("verbose summary has no DETAILS section:\n%s", out)
	}
	lines := strings.Split(strings.TrimRight(section, "\n"), "\n")
	var got []string
	for _, l := range lines {
		got = append(got, strings.TrimLeft(strings.TrimSpace(l), ui.TreeLast))
	}
	want := []string{"alpha-check", "alpha-detail", "beta-check", "beta-detail-1", "beta-detail-2"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("DETAILS section = %q, want %q\nfull output:\n%s", got, want, out)
	}

	// Non-verbose output has no DETAILS section.
	buf.Reset()
	r.PrintSummaryOnly(&buf, false, 0)
	if strings.Contains(buf.String(), "DETAILS") || strings.Contains(buf.String(), "alpha-detail") {
		t.Errorf("non-verbose summary changed:\n%s", buf.String())
	}
}

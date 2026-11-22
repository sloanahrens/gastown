package deployerinbox_test

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/clusterapproval"
	"github.com/steveyegge/gastown/internal/deployerinbox"
)

// handleRequestID is the id of the request each Handle test runs, in the shape
// ParseRequest accepts.
const handleRequestID = "20261119T161530Z-89abcdef"

// handleNow is the moment each Handle test passes, so the answer's time is
// exact.
var handleNow = time.Date(2026, 11, 19, 16, 15, 30, 0, time.UTC)

// handlePlan returns a valid plan: a real cluster, a 64-character lower-case
// hash and the phases in their only order.
func handlePlan() clusterapproval.Plan {
	return clusterapproval.Plan{
		Cluster:  clusterapproval.ClusterControl,
		SHA256:   strings.Repeat("a1", 32),
		Phases:   []string{"policy", "workloads"},
		Versions: map[string]string{"Deployment/control/gt": "7"},
	}
}

// handleToken is built from parts: no token-shaped literal in this file.
var handleToken = "AK" + "IA" + strings.Repeat("Q", 16)

// handleRecord returns the broker's approval record for plan, valid at
// handleNow.
func handleRecord(plan clusterapproval.Plan) clusterapproval.Record {
	return clusterapproval.Record{
		Act:        clusterapproval.ActApplyPlan,
		Cluster:    plan.Cluster,
		PlanSHA256: plan.SHA256,
		ProposalID: "0f0f0f0f0f0f",
		ApprovedAt: handleNow.Add(-time.Minute),
		ExpiresAt:  handleNow.Add(time.Minute),
	}
}

// fakeSteps is Steps in memory, recording the calls Handle made in order, what
// it handed Apply and the proposal it wrote, so a test sees the sequence and
// every step it skips.
type fakeSteps struct {
	plan      clusterapproval.Plan
	planErr   error
	writeErr  error
	live      map[string]string
	liveErr   error
	applyErr  error
	record    clusterapproval.Record
	recordErr error

	// afterLive runs at the end of LiveVersions, so a test can move the files or
	// the live versions between the verify and the apply.
	afterLive func()

	calls       []string
	proposal    []byte
	proposalID  string
	recordID    string
	appliedPlan clusterapproval.Plan
	appliedLive map[string]string
}

func (f *fakeSteps) Plan(_ context.Context, name string) (clusterapproval.Plan, error) {
	f.calls = append(f.calls, "Plan "+name)
	return f.plan, f.planErr
}

func (f *fakeSteps) WriteProposal(_ context.Context, name string, proposal []byte, id string) error {
	f.calls = append(f.calls, "WriteProposal "+name)
	f.proposal = append([]byte(nil), proposal...)
	f.proposalID = id
	return f.writeErr
}

func (f *fakeSteps) LiveVersions(_ context.Context, _ clusterapproval.Plan) (map[string]string, error) {
	f.calls = append(f.calls, "LiveVersions")
	live := maps.Clone(f.live)
	if f.afterLive != nil {
		f.afterLive()
	}
	return live, f.liveErr
}

// Apply keeps the contract of Steps.Apply: it re-derives the files' digest and
// re-reads the live versions, and refuses a moved one before applying anything.
func (f *fakeSteps) Apply(_ context.Context, _ string, verified clusterapproval.Plan, verifiedLive map[string]string) error {
	f.calls = append(f.calls, "Apply")
	f.appliedPlan = verified
	f.appliedLive = maps.Clone(verifiedLive)
	if f.plan.SHA256 != verified.SHA256 {
		return fmt.Errorf("the files' digest is no longer %s: %w", verified.SHA256, deployerinbox.ErrApplyRefused)
	}
	for _, key := range slices.Sorted(maps.Keys(verified.Versions)) {
		if f.live[key] != verifiedLive[key] {
			return fmt.Errorf("the live version of %q moved: %w", key, deployerinbox.ErrApplyRefused)
		}
	}
	return f.applyErr
}

func (f *fakeSteps) ReadRecord(_ context.Context, id string) (clusterapproval.Record, error) {
	f.calls = append(f.calls, "ReadRecord "+id)
	f.recordID = id
	return f.record, f.recordErr
}

func TestDeployerInboxHandlePlan(t *testing.T) {
	t.Parallel()

	t.Run("proposes a valid plan", func(t *testing.T) {
		t.Parallel()

		plan := handlePlan()
		wantProposal, wantID, err := clusterapproval.NewProposal(plan)
		if err != nil {
			t.Fatalf("NewProposal(%+v) = %v, want a valid plan to have a proposal", plan, err)
		}
		steps := &fakeSteps{plan: plan}
		req := deployerinbox.Request{ID: handleRequestID, Kind: deployerinbox.KindPlan, Plan: "2026-11-19-first"}

		answer := deployerinbox.Handle(context.Background(), steps, req, handleNow)

		if answer.ID != req.ID || !answer.At.Equal(handleNow) {
			t.Errorf("answer names %q at %s, want %q at %s", answer.ID, answer.At, req.ID, handleNow)
		}
		if answer.Status != deployerinbox.StatusProposed {
			t.Errorf("status = %q, want %q; detail %q", answer.Status, deployerinbox.StatusProposed, answer.Detail)
		}
		if answer.PlanSHA256 != plan.SHA256 {
			t.Errorf("plan_sha256 = %q, want the plan's full hash %q", answer.PlanSHA256, plan.SHA256)
		}
		wantCalls := []string{"Plan " + req.Plan, "WriteProposal " + req.Plan}
		if !slices.Equal(steps.calls, wantCalls) {
			t.Errorf("calls = %v, want %v: a plan applies nothing", steps.calls, wantCalls)
		}
		if string(steps.proposal) != string(wantProposal) {
			t.Errorf("wrote the proposal %q, want the canonical proposal %q", steps.proposal, wantProposal)
		}
		if steps.proposalID != wantID {
			t.Errorf("wrote the proposal id %q, want %q", steps.proposalID, wantID)
		}
	})

	t.Run("refuses an invalid plan and writes nothing", func(t *testing.T) {
		t.Parallel()

		badCluster := handlePlan()
		badCluster.Cluster = "staging"
		badPhases := handlePlan()
		badPhases.Phases = []string{"workloads", "policy"}

		cases := []struct {
			name string
			plan clusterapproval.Plan
		}{
			{name: "an invalid cluster", plan: badCluster},
			{name: "an invalid phase order", plan: badPhases},
		}
		for _, tc := range cases {
			tc := tc
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()

				_, _, err := clusterapproval.NewProposal(tc.plan)
				if err == nil {
					t.Fatalf("NewProposal(%+v) = nil error, want the plan refused", tc.plan)
				}
				steps := &fakeSteps{plan: tc.plan}
				req := deployerinbox.Request{ID: handleRequestID, Kind: deployerinbox.KindPlan, Plan: "p"}

				answer := deployerinbox.Handle(context.Background(), steps, req, handleNow)

				if answer.Status != deployerinbox.StatusRefused {
					t.Errorf("status = %q, want %q; detail %q", answer.Status, deployerinbox.StatusRefused, answer.Detail)
				}
				if answer.Detail != err.Error() {
					t.Errorf("detail = %q, want the reason %q", answer.Detail, err.Error())
				}
				if want := []string{"Plan p"}; !slices.Equal(steps.calls, want) {
					t.Errorf("calls = %v, want %v: an invalid plan writes nothing", steps.calls, want)
				}
				if steps.proposal != nil || steps.proposalID != "" {
					t.Errorf("wrote the proposal %q with id %q, want nothing written", steps.proposal, steps.proposalID)
				}
			})
		}
	})
}

func TestDeployerInboxHandleApply(t *testing.T) {
	t.Parallel()

	t.Run("applies the verified plan and live versions", func(t *testing.T) {
		t.Parallel()

		plan := handlePlan()
		_, id, err := clusterapproval.NewProposal(plan)
		if err != nil {
			t.Fatalf("NewProposal(%+v) = %v, want a valid plan to have a proposal", plan, err)
		}
		steps := &fakeSteps{plan: plan, live: maps.Clone(plan.Versions), record: handleRecord(plan)}
		req := deployerinbox.Request{ID: handleRequestID, Kind: deployerinbox.KindApply, Plan: "p"}

		answer := deployerinbox.Handle(context.Background(), steps, req, handleNow)

		if answer.ID != req.ID || !answer.At.Equal(handleNow) {
			t.Errorf("answer names %q at %s, want %q at %s", answer.ID, answer.At, req.ID, handleNow)
		}
		if answer.Status != deployerinbox.StatusApplied {
			t.Errorf("status = %q, want %q; detail %q", answer.Status, deployerinbox.StatusApplied, answer.Detail)
		}
		if answer.PlanSHA256 != plan.SHA256 {
			t.Errorf("plan_sha256 = %q, want the plan's full hash %q", answer.PlanSHA256, plan.SHA256)
		}
		wantCalls := []string{"Plan p", "ReadRecord " + id, "LiveVersions", "Apply"}
		if !slices.Equal(steps.calls, wantCalls) {
			t.Errorf("calls = %v, want %v: the plan is rendered once and only the verified plan is applied", steps.calls, wantCalls)
		}
		if steps.recordID != id {
			t.Errorf("read the record of proposal %q, want %q", steps.recordID, id)
		}
		if !reflect.DeepEqual(steps.appliedPlan, plan) {
			t.Errorf("Apply was handed the plan %+v, want the verified plan %+v", steps.appliedPlan, plan)
		}
		if !maps.Equal(steps.appliedLive, plan.Versions) {
			t.Errorf("Apply was handed the live versions %v, want the verified %v", steps.appliedLive, plan.Versions)
		}
	})

	t.Run("refuses a plan that is not a proposal", func(t *testing.T) {
		t.Parallel()

		badPlan := handlePlan()
		badPlan.Cluster = "staging"
		_, _, wantErr := clusterapproval.NewProposal(badPlan)
		if wantErr == nil {
			t.Fatalf("NewProposal(%+v) = nil error, want the plan refused", badPlan)
		}
		steps := &fakeSteps{plan: badPlan, live: maps.Clone(badPlan.Versions), record: handleRecord(badPlan)}
		req := deployerinbox.Request{ID: handleRequestID, Kind: deployerinbox.KindApply, Plan: "p"}

		answer := deployerinbox.Handle(context.Background(), steps, req, handleNow)

		if answer.Status != deployerinbox.StatusRefused {
			t.Errorf("status = %q, want %q; detail %q", answer.Status, deployerinbox.StatusRefused, answer.Detail)
		}
		if answer.Detail != wantErr.Error() {
			t.Errorf("detail = %q, want the reason %q", answer.Detail, wantErr.Error())
		}
		if want := []string{"Plan p"}; !slices.Equal(steps.calls, want) {
			t.Errorf("calls = %v, want %v: a plan without a proposal reads no record and applies nothing", steps.calls, want)
		}
		if steps.appliedPlan.SHA256 != "" || steps.appliedLive != nil {
			t.Errorf("Apply was handed %+v and %v, want nothing applied", steps.appliedPlan, steps.appliedLive)
		}
	})
}

func TestDeployerInboxHandlePlanChangesBetweenVerifyAndApply(t *testing.T) {
	t.Parallel()

	plan := handlePlan()
	changed := plan
	changed.SHA256 = strings.Repeat("b2", 32)
	steps := &fakeSteps{plan: plan, live: maps.Clone(plan.Versions), record: handleRecord(plan)}
	// The files change after LiveVersions, which Handle calls before Verify, so
	// Verify still checks the plan Handle rendered and Apply re-derives another.
	steps.afterLive = func() { steps.plan = changed }
	req := deployerinbox.Request{ID: handleRequestID, Kind: deployerinbox.KindApply, Plan: "p"}

	answer := deployerinbox.Handle(context.Background(), steps, req, handleNow)

	if answer.Status != deployerinbox.StatusRefused {
		t.Errorf("status = %q, want %q; detail %q", answer.Status, deployerinbox.StatusRefused, answer.Detail)
	}
	if answer.Detail != deployerinbox.ErrApplyRefused.Error() {
		t.Errorf("detail = %q, want the fixed refusal %q, not the callee's text", answer.Detail, deployerinbox.ErrApplyRefused.Error())
	}
	planCalls := 0
	for _, call := range steps.calls {
		if call == "Plan p" {
			planCalls++
		}
	}
	if planCalls != 1 {
		t.Errorf("Plan was called %d times in %v, want once: the plan applied is the plan verified", planCalls, steps.calls)
	}
	if !slices.Contains(steps.calls, "Apply") {
		t.Errorf("calls = %v, want Apply to have refused the moved files", steps.calls)
	}
	if !reflect.DeepEqual(steps.appliedPlan, plan) {
		t.Errorf("Apply was handed the plan %+v, want the verified plan %+v", steps.appliedPlan, plan)
	}
}

func TestDeployerInboxHandleApplyFails(t *testing.T) {
	t.Parallel()

	plan := handlePlan()
	_, id, err := clusterapproval.NewProposal(plan)
	if err != nil {
		t.Fatalf("NewProposal(%+v) = %v, want a valid plan to have a proposal", plan, err)
	}
	cases := []struct {
		name   string
		steps  *fakeSteps
		calls  []string
		detail string
		cause  string
	}{
		{
			name:   "the plan step errors",
			steps:  &fakeSteps{planErr: errors.New("read the plan directory")},
			calls:  []string{"Plan p"},
			detail: "the plan step failed",
			cause:  "read the plan directory",
		},
		{
			name:   "the record cannot be read",
			steps:  &fakeSteps{plan: plan, recordErr: errors.New("open the record")},
			calls:  []string{"Plan p", "ReadRecord " + id},
			detail: "reading the approval record failed",
			cause:  "open the record",
		},
		{
			name:   "the live versions cannot be read",
			steps:  &fakeSteps{plan: plan, record: handleRecord(plan), liveErr: errors.New("list the objects")},
			calls:  []string{"Plan p", "ReadRecord " + id, "LiveVersions"},
			detail: "reading the live versions failed",
			cause:  "list the objects",
		},
		{
			name:   "the apply fails",
			steps:  &fakeSteps{plan: plan, live: maps.Clone(plan.Versions), record: handleRecord(plan), applyErr: errors.New("kubectl: " + handleToken)},
			calls:  []string{"Plan p", "ReadRecord " + id, "LiveVersions", "Apply"},
			detail: "the apply failed",
			cause:  handleToken,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			req := deployerinbox.Request{ID: handleRequestID, Kind: deployerinbox.KindApply, Plan: "p"}

			answer := deployerinbox.Handle(context.Background(), tc.steps, req, handleNow)

			if answer.Status != deployerinbox.StatusFailed {
				t.Errorf("status = %q, want %q; detail %q", answer.Status, deployerinbox.StatusFailed, answer.Detail)
			}
			if answer.Detail != tc.detail || strings.Contains(answer.Detail, tc.cause) {
				t.Errorf("detail = %q, want the fixed text %q and none of the step's error", answer.Detail, tc.detail)
			}
			if !slices.Equal(tc.steps.calls, tc.calls) {
				t.Errorf("calls = %v, want %v: no step runs after a failure", tc.steps.calls, tc.calls)
			}
		})
	}
}

func TestDeployerInboxHandleRefuses(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		kind string
	}{
		{name: "the rollback kind, which is gt-iiccy3", kind: deployerinbox.KindRollback},
		{name: "an empty kind", kind: ""},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			steps := &fakeSteps{plan: handlePlan()}
			req := deployerinbox.Request{ID: handleRequestID, Kind: tc.kind, Plan: "p"}

			answer := deployerinbox.Handle(context.Background(), steps, req, handleNow)

			if answer.Status != deployerinbox.StatusRefused {
				t.Errorf("status = %q, want %q; detail %q", answer.Status, deployerinbox.StatusRefused, answer.Detail)
			}
			if want := fmt.Sprintf("the kind %q is not handled", tc.kind); answer.Detail != want {
				t.Errorf("detail = %q, want the reason %q", answer.Detail, want)
			}
			if len(steps.calls) != 0 {
				t.Errorf("calls = %v, want no step for an unhandled kind", steps.calls)
			}
			if steps.proposal != nil || steps.proposalID != "" {
				t.Errorf("wrote the proposal %q with id %q, want nothing written", steps.proposal, steps.proposalID)
			}
		})
	}

	plan := handlePlan()
	_, id, err := clusterapproval.NewProposal(plan)
	if err != nil {
		t.Fatalf("NewProposal(%+v) = %v, want a valid plan to have a proposal", plan, err)
	}
	mismatched := handleRecord(plan)
	mismatched.PlanSHA256 = strings.Repeat("0", len(plan.SHA256))
	expired := handleRecord(plan)
	expired.ExpiresAt = handleNow.Add(-time.Second)
	movedLive := maps.Clone(plan.Versions)
	movedLive["Deployment/control/gt"] = "8"
	applyCases := []struct {
		name   string
		steps  *fakeSteps
		calls  []string
		reason string
	}{
		{
			name:   "the record is missing",
			steps:  &fakeSteps{plan: plan, recordErr: deployerinbox.ErrNoApproval},
			calls:  []string{"Plan p", "ReadRecord " + id},
			reason: fmt.Sprintf("the plan %q is not approved yet: %s", "p", deployerinbox.ErrNoApproval),
		},
		{
			name:   "the record approves another digest",
			steps:  &fakeSteps{plan: plan, live: maps.Clone(plan.Versions), record: mismatched},
			calls:  []string{"Plan p", "ReadRecord " + id, "LiveVersions"},
			reason: refusedByVerify(t, mismatched, plan, plan.Versions),
		},
		{
			name:   "the record expired",
			steps:  &fakeSteps{plan: plan, live: maps.Clone(plan.Versions), record: expired},
			calls:  []string{"Plan p", "ReadRecord " + id, "LiveVersions"},
			reason: refusedByVerify(t, expired, plan, plan.Versions),
		},
		{
			name:   "a live version moved before the verify",
			steps:  &fakeSteps{plan: plan, live: movedLive, record: handleRecord(plan)},
			calls:  []string{"Plan p", "ReadRecord " + id, "LiveVersions"},
			reason: refusedByVerify(t, handleRecord(plan), plan, movedLive),
		},
	}
	for _, tc := range applyCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			req := deployerinbox.Request{ID: handleRequestID, Kind: deployerinbox.KindApply, Plan: "p"}

			answer := deployerinbox.Handle(context.Background(), tc.steps, req, handleNow)

			if answer.Status != deployerinbox.StatusRefused {
				t.Errorf("status = %q, want %q; detail %q", answer.Status, deployerinbox.StatusRefused, answer.Detail)
			}
			if answer.Detail != tc.reason {
				t.Errorf("detail = %q, want the reason %q", answer.Detail, tc.reason)
			}
			if !slices.Equal(tc.steps.calls, tc.calls) {
				t.Errorf("calls = %v, want %v: Apply is not reached", tc.steps.calls, tc.calls)
			}
			if tc.steps.appliedPlan.SHA256 != "" || tc.steps.appliedLive != nil {
				t.Errorf("Apply was handed %+v and %v, want nothing applied", tc.steps.appliedPlan, tc.steps.appliedLive)
			}
		})
	}
}

// refusedByVerify returns the reason Verify gives for rec, and fails the test
// unless Verify refuses it, so each row's reason is one Verify really gives.
func refusedByVerify(t *testing.T, rec clusterapproval.Record, plan clusterapproval.Plan, live map[string]string) string {
	t.Helper()
	err := clusterapproval.Verify(rec, plan, live, handleNow)
	if err == nil {
		t.Fatalf("Verify(%+v) = nil, want the record refused", rec)
	}
	return err.Error()
}

func TestDeployerInboxHandleFails(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		steps  *fakeSteps
		calls  []string
		detail string
		cause  string
	}{
		{
			name:   "the plan step errors",
			steps:  &fakeSteps{planErr: errors.New("read the plan directory")},
			calls:  []string{"Plan p"},
			detail: "the plan step failed",
			cause:  "read the plan directory",
		},
		{
			name:   "the proposal write errors",
			steps:  &fakeSteps{plan: handlePlan(), writeErr: errors.New("write the proposal file")},
			calls:  []string{"Plan p", "WriteProposal p"},
			detail: "writing the proposal failed",
			cause:  "write the proposal file",
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			req := deployerinbox.Request{ID: handleRequestID, Kind: deployerinbox.KindPlan, Plan: "p"}

			answer := deployerinbox.Handle(context.Background(), tc.steps, req, handleNow)

			if answer.Status != deployerinbox.StatusFailed {
				t.Errorf("status = %q, want %q; detail %q", answer.Status, deployerinbox.StatusFailed, answer.Detail)
			}
			if answer.Detail != tc.detail || strings.Contains(answer.Detail, tc.cause) {
				t.Errorf("detail = %q, want the fixed text %q and none of the step's error", answer.Detail, tc.detail)
			}
			if !slices.Equal(tc.steps.calls, tc.calls) {
				t.Errorf("calls = %v, want %v: no step runs after a failure", tc.steps.calls, tc.calls)
			}
		})
	}
}

// TestDeployerInboxHandleApplyRefusesAnUnverifiedPlan: no record, or a record for another digest, is refused and Apply is not reached.
func TestDeployerInboxHandleApplyRefusesAnUnverifiedPlan(t *testing.T) {
	t.Parallel()
	plan := handlePlan()
	_, id, err := clusterapproval.NewProposal(plan)
	if err != nil {
		t.Fatalf("NewProposal(%+v) = %v, want a valid plan to have a proposal", plan, err)
	}
	other := handleRecord(plan)
	other.PlanSHA256 = strings.Repeat("0", len(plan.SHA256))
	cases := []struct {
		name  string
		steps *fakeSteps
		calls []string
	}{
		{"no record is written", &fakeSteps{plan: plan, recordErr: deployerinbox.ErrNoApproval}, []string{"Plan p", "ReadRecord " + id}},
		{"no record is written, the callee wraps its path and a token", &fakeSteps{plan: plan, recordErr: fmt.Errorf("%w: reading /secret/path token=%s", deployerinbox.ErrNoApproval, handleToken)}, []string{"Plan p", "ReadRecord " + id}},
		{"the record approves another digest", &fakeSteps{plan: plan, live: maps.Clone(plan.Versions), record: other}, []string{"Plan p", "ReadRecord " + id, "LiveVersions"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req := deployerinbox.Request{ID: handleRequestID, Kind: deployerinbox.KindApply, Plan: "p"}
			answer := deployerinbox.Handle(context.Background(), tc.steps, req, handleNow)
			if answer.Status != deployerinbox.StatusRefused {
				t.Errorf("status = %q, want %q; detail %q", answer.Status, deployerinbox.StatusRefused, answer.Detail)
			}
			if !slices.Equal(tc.steps.calls, tc.calls) {
				t.Errorf("calls = %v, want %v: Apply is not reached", tc.steps.calls, tc.calls)
			}
			if strings.Contains(answer.Detail, "/secret/path") || strings.Contains(answer.Detail, handleToken) {
				t.Errorf("detail = %q, want none of the callee's path or token", answer.Detail)
			}
		})
	}
}

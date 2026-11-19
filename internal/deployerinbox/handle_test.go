package deployerinbox_test

import (
	"context"
	"errors"
	"fmt"
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

// fakeSteps is Steps in memory, recording the calls Handle made in order and
// the proposal it wrote, so a test sees the sequence and every step it skips.
type fakeSteps struct {
	plan      clusterapproval.Plan
	planErr   error
	writeErr  error
	live      map[string]string
	liveErr   error
	applyErr  error
	record    clusterapproval.Record
	recordErr error

	calls      []string
	proposal   []byte
	proposalID string
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
	return f.live, f.liveErr
}

func (f *fakeSteps) Apply(_ context.Context, _ string, _ clusterapproval.Plan, _ map[string]string) error {
	f.calls = append(f.calls, "Apply")
	return f.applyErr
}

func (f *fakeSteps) ReadRecord(_ context.Context, id string) (clusterapproval.Record, error) {
	f.calls = append(f.calls, "ReadRecord "+id)
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

func TestDeployerInboxHandleRefuses(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		kind string
	}{
		{name: "the apply kind, which is gt-sj8wdq", kind: deployerinbox.KindApply},
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
}

func TestDeployerInboxHandleFails(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		steps  *fakeSteps
		calls  []string
		reason string
	}{
		{
			name:   "the plan step errors",
			steps:  &fakeSteps{planErr: errors.New("read the plan directory")},
			calls:  []string{"Plan p"},
			reason: "read the plan directory",
		},
		{
			name:   "the proposal write errors",
			steps:  &fakeSteps{plan: handlePlan(), writeErr: errors.New("write the proposal file")},
			calls:  []string{"Plan p", "WriteProposal p"},
			reason: "write the proposal file",
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
			if !strings.Contains(answer.Detail, tc.reason) {
				t.Errorf("detail = %q, want it to name the reason %q", answer.Detail, tc.reason)
			}
			if !slices.Equal(tc.steps.calls, tc.calls) {
				t.Errorf("calls = %v, want %v: no step runs after a failure", tc.steps.calls, tc.calls)
			}
		})
	}
}

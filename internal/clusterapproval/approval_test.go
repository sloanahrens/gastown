package clusterapproval

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

func TestMain(m *testing.M) {
	os.Exit(unittier.Main(m))
}

var goodSHA = strings.Repeat("a1", 32)

const (
	approvedText = "2026-11-18T12:00:00Z"
	expiresText  = "2026-11-18T18:00:00Z"
	goodID       = "0123456789ab"
)

// recordText is a JSON object of the given keys. A string value is quoted and
// any other value is encoded as JSON, so a row can pass null; keys are sorted
// so the body does not depend on map iteration.
func recordText(fields map[string]any) string {
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var body strings.Builder
	body.WriteByte('{')
	for i, key := range keys {
		if i > 0 {
			body.WriteByte(',')
		}
		value, _ := json.Marshal(fields[key])
		fmt.Fprintf(&body, "%q:%s", key, value)
	}
	body.WriteByte('}')
	return body.String()
}

// duplicatedKey is the good record with key written twice, which a Go map
// cannot express.
func duplicatedKey(key string) string {
	fields := goodRecord()
	value, err := json.Marshal(fields[key])
	if err != nil {
		panic(err)
	}
	return fmt.Sprintf("{%q:%s,%s", key, value, recordText(fields)[1:])
}

// goodRecord is the fields of a record ParseRecord accepts; each refusal row
// changes or drops one.
func goodRecord() map[string]any {
	return map[string]any{
		"act":         ActApplyPlan,
		"cluster":     ClusterControl,
		"plan_sha256": goodSHA,
		"proposal_id": goodID,
		"approved_at": approvedText,
		"expires_at":  expiresText,
	}
}

func with(fields map[string]any, changes map[string]any) map[string]any {
	out := maps.Clone(fields)
	for key, value := range changes {
		out[key] = value
	}
	return out
}

func without(fields map[string]any, keys ...string) map[string]any {
	out := maps.Clone(fields)
	for _, key := range keys {
		delete(out, key)
	}
	return out
}

func TestClusterApprovalParse(t *testing.T) {
	t.Parallel()
	approved := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	accepted := Record{
		Act:        ActApplyPlan,
		Cluster:    ClusterControl,
		PlanSHA256: goodSHA,
		ProposalID: goodID,
		ApprovedAt: approved,
		ExpiresAt:  approved.Add(6 * time.Hour),
	}
	longest := accepted
	longest.ExpiresAt = approved.Add(MaxValidity)

	tests := []struct {
		name    string
		body    string
		want    *Record
		wantErr string
	}{
		{"good", recordText(goodRecord()), &accepted, ""},
		{"good at the longest window", recordText(with(goodRecord(), map[string]any{"expires_at": "2026-11-19T12:00:00Z"})), &longest, ""},
		{"unknown key", recordText(with(goodRecord(), map[string]any{"extra": "1"})), nil, "unknown key"},
		{"act key in the wrong case", recordText(with(goodRecord(), map[string]any{"Act": ActApplyPlan})), nil, "unknown key"},
		{"sha key in the wrong case", recordText(with(goodRecord(), map[string]any{"PLAN_SHA256": goodSHA})), nil, "unknown key"},
		{"duplicate act key", duplicatedKey("act"), nil, "twice"},
		{"missing act", recordText(without(goodRecord(), "act")), nil, "has no act"},
		{"empty act", recordText(with(goodRecord(), map[string]any{"act": ""})), nil, "has no act"},
		{"null act", recordText(with(goodRecord(), map[string]any{"act": nil})), nil, "has no act"},
		{"act is not a string", recordText(with(goodRecord(), map[string]any{"act": 5})), nil, "not a JSON string"},
		{"cluster is not a string", recordText(with(goodRecord(), map[string]any{"cluster": 5})), nil, "cluster is not a JSON string"},
		{"sha is not a string", recordText(with(goodRecord(), map[string]any{"plan_sha256": 5})), nil, "plan_sha256 is not a JSON string"},
		{"proposal_id is not a string", recordText(with(goodRecord(), map[string]any{"proposal_id": 5})), nil, "proposal_id is not a JSON string"},
		{"empty sha", recordText(with(goodRecord(), map[string]any{"plan_sha256": ""})), nil, "plan_sha256"},
		{"short sha", recordText(with(goodRecord(), map[string]any{"plan_sha256": goodSHA[:63]})), nil, "plan_sha256"},
		{"upper-case sha", recordText(with(goodRecord(), map[string]any{"plan_sha256": strings.ToUpper(goodSHA)})), nil, "plan_sha256"},
		{"missing cluster", recordText(without(goodRecord(), "cluster")), nil, "cluster"},
		{"null cluster", recordText(with(goodRecord(), map[string]any{"cluster": nil})), nil, "cluster"},
		{"unknown cluster", recordText(with(goodRecord(), map[string]any{"cluster": "production"})), nil, "cluster"},
		{"missing proposal_id", recordText(without(goodRecord(), "proposal_id")), nil, "has no proposal_id"},
		{"empty proposal_id", recordText(with(goodRecord(), map[string]any{"proposal_id": ""})), nil, "has no proposal_id"},
		{"null proposal_id", recordText(with(goodRecord(), map[string]any{"proposal_id": nil})), nil, "has no proposal_id"},
		{"missing approved_at", recordText(without(goodRecord(), "approved_at")), nil, "no approved_at"},
		{"null approved_at", recordText(with(goodRecord(), map[string]any{"approved_at": nil})), nil, "no approved_at"},
		{"approved_at is not a time", recordText(with(goodRecord(), map[string]any{"approved_at": "yesterday"})), nil, "not an RFC 3339 time"},
		{"expires_at is not a time", recordText(with(goodRecord(), map[string]any{"expires_at": "yesterday"})), nil, "not an RFC 3339 time"},
		{"missing expires_at", recordText(without(goodRecord(), "expires_at")), nil, "no expires_at"},
		{"non-UTC approved_at", recordText(with(goodRecord(), map[string]any{"approved_at": "2026-11-18T13:00:00+01:00"})), nil, "approved_at is not in UTC"},
		{"non-UTC expires_at", recordText(with(goodRecord(), map[string]any{"expires_at": "2026-11-18T11:00:00-07:00"})), nil, "expires_at is not in UTC"},
		{"zero window", recordText(with(goodRecord(), map[string]any{"expires_at": approvedText})), nil, "not after approved_at"},
		{"negative window", recordText(with(goodRecord(), map[string]any{"expires_at": "2026-11-18T11:00:00Z"})), nil, "not after approved_at"},
		{"window over the longest", recordText(with(goodRecord(), map[string]any{"expires_at": "2026-11-19T12:00:01Z"})), nil, "more than"},
		{"not an object", "[]", nil, "not one JSON object"},
		{"empty body", "", nil, "not one JSON object"},
		{"truncated key", `{"act`, nil, "read the approval record"},
		{"truncated value", `{"act":`, nil, "read the value"},
		{"no closing brace", `{"act":"apply-plan"`, nil, "read the approval record"},
		{"trailing data", recordText(goodRecord()) + "{}", nil, "trailing data"},
		{"trailing garbage", recordText(goodRecord()) + "xx", nil, "trailing data"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseRecord([]byte(tt.body))
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("ParseRecord accepted a record it must refuse: %+v", got)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %q, want it to name %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseRecord: %v", err)
			}
			if !sameRecord(got, *tt.want) {
				t.Errorf("record = %+v, want %+v", got, *tt.want)
			}
		})
	}
}

// sameRecord reports whether a and b name the same approval in the same UTC
// form.
func sameRecord(a, b Record) bool {
	return a.Act == b.Act && a.Cluster == b.Cluster && a.PlanSHA256 == b.PlanSHA256 &&
		a.ProposalID == b.ProposalID && a.ApprovedAt.Equal(b.ApprovedAt) &&
		a.ExpiresAt.Equal(b.ExpiresAt) &&
		a.ApprovedAt.Location() == time.UTC && a.ExpiresAt.Location() == time.UTC
}

func TestClusterApprovalProposal(t *testing.T) {
	t.Parallel()
	plan := func(versions map[string]string) Plan {
		return Plan{
			Cluster:  ClusterControl,
			SHA256:   goodSHA,
			Phases:   []string{"policy", "workloads"},
			Versions: versions,
		}
	}
	body, id, err := NewProposal(plan(map[string]string{
		"apps/gt/deployer": "100",
		"batch/gt/nightly": "200",
	}))
	if err != nil {
		t.Fatalf("NewProposal: %v", err)
	}

	wantBody := `{"act":"apply-plan","cluster":"control","plan_sha256":"` + goodSHA + `","phases":["policy","workloads"],"versions":{"apps/gt/deployer":"100","batch/gt/nightly":"200"}}`
	if string(body) != wantBody {
		t.Fatalf("proposal = %s, want %s", body, wantBody)
	}
	sum := sha256.Sum256(body)
	if wantID := hex.EncodeToString(sum[:])[:12]; id != wantID {
		t.Fatalf("id = %q, want %q", id, wantID)
	}

	// The same plan built by filling its Versions map in the other order has
	// the same bytes and id.
	reversed := map[string]string{}
	reversed["batch/gt/nightly"] = "200"
	reversed["apps/gt/deployer"] = "100"
	reversedBody, reversedID, err := NewProposal(plan(reversed))
	if err != nil {
		t.Fatalf("NewProposal: %v", err)
	}
	if !bytes.Equal(body, reversedBody) || id != reversedID {
		t.Errorf("the same plan in another map order gave %s id %q, want %s id %q", reversedBody, reversedID, body, id)
	}

	// Moving one live version changes both the bytes and the id.
	movedBody, movedID, err := NewProposal(plan(map[string]string{
		"apps/gt/deployer": "101",
		"batch/gt/nightly": "200",
	}))
	if err != nil {
		t.Fatalf("NewProposal: %v", err)
	}
	if bytes.Equal(body, movedBody) || id == movedID {
		t.Errorf("a moved version gave %s id %q, want bytes and id that differ from %s id %q", movedBody, movedID, body, id)
	}
}

func TestClusterApprovalProposalRefuses(t *testing.T) {
	t.Parallel()
	valid := Plan{
		Cluster: ClusterControl,
		SHA256:  goodSHA,
		Phases:  []string{"policy", "workloads"},
	}
	tests := []struct {
		name string
		plan Plan
	}{
		{"empty cluster", withPlan(valid, func(p *Plan) { p.Cluster = "" })},
		{"unknown cluster", withPlan(valid, func(p *Plan) { p.Cluster = "production" })},
		{"empty sha", withPlan(valid, func(p *Plan) { p.SHA256 = "" })},
		{"short sha", withPlan(valid, func(p *Plan) { p.SHA256 = goodSHA[:63] })},
		{"upper-case sha", withPlan(valid, func(p *Plan) { p.SHA256 = strings.ToUpper(goodSHA) })},
		{"nil phases", withPlan(valid, func(p *Plan) { p.Phases = nil })},
		{"empty phases", withPlan(valid, func(p *Plan) { p.Phases = []string{} })},
		{"unknown phase", withPlan(valid, func(p *Plan) { p.Phases = []string{"secrets"} })},
		{"phases out of order", withPlan(valid, func(p *Plan) { p.Phases = []string{"workloads", "policy"} })},
		{"duplicate phase", withPlan(valid, func(p *Plan) { p.Phases = []string{"policy", "policy"} })},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body, id, err := NewProposal(tt.plan)
			if err == nil {
				t.Fatalf("NewProposal accepted an invalid plan and returned %s", body)
			}
			if body != nil || id != "" {
				t.Errorf("NewProposal returned bytes %s and id %q with its error, want neither", body, id)
			}
		})
	}
}

// withPlan copies p and applies change to the copy, so a row cannot leak its
// edit into another.
func withPlan(p Plan, change func(*Plan)) Plan {
	change(&p)
	return p
}

func TestClusterApprovalProposalEmpty(t *testing.T) {
	t.Parallel()
	body, _, err := NewProposal(Plan{Cluster: ClusterGTSbx, SHA256: goodSHA, Phases: []string{"workloads"}})
	if err != nil {
		t.Fatalf("NewProposal: %v", err)
	}
	wantBody := `{"act":"apply-plan","cluster":"gt-sbx","plan_sha256":"` + goodSHA + `","phases":["workloads"],"versions":{}}`
	if string(body) != wantBody {
		t.Errorf("proposal = %s, want %s", body, wantBody)
	}
}

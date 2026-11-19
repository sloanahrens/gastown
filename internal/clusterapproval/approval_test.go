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
		{"unknown key", recordText(with(goodRecord(), map[string]any{"extra": "1"})), nil, "unknown field"},
		{"short sha", recordText(with(goodRecord(), map[string]any{"plan_sha256": goodSHA[:63]})), nil, "plan_sha256"},
		{"upper-case sha", recordText(with(goodRecord(), map[string]any{"plan_sha256": strings.ToUpper(goodSHA)})), nil, "plan_sha256"},
		{"empty sha", recordText(with(goodRecord(), map[string]any{"plan_sha256": ""})), nil, "plan_sha256"},
		{"missing cluster", recordText(without(goodRecord(), "cluster")), nil, "cluster"},
		{"unknown cluster", recordText(with(goodRecord(), map[string]any{"cluster": "production"})), nil, "cluster"},
		{"missing approved_at", recordText(without(goodRecord(), "approved_at")), nil, "no approved_at"},
		{"null approved_at", recordText(with(goodRecord(), map[string]any{"approved_at": nil})), nil, "no approved_at"},
		{"missing expires_at", recordText(without(goodRecord(), "expires_at")), nil, "no expires_at"},
		{"non-UTC approved_at", recordText(with(goodRecord(), map[string]any{"approved_at": "2026-11-18T13:00:00+01:00"})), nil, "approved_at is not in UTC"},
		{"non-UTC expires_at", recordText(with(goodRecord(), map[string]any{"expires_at": "2026-11-18T11:00:00-07:00"})), nil, "expires_at is not in UTC"},
		{"window over the longest", recordText(with(goodRecord(), map[string]any{"expires_at": "2026-11-19T12:00:01Z"})), nil, "more than"},
		{"not an object", "[]", nil, "decode"},
		{"trailing data", recordText(goodRecord()) + "{}", nil, "trailing data"},
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
	body, id := NewProposal(plan(map[string]string{
		"apps/gt/deployer": "100",
		"batch/gt/nightly": "200",
	}))

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
	reversedBody, reversedID := NewProposal(plan(reversed))
	if !bytes.Equal(body, reversedBody) || id != reversedID {
		t.Errorf("the same plan in another map order gave %s id %q, want %s id %q", reversedBody, reversedID, body, id)
	}

	// Moving one live version changes both the bytes and the id.
	movedBody, movedID := NewProposal(plan(map[string]string{
		"apps/gt/deployer": "101",
		"batch/gt/nightly": "200",
	}))
	if bytes.Equal(body, movedBody) || id == movedID {
		t.Errorf("a moved version gave %s id %q, want bytes and id that differ from %s id %q", movedBody, movedID, body, id)
	}
}

func TestClusterApprovalProposalEmpty(t *testing.T) {
	t.Parallel()
	body, _ := NewProposal(Plan{Cluster: ClusterGTSbx, SHA256: goodSHA})
	wantBody := `{"act":"apply-plan","cluster":"gt-sbx","plan_sha256":"` + goodSHA + `","phases":[],"versions":{}}`
	if string(body) != wantBody {
		t.Errorf("proposal = %s, want %s", body, wantBody)
	}
}

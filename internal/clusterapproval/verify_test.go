package clusterapproval

import (
	"strings"
	"testing"
	"time"
)

// verifyNow is the fixed clock every Verify row reads.
var verifyNow = time.Date(2026, 11, 18, 13, 0, 0, 0, time.UTC)

const (
	planDeployerVersion = "v-plan-deployer-101"
	planNightlyVersion  = "v-plan-nightly-7"
	movedLiveVersion    = "v-live-deployer-202"
)

// verifyPlan is a plan of two objects that the matching record approves.
func verifyPlan() Plan {
	return Plan{
		Cluster: ClusterControl,
		SHA256:  goodSHA,
		Phases:  []string{"policy", "workloads"},
		Versions: map[string]string{
			"apps/gt/deployer": planDeployerVersion,
			"batch/gt/nightly": planNightlyVersion,
		},
	}
}

// verifyLive is the cluster as the plan expects it.
func verifyLive() map[string]string {
	return map[string]string{
		"apps/gt/deployer": planDeployerVersion,
		"batch/gt/nightly": planNightlyVersion,
	}
}

// verifyRecord is a record Verify accepts for verifyPlan at verifyNow; each
// refusal row changes or drops one thing.
func verifyRecord() Record {
	return Record{
		Act:        ActApplyPlan,
		Cluster:    ClusterControl,
		PlanSHA256: goodSHA,
		ProposalID: goodID,
		ApprovedAt: verifyNow.Add(-10 * time.Minute),
		ExpiresAt:  verifyNow.Add(DefaultValidity - 10*time.Minute),
	}
}

func TestClusterApprovalVerify(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		rec     Record
		plan    Plan
		live    map[string]string
		now     time.Time
		wantErr string
		wantKey string
		hidden  []string
	}{
		{name: "matching", rec: verifyRecord(), plan: verifyPlan(), live: verifyLive()},
		{
			name: "approved exactly now",
			rec:  withVerifyRecord(func(r *Record) { r.ApprovedAt = verifyNow }),
			plan: verifyPlan(), live: verifyLive(),
		},
		{
			name:    "wrong act",
			rec:     withVerifyRecord(func(r *Record) { r.Act = "rollback-plan" }),
			plan:    verifyPlan(),
			live:    verifyLive(),
			wantErr: "act",
		},
		{
			name:    "empty cluster",
			rec:     withVerifyRecord(func(r *Record) { r.Cluster = "" }),
			plan:    withVerifyPlan(func(p *Plan) { p.Cluster = "" }),
			live:    verifyLive(),
			wantErr: "cluster",
		},
		{
			name:    "unknown cluster",
			rec:     withVerifyRecord(func(r *Record) { r.Cluster = "production" }),
			plan:    withVerifyPlan(func(p *Plan) { p.Cluster = "production" }),
			live:    verifyLive(),
			wantErr: "cluster",
		},
		{
			// Both names are valid, but the record approves the other one.
			name:    "other cluster",
			rec:     withVerifyRecord(func(r *Record) { r.Cluster = ClusterGTSbx }),
			plan:    verifyPlan(),
			live:    verifyLive(),
			wantErr: `"gt-sbx"`,
		},
		{
			name:    "other plan",
			rec:     withVerifyRecord(func(r *Record) { r.PlanSHA256 = strings.Repeat("b2", 32) }),
			plan:    verifyPlan(),
			live:    verifyLive(),
			wantErr: "plan",
		},
		{
			name:    "expired",
			rec:     withVerifyRecord(func(r *Record) { r.ExpiresAt = verifyNow.Add(-time.Second) }),
			plan:    verifyPlan(),
			live:    verifyLive(),
			wantErr: "expired",
		},
		{
			name:    "at expiry",
			rec:     withVerifyRecord(func(r *Record) { r.ExpiresAt = verifyNow }),
			plan:    verifyPlan(),
			live:    verifyLive(),
			wantErr: "expired",
		},
		{
			name:    "not yet valid",
			rec:     withVerifyRecord(func(r *Record) { r.ApprovedAt = verifyNow.Add(time.Second) }),
			plan:    verifyPlan(),
			live:    verifyLive(),
			wantErr: "not valid until",
		},
		{
			name:    "moved live version",
			rec:     verifyRecord(),
			plan:    verifyPlan(),
			live:    withLive(map[string]string{"apps/gt/deployer": movedLiveVersion}),
			wantErr: "not the version the plan names",
			wantKey: "apps/gt/deployer",
			hidden:  []string{movedLiveVersion, planDeployerVersion},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			live := tt.live
			err := Verify(tt.rec, tt.plan, live, verifyNow)
			if tt.wantErr == "" && tt.wantKey == "" {
				if err != nil {
					t.Fatalf("Verify refused a record it must accept: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("Verify accepted a record it must refuse")
			}
			if tt.wantErr != "" && !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %q, want it to name %q", err, tt.wantErr)
			}
			if tt.wantKey != "" && !strings.Contains(err.Error(), tt.wantKey) {
				t.Errorf("error = %q, want it to name the object %q", err, tt.wantKey)
			}
			for _, secret := range tt.hidden {
				if strings.Contains(err.Error(), secret) {
					t.Errorf("error = %q, must not carry the value %q", err, secret)
				}
			}
		})
	}
}

func TestClusterApprovalVerifyKeys(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		live    map[string]string
		wantErr bool
		// wantText is the message only the absent-key branch emits, so a row
		// goes red if that branch alone is removed.
		wantText string
		wantKey  string
		hidden   []string
	}{
		{
			// The plan ignores an object it does not name: the apply changes it,
			// so its drift is not the approval's to refuse.
			name: "live has a key the plan lacks",
			live: withLive(map[string]string{
				"batch/gt/extra": "v-live-extra-9",
			}),
		},
		{
			name:     "key absent from live",
			live:     withoutLive("batch/gt/nightly"),
			wantErr:  true,
			wantText: "has no object",
			wantKey:  "batch/gt/nightly",
			hidden:   []string{planNightlyVersion, planDeployerVersion},
		},
		{
			// An empty listing is a refusal, never a match: every plan key is
			// absent and the first one in sorted order is named.
			name:     "no live versions at all",
			live:     nil,
			wantErr:  true,
			wantText: "has no object",
			wantKey:  "apps/gt/deployer",
			hidden:   []string{planNightlyVersion, planDeployerVersion},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := Verify(verifyRecord(), verifyPlan(), tt.live, verifyNow)
			if !tt.wantErr {
				if err != nil {
					t.Fatalf("Verify refused a record it must accept: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("Verify accepted a record it must refuse")
			}
			if tt.wantText != "" && !strings.Contains(err.Error(), tt.wantText) {
				t.Errorf("error = %q, want it to say %q", err, tt.wantText)
			}
			if !strings.Contains(err.Error(), tt.wantKey) {
				t.Errorf("error = %q, want it to name the object %q", err, tt.wantKey)
			}
			for _, secret := range tt.hidden {
				if strings.Contains(err.Error(), secret) {
					t.Errorf("error = %q, must not carry the value %q", err, secret)
				}
			}
		})
	}
}

func withVerifyRecord(change func(*Record)) Record {
	rec := verifyRecord()
	change(&rec)
	return rec
}

func withVerifyPlan(change func(*Plan)) Plan {
	p := verifyPlan()
	change(&p)
	return p
}

// withLive copies the live cluster and sets the versions in changes.
func withLive(changes map[string]string) map[string]string {
	live := verifyLive()
	for key, version := range changes {
		live[key] = version
	}
	return live
}

// withoutLive copies the live cluster and drops keys.
func withoutLive(keys ...string) map[string]string {
	live := verifyLive()
	for _, key := range keys {
		delete(live, key)
	}
	return live
}

package cmd

import (
	"io"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/clusterapproval"
)

// TestClusterApplyCommandRegistered holds the command to its place under
// townClusterCmd: apply, with the four string flags the apply takes.
func TestClusterApplyCommandRegistered(t *testing.T) {
	t.Parallel()
	commands := townClusterCmd.Commands()
	i := slices.IndexFunc(commands, func(c *cobra.Command) bool { return c.Name() == "apply" })
	if i < 0 {
		t.Fatalf("townClusterCmd has no apply command")
	}
	for _, name := range []string{"plan", "approval", "policy-kubeconfig", "apply-kubeconfig"} {
		flag := commands[i].Flags().Lookup(name)
		if flag == nil {
			t.Errorf("apply has no --%s flag", name)
			continue
		}
		if flag.Value.Type() != "string" {
			t.Errorf("apply --%s is a %s flag, want string", name, flag.Value.Type())
		}
	}
}

// TestClusterApplyCommandRefuses drives each refusal on a fresh command, so no
// case sets a flag on the registered one.
func TestClusterApplyCommandRefuses(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{name: "a missing --plan", args: []string{"--approval", "approval.json"}, want: `required flag(s) "plan" not set`},
		{name: "a missing --approval", args: []string{"--plan", "plan"}, want: `required flag(s) "approval" not set`},
		{name: "both required flags missing", args: []string{}, want: `required flag(s) "approval", "plan" not set`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cmd := newTownClusterApplyCmd()
			cmd.SetArgs(tc.args)
			cmd.SetOut(io.Discard)
			cmd.SetErr(io.Discard)
			err := cmd.Execute()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("apply %q = %v, want an error holding %q", tc.args, err, tc.want)
			}
		})
	}
}

// TestClusterApplyRecordVerifier holds the verifier the command gives the apply
// to the clock it is built with: a record for this plan inside its window
// authorizes it; an expired record and a record for the other cluster do not.
func TestClusterApplyRecordVerifier(t *testing.T) {
	t.Parallel()
	sha := strings.Repeat("a", 64)
	approvedAt := time.Date(2026, 11, 26, 8, 0, 0, 0, time.UTC)
	record := func(cluster string) clusterapproval.Record {
		return clusterapproval.Record{Act: clusterapproval.ActApplyPlan, Cluster: cluster, PlanSHA256: sha, ProposalID: "0123456789ab", ApprovedAt: approvedAt, ExpiresAt: approvedAt.Add(time.Hour)}
	}
	for _, tc := range []struct {
		name    string
		rec     clusterapproval.Record
		now     time.Time
		wantErr bool
	}{
		{name: "a record for this plan inside its window", rec: record(clusterapproval.ClusterControl), now: approvedAt.Add(30 * time.Minute)},
		{name: "an expired record", rec: record(clusterapproval.ClusterControl), now: approvedAt.Add(time.Hour), wantErr: true},
		{name: "a record for the other cluster", rec: record(clusterapproval.ClusterGTSbx), now: approvedAt.Add(30 * time.Minute), wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			verify := applyRecordVerifier(func() time.Time { return tc.now })
			if err := verify(tc.rec, planRecord{ID: sha}); (err != nil) != tc.wantErr {
				t.Errorf("verify = %v, want an error: %t", err, tc.wantErr)
			}
		})
	}
}

// TestClusterApplyCommandNoPlanRecord runs the command on a plan directory with
// no plan.json. The plan is read before anything else, so the error names
// nothing as applied.
func TestClusterApplyCommandNoPlanRecord(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cmd := newTownClusterApplyCmd()
	cmd.SetArgs([]string{"--plan", dir, "--approval", filepath.Join(dir, "approval.json"), "--policy-kubeconfig", "policy", "--apply-kubeconfig", "apply"})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "the plan record cannot be read, so nothing was applied") {
		t.Errorf("apply of a directory with no plan.json = %v, want an error saying the plan record cannot be read and nothing was applied", err)
	}
}

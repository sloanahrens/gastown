package refinery

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
)

// TestIssueToMRInfoCarriesSubmitter pins the bead-to-MRInfo half of the
// gt-arqw3 chain: the identity a review attributes its verdict to is the MR's
// attribution, and a crew or hand-cut submission — one with no polecat behind
// it — must not arrive at the reviewer with nothing.
func TestIssueToMRInfoCarriesSubmitter(t *testing.T) {
	tests := []struct {
		name              string
		description       string
		wantWorker        string
		wantSubmitterSeen string
	}{
		{
			name:              "crew submission names its submitter",
			description:       "branch: crew/sloan/convert-batch1\ntarget: main\nsource_issue: gt-x\nsubmitter: sloan\nrig: test-rig",
			wantWorker:        "",
			wantSubmitterSeen: "sloan",
		},
		{
			name:              "polecat submission carries both",
			description:       "branch: polecat/pearl/gt-x+abc\ntarget: main\nsource_issue: gt-x\nworker: pearl\nsubmitter: pearl\nrig: test-rig",
			wantWorker:        "pearl",
			wantSubmitterSeen: "pearl",
		},
		{
			// Every MR bead written before the field existed.
			name:              "worker alone answers as the attribution",
			description:       "branch: polecat/pearl/gt-x+abc\ntarget: main\nsource_issue: gt-x\nworker: pearl\nrig: test-rig",
			wantWorker:        "pearl",
			wantSubmitterSeen: "pearl",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			issue := &beads.Issue{ID: "mr-a", Description: tt.description}
			fields := beads.ParseMRFields(issue)
			if fields == nil {
				t.Fatal("ParseMRFields() = nil, want non-nil")
			}

			mr := issueToMRInfo(issue, fields)
			if mr.Worker != tt.wantWorker {
				t.Errorf("Worker = %q, want %q", mr.Worker, tt.wantWorker)
			}
			if mr.Submitter != tt.wantSubmitterSeen {
				t.Errorf("Submitter = %q, want %q", mr.Submitter, tt.wantSubmitterSeen)
			}
		})
	}
}

// TestReviewBatchCandidates_NoteCarriesSubmitter pins the other half: the
// verdict note written to refs/notes/om carries the MR's submitter as its
// worker, and the gate script is handed the same value. That field is what
// the quality-review plugin groups a window by, so a submit path that leaves
// it empty makes every review it produces unattributable — which is what the
// crew's `crew/` and topic branches did from 2026-09-28 (gt-arqw3).
func TestReviewBatchCandidates_NoteCarriesSubmitter(t *testing.T) {
	fakeBDForBatch(t)
	workDir, g, cleanup := testGitRepo(t)
	defer cleanup()

	createFeatureBranch(t, workDir, "docs-design", "design.md", "hello\n")
	// editorial.Run's rehearsal reads origin/<branch>, not the local branch.
	run(t, workDir, "git", "push", "origin", "docs-design")
	writeEditorialManifest(t, workDir)

	// The shape a crew submission produces: a submitter, no polecat branch.
	issue := batchMRIssue("mr-a", "docs-design", "main", "")
	issue.Description = beads.FormatMRFields(&beads.MRFields{
		Branch:      "docs-design",
		Target:      "main",
		SourceIssue: "gt-x",
		Submitter:   "sloan",
		Rig:         "test-rig",
	})

	e := newTestEngineer(t, workDir, g)
	e.config.Editorial = &config.EditorialConfig{Required: true, ReviewParallelism: 1}
	e.beads = beads.NewWithStore(workDir, newBatchReviewStore(issue))

	var seenWorker string
	e.editorialExec = func(_ context.Context, _ string, args []string, _ string) (string, int, error) {
		seenWorker = workerFromArgs(args)
		data, _ := json.Marshal(map[string]interface{}{"score": 0.8, "verdict": "approve"})
		if err := os.WriteFile(verdictPathFromArgs(args), data, 0644); err != nil {
			return "", 0, err
		}
		return "", 0, nil
	}

	mr := makeMR("mr-a", "docs-design", "main")
	mr.Submitter = "sloan"

	_, _, notes := e.reviewBatchCandidates(context.Background(), []*MRInfo{mr}, "main")
	if notes["mr-a"] == nil {
		t.Fatalf("no note recorded for mr-a (output:\n%s)", e.output)
	}
	if got := notes["mr-a"].Worker; got != "sloan" {
		t.Errorf("note worker = %q, want %q (the MR's submitter)", got, "sloan")
	}
	if seenWorker != "sloan" {
		t.Errorf("gate script --worker = %q, want %q", seenWorker, "sloan")
	}
}

// workerFromArgs reads the --worker value the gate script is invoked with.
func workerFromArgs(args []string) string {
	for i, a := range args {
		if a == "--worker" && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

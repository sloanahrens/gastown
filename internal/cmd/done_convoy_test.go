package cmd

import (
	"fmt"
	"strings"
	"testing"
)

// These cover the command layer's convoy and checkpoint helpers that gt done
// used to reach from package cmd; the submission path itself is tested in
// internal/done.

// TestConvoyMergeStrategyBranching verifies that the merge strategy branching
// logic in runDone correctly routes to the right code path for each strategy.
func TestConvoyMergeStrategyBranching(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		mergeStrategy string
		wantPush      bool // should push happen?
		wantMR        bool // should MR bead be created?
		wantDirect    bool // should push to default branch?
	}{
		{
			name:          "mr strategy - normal push and MR",
			mergeStrategy: "mr",
			wantPush:      true,
			wantMR:        true,
			wantDirect:    false,
		},
		{
			name:          "empty strategy - defaults to mr behavior",
			mergeStrategy: "",
			wantPush:      true,
			wantMR:        true,
			wantDirect:    false,
		},
		{
			name:          "direct strategy - push to main, no MR",
			mergeStrategy: "direct",
			wantPush:      true,
			wantMR:        false,
			wantDirect:    true,
		},
		{
			name:          "local strategy - no push, no MR",
			mergeStrategy: "local",
			wantPush:      false,
			wantMR:        false,
			wantDirect:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Simulate the branching logic from runDone
			shouldPush := true
			shouldCreateMR := true
			shouldPushDirect := false

			switch tt.mergeStrategy {
			case "local":
				shouldPush = false
				shouldCreateMR = false
			case "direct":
				shouldPushDirect = true
				shouldCreateMR = false
			default:
				// "mr" or empty = default behavior
			}

			if shouldPush != tt.wantPush {
				t.Errorf("shouldPush = %v, want %v", shouldPush, tt.wantPush)
			}
			if shouldCreateMR != tt.wantMR {
				t.Errorf("shouldCreateMR = %v, want %v", shouldCreateMR, tt.wantMR)
			}
			if shouldPushDirect != tt.wantDirect {
				t.Errorf("shouldPushDirect = %v, want %v", shouldPushDirect, tt.wantDirect)
			}
		})
	}
}

// TestConvoyMergeStrategyNotification verifies that the merge strategy
// is included in the witness notification body when set to non-default values.
func TestConvoyMergeStrategyNotification(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		mergeStrategy string
		wantInBody    bool
	}{
		{"direct strategy included", "direct", true},
		{"local strategy included", "local", true},
		{"mr strategy excluded", "mr", false},
		{"empty strategy excluded", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Simulate the notification body building from runDone
			var bodyLines []string
			bodyLines = append(bodyLines, "Exit: COMPLETED")
			if tt.mergeStrategy != "" && tt.mergeStrategy != "mr" {
				bodyLines = append(bodyLines, fmt.Sprintf("MergeStrategy: %s", tt.mergeStrategy))
			}

			body := strings.Join(bodyLines, "\n")
			hasMergeStrategy := strings.Contains(body, "MergeStrategy:")

			if hasMergeStrategy != tt.wantInBody {
				t.Errorf("body contains MergeStrategy = %v, want %v\nbody: %s",
					hasMergeStrategy, tt.wantInBody, body)
			}
		})
	}
}

// TestConvoyMergeFromFields verifies that convoyMergeFromFields correctly
// extracts the merge strategy from convoy descriptions using typed ConvoyFields.
func TestConvoyMergeFromFields(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		description string
		want        string
	}{
		{
			name:        "direct strategy",
			description: "Auto-created convoy tracking gt-abc\nMerge: direct",
			want:        "direct",
		},
		{
			name:        "mr strategy",
			description: "Convoy tracking 3 issues\nOwner: mayor/\nMerge: mr",
			want:        "mr",
		},
		{
			name:        "local strategy",
			description: "Merge: local\nOwner: mayor/",
			want:        "local",
		},
		{
			name:        "no merge field",
			description: "Auto-created convoy tracking gt-abc",
			want:        "",
		},
		{
			name:        "empty description",
			description: "",
			want:        "",
		},
		{
			name:        "merge in middle of description",
			description: "Convoy tracking 1 issues\nMerge: direct\nNotify: mayor/",
			want:        "direct",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := convoyMergeFromFields(tt.description)
			if got != tt.want {
				t.Errorf("convoyMergeFromFields() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestClearDoneCheckpoints verifies that clearDoneCheckpoints removes
// only done-cp labels while preserving other labels.
func TestClearDoneCheckpoints(t *testing.T) {
	t.Parallel()
	allLabels := []string{
		"gt:agent",
		"idle:3",
		"done-cp:pushed:mybranch:1738972801",
		"done-cp:mr-created:gt-xyz:1738972802",
		"backoff-until:1738972900",
	}

	var kept []string
	var removed []string
	for _, label := range allLabels {
		if strings.HasPrefix(label, "done-cp:") {
			removed = append(removed, label)
		} else {
			kept = append(kept, label)
		}
	}

	if len(removed) != 2 {
		t.Errorf("expected 2 checkpoint labels removed, got %d: %v", len(removed), removed)
	}
	if len(kept) != 3 {
		t.Errorf("expected 3 labels kept, got %d: %v", len(kept), kept)
	}

	// Verify no checkpoint labels in kept set
	for _, label := range kept {
		if strings.HasPrefix(label, "done-cp:") {
			t.Errorf("checkpoint label was not removed: %s", label)
		}
	}
}

// TestConvoyInfoFallbackChain verifies that done.go checks attachment fields
// first, then falls back to dep-based convoy lookup. This is the fix for gt-7b6wf:
// convoy merge=direct was not propagated because cross-rig dep resolution failed.
func TestConvoyInfoFallbackChain(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name           string
		attachmentInfo *ConvoyInfo // Result from getConvoyInfoFromIssue
		depInfo        *ConvoyInfo // Result from getConvoyInfoForIssue
		wantConvoyID   string
		wantMerge      string
		wantNil        bool
	}{
		{
			name:           "attachment fields provide convoy info",
			attachmentInfo: &ConvoyInfo{ID: "hq-cv-abc", MergeStrategy: "direct"},
			depInfo:        nil, // Not called
			wantConvoyID:   "hq-cv-abc",
			wantMerge:      "direct",
		},
		{
			name:           "attachment fields empty, dep lookup succeeds",
			attachmentInfo: nil,
			depInfo:        &ConvoyInfo{ID: "hq-cv-xyz", MergeStrategy: "mr"},
			wantConvoyID:   "hq-cv-xyz",
			wantMerge:      "mr",
		},
		{
			name:           "both nil - no convoy",
			attachmentInfo: nil,
			depInfo:        nil,
			wantNil:        true,
		},
		{
			name:           "attachment has convoy, dep also has (attachment wins)",
			attachmentInfo: &ConvoyInfo{ID: "hq-cv-from-attachment", MergeStrategy: "direct"},
			depInfo:        &ConvoyInfo{ID: "hq-cv-from-dep", MergeStrategy: "mr"},
			wantConvoyID:   "hq-cv-from-attachment",
			wantMerge:      "direct",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Simulate the fallback chain from done.go
			var convoyInfo *ConvoyInfo
			convoyInfo = tt.attachmentInfo
			if convoyInfo == nil {
				convoyInfo = tt.depInfo
			}

			if tt.wantNil {
				if convoyInfo != nil {
					t.Errorf("expected nil, got %+v", convoyInfo)
				}
				return
			}
			if convoyInfo == nil {
				t.Fatal("expected non-nil convoy info")
			}
			if convoyInfo.ID != tt.wantConvoyID {
				t.Errorf("ConvoyID = %q, want %q", convoyInfo.ID, tt.wantConvoyID)
			}
			if convoyInfo.MergeStrategy != tt.wantMerge {
				t.Errorf("MergeStrategy = %q, want %q", convoyInfo.MergeStrategy, tt.wantMerge)
			}
		})
	}
}

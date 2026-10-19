package beads

import (
	"strings"
	"testing"
)

// --- parseIntField (not covered in beads_test.go) ---

func TestParseIntField(t *testing.T) {
	tests := []struct {
		input   string
		want    int
		wantErr bool
	}{
		{"42", 42, false},
		{"0", 0, false},
		{"-1", -1, false},
		{"abc", 0, true},
		{"", 0, true},
		{"3.14", 3, false}, // Sscanf reads the integer part
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got, err := parseIntField(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("parseIntField(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("parseIntField(%q) = %d, want %d", tt.input, got, tt.want)
			}
		})
	}
}

// --- AttachmentFields Mode round-trip ---

func TestAttachmentFieldsModeRoundTrip(t *testing.T) {
	original := &AttachmentFields{
		AttachedMolecule: "gt-wisp-123",
		AttachedAt:       "2026-02-18T12:00:00Z",
		Mode:             "ralph",
	}

	formatted := FormatAttachmentFields(original)
	if !strings.Contains(formatted, "mode: ralph") {
		t.Errorf("FormatAttachmentFields missing mode field, got:\n%s", formatted)
	}

	issue := &Issue{Description: formatted}
	parsed := ParseAttachmentFields(issue)
	if parsed == nil {
		t.Fatal("round-trip parse returned nil")
	}
	if parsed.Mode != "ralph" {
		t.Errorf("Mode: got %q, want %q", parsed.Mode, "ralph")
	}
	if parsed.AttachedMolecule != "gt-wisp-123" {
		t.Errorf("AttachedMolecule: got %q, want %q", parsed.AttachedMolecule, "gt-wisp-123")
	}
}

func TestSetAttachmentFieldsPreservesMode(t *testing.T) {
	issue := &Issue{
		Description: "mode: ralph\nattached_molecule: gt-wisp-old\nSome other content",
	}
	fields := &AttachmentFields{
		AttachedMolecule: "gt-wisp-new",
		Mode:             "ralph",
	}
	newDesc := SetAttachmentFields(issue, fields)
	if !strings.Contains(newDesc, "mode: ralph") {
		t.Errorf("SetAttachmentFields lost mode field, got:\n%s", newDesc)
	}
	if !strings.Contains(newDesc, "attached_molecule: gt-wisp-new") {
		t.Errorf("SetAttachmentFields lost attached_molecule, got:\n%s", newDesc)
	}
	if !strings.Contains(newDesc, "Some other content") {
		t.Errorf("SetAttachmentFields lost non-attachment content, got:\n%s", newDesc)
	}
}

func TestAttachmentFormulaVarsRoundTrip(t *testing.T) {
	fields := &AttachmentFields{
		AttachedFormula: "mol-polecat-work",
		FormulaVars:     "feature=Bug to fix\nissue=gt-abc123\nbase_branch=main",
	}

	formatted := FormatAttachmentFields(fields)
	if !strings.Contains(formatted, `formula_vars: ["feature=Bug to fix","issue=gt-abc123","base_branch=main"]`) {
		t.Fatalf("formula_vars should use single-line JSON array, got:\n%s", formatted)
	}
	if strings.Contains(formatted, "\nissue=gt-abc123") {
		t.Fatalf("formula_vars leaked continuation lines:\n%s", formatted)
	}

	parsed := ParseAttachmentFields(&Issue{Description: formatted})
	if parsed == nil {
		t.Fatal("round-trip parse returned nil")
	}
	want := "feature=Bug to fix\nissue=gt-abc123\nbase_branch=main"
	if parsed.FormulaVars != want {
		t.Fatalf("FormulaVars = %q, want %q", parsed.FormulaVars, want)
	}
}

func TestAttachmentArgsMultilineRoundTrip(t *testing.T) {
	args := "Step 1: do X\nno_merge: true\nmode: ralph\n  \"quoted\" <b> & tail"
	formatted := FormatAttachmentFields(&AttachmentFields{AttachedFormula: "mol-polecat-work", AttachedArgs: args})
	if strings.Count(formatted, "\n") != 1 {
		t.Fatalf("attached_args leaked continuation lines:\n%s", formatted)
	}

	parsed := ParseAttachmentFields(&Issue{Description: formatted})
	if parsed == nil {
		t.Fatal("round-trip parse returned nil")
	}
	if parsed.AttachedArgs != args {
		t.Fatalf("AttachedArgs = %q, want %q", parsed.AttachedArgs, args)
	}
	if parsed.NoMerge || parsed.Mode != "" {
		t.Fatalf("continuation lines became fields: NoMerge=%v Mode=%q", parsed.NoMerge, parsed.Mode)
	}
}

func TestSetAttachmentFieldsReplacesMultilineArgs(t *testing.T) {
	first := SetAttachmentFields(&Issue{Description: "Body text"}, &AttachmentFields{
		AttachedMolecule: "gt-wisp-1",
		AttachedArgs:     "Step 1: do X\nStep 2: do Y\nno_merge: true",
	})
	second := SetAttachmentFields(&Issue{Description: first}, &AttachmentFields{
		AttachedMolecule: "gt-wisp-2",
		AttachedArgs:     "fresh args",
	})

	for _, stale := range []string{"Step 1", "Step 2", "no_merge", "gt-wisp-1"} {
		if strings.Contains(second, stale) {
			t.Fatalf("stale %q left in description:\n%s", stale, second)
		}
	}
	parsed := ParseAttachmentFields(&Issue{Description: second})
	if parsed == nil || parsed.AttachedArgs != "fresh args" || parsed.NoMerge {
		t.Fatalf("parsed = %+v, want AttachedArgs %q and NoMerge unset", parsed, "fresh args")
	}
	if !strings.Contains(second, "Body text") {
		t.Fatalf("prose lost:\n%s", second)
	}
}

func TestParseAttachmentArgsLegacyRawLine(t *testing.T) {
	for _, raw := range []string{
		"Fix the flaky test",
		`"half quoted and then more`,
		`"quoted" then text`,
		`{"json": "like"}`,
	} {
		parsed := ParseAttachmentFields(&Issue{Description: "attached_args: " + raw})
		if parsed == nil || parsed.AttachedArgs != raw {
			t.Errorf("legacy %q parsed to %+v", raw, parsed)
		}
	}
}

func TestAttachmentArgsSingleLineStaysRaw(t *testing.T) {
	formatted := FormatAttachmentFields(&AttachmentFields{AttachedArgs: "plain args"})
	if formatted != "attached_args: plain args" {
		t.Fatalf("single-line args should stay raw, got %q", formatted)
	}
	quoted := `"already" quoted`
	parsed := ParseAttachmentFields(&Issue{Description: FormatAttachmentFields(&AttachmentFields{AttachedArgs: quoted})})
	if parsed == nil || parsed.AttachedArgs != quoted {
		t.Fatalf("quote-leading args round trip = %+v, want %q", parsed, quoted)
	}
}

func TestParseAttachmentFieldsDoesNotConsumeAdjacentKeyValueLines(t *testing.T) {
	desc := "formula_vars: feature=Bug to fix\nissue=gt-abc123\nbase_branch=main\nexample=value\nmode: ralph"
	fields := ParseAttachmentFields(&Issue{Description: desc})
	if fields == nil {
		t.Fatal("ParseAttachmentFields returned nil")
	}
	want := "feature=Bug to fix"
	if fields.FormulaVars != want {
		t.Fatalf("FormulaVars = %q, want %q", fields.FormulaVars, want)
	}
	for _, adjacent := range []string{"issue=gt-abc123", "base_branch=main", "example=value"} {
		if strings.Contains(fields.FormulaVars, adjacent) {
			t.Fatalf("adjacent key=value line should not be parsed as formula var: %q", fields.FormulaVars)
		}
	}
	if fields.Mode != "ralph" {
		t.Fatalf("Mode = %q, want ralph", fields.Mode)
	}
}

func TestSetAttachmentFieldsPreservesAdjacentKeyValueLines(t *testing.T) {
	issue := &Issue{Description: "formula_vars: old=1\nissue=old\nbase_branch=old\nexample=value\n\nBody"}
	fields := &AttachmentFields{FormulaVars: "feature=New\nissue=gt-new"}

	newDesc := SetAttachmentFields(issue, fields)
	if !strings.Contains(newDesc, `formula_vars: ["feature=New","issue=gt-new"]`) {
		t.Fatalf("new formula_vars missing, got:\n%s", newDesc)
	}
	for _, adjacent := range []string{"issue=old", "base_branch=old", "example=value"} {
		if !strings.Contains(newDesc, adjacent) {
			t.Fatalf("adjacent key=value line %q should be preserved, got:\n%s", adjacent, newDesc)
		}
	}
	if !strings.Contains(newDesc, "Body") {
		t.Fatalf("prose should be preserved, got:\n%s", newDesc)
	}
}

// --- AgentFields Mode round-trip ---

func TestAgentFieldsModeRoundTrip(t *testing.T) {
	original := &AgentFields{
		RoleType:   "polecat",
		Rig:        "gastown",
		AgentState: "working",
		HookBead:   "gt-abc",
		Mode:       "ralph",
	}

	formatted := FormatAgentDescription("Polecat Test", original)
	if !strings.Contains(formatted, "mode: ralph") {
		t.Errorf("FormatAgentDescription missing mode field, got:\n%s", formatted)
	}

	parsed := ParseAgentFields(formatted)
	if parsed.Mode != "ralph" {
		t.Errorf("Mode: got %q, want %q", parsed.Mode, "ralph")
	}
	if parsed.RoleType != "polecat" {
		t.Errorf("RoleType: got %q, want %q", parsed.RoleType, "polecat")
	}
}

func TestAgentFieldsModeOmittedWhenEmpty(t *testing.T) {
	fields := &AgentFields{
		RoleType:   "polecat",
		Rig:        "gastown",
		AgentState: "working",
		// Mode intentionally empty
	}

	formatted := FormatAgentDescription("Polecat Test", fields)
	if strings.Contains(formatted, "mode:") {
		t.Errorf("FormatAgentDescription should not include mode when empty, got:\n%s", formatted)
	}
}

func TestMRFieldsSubmitter(t *testing.T) {
	tests := []struct {
		name          string
		description   string
		wantWorker    string
		wantSubmitter string
		wantAttrib    string
	}{
		{
			name:          "explicit submitter beside a polecat worker",
			description:   "branch: polecat/pearl/gt-x+abc\nworker: pearl\nsubmitter: pearl",
			wantWorker:    "pearl",
			wantSubmitter: "pearl",
			wantAttrib:    "pearl",
		},
		{
			name:          "crew submitter with no polecat worker",
			description:   "branch: crew/sloan/ci\nsubmitter: sloan",
			wantWorker:    "",
			wantSubmitter: "sloan",
			wantAttrib:    "sloan",
		},
		{
			// The shape every MR bead written before the field existed has.
			name:          "worker alone answers as the attribution",
			description:   "branch: polecat/pearl/gt-x+abc\nworker: pearl",
			wantWorker:    "pearl",
			wantSubmitter: "",
			wantAttrib:    "pearl",
		},
		{
			name:          "no identity at all stays empty",
			description:   "branch: docs/design",
			wantWorker:    "",
			wantSubmitter: "",
			wantAttrib:    "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fields := ParseMRFields(&Issue{Description: tt.description})
			if fields == nil {
				t.Fatal("ParseMRFields() = nil, want non-nil")
			}
			if fields.Worker != tt.wantWorker {
				t.Errorf("Worker = %q, want %q", fields.Worker, tt.wantWorker)
			}
			if fields.Submitter != tt.wantSubmitter {
				t.Errorf("Submitter = %q, want %q", fields.Submitter, tt.wantSubmitter)
			}
			if got := fields.Attribution(); got != tt.wantAttrib {
				t.Errorf("Attribution() = %q, want %q", got, tt.wantAttrib)
			}
		})
	}
}

func TestMRFieldsSubmitterRoundTrip(t *testing.T) {
	original := &MRFields{
		Branch:      "crew/sloan/ci",
		Target:      "main",
		SourceIssue: "gt-x",
		Submitter:   "sloan",
		Rig:         "gastown",
	}
	parsed := ParseMRFields(&Issue{Description: FormatMRFields(original)})
	if parsed == nil {
		t.Fatal("round-trip parse returned nil")
	}
	if parsed.Submitter != original.Submitter {
		t.Errorf("Submitter: got %q, want %q", parsed.Submitter, original.Submitter)
	}
	if parsed.Worker != "" {
		t.Errorf("Worker: got %q, want \"\" (a crew submit has no polecat branch)", parsed.Worker)
	}
}

func TestFormatMRFieldsOmitsEmptySubmitter(t *testing.T) {
	got := FormatMRFields(&MRFields{Branch: "docs/design", Target: "main"})
	if strings.Contains(got, "submitter") {
		t.Errorf("FormatMRFields should not include submitter when empty, got:\n%s", got)
	}
}

func TestParseAgentFields_AllFields(t *testing.T) {
	desc := "role_type: polecat\nrig: gastown\nagent_state: working\nhook_bead: gt-abc\ncleanup_status: clean\nactive_mr: gt-mr1\nlast_source_issue: gt-src\nnotification_level: verbose"
	got := ParseAgentFields(desc)
	if got.RoleType != "polecat" {
		t.Errorf("RoleType = %q, want %q", got.RoleType, "polecat")
	}
	if got.Rig != "gastown" {
		t.Errorf("Rig = %q, want %q", got.Rig, "gastown")
	}
	if got.AgentState != "working" {
		t.Errorf("AgentState = %q, want %q", got.AgentState, "working")
	}
	if got.HookBead != "gt-abc" {
		t.Errorf("HookBead = %q, want %q", got.HookBead, "gt-abc")
	}
	if got.CleanupStatus != "clean" {
		t.Errorf("CleanupStatus = %q, want %q", got.CleanupStatus, "clean")
	}
	if got.ActiveMR != "gt-mr1" {
		t.Errorf("ActiveMR = %q, want %q", got.ActiveMR, "gt-mr1")
	}
	if got.LastSourceIssue != "gt-src" {
		t.Errorf("LastSourceIssue = %q, want %q", got.LastSourceIssue, "gt-src")
	}
	if got.NotificationLevel != "verbose" {
		t.Errorf("NotificationLevel = %q, want %q", got.NotificationLevel, "verbose")
	}
}

// --- Completion metadata fields (gt-x7t9) ---

func TestAgentFieldsCompletionMetadataRoundTrip(t *testing.T) {
	original := &AgentFields{
		RoleType:        "polecat",
		Rig:             "gastown",
		AgentState:      "done",
		HookBead:        "gt-abc",
		ExitType:        "COMPLETED",
		MRID:            "gt-mr-xyz",
		Branch:          "polecat/nux/gt-abc@hash",
		LastSourceIssue: "gt-abc",
		MRFailed:        false,
		CompletionTime:  "2026-02-28T01:00:00Z",
	}

	formatted := FormatAgentDescription("Polecat nux", original)

	// Verify all completion fields are present
	if !strings.Contains(formatted, "exit_type: COMPLETED") {
		t.Errorf("missing exit_type in formatted output:\n%s", formatted)
	}
	if !strings.Contains(formatted, "mr_id: gt-mr-xyz") {
		t.Errorf("missing mr_id in formatted output:\n%s", formatted)
	}
	if !strings.Contains(formatted, "branch: polecat/nux/gt-abc@hash") {
		t.Errorf("missing branch in formatted output:\n%s", formatted)
	}
	if !strings.Contains(formatted, "last_source_issue: gt-abc") {
		t.Errorf("missing last_source_issue in formatted output:\n%s", formatted)
	}
	if !strings.Contains(formatted, "completion_time: 2026-02-28T01:00:00Z") {
		t.Errorf("missing completion_time in formatted output:\n%s", formatted)
	}
	// mr_failed=false should NOT appear
	if strings.Contains(formatted, "mr_failed") {
		t.Errorf("mr_failed should not appear when false:\n%s", formatted)
	}

	// Parse and verify round-trip
	parsed := ParseAgentFields(formatted)
	if parsed.ExitType != "COMPLETED" {
		t.Errorf("ExitType: got %q, want %q", parsed.ExitType, "COMPLETED")
	}
	if parsed.MRID != "gt-mr-xyz" {
		t.Errorf("MRID: got %q, want %q", parsed.MRID, "gt-mr-xyz")
	}
	if parsed.Branch != "polecat/nux/gt-abc@hash" {
		t.Errorf("Branch: got %q, want %q", parsed.Branch, "polecat/nux/gt-abc@hash")
	}
	if parsed.LastSourceIssue != "gt-abc" {
		t.Errorf("LastSourceIssue: got %q, want %q", parsed.LastSourceIssue, "gt-abc")
	}
	if parsed.MRFailed != false {
		t.Errorf("MRFailed: got %v, want false", parsed.MRFailed)
	}
	if parsed.CompletionTime != "2026-02-28T01:00:00Z" {
		t.Errorf("CompletionTime: got %q, want %q", parsed.CompletionTime, "2026-02-28T01:00:00Z")
	}
	// Verify non-completion fields survive
	if parsed.RoleType != "polecat" {
		t.Errorf("RoleType: got %q, want %q", parsed.RoleType, "polecat")
	}
	if parsed.HookBead != "gt-abc" {
		t.Errorf("HookBead: got %q, want %q", parsed.HookBead, "gt-abc")
	}
}

func TestAgentFieldsMRFailedTrue(t *testing.T) {
	fields := &AgentFields{
		RoleType:   "polecat",
		Rig:        "gastown",
		AgentState: "done",
		ExitType:   "COMPLETED",
		MRFailed:   true,
	}

	formatted := FormatAgentDescription("Polecat nux", fields)
	if !strings.Contains(formatted, "mr_failed: true") {
		t.Errorf("missing mr_failed: true in formatted output:\n%s", formatted)
	}

	parsed := ParseAgentFields(formatted)
	if !parsed.MRFailed {
		t.Errorf("MRFailed: got false, want true")
	}
}

func TestAgentFieldsCompletionOmittedWhenEmpty(t *testing.T) {
	fields := &AgentFields{
		RoleType:   "polecat",
		Rig:        "gastown",
		AgentState: "working",
		// All completion fields intentionally empty
	}

	formatted := FormatAgentDescription("Polecat nux", fields)
	for _, keyword := range []string{"exit_type:", "mr_id:", "branch:", "last_source_issue:", "mr_failed:", "completion_time:"} {
		if strings.Contains(formatted, keyword) {
			t.Errorf("empty completion field %q should not appear in output:\n%s", keyword, formatted)
		}
	}
}

func TestParseAgentFields_WithCompletionMetadata(t *testing.T) {
	desc := "role_type: polecat\nrig: gastown\nagent_state: done\nhook_bead: gt-abc\nexit_type: ESCALATED\nbranch: polecat/nux/gt-abc@hash\nlast_source_issue: gt-abc\nmr_failed: true\ncompletion_time: 2026-02-28T02:00:00Z"
	got := ParseAgentFields(desc)
	if got.ExitType != "ESCALATED" {
		t.Errorf("ExitType = %q, want %q", got.ExitType, "ESCALATED")
	}
	if got.Branch != "polecat/nux/gt-abc@hash" {
		t.Errorf("Branch = %q, want %q", got.Branch, "polecat/nux/gt-abc@hash")
	}
	if !got.MRFailed {
		t.Errorf("MRFailed = false, want true")
	}
	if got.LastSourceIssue != "gt-abc" {
		t.Errorf("LastSourceIssue = %q, want %q", got.LastSourceIssue, "gt-abc")
	}
	if got.CompletionTime != "2026-02-28T02:00:00Z" {
		t.Errorf("CompletionTime = %q, want %q", got.CompletionTime, "2026-02-28T02:00:00Z")
	}
	if got.MRID != "" {
		t.Errorf("MRID = %q, want empty (not in desc)", got.MRID)
	}
}

// TestStripAttachmentFields: the dispatcher's attachment lines go, a
// multi-line attached_args (one JSON string line) goes with its key, and the
// bead's own text stays (gt-dhc6o).
func TestStripAttachmentFields(t *testing.T) {
	block := FormatAttachmentFields(&AttachmentFields{
		AttachedMolecule: "gt-wisp-1",
		AttachedFormula:  "mol-polecat-work",
		AttachedAt:       "2026-10-16T03:33:52Z",
		AttachedArgs:     "line one\nno_merge: true\nline three",
		AttachedVars:     []string{"issue=gt-1", "feature=x"},
		DispatchedBy:     "daemon/spec-dispatch",
		FormulaVars:      "issue=gt-1\nfeature=x",
	})
	tests := []struct {
		name, desc, want string
	}{
		{"block then body", block + "\n\n## Goal\n\nDo it.", "## Goal\n\nDo it."},
		{"only attachment lines", block, ""},
		{"no attachment lines", "plain text\n\nmore", "plain text\n\nmore"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := StripAttachmentFields(tt.desc); got != tt.want {
				t.Errorf("StripAttachmentFields = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestAttachmentKeySpellings: every spelling of every attachment key is
// stripped by both entry points, and a near-miss key is kept.
func TestAttachmentKeySpellings(t *testing.T) {
	var keys []string
	for _, base := range []string{
		"attached_molecule", "attached_formula", "attached_at", "attached_args",
		"attached_vars", "dispatched_by", "no_merge", "review_only",
		"merge_strategy", "formula_vars",
	} {
		keys = append(keys, base, strings.ReplaceAll(base, "_", "-"), strings.ReplaceAll(base, "_", ""))
	}
	for _, key := range keys {
		desc := strings.ToUpper(key) + ": x\nbody"
		if got := StripAttachmentFields(desc); got != "body" {
			t.Errorf("StripAttachmentFields(%q) = %q, want %q", desc, got, "body")
		}
		if got := SetAttachmentFields(&Issue{Description: desc}, nil); got != "body" {
			t.Errorf("SetAttachmentFields(%q, nil) = %q, want %q", desc, got, "body")
		}
	}
	// mode opens ordinary prose lines: the lowercase key counts with any
	// value, other cases only with the value ralph.
	for _, desc := range []string{"mode: x\nbody", "mode: ralph\nbody", "Mode: ralph\nbody", "MODE: ralph\nbody", "MODE: Ralph\nbody"} {
		if got := StripAttachmentFields(desc); got != "body" {
			t.Errorf("StripAttachmentFields(%q) = %q, want %q", desc, got, "body")
		}
	}
	if got, keepMode := StripAttachmentFields("Mode: x\nbody"), "Mode: x\nbody"; got != keepMode {
		t.Errorf("StripAttachmentFields(prose Mode line) = %q, want it kept", got)
	}
	const keep = "attached_other: x\nbody"
	if got := StripAttachmentFields(keep); got != keep {
		t.Errorf("StripAttachmentFields(%q) = %q, want it kept", keep, got)
	}
}

// --- AgentFields Agent round-trip (gt-y6cja) ---

func TestAgentFieldsAgentRoundTrip(t *testing.T) {
	original := &AgentFields{
		RoleType:   "polecat",
		Rig:        "gastown",
		AgentState: "spawning",
		HookBead:   "gt-abc",
		Agent:      "claude-sonnet",
	}

	formatted := FormatAgentDescription("Polecat Test", original)
	if !strings.Contains(formatted, "agent: claude-sonnet") {
		t.Errorf("FormatAgentDescription missing agent line, got:\n%s", formatted)
	}
	parsed := ParseAgentFields(formatted)
	if parsed.Agent != "claude-sonnet" || parsed.HookBead != "gt-abc" {
		t.Errorf("parsed agent %q hook_bead %q, want claude-sonnet and gt-abc", parsed.Agent, parsed.HookBead)
	}
}

// An agent bead written before the field existed has no agent line; it parses
// to an empty Agent and formats back without one.
func TestAgentFieldsAgentAbsentOnOlderBeads(t *testing.T) {
	older := "Polecat Old\n\nrole_type: polecat\nrig: gastown\nagent_state: done\nhook_bead: null\n"
	if got := ParseAgentFields(older).Agent; got != "" {
		t.Errorf("Agent parsed from a bead with no agent line = %q, want empty", got)
	}
	formatted := FormatAgentDescription("Polecat Old", &AgentFields{RoleType: "polecat", Rig: "gastown", AgentState: "done"})
	if strings.Contains(formatted, "agent:") {
		t.Errorf("FormatAgentDescription wrote an agent line for an empty Agent:\n%s", formatted)
	}
}

// A line break in Agent must not start a second description line that
// ParseAgentFields would read as another field (gt-s6e7z).
func TestAgentFieldsAgentLineBreakCannotInjectField(t *testing.T) {
	for _, agent := range []string{"claude\nhook_bead: gt-evil", "claude\r\nhook_bead: gt-evil"} {
		formatted := FormatAgentDescription("Polecat Test", &AgentFields{
			RoleType: "polecat", Rig: "gastown", AgentState: "spawning", HookBead: "gt-abc", Agent: agent,
		})
		parsed := ParseAgentFields(formatted)
		if parsed.HookBead != "gt-abc" {
			t.Errorf("agent %q: hook_bead = %q, want gt-abc", agent, parsed.HookBead)
		}
		if !strings.HasPrefix(parsed.Agent, "claude") {
			t.Errorf("agent %q: parsed Agent = %q, want it to keep the claude prefix", agent, parsed.Agent)
		}
	}
}

// Descriptions written by hand carry "Mode: ralph" or "MODE: ralph"; both still
// parse as the Mode field (gt-dk6vt).
func TestParseAttachmentFieldsAcceptsMixedCaseRalphMode(t *testing.T) {
	for _, line := range []string{"Mode: ralph", "MODE: ralph", "MODE: Ralph"} {
		parsed := ParseAttachmentFields(&Issue{Description: line + "\n\n## Goal\n\nDo it."})
		if parsed == nil || parsed.Mode != "ralph" {
			t.Errorf("ParseAttachmentFields(%q) = %+v, want Mode ralph", line, parsed)
		}
	}
	if parsed := ParseAttachmentFields(&Issue{Description: "Mode: read-only\nDo it."}); parsed != nil {
		t.Errorf("prose Mode line parsed as fields: %+v", parsed)
	}
}

// A prose line that starts with the word Mode is the bead's own text, not the
// attachment field: SetAttachmentFields leaves it where it is and does not
// promote it to a mode: line at the top.
func TestSetAttachmentFieldsKeepsProseModeLine(t *testing.T) {
	issue := &Issue{Description: "attached_molecule: gt-wisp-old\n\n## Goal\n\nMode: read-only\nDo it."}
	newDesc := SetAttachmentFields(issue, &AttachmentFields{AttachedMolecule: "gt-wisp-new"})

	want := "attached_molecule: gt-wisp-new\n\n## Goal\n\nMode: read-only\nDo it."
	if newDesc != want {
		t.Errorf("SetAttachmentFields =\n%q\nwant\n%q", newDesc, want)
	}
	if strings.Contains(newDesc, "mode: read-only") {
		t.Errorf("prose Mode line became an attachment line:\n%s", newDesc)
	}
	if parsed := ParseAttachmentFields(&Issue{Description: newDesc}); parsed == nil || parsed.Mode != "" {
		t.Errorf("parsed Mode = %+v, want empty", parsed)
	}
}

package formula

import (
	"strings"
	"testing"
)

// orphanSurvivalBlocks returns the three shell blocks of mol-witness-patrol
// survey-workers step 5 (gt-vm5g4): exit 0 (work survives), exit 3 (reset),
// and any other exit (cannot tell). Each is dedented the way markdown strips
// a fenced block's indentation, so a heredoc terminator lands at column 0.
func orphanSurvivalBlocks(t *testing.T) (survives, reset, unknown string) {
	t.Helper()
	content, err := formulasFS.ReadFile("formulas/mol-witness-patrol.formula.toml")
	if err != nil {
		t.Fatal(err)
	}
	f, err := Parse(content)
	if err != nil {
		t.Fatal(err)
	}
	var desc string
	for _, s := range f.Steps {
		if s.ID == "survey-workers" {
			desc = s.Description
		}
	}
	start := strings.Index(desc, "5. If BOTH session dead AND directory missing")
	end := strings.Index(desc, "6. If directory exists but session dead")
	if start < 0 || end < start {
		t.Fatal("survey-workers step 5 not found")
	}
	step := desc[start:end]

	var blocks []string
	lines := strings.Split(step, "\n")
	for i := 0; i < len(lines); i++ {
		fence := strings.TrimLeft(lines[i], " ")
		if fence != "```bash" {
			continue
		}
		indent := len(lines[i]) - len(fence)
		var body []string
		for i++; i < len(lines) && strings.TrimSpace(lines[i]) != "```"; i++ {
			l := lines[i]
			if len(l) >= indent && strings.TrimSpace(l[:indent]) == "" {
				l = l[indent:]
			}
			body = append(body, l)
		}
		blocks = append(blocks, strings.Join(body, "\n")+"\n")
	}
	// blocks[0] is the surviving-work call itself.
	if len(blocks) != 4 {
		t.Fatalf("step 5 has %d bash blocks, want 4 (check, exit 0, exit 3, other)", len(blocks))
	}
	return blocks[1], blocks[2], blocks[3]
}

// TestWitnessOrphanSurvivalBlocksAreSafeShell checks the blocks' text; that
// they parse and run as bash is the integration tier's
// (TestIntegrationWitnessOrphanSurvivalBlocksParseAsBash).
func TestWitnessOrphanSurvivalBlocksAreSafeShell(t *testing.T) {
	t.Parallel()
	survives, reset, unknown := orphanSurvivalBlocks(t)
	for name, b := range map[string]string{"exit 0": survives, "exit 3": reset, "other": unknown} {
		if strings.Contains(b, "{{") {
			t.Errorf("%s block splices a template variable into shell:\n%s", name, b)
		}
		if strings.Contains(b, "<<EOF") || strings.Contains(b, "<< EOF") {
			t.Errorf("%s block has an unquoted heredoc:\n%s", name, b)
		}
		if strings.Contains(b, "<<'EOF'") && !strings.Contains(b, "\nEOF\n") {
			t.Errorf("%s block's heredoc terminator is not at column 0 after dedent:\n%s", name, b)
		}
	}
}

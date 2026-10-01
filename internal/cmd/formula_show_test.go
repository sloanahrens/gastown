package cmd

import (
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
)

// cookTree is tree as the formula engine's Cook returns it.
func cookTree(tree string) []byte {
	return []byte(tree)
}

// docAuditTree is bd's cooked tree for a formula shaped like mol-doc-audit: an
// inherited step, an expanded one with a child, a declared var.
const docAuditTree = `{
  "formula": "mol-doc-audit", "type": "workflow", "description": "Audit docs",
  "vars": [
    {"name": "issue", "description": "The issue", "required": true, "default": null, "value": "gt-1", "provided": true},
    {"name": "slice_docs", "description": "Docs to audit", "required": false, "default": "", "value": "", "provided": false}
  ],
  "unresolved_vars": [], "warnings": [],
  "steps": [
    {"id": "load-context", "title": "Load gt-1", "description": "Read gt-1.", "needs": [], "children": []},
    {"id": "audit", "title": "Audit", "description": "Audit body.", "needs": ["load-context"],
     "children": [{"id": "audit.check", "title": "Check links", "description": "", "needs": [], "children": []}]}
  ]
}`

// fakeCook is bd's formula engine in memory. Cook prints out, or fails with
// bd's message msg when msg is set; Bond answers through bond (n counts the
// bonds from 1), or prints bondOut when bond is nil; Wisp prints wispOut or
// fails as Cook does. Every call is recorded with the site it ran at.
type fakeCook struct {
	out     []byte
	msg     string
	bond    func(n int) ([]byte, error)
	bondOut []byte
	// show answers formula show; nil fails every one as not found.
	show    func(formula string) []byte
	wispOut []byte

	mu    sync.Mutex
	calls []formulaCall
}

// formulaCall is one engine call: its site, and the call as bd's argv reads
// ("cook <name> --var k=v ..." or "mol bond <proto> <bead> --json
// --ephemeral --var k=v ...").
type formulaCall struct {
	site formulaSite
	argv string
	vars []string
}

func (f *fakeCook) open(site formulaSite) formulaEngine { return fakeCookAt{f, site} }

func (f *fakeCook) record(site formulaSite, argv string, vars []string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, v := range vars {
		argv += " --var " + v
	}
	f.calls = append(f.calls, formulaCall{site: site, argv: argv, vars: append([]string(nil), vars...)})
	n := 0
	for _, c := range f.calls {
		if strings.HasPrefix(c.argv, "mol bond ") {
			n++
		}
	}
	return n
}

// log is every call's argv, one per line.
func (f *fakeCook) log() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	lines := make([]string, len(f.calls))
	for i, c := range f.calls {
		lines[i] = c.argv
	}
	return strings.Join(lines, "\n")
}

// called returns the calls whose argv starts with prefix.
func (f *fakeCook) called(prefix string) []formulaCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []formulaCall
	for _, c := range f.calls {
		if strings.HasPrefix(c.argv, prefix) {
			out = append(out, c)
		}
	}
	return out
}

// fakeCookAt is fakeCook at one site.
type fakeCookAt struct {
	f    *fakeCook
	site formulaSite
}

func (e fakeCookAt) Cook(formula string, vars []string) ([]byte, error) {
	e.f.record(e.site, "cook "+formula, vars)
	if e.f.msg != "" {
		return nil, errors.New(e.f.msg)
	}
	return e.f.out, nil
}

func (e fakeCookAt) Bond(proto, beadID string, vars []string) ([]byte, error) {
	n := e.f.record(e.site, "mol bond "+proto+" "+beadID+" --json --ephemeral", vars)
	if e.f.bond != nil {
		return e.f.bond(n)
	}
	return e.f.bondOut, nil
}

func (e fakeCookAt) FormulaShow(formula string) ([]byte, error) {
	e.f.record(e.site, "formula show "+formula, nil)
	if e.f.show == nil {
		return nil, errors.New("formula not found: " + formula)
	}
	return e.f.show(formula), nil
}

func (e fakeCookAt) Wisp(formula string, vars []string) ([]byte, error) {
	e.f.record(e.site, "mol wisp "+formula+" --json", vars)
	if e.f.msg != "" {
		return nil, errors.New(e.f.msg)
	}
	return e.f.wispOut, nil
}

// polecatChecklistRun answers bd cook with a work formula the size of
// mol-polecat-work (8 steps, ~19 KB of bodies), so the prime budget tests
// measure a realistic checklist without starting bd.
func polecatChecklistRun() func(formulaSite) formulaEngine {
	titles := []string{"Load context and verify assignment", "Set up working branch", "Implement the work",
		"Self-review", "Run the gates", "Pre-verify", "Commit", "Submit work and self-clean"}
	steps := make([]string, len(titles))
	for i, title := range titles {
		body := jsonString(strings.Repeat("Body line of a polecat work step with commands to run.\n", 42))
		steps[i] = `{"id": "s` + string(rune('1'+i)) + `", "title": ` + jsonString(title) + `, "description": ` + body + `, "children": []}`
	}
	fake := &fakeCook{out: cookTree(`{"formula": "mol-polecat-work", "type": "workflow", "steps": [` + strings.Join(steps, ",") + `]}`)}
	return fake.open
}

func cookedFixture(t *testing.T, tree string) *cookedFormula {
	t.Helper()
	fake := &fakeCook{out: cookTree(tree)}
	f, err := formulaCooker{open: fake.open}.cookForRender("mol-doc-audit", "", "", nil)
	if err != nil {
		t.Fatalf("cookForRender: %v", err)
	}
	return f
}

func TestRenderCookedFormula(t *testing.T) {
	t.Parallel()
	var sb strings.Builder
	renderCookedFormula(&sb, cookedFixture(t, docAuditTree))
	out := sb.String()
	for _, want := range []string{
		"mol-doc-audit (cooked)",
		"gt formula show mol-doc-audit --raw",
		"{{issue}}: The issue [required]",
		`{{slice_docs}}: Docs to audit [default=""]`,
		"Steps (3):",
		"├── load-context: Load gt-1",
		"└── audit: Audit [needs: load-context]",
		"    └── audit.check: Check links",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestWriteCookedFormulaJSON_IsBdTree(t *testing.T) {
	t.Parallel()
	var sb strings.Builder
	if err := writeCookedFormulaJSON(&sb, cookedFixture(t, docAuditTree)); err != nil {
		t.Fatalf("writeCookedFormulaJSON: %v", err)
	}
	var got struct {
		Formula string `json:"formula"`
		Steps   []struct {
			ID       string            `json:"id"`
			Children []json.RawMessage `json:"children"`
		} `json:"steps"`
	}
	if err := json.Unmarshal([]byte(sb.String()), &got); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, sb.String())
	}
	if got.Formula != "mol-doc-audit" || len(got.Steps) != 2 || len(got.Steps[1].Children) != 1 {
		t.Errorf("JSON is not bd's tree: %+v", got)
	}
}

package formula

import (
	"io/fs"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

// The structs below mirror the formula keys bd's strict decode accepts
// (beads internal/formula/types.go, strict.go), cut to what shipped formulas
// use: a key outside them is one bd refuses to cook. Gastown's own fields
// ride in step metadata and vars, which bd accepts whole. A key bd accepts
// but no shipped formula uses yet must be added here before a formula uses it.
type bdFormula struct {
	Formula     string                    `toml:"formula"`
	Description string                    `toml:"description"`
	Version     int                       `toml:"version"`
	Type        string                    `toml:"type"`
	Extends     []string                  `toml:"extends"`
	Vars        map[string]toml.Primitive `toml:"vars"`
	Steps       []bdStep                  `toml:"steps"`
	Template    []bdStep                  `toml:"template"`
	Compose     *struct {
		Expand []struct {
			Target string `toml:"target"`
			With   string `toml:"with"`
		} `toml:"expand"`
		Aspects []string `toml:"aspects"`
	} `toml:"compose"`
	Advice []struct {
		Target string        `toml:"target"`
		Before *bdAdviceStep `toml:"before"`
		After  *bdAdviceStep `toml:"after"`
		Around *struct {
			Before []bdAdviceStep `toml:"before"`
			After  []bdAdviceStep `toml:"after"`
		} `toml:"around"`
	} `toml:"advice"`
	Pointcuts []struct {
		Glob  string `toml:"glob"`
		Type  string `toml:"type"`
		Label string `toml:"label"`
	} `toml:"pointcuts"`
	Phase string `toml:"phase"`
	Pour  bool   `toml:"pour"`
}

type bdStep struct {
	ID          string            `toml:"id"`
	Title       string            `toml:"title"`
	Description string            `toml:"description"`
	Notes       string            `toml:"notes"`
	Type        string            `toml:"type"`
	Priority    *int              `toml:"priority"`
	Labels      []string          `toml:"labels"`
	Metadata    map[string]any    `toml:"metadata"`
	DependsOn   []string          `toml:"depends_on"`
	Needs       []string          `toml:"needs"`
	Assignee    string            `toml:"assignee"`
	Expand      string            `toml:"expand"`
	ExpandVars  map[string]string `toml:"expand_vars"`
	Condition   string            `toml:"condition"`
	Children    []bdStep          `toml:"children"`
}

type bdAdviceStep struct {
	ID          string `toml:"id"`
	Title       string `toml:"title"`
	Description string `toml:"description"`
	Type        string `toml:"type"`
}

// bdVar is a [vars.<name>] table; a var may also be a bare string default.
type bdVar struct {
	Description string   `toml:"description"`
	Default     *string  `toml:"default"`
	Required    bool     `toml:"required"`
	Enum        []string `toml:"enum"`
	Pattern     string   `toml:"pattern"`
	Type        string   `toml:"type"`
}

// TestShippedFormulasUseOnlyKeysBdAccepts strict-decodes every embedded
// formula against the keys bd accepts, without running bd: an unknown key, a
// var both required and defaulted, a bad type or version, or a duplicate step
// id is a formula bd refuses to cook (gt-fd2cu.1.1). The real-bd cook of every
// formula is TestIntegrationFormulaCook in internal/cmd.
func TestShippedFormulasUseOnlyKeysBdAccepts(t *testing.T) {
	t.Parallel()
	entries, err := fs.ReadDir(formulasFS, "formulas")
	if err != nil {
		t.Fatalf("reading embedded formulas: %v", err)
	}
	checked := 0
	for _, entry := range entries {
		name, ok := strings.CutSuffix(entry.Name(), ".formula.toml")
		if entry.IsDir() || !ok {
			continue
		}
		checked++
		data, err := formulasFS.ReadFile("formulas/" + entry.Name())
		if err != nil {
			t.Fatalf("%s: %v", entry.Name(), err)
		}
		for _, problem := range bdStrictProblems(name, data) {
			t.Errorf("%s: %s", entry.Name(), problem)
		}
	}
	if checked == 0 {
		t.Fatal("no embedded *.formula.toml files found")
	}
}

// TestBdStrictProblemsCatchesWhatBdRejects pins the checker on the shapes bd
// refused before gt-fd2cu.1.1: gastown-only keys at the top level and on a
// step, and a var both required and defaulted.
func TestBdStrictProblemsCatchesWhatBdRejects(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, toml, want string }{
		{"top-level key", "formula = \"x\"\nversion = 1\n[[legs]]\nid = \"a\"\n", "unknown key legs"},
		{"step key", "formula = \"x\"\nversion = 1\n[[steps]]\nid = \"a\"\ntarget = \"mayor\"\n", "unknown key steps.target"},
		{"var field", "formula = \"x\"\nversion = 1\n[vars.v]\nrequired_unless = [\"w\"]\n", "unknown key vars.v.required_unless"},
		{"required and default", "formula = \"x\"\nversion = 1\n[vars.v]\nrequired = true\ndefault = \"d\"\n", "required and a default"},
		{"duplicate step", "formula = \"x\"\nversion = 1\n[[steps]]\nid = \"a\"\n[[steps]]\nid = \"a\"\n", "duplicate step id a"},
		{"name", "formula = \"y\"\nversion = 1\n", "formula name y"},
	} {
		got := strings.Join(bdStrictProblems("x", []byte(tc.toml)), "; ")
		if !strings.Contains(got, tc.want) {
			t.Errorf("%s: problems %q, want %q", tc.name, got, tc.want)
		}
	}
	if got := bdStrictProblems("x", []byte("formula = \"x\"\nversion = 1\n[vars]\nv = \"d\"\n[[steps]]\nid = \"a\"\nmetadata.focus = \"f\"\n")); len(got) > 0 {
		t.Errorf("clean formula: problems %v", got)
	}
}

// bdStrictProblems is every reason bd's strict decode would refuse a formula
// file named name.
func bdStrictProblems(name string, data []byte) []string {
	var f bdFormula
	md, err := toml.Decode(string(data), &f)
	if err != nil {
		return []string{err.Error()}
	}
	var problems []string
	for _, k := range md.Undecoded() {
		if len(k) >= 2 && k[0] == "vars" {
			continue // var tables are checked one by one below
		}
		problems = append(problems, "unknown key "+k.String())
	}
	for v, prim := range f.Vars {
		if md.Type("vars", v) == "String" {
			continue
		}
		var def bdVar
		if err := md.PrimitiveDecode(prim, &def); err != nil {
			problems = append(problems, "vars."+v+": "+err.Error())
			continue
		}
		for _, k := range md.Keys() {
			if len(k) == 3 && k[0] == "vars" && k[1] == v && !bdVarFields[k[2]] {
				problems = append(problems, "unknown key "+k.String())
			}
		}
		if def.Required && def.Default != nil {
			problems = append(problems, "vars."+v+": required and a default")
		}
	}
	if f.Formula != name {
		problems = append(problems, "formula name "+f.Formula+" is not the file name "+name)
	}
	if f.Version < 1 {
		problems = append(problems, "version must be >= 1")
	}
	switch f.Type {
	case "", "workflow", "expansion", "aspect", "convoy":
	default:
		problems = append(problems, "invalid type "+f.Type)
	}
	seen := map[string]bool{}
	var walk func([]bdStep)
	walk = func(steps []bdStep) {
		for _, s := range steps {
			if seen[s.ID] {
				problems = append(problems, "duplicate step id "+s.ID)
			}
			seen[s.ID] = true
			walk(s.Children)
		}
	}
	walk(f.Steps)
	return problems
}

var bdVarFields = map[string]bool{"description": true, "default": true, "required": true, "enum": true, "pattern": true, "type": true}

package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/formula"
	"github.com/steveyegge/gastown/internal/style"
)

// cookedFormula is a formula as bd cooks it. bd is the one formula engine (D6,
// gt-fd2cu.1): `bd cook <name>` under machine mode returns the step tree pour
// creates, with extends, expansions, aspects, advice and the town overlay
// applied and the vars substituted. Gastown renders what it returns and parses
// no formula itself.
type cookedFormula struct {
	Formula        string       `json:"formula"`
	Type           string       `json:"type"`
	Description    string       `json:"description"`
	Vars           []cookedVar  `json:"vars"`
	UnresolvedVars []string     `json:"unresolved_vars"`
	Warnings       []string     `json:"warnings"`
	Steps          []cookedStep `json:"steps"`

	// raw is bd's tree as it printed it, for gt formula show --json.
	raw json.RawMessage
}

// cookedVar is one declared var. Value is what bd substituted (the --var,
// else the default); nil when the var has neither.
type cookedVar struct {
	Name        string  `json:"name"`
	Description string  `json:"description"`
	Required    bool    `json:"required"`
	Default     *string `json:"default"`
	Value       *string `json:"value"`
	Provided    bool    `json:"provided"`
}

type cookedStep struct {
	ID          string       `json:"id"`
	Title       string       `json:"title"`
	Description string       `json:"description"`
	Needs       []string     `json:"needs"`
	Children    []cookedStep `json:"children"`
}

// checklist returns every step an agent runs, each parent before its
// children: the order pour creates them in.
func (f *cookedFormula) checklist() []checklistStep {
	var out []checklistStep
	var walk func([]cookedStep)
	walk = func(steps []cookedStep) {
		for _, s := range steps {
			out = append(out, checklistStep{ID: s.ID, Title: s.Title, Description: s.Description})
			walk(s.Children)
		}
	}
	walk(f.Steps)
	return out
}

// formulaCooker reaches bd's cook. run answers the call; nil is the bd on PATH.
type formulaCooker struct {
	run beads.BDRunner
}

// realFormulaCooker cooks with the bd on PATH.
func realFormulaCooker() formulaCooker {
	return formulaCooker{}
}

// cookForRender cooks formulaName where prime renders it: in the rig's
// directory (the town root without a rig), so bd searches the formula dirs pour
// searches, with the town's overlay dir. A stale overlay is one warning line.
func (c formulaCooker) cookForRender(formulaName, townRoot, rigName string, vars []string) (*cookedFormula, error) {
	dir := townRoot
	if townRoot != "" && rigName != "" {
		dir = filepath.Join(townRoot, rigName)
	}
	f, err := cookFormula(formulaName, BdCmd(cookArgs(formulaName, vars)...).
		Dir(dir).
		WithGTRoot(townRoot).
		WithEnv(formulaOverlayEnv(townRoot)).
		Via(c.run))
	if err != nil {
		return nil, err
	}
	for _, w := range f.Warnings {
		style.PrintWarning("formula %s: %s", formulaName, w)
	}
	return f, nil
}

// cookArgs is the bd argv that cooks formulaName with vars ("key=value").
func cookArgs(formulaName string, vars []string) []string {
	args := []string{"cook", formulaName}
	for _, v := range vars {
		args = append(args, "--var", v)
	}
	return args
}

// cookFormula runs the cook cmd carries and decodes bd's tree. A failure is
// one line naming the formula and bd's own message.
func cookFormula(formulaName string, cmd *bdCmd) (*cookedFormula, error) {
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("cook formula %s: %s", formulaName, cookFailure(out, err))
	}
	var f cookedFormula
	if err := json.Unmarshal(out, &f); err != nil {
		return nil, fmt.Errorf("cook formula %s: bd printed no step tree: %v", formulaName, err)
	}
	f.raw = append(json.RawMessage(nil), out...)
	return &f, nil
}

// cookFailure is the first line of bd's message from the error envelope a
// failed machine-mode call leaves on stdout, else of err.
func cookFailure(out []byte, err error) string {
	var env struct {
		Error *struct {
			Kind    string `json:"kind"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(out, &env) == nil && env.Error != nil && env.Error.Message != "" {
		return firstTextLine(env.Error.Message)
	}
	return firstTextLine(err.Error())
}

func firstTextLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// formulaOverlayEnv is the process environment with bd pointed at the town's
// one overlay dir, so a cook and a pour apply the same overlay.
func formulaOverlayEnv(townRoot string) []string {
	env := beads.StripEnvKey(os.Environ(), "BD_FORMULA_OVERLAY_DIR")
	if townRoot == "" {
		return env
	}
	return append(env, "BD_FORMULA_OVERLAY_DIR="+formula.OverlayDir(townRoot))
}

//go:build integration

package cmd

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/formula"
)

// TestIntegrationFormulaEnginesAgree is the TEMPORARY two-engine diff of
// gt-fd2cu.1: it renders every embedded formula through gastown's own parser
// (the render path prime used) and through `bd cook` under machine mode, and
// fails on any difference in the steps an agent would run. A difference is bd
// being right; the test goes away with the gastown parser.
func TestIntegrationFormulaEnginesAgree(t *testing.T) {
	town := t.TempDir()
	if _, err := formula.ProvisionFormulas(town); err != nil {
		t.Fatalf("provision formulas: %v", err)
	}
	vars := []string{"issue=gt-diff", "feature=Diff feature"}

	names := embeddedFormulaNames(t, town)
	var diffs []string
	for _, name := range names {
		gtSteps, gtErr := gastownEngineSteps(name, town, vars)
		bdSteps, bdErr := bdEngineSteps(name, town, vars)
		if d := diffEngines(name, gtSteps, gtErr, bdSteps, bdErr); d != "" {
			diffs = append(diffs, d)
		}
	}
	if len(diffs) > 0 {
		t.Errorf("%d of %d formulas render differently in gastown and bd:\n%s",
			len(diffs), len(names), strings.Join(diffs, "\n"))
	}
}

type engineStep struct{ ID, Title, Description string }

func embeddedFormulaNames(t *testing.T, town string) []string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(town, ".beads", "formulas", "*.formula.toml"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("list provisioned formulas: %v (%d found)", err, len(paths))
	}
	names := make([]string, 0, len(paths))
	for _, p := range paths {
		names = append(names, strings.TrimSuffix(filepath.Base(p), ".formula.toml"))
	}
	sort.Strings(names)
	return names
}

func gastownEngineSteps(name, town string, vars []string) ([]engineStep, error) {
	f, varMap, err := resolveFormulaForRendering(name, town, "", vars)
	if err != nil {
		return nil, err
	}
	out := make([]engineStep, 0, len(f.Steps))
	for _, s := range f.Steps {
		out = append(out, engineStep{
			ID:          s.ID,
			Title:       applyFormulaVars(s.Title, varMap),
			Description: applyFormulaVars(s.Description, varMap),
		})
	}
	return out, nil
}

type bdEngineStep struct {
	ID          string         `json:"id"`
	Title       string         `json:"title"`
	Description string         `json:"description"`
	Children    []bdEngineStep `json:"children"`
}

func bdEngineSteps(name, town string, vars []string) ([]engineStep, error) {
	args := []string{"cook", name}
	for _, v := range vars {
		args = append(args, "--var", v)
	}
	out, err := beads.NewBdCmd(args...).Dir(town).WithGTRoot(town).Output()
	if err != nil {
		var env struct {
			Error *struct{ Kind, Message string } `json:"error"`
		}
		if json.Unmarshal(out, &env) == nil && env.Error != nil {
			return nil, fmt.Errorf("%s: %s", env.Error.Kind, oneLine(env.Error.Message))
		}
		return nil, err
	}
	var tree struct {
		Steps []bdEngineStep `json:"steps"`
	}
	if err := json.Unmarshal(out, &tree); err != nil {
		return nil, fmt.Errorf("decode cook tree: %w", err)
	}
	var flat []engineStep
	var walk func([]bdEngineStep)
	walk = func(steps []bdEngineStep) {
		for _, s := range steps {
			flat = append(flat, engineStep{ID: s.ID, Title: s.Title, Description: s.Description})
			walk(s.Children)
		}
	}
	walk(tree.Steps)
	return flat, nil
}

func diffEngines(name string, gt []engineStep, gtErr error, bd []engineStep, bdErr error) string {
	switch {
	case gtErr != nil && bdErr != nil:
		return fmt.Sprintf("  %s: both fail: gastown %v; bd %v", name, oneLine(gtErr.Error()), bdErr)
	case gtErr != nil:
		return fmt.Sprintf("  %s: gastown fails (%v), bd renders %d steps", name, oneLine(gtErr.Error()), len(bd))
	case bdErr != nil:
		return fmt.Sprintf("  %s: bd fails (%v), gastown renders %d steps", name, bdErr, len(gt))
	}
	ids := func(steps []engineStep) string {
		s := make([]string, len(steps))
		for i, st := range steps {
			s[i] = st.ID
		}
		return strings.Join(s, ",")
	}
	if len(gt) != len(bd) || ids(gt) != ids(bd) {
		return fmt.Sprintf("  %s: steps differ: gastown %d [%s], bd %d [%s]", name, len(gt), ids(gt), len(bd), ids(bd))
	}
	for i := range gt {
		if gt[i].Title != bd[i].Title {
			return fmt.Sprintf("  %s: step %s title: gastown %q, bd %q", name, gt[i].ID, gt[i].Title, bd[i].Title)
		}
		if gt[i].Description != bd[i].Description {
			return fmt.Sprintf("  %s: step %s description: %s", name, gt[i].ID, firstDifference(gt[i].Description, bd[i].Description))
		}
	}
	return ""
}

func firstDifference(a, b string) string {
	i := 0
	for i < len(a) && i < len(b) && a[i] == b[i] {
		i++
	}
	clip := func(s string) string {
		end := i + 60
		if end > len(s) {
			end = len(s)
		}
		start := i - 20
		if start < 0 {
			start = 0
		}
		return s[start:end]
	}
	return fmt.Sprintf("at byte %d: gastown %q, bd %q", i, clip(a), clip(b))
}

func oneLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

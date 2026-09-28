package testpolicy

import (
	"go/ast"
	"go/token"
	"strings"
)

const allowPrefix = "//testpolicy:allow "

type allowKey struct {
	file string
	line int
	rule string
}

// Exemption is one violation that a "//testpolicy:allow <rule> — <reason>"
// comment suppressed, recorded so the caller can log it (spec §4: "The test
// logs every exemption").
type Exemption struct {
	Pos    token.Position
	Rule   string
	Reason string
}

// applyAllows drops violations exempted by a "//testpolicy:allow <rule> — <reason>"
// comment on the same line or the line above, and returns the exemptions it
// honored (one per suppressed violation). An exemption with no reason still
// suppresses its rule, but adds an allow-reason violation.
func applyAllows(fset *token.FileSet, files []*ast.File, vs []Violation) ([]Violation, []Exemption) {
	allowed := map[allowKey]string{} // rule key -> reason
	var out []Violation
	for _, f := range files {
		for _, cg := range f.Comments {
			for _, c := range cg.List {
				if !strings.HasPrefix(c.Text, allowPrefix) {
					continue
				}
				fields := strings.Fields(strings.TrimPrefix(c.Text, allowPrefix))
				if len(fields) == 0 {
					continue
				}
				pos := fset.Position(c.Pos())
				rule := fields[0]
				reason := strings.TrimSpace(strings.TrimLeft(strings.Join(fields[1:], " "), "—-"))
				allowed[allowKey{pos.Filename, pos.Line, rule}] = reason
				allowed[allowKey{pos.Filename, pos.Line + 1, rule}] = reason
				if reason == "" {
					out = append(out, Violation{Pos: pos, Rule: RuleAllowReason, Msg: "exemption for " + rule + " has no reason"})
				}
			}
		}
	}
	var exemptions []Exemption
	for _, v := range vs {
		key := allowKey{v.Pos.Filename, v.Pos.Line, v.Rule}
		if reason, ok := allowed[key]; ok {
			exemptions = append(exemptions, Exemption{Pos: v.Pos, Rule: v.Rule, Reason: reason})
			continue
		}
		out = append(out, v)
	}
	return out, exemptions
}

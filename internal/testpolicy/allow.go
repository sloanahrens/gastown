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

// applyAllows drops violations exempted by a "//testpolicy:allow <rule> — <reason>"
// comment on the same line or the line above. An exemption with no reason
// still suppresses its rule, but adds an allow-reason violation.
func applyAllows(fset *token.FileSet, files []*ast.File, vs []Violation) []Violation {
	allowed := map[allowKey]bool{}
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
				allowed[allowKey{pos.Filename, pos.Line, rule}] = true
				allowed[allowKey{pos.Filename, pos.Line + 1, rule}] = true
				reason := strings.TrimSpace(strings.TrimLeft(strings.Join(fields[1:], " "), "—-"))
				if reason == "" {
					out = append(out, Violation{Pos: pos, Rule: RuleAllowReason, Msg: "exemption for " + rule + " has no reason"})
				}
			}
		}
	}
	for _, v := range vs {
		if !allowed[allowKey{v.Pos.Filename, v.Pos.Line, v.Rule}] {
			out = append(out, v)
		}
	}
	return out
}

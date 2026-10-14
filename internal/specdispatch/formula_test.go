package specdispatch

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseFormulaVars(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		body    string
		want    []string
		wantErr string
	}{
		{name: "empty section", body: "", want: []string{}},
		{name: "good", body: "feature=deep review\nbase_branch=main", want: []string{"feature=deep review", "base_branch=main"}},
		{name: "blank and comment lines", body: "\n# why\nk=v\n\n  # indented comment\nj=w\n", want: []string{"k=v", "j=w"}},
		{name: "value keeps later equals", body: "cmd=go test -run X=1", want: []string{"cmd=go test -run X=1"}},
		{name: "spaces trimmed", body: "  k = v  ", want: []string{"k=v"}},
		{name: "empty value", body: "k=", want: []string{"k="}},
		{name: "duplicate key last wins, first position", body: "a=1\nb=2\na=3", want: []string{"a=3", "b=2"}},
		{name: "no equals", body: "a=1\nnot a var", wantErr: "bad formula vars: not a var"},
		{name: "empty key", body: "=v", wantErr: "bad formula vars: =v"},
		{name: "blank key", body: "  =v", wantErr: "bad formula vars: =v"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := ParseFormulaVars(tc.body)
			if tc.wantErr != "" {
				if err == nil || err.Error() != tc.wantErr {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("vars = %q, want %q", got, tc.want)
			}
		})
	}
}

func formulaSpec(labels []string, vars string) Spec {
	s := Spec{ID: "gt-f", Labels: labels, Description: "## Goal\nG.\n\n## Gate\nmake gate"}
	if vars != "" {
		s.Description += "\n\n## Formula vars\n" + vars
	}
	return s
}

func TestFormulaRunOf(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		spec    Spec
		want    FormulaRun
		wantErr string
	}{
		{name: "no label declares nothing", spec: formulaSpec(nil, "k=v"), want: FormulaRun{}},
		{name: "label without section", spec: formulaSpec([]string{"formula:mol-x"}, ""), want: FormulaRun{Formula: "mol-x", Vars: []string{}}},
		{name: "label and vars", spec: formulaSpec([]string{"formula:mol-x"}, "a=1\n# c\nb=2"),
			want: FormulaRun{Formula: "mol-x", Vars: []string{"a=1", "b=2"}}},
		{name: "label case and space", spec: formulaSpec([]string{" Formula:mol-x "}, ""), want: FormulaRun{Formula: "mol-x", Vars: []string{}}},
		{name: "scheduled label sets actor", spec: formulaSpec([]string{"formula:mol-x", "scheduled:deep-review"}, "a=1"),
			want: FormulaRun{Formula: "mol-x", Vars: []string{"a=1"}, Actor: "daemon/scheduled:deep-review"}},
		{name: "scheduled alone declares nothing", spec: formulaSpec([]string{"scheduled:deep-review"}, ""), want: FormulaRun{}},
		{name: "bad vars", spec: formulaSpec([]string{"formula:mol-x"}, "a=1\noops"), wantErr: "bad formula vars: oops"},
		{name: "bad vars without label is ignored", spec: formulaSpec(nil, "oops"), want: FormulaRun{}},
		{name: "empty formula label", spec: formulaSpec([]string{"formula:"}, ""), wantErr: "bad formula label: formula:"},
		{name: "conflicting labels", spec: formulaSpec([]string{"formula:a", "formula:b"}, ""), wantErr: "conflicting formula labels: a, b"},
		{name: "repeated label is one", spec: formulaSpec([]string{"formula:a", "Formula:a"}, ""), want: FormulaRun{Formula: "a", Vars: []string{}}},
		{name: "vars section inside a fence is body, not structure",
			spec: Spec{Labels: []string{"formula:a"}, Description: "## Goal\nG\n\n```\n## Formula vars\noops\n```\n"},
			want: FormulaRun{Formula: "a", Vars: []string{}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := FormulaRunOf(tc.spec)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("run = %+v, want %+v", got, tc.want)
			}
		})
	}
}

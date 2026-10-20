package targets

import (
	"strings"
	"testing"
)

func TestJevTargetsParseRanked(t *testing.T) {
	t.Parallel()
	rows, err := ParseRanked(strings.NewReader(
		"0\t0000-00-00\t12.5000\t1234\tinternal/a/one.go\n" +
			"1\t2026-10-01\t3.2500\t90\tinternal/c/three.go\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := []Row{
		{Tier: 0, LastReview: "0000-00-00", RiskScore: 12.5, Bytes: 1234, Path: "internal/a/one.go"},
		{Tier: 1, LastReview: "2026-10-01", RiskScore: 3.25, Bytes: 90, Path: "internal/c/three.go"},
	}
	if len(rows) != len(want) {
		t.Fatalf("parsed %d rows, want %d", len(rows), len(want))
	}
	for i := range want {
		if rows[i] != want[i] {
			t.Errorf("row %d = %+v, want %+v", i, rows[i], want[i])
		}
	}
}

func TestJevTargetsParseRankedRefusesMalformedRows(t *testing.T) {
	t.Parallel()
	for name, line := range map[string]string{
		"four fields":        "0\t0000-00-00\t1.0\t10\n",
		"tier not a number":  "x\t0000-00-00\t1.0\t10\tinternal/a.go\n",
		"score not a number": "0\t0000-00-00\tx\t10\tinternal/a.go\n",
		"bytes not a number": "0\t0000-00-00\t1.0\tx\tinternal/a.go\n",
		"empty path":         "0\t0000-00-00\t1.0\t10\t\n",
		"option path":        "0\t0000-00-00\t1.0\t10\t-rf\n",
		"absolute path":      "0\t0000-00-00\t1.0\t10\t/internal/a.go\n",
		"escaping path":      "0\t0000-00-00\t1.0\t10\tinternal/../../x.go\n",
	} {
		if _, err := ParseRanked(strings.NewReader(line)); err == nil {
			t.Errorf("%s: parsed, want an error", name)
		}
	}
}

func TestJevTargetsParseHistory(t *testing.T) {
	t.Parallel()
	hist := ParseHistory(strings.NewReader(`internal/orphan.go
@fix: one (gt-1)
internal/a/one.go
internal/b/two.go
@refactor: two (gt-2)
internal/b/two.go
`))
	if got := hist["internal/b/two.go"]; len(got) != 2 || got[0] != "fix: one (gt-1)" || got[1] != "refactor: two (gt-2)" {
		t.Errorf("two.go = %v, want both subjects newest first", got)
	}
	if got := hist["internal/a/one.go"]; len(got) != 1 || got[0] != "fix: one (gt-1)" {
		t.Errorf("one.go = %v", got)
	}
	if _, ok := hist["internal/orphan.go"]; ok {
		t.Error("a path before the first subject was attributed to a commit")
	}
}

func TestJevTargetsExportedNames(t *testing.T) {
	t.Parallel()
	src := `package p

func Exported() {}
func hidden() {}
func (T) Method() {}
type T struct{}
type lower struct{}
const C = 1
`
	if got := strings.Join(exportedNames(src), ","); got != "Exported,Method,T" {
		t.Errorf("names = %q, want the exported funcs, methods and types in source order", got)
	}
	if got := exportedNames("not go at all"); len(got) != 0 {
		t.Errorf("unparseable text gave %v, want no names", got)
	}

	var many strings.Builder
	many.WriteString("package p\n")
	for i := 0; i < maxIdentifiers+5; i++ {
		many.WriteString("func F")
		many.WriteString(strings.Repeat("x", i+1))
		many.WriteString("() {}\n")
	}
	if got := exportedNames(many.String()); len(got) != maxIdentifiers {
		t.Errorf("names = %d, want the cap of %d", len(got), maxIdentifiers)
	}
}

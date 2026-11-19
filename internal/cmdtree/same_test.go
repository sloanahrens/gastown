package cmdtree

import (
	"slices"
	"strings"
	"testing"
)

const gtCall = `exec.Command("gt", "mq", "close")`
const bdCall = `exec.Command("bd", "close", "gt-abc")`
const sameSrc = `package p

func f() {
	` + gtCall + `
	` + bdCall + `
}
`

func TestSameRefs(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, old, new string
		want           bool
	}{
		{"line numbers", sameSrc, "\n\n" + sameSrc, true},
		{"other code", sameSrc, sameSrc + "func g() {}\n", true},
		{"order", sameSrc, strings.Replace(sameSrc, gtCall+"\n\t"+bdCall, bdCall+"\n\t"+gtCall, 1), true},
		{"added invocation", sameSrc, sameSrc + `func g() { exec.Command("gt", "nosuchcmd", "x") }` + "\n", false},
		{"removed invocation", sameSrc, strings.Replace(sameSrc, "\n\t"+bdCall, "", 1), false},
		{"duplicate invocation", sameSrc, sameSrc + "func g() { " + gtCall + " }\n", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := SameRefs("internal/x/x.go", []byte(tc.old), []byte(tc.new))
			if err != nil {
				t.Fatalf("SameRefs returned error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("SameRefs = %v, want %v", got, tc.want)
			}
		})
	}
}

// ScanGo never sets Comment on a Go invocation, so that field is compared here.
func TestSameRefsCommentFlag(t *testing.T) {
	t.Parallel()
	plain := refKeys([]Ref{{Bin: "gt", Words: []string{"mq", "close"}}})
	if slices.Equal(plain, refKeys([]Ref{{Bin: "gt", Words: []string{"mq", "close"}, Comment: true}})) {
		t.Fatal("refs differing only in Comment compared equal")
	}
}

func TestSameRefsScanError(t *testing.T) {
	t.Parallel()
	for _, src := range [][2]string{{"package p\nfunc (", sameSrc}, {sameSrc, "package p\nfunc ("}} {
		if _, err := SameRefs("internal/x/x.go", []byte(src[0]), []byte(src[1])); err == nil {
			t.Fatal("a source ScanGo rejects returned no error")
		}
	}
}

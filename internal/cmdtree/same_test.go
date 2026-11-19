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

// seamSrc holds a bd seam call, which ScanGo reads only under internal/beads/.
const seamSrc = `package beads
func f() { b.run("show", "x") }
`

func TestSameRefsAt(t *testing.T) {
	t.Parallel()
	const plain, seam = "internal/x/x.go", "internal/beads/x.go"
	cases := []struct {
		name             string
		oldFile, newFile string
		old, new         string
		want, wantErr    bool
	}{
		{"the path decides whether the seam call counts", seam, plain, seamSrc, seamSrc, false, false},
		{"two plain paths read the same source alike", plain, "internal/y/y.go", sameSrc, sameSrc, true, false},
		{"identical source under one path", plain, plain, sameSrc, sameSrc, true, false},
		{"old source does not parse", plain, plain, "package p\nfunc (", sameSrc, false, true},
		{"new source does not parse", plain, plain, sameSrc, "package p\nfunc (", false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := SameRefsAt(tc.oldFile, []byte(tc.old), tc.newFile, []byte(tc.new))
			if (err != nil) != tc.wantErr || tc.wantErr && got {
				t.Fatalf("SameRefsAt = %v, err = %v; wantErr %v", got, err, tc.wantErr)
			}
			if !tc.wantErr && got != tc.want {
				t.Fatalf("SameRefsAt = %v, want %v", got, tc.want)
			}
		})
	}
}

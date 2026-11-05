package shellparse

import (
	"errors"
	"strings"
	"testing"

	"mvdan.cc/sh/v3/syntax"
)

// TestParseUnparseableNamesConstructSourcePositionOutOfRange covers nodeRaw's
// position guard. The parser never hands a node outside its source, so the test
// pairs a parsed file with source truncated after the first word. The name
// shares the public table's prefix so one -run filter runs both.
func TestParseUnparseableNamesConstructSourcePositionOutOfRange(t *testing.T) {
	t.Parallel()
	file, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader("echo hello"), "")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	_, err = flatten("echo", file)
	wantUnparseable(t, err, "source position out of range")
}

// TestParseUnparseableNamesConstructFailClosed covers the walk's refusal
// branches that Parse cannot reach from source. The name shares the prefix of
// the public table so one -run TestParseUnparseableNamesConstruct runs both. The bash
// parser cannot produce the node each row hands the walk, so the row calls the
// walk's entry point for that node directly: they are forward-compatibility
// guards against a syntax node this package does not read, and a fail-closed
// answer is what keeps such a node from passing as an empty command.
func TestParseUnparseableNamesConstructFailClosed(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		run  func(f *flattener)
		want string
	}{
		{
			name: "unknown command construct",
			run:  func(f *flattener) { f.command(nil, &Script{}) },
			want: "unrecognized command construct",
		},
		{
			name: "word without source",
			run:  func(f *flattener) { f.word(nil) },
			want: "word without source",
		},
		{
			name: "unrecognized word construct",
			run:  func(f *flattener) { f.word(&syntax.Word{Parts: []syntax.WordPart{nil}}) },
			want: "unrecognized word construct",
		},
		{
			name: "unrecognized word construct in a substitution",
			run:  func(f *flattener) { f.partsSubs([]syntax.WordPart{nil}, &[]Script{}) },
			want: "unrecognized word construct",
		},
		{
			name: "unrecognized arithmetic expression",
			run:  func(f *flattener) { f.arithmSubs(nil, &[]Script{}) },
			want: "unrecognized arithmetic expression",
		},
		{
			name: "unrecognized test expression",
			run:  func(f *flattener) { f.testSubs(nil, &[]Script{}) },
			want: "unrecognized test expression",
		},
		{
			name: "here-document without a body",
			run: func(f *flattener) {
				f.stmt(&syntax.Stmt{Redirs: []*syntax.Redirect{{Op: syntax.Hdoc}}}, &Script{})
			},
			want: "here-document without a body",
		},
		{
			name: "here-document without a delimiter",
			run: func(f *flattener) {
				f.stmt(&syntax.Stmt{Redirs: []*syntax.Redirect{{Op: syntax.Hdoc, Hdoc: &syntax.Word{}}}}, &Script{})
			},
			want: "here-document without a delimiter",
		},
		{
			name: "assignment without a name or value",
			run:  func(f *flattener) { f.assign(nil, &Segment{}) },
			want: "assignment without a name or value",
		},
		{
			name: "array element without a value",
			run: func(f *flattener) {
				f.assign(&syntax.Assign{Array: &syntax.ArrayExpr{Elems: []*syntax.ArrayElem{nil}}}, &Segment{})
			},
			want: "array element without a value",
		},
		{
			name: "quoted here-document with an expansion",
			run: func(f *flattener) {
				f.heredocBody(&syntax.Word{Parts: []syntax.WordPart{&syntax.ParamExp{}}}, true, false)
			},
			want: "quoted here-document with an expansion",
		},
		{
			name: "zero position for a node with content",
			run: func(f *flattener) {
				f.word(&syntax.Word{Parts: []syntax.WordPart{&syntax.Lit{Value: "x"}}})
			},
			want: "source position out of range",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := &flattener{src: ""}
			tt.run(f)
			wantUnparseable(t, f.err, tt.want)
		})
	}
}

// TestConstructOfUnknownError covers constructOf's last return. The bash parser
// reports only syntax.ParseError and syntax.LangError, so an error of another
// type is handed to the classifier directly.
func TestConstructOfUnknownError(t *testing.T) {
	t.Parallel()
	if got := constructOf(errors.New("boom")); got != "unrecognized shell syntax" {
		t.Errorf("constructOf = %q, want %q", got, "unrecognized shell syntax")
	}
}

func wantUnparseable(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("got nil error, want *UnparseableError %q", want)
	}
	var unparseable *UnparseableError
	if !errors.As(err, &unparseable) {
		t.Fatalf("error = %T %v, want *UnparseableError", err, err)
	}
	if unparseable.Construct != want {
		t.Errorf("Construct = %q, want %q", unparseable.Construct, want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Errorf("error %q does not name %q", err.Error(), want)
	}
}

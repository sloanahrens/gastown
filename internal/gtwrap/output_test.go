package gtwrap

import (
	"bytes"
	"errors"
	"testing"
)

// The two roots the rewriting tests use: the pod's, from pod_town_root, and the
// Mac's, the town the wrapper maps onto.
const (
	rewritePodRoot = "/town"
	rewriteTown    = "/Users/sloan/gt"
)

// TestOutputRewritesThePodRoot: a line that holds the pod town root is rewritten to
// the Mac town root, only where the root names a whole path segment, and a complete
// line is written as it arrives rather than held until the stream ends.
func TestOutputRewritesThePodRoot(t *testing.T) {
	t.Parallel()
	cases := []struct{ name, line, want string }{
		{"a path under the root", rewritePodRoot + "/rig/polecats/coma\n", rewriteTown + "/rig/polecats/coma\n"},
		{"the root at the end of a line", "the town is " + rewritePodRoot + "\n", "the town is " + rewriteTown + "\n"},
		{"the root followed by punctuation", "(" + rewritePodRoot + "), see " + rewritePodRoot + ":\n", "(" + rewriteTown + "), see " + rewriteTown + ":\n"},
		{"several roots on one line", rewritePodRoot + "/a and " + rewritePodRoot + "/b\n", rewriteTown + "/a and " + rewriteTown + "/b\n"},
		{"a longer directory name is not the root", rewritePodRoot + "ship/x\n", rewritePodRoot + "ship/x\n"},
		{"a path whose last segment starts with the root is not the root", "/srv" + rewritePodRoot + "/x\n", "/srv" + rewritePodRoot + "/x\n"},
		{"a root after a slash is not the root", "https://example.test" + rewritePodRoot + "/x\n", "https://example.test" + rewritePodRoot + "/x\n"},
		{"a line with no root passes unchanged", "nothing to rewrite here\n", "nothing to rewrite here\n"},
		{"a last line without a newline", rewritePodRoot + "/rig", rewriteTown + "/rig"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var out bytes.Buffer
			r := newOutputRewriter(&out, rewritePodRoot, rewriteTown, false)
			if _, err := r.Write([]byte(tt.line)); err != nil {
				t.Fatalf("Write(%q) = %v", tt.line, err)
			}
			if err := r.Flush(); err != nil {
				t.Fatalf("Flush = %v", err)
			}
			if got := out.String(); got != tt.want {
				t.Fatalf("output = %q, want %q", got, tt.want)
			}
		})
	}
	t.Run("equal roots rewrite nothing", func(t *testing.T) {
		t.Parallel()
		var out bytes.Buffer
		r := newOutputRewriter(&out, rewriteTown, rewriteTown, false)
		if _, err := r.Write([]byte(rewriteTown + "/rig\n")); err != nil {
			t.Fatalf("Write = %v", err)
		}
		if err := r.Flush(); err != nil {
			t.Fatalf("Flush = %v", err)
		}
		if got, want := out.String(), rewriteTown+"/rig\n"; got != want {
			t.Fatalf("output = %q, want the line unchanged %q", got, want)
		}
	})
	t.Run("a complete line is written before the stream ends", func(t *testing.T) {
		t.Parallel()
		var out bytes.Buffer
		r := newOutputRewriter(&out, rewritePodRoot, rewriteTown, false)
		if _, err := r.Write([]byte("see " + rewritePodRoot + "/rig\n")); err != nil {
			t.Fatalf("Write = %v", err)
		}
		// No Flush yet: the line ended, so a reader has it.
		if got, want := out.String(), "see "+rewriteTown+"/rig\n"; got != want {
			t.Fatalf("output before the stream ends = %q, want %q", got, want)
		}
		// A partial line is the only thing the rewriter holds back.
		if _, err := r.Write([]byte("half a " + rewritePodRoot)); err != nil {
			t.Fatalf("Write = %v", err)
		}
		if got := out.String(); got != "see "+rewriteTown+"/rig\n" {
			t.Fatalf("output with an unterminated line = %q, want the line not yet written", got)
		}
	})
}

// TestOutputPromptModeWritesThePromptBeforeTheStreamEnds: in prompt mode an
// unterminated line with no part of the pod root in it is written at once, so a
// prompt the tool prints without a newline reaches the terminal before it waits.
func TestOutputPromptModeWritesThePromptBeforeTheStreamEnds(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	r := newOutputRewriter(&out, rewritePodRoot, rewriteTown, true)
	if _, err := r.Write([]byte("continue? ")); err != nil {
		t.Fatalf("Write = %v", err)
	}
	if got, want := out.String(), "continue? "; got != want {
		t.Fatalf("output before the stream ends = %q, want the prompt %q", got, want)
	}
}

// TestOutputPromptModeRewritesAnUnterminatedLine: prompt mode rewrites the root it
// can decide, rather than passing the line through to get it out early.
func TestOutputPromptModeRewritesAnUnterminatedLine(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	r := newOutputRewriter(&out, rewritePodRoot, rewriteTown, true)
	if _, err := r.Write([]byte(rewritePodRoot + "/rig> ")); err != nil {
		t.Fatalf("Write = %v", err)
	}
	if got, want := out.String(), rewriteTown+"/rig> "; got != want {
		t.Fatalf("output before the stream ends = %q, want the rewritten prompt %q", got, want)
	}
}

// TestOutputPromptModeHoldsTheRootAtTheEndOfTheBuffer: the byte after a root that
// ends the buffer decides whether it names the root, so prompt mode writes what is
// before it and holds the root until the next write.
func TestOutputPromptModeHoldsTheRootAtTheEndOfTheBuffer(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	r := newOutputRewriter(&out, rewritePodRoot, rewriteTown, true)
	if _, err := r.Write([]byte("half a " + rewritePodRoot)); err != nil {
		t.Fatalf("Write = %v", err)
	}
	if got, want := out.String(), "half a "; got != want {
		t.Fatalf("output with the root ending the buffer = %q, want %q", got, want)
	}
	if _, err := r.Write([]byte("/rig\n")); err != nil {
		t.Fatalf("Write = %v", err)
	}
	if got, want := out.String(), "half a "+rewriteTown+"/rig\n"; got != want {
		t.Fatalf("output after the line ended = %q, want %q", got, want)
	}
}

// TestOutputPromptModeHoldsAPartialRoot: a suffix shorter than the root is written
// up to its start and held, so a root split across two writes is still rewritten.
func TestOutputPromptModeHoldsAPartialRoot(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	r := newOutputRewriter(&out, rewritePodRoot, rewriteTown, true)
	if _, err := r.Write([]byte("the town " + rewritePodRoot[:3])); err != nil {
		t.Fatalf("Write = %v", err)
	}
	if got, want := out.String(), "the town "; got != want {
		t.Fatalf("output with a partial root = %q, want %q", got, want)
	}
	if _, err := r.Write([]byte(rewritePodRoot[3:] + "/rig\n")); err != nil {
		t.Fatalf("Write = %v", err)
	}
	if got, want := out.String(), "the town "+rewriteTown+"/rig\n"; got != want {
		t.Fatalf("output after the root completed = %q, want %q", got, want)
	}
}

// TestOutputPromptModeHoldsAMatchCrossingTheHeldSuffix: a root that repeats its own
// prefix (/x/x) can begin before the held suffix; the occurrence is held from its
// own start, so splitting the stream does not lose the rewrite.
func TestOutputPromptModeHoldsAMatchCrossingTheHeldSuffix(t *testing.T) {
	t.Parallel()
	const root = "/x/x"
	var out bytes.Buffer
	r := newOutputRewriter(&out, root, "/mac", true)
	if _, err := r.Write([]byte(root + "/")); err != nil {
		t.Fatalf("Write = %v", err)
	}
	// The suffix "/x/" could grow into the root, and the root at the start of the
	// line ends inside it, so nothing is decided yet.
	if got := out.String(); got != "" {
		t.Fatalf("output with a match crossing the held suffix = %q, want nothing written", got)
	}
	if _, err := r.Write([]byte("\n")); err != nil {
		t.Fatalf("Write = %v", err)
	}
	if got, want := out.String(), "/mac/\n"; got != want {
		t.Fatalf("output after the line ended = %q, want %q", got, want)
	}
}

// TestOutputPromptModeKeepsTheLeftContextOfAHeldRoot: the byte written before a
// held root is its left context, so a relative path such as x/town is not rewritten
// as if the held root began a line.
func TestOutputPromptModeKeepsTheLeftContextOfAHeldRoot(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	r := newOutputRewriter(&out, rewritePodRoot, rewriteTown, true)
	if _, err := r.Write([]byte("x" + rewritePodRoot[:3])); err != nil {
		t.Fatalf("Write = %v", err)
	}
	if got, want := out.String(), "x"; got != want {
		t.Fatalf("output with a partial root after a path byte = %q, want %q", got, want)
	}
	if _, err := r.Write([]byte(rewritePodRoot[3:] + "\n")); err != nil {
		t.Fatalf("Write = %v", err)
	}
	if got, want := out.String(), "x"+rewritePodRoot+"\n"; got != want {
		t.Fatalf("output after the line ended = %q, want the relative path %q", got, want)
	}
}

// TestOutputRewriteLeavesBinaryAlone: a stream that carries a NUL byte is binary, so
// it is copied unchanged, though it holds text the rewriter would otherwise rewrite.
// The bytes are fed one at a time, so detection cannot depend on where a write ends.
func TestOutputRewriteLeavesBinaryAlone(t *testing.T) {
	t.Parallel()
	stream := []byte("\x89PNG\r\n\x1a\n" + rewritePodRoot + "/rig\x00\x01" + rewritePodRoot + "/more\n" + rewritePodRoot + "/last\n")
	var out bytes.Buffer
	r := newOutputRewriter(&out, rewritePodRoot, rewriteTown, false)
	for i := range stream {
		if _, err := r.Write(stream[i : i+1]); err != nil {
			t.Fatalf("Write byte %d = %v", i, err)
		}
	}
	if err := r.Flush(); err != nil {
		t.Fatalf("Flush = %v", err)
	}
	if !bytes.Equal(out.Bytes(), stream) {
		t.Fatalf("output = %q, want the binary stream unchanged %q", out.Bytes(), stream)
	}
}

// errorWriter is a destination whose every write fails.
type errorWriter struct{ err error }

func (w errorWriter) Write([]byte) (int, error) { return 0, w.err }

// TestOutputReportsAWriteFailure: a destination that cannot take the output is
// reported from every path that writes to it, so a stream lost at the terminal is
// never taken for a stream delivered.
func TestOutputReportsAWriteFailure(t *testing.T) {
	t.Parallel()
	boom := errors.New("the terminal is gone")
	cases := []struct {
		name   string
		prompt bool
		writes [][]byte
	}{
		{name: "a completed line", writes: [][]byte{[]byte(rewritePodRoot + "/rig\n")}},
		{name: "a pending line when the stream turns binary", writes: [][]byte{[]byte(rewritePodRoot + "/rig"), []byte("\x00")}},
		{name: "the last line at the end of the stream", writes: [][]byte{[]byte(rewritePodRoot + "/rig")}},
		{name: "a prompt written before the stream ends", prompt: true, writes: [][]byte{[]byte("continue? ")}},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := newOutputRewriter(errorWriter{err: boom}, rewritePodRoot, rewriteTown, tt.prompt)
			var got error
			for _, p := range tt.writes {
				if _, err := r.Write(p); err != nil {
					got = err
					break
				}
			}
			if got == nil {
				got = r.Flush()
			}
			if !errors.Is(got, boom) {
				t.Fatalf("the failed write = %v, want %v", got, boom)
			}
		})
	}
}

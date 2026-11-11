package shellparse

import (
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

// memoCorpus holds commands that fill every slice field of the Script types:
// words, assignments, redirections, here-documents, substitutions of both kinds,
// a process substitution, shell payload indexes, and commands the parser refuses.
var memoCorpus = []string{
	"echo hi",
	"A=1 B=2 env | sort > out 2>&1",
	"bd close gt-x && echo $(bd close gt-y) `bd close gt-z`",
	"cat <<EOF\nbody $(echo in)\nEOF\n",
	"cat <<'EOF' >f\nraw\nEOF\n",
	"diff <(sort a) >(tee b)",
	"for i in $(ls); do echo $i; done",
	"case $(x) in a) echo ;; esac",
	"sh -c 'bd close gt-x'; eval \"echo $((1+2))\"",
	"let y=1<<x",
	"echo \"unterminated",
	"bd close gt-x && (",
	"",
}

// TestMemoServesWhatParseReturns: for each command, the memoized result equals a
// fresh parse, on the first call and on the repeat, error included.
func TestMemoServesWhatParseReturns(t *testing.T) {
	t.Parallel()
	m := &parseMemo{results: map[string]memoResult{}}
	for _, command := range memoCorpus {
		want, wantErr := parseUncached(command)
		for call := 1; call <= 2; call++ {
			got, err := m.parse(command, parseUncached)
			if !reflect.DeepEqual(got, want) || !reflect.DeepEqual(err, wantErr) {
				t.Errorf("call %d for %q = %+v, %v; want %+v, %v", call, command, got, err, want, wantErr)
			}
		}
	}
}

// TestMemoCallersDoNotShareWhatTheyEdit: a caller that overwrites every slice
// element of its result leaves the next caller's result as a fresh parse gives it.
func TestMemoCallersDoNotShareWhatTheyEdit(t *testing.T) {
	t.Parallel()
	m := &parseMemo{results: map[string]memoResult{}}
	for _, command := range memoCorpus {
		first, err := m.parse(command, parseUncached)
		if err != nil {
			continue
		}
		scribble(reflect.ValueOf(&first).Elem())
		want, _ := parseUncached(command)
		got, _ := m.parse(command, parseUncached)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("an edit to the first result of %q reached the second: %+v, want %+v", command, got, want)
		}
	}
}

// scribble overwrites every string, int and bool reachable through slices in v.
func scribble(v reflect.Value) {
	switch v.Kind() {
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			scribble(v.Field(i))
		}
	case reflect.Slice:
		for i := 0; i < v.Len(); i++ {
			scribble(v.Index(i))
		}
	case reflect.String:
		if v.CanSet() {
			v.SetString("scribbled")
		}
	case reflect.Int:
		if v.CanSet() {
			v.SetInt(-1)
		}
	case reflect.Bool:
		if v.CanSet() {
			v.SetBool(!v.Bool())
		}
	}
}

// TestScriptTypesHoldNothingTheCloneSkips: every field of the Script types is a
// scalar, or one of the slice fields cloneScript copies. A new slice, map or
// pointer field fails here until the clone handles it.
func TestScriptTypesHoldNothingTheCloneSkips(t *testing.T) {
	t.Parallel()
	handled := map[string]bool{
		"Script.Segments": true, "Script.Subs": true,
		"Segment.Words": true, "Segment.Assigns": true, "Segment.Redirs": true,
		"Segment.Heredocs": true, "Segment.Subs": true, "Segment.Shells": true,
		"Word.ProcSubs": true,
	}
	for _, typ := range []reflect.Type{
		reflect.TypeOf(Script{}), reflect.TypeOf(Segment{}), reflect.TypeOf(Word{}),
		reflect.TypeOf(ProcSubst{}), reflect.TypeOf(Redirection{}), reflect.TypeOf(Heredoc{}),
	} {
		for i := 0; i < typ.NumField(); i++ {
			f := typ.Field(i)
			switch f.Type.Kind() {
			case reflect.Slice, reflect.Map, reflect.Pointer, reflect.Interface, reflect.Func, reflect.Chan:
				if !handled[typ.Name()+"."+f.Name] {
					t.Errorf("%s.%s is a %s: copy it in cloneScript, then list it here", typ.Name(), f.Name, f.Type.Kind())
				}
			}
		}
	}
}

// TestMemoParsesARepeatOnce: a repeated command is parsed once; a command over the
// size cap is parsed every time; a full memo drops its entries and keeps working.
func TestMemoParsesARepeatOnce(t *testing.T) {
	t.Parallel()
	var calls atomic.Int64
	count := func(c string) (Script, error) { calls.Add(1); return parseUncached(c) }
	m := &parseMemo{results: map[string]memoResult{}}
	for i := 0; i < 3; i++ {
		if _, err := m.parse("echo hi", count); err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 1 {
		t.Errorf("a command asked for three times was parsed %d times, want 1", calls.Load())
	}
	big := "echo " + strings.Repeat("x", memoMaxCommand)
	calls.Store(0)
	for i := 0; i < 2; i++ {
		if _, err := m.parse(big, count); err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 2 {
		t.Errorf("a command over the cap was parsed %d times in two asks, want 2", calls.Load())
	}
	calls.Store(0)
	for i := 0; i < memoMaxEntries+1; i++ {
		if _, err := m.parse("echo "+strings.Repeat("a", i%7)+string(rune('a'+i%26))+strings.Repeat("b", i), count); err != nil {
			t.Fatal(err)
		}
	}
	if len(m.results) > memoMaxEntries {
		t.Errorf("the memo holds %d entries, cap %d", len(m.results), memoMaxEntries)
	}
}

package liveness

import "testing"

const cutRecord = `{"type":"assistant","isApiErrorMessage":true,"streamCutCause":"stale_connection"}`

func TestStreamCutTail(t *testing.T) {
	t.Parallel()
	for _, tail := range []string{cutRecord + "\n", `{"type":"assis` + "\n" + cutRecord + "\n", cutRecord + "\n" + `{"type":"system"}` + "\n"} {
		if !StreamCutTail([]byte(tail)) {
			t.Errorf("%q: not a cut", tail)
		}
	}
}

func TestStreamCutTailFalseCases(t *testing.T) {
	t.Parallel()
	for _, tail := range []string{
		cutRecord + "\n" + `{"type":"user"}` + "\n", cutRecord + "\n" + `{"type":"assistant"}` + "\n",
		`{"type":"assistant","message":{"content":[{"type":"tool_use"}]}}` + "\n", `{"type":"assistant",` + "\n",
		"", `{"type":"assistant","isApiErrorMessage":true}` + "\n", cutRecord,
		`{"type":"user","isApiErrorMessage":true,"streamCutCause":"stale_connection"}` + "\n", `{"type":"assistant","streamCutCause":"stale_connection"}` + "\n",
	} {
		if StreamCutTail([]byte(tail)) {
			t.Fatalf("%q: a non-cut tail read as a cut", tail)
		}
	}
}

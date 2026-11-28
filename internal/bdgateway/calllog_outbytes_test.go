package bdgateway

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestCallLogOutBytes: a record's StdoutBytes is written as out_bytes and read
// back equal, 0 is written rather than omitted, nil omits the key, an old line
// decodes to nil, and a served call's line carries the executor's figure.
func TestCallLogOutBytes(t *testing.T) {
	t.Parallel()
	seven, zero := int64(7), int64(0)
	cases := []struct {
		name string
		line string
		rec  CallRecord
	}{
		{"figure", "", CallRecord{Argv: []string{"show", "gt-abc"}, StdoutBytes: &seven}},
		{"zero", "", CallRecord{Argv: []string{"list"}, StdoutBytes: &zero}},
		{"none", "", CallRecord{Argv: []string{"list"}}},
		{"old line", `{"ts":"2026-11-13T12:00:00Z","class":"read","caller":"old","verb":"show","flags":[],"beads":[],"exit_code":0,"duration_ms":5}`, CallRecord{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			opts := testOptions(t)
			if tc.line == "" {
				log := newTestCallLogWithDir(t, opts.Dir)
				log.Log(tc.rec)
				if err := log.Close(); err != nil {
					t.Fatalf("Close: %v", err)
				}
			} else if err := os.WriteFile(filepath.Join(opts.Dir, DefaultCallLogName), []byte(tc.line+"\n"), 0o644); err != nil {
				t.Fatalf("write old line: %v", err)
			}
			line := readLines(t, filepath.Join(opts.Dir, DefaultCallLogName))[0]
			if present := strings.Contains(line, `"out_bytes"`); present != (tc.rec.StdoutBytes != nil) {
				t.Fatalf("line carries out_bytes = %v, want %v: %s", present, tc.rec.StdoutBytes != nil, line)
			}
			entries, err := ReadCalls(opts.Dir, time.Time{}, 10)
			if err != nil || len(entries) != 1 {
				t.Fatalf("ReadCalls = %d entries, %v; want 1", len(entries), err)
			}
			if got := outBytesText(entries[0].StdoutBytes); got != outBytesText(tc.rec.StdoutBytes) {
				t.Fatalf("read out_bytes = %v, want %v", got, outBytesText(tc.rec.StdoutBytes))
			}
		})
	}

	t.Run("served call", func(t *testing.T) {
		t.Parallel()
		runner := &fakeRunner{queue: []Process{newExitedFake(0, "hello world", "")}}
		log := newTestCallLog(t)
		g := newTestGateway(t, mustStore(t), log, newTestExecutor(t, testWorkspace(t), runner))
		if status := postExec(t, g, testReadToken, Request{Argv: []string{"show", "gt-abc"}, Rig: "gastown"}); status != http.StatusOK {
			t.Fatalf("served call status = %d, want 200", status)
		}
		lines := closedCallLines(t, log, log.Dir())
		if len(lines) != 1 {
			t.Fatalf("call log lines = %d, want 1", len(lines))
		}
		if e := decodeEntry(t, lines[0]); outBytesText(e.StdoutBytes) != int64(11) {
			t.Fatalf("served call out_bytes = %v, want 11", outBytesText(e.StdoutBytes))
		}
	})
}

// outBytesText renders an out_bytes value for a failure message.
func outBytesText(p *int64) any {
	if p == nil {
		return "none"
	}
	return *p
}

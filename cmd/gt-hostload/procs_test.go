package main

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/hostload"
)

// scannerPath is the command of the antivirus scanner's daemon, as ps prints it.
const scannerPath = "/Applications/Bitdefender/CoreSecurity.app/Contents/Daemons/BDLDaemon.app/Contents/MacOS/BDLDaemon"

// procsSample is a ps listing of the scanner's daemon at 2:05.25 and a shell at 0:01.50 (gt-efoycy).
const procsSample = " 1023 2:05.25 " + scannerPath + "\n  42 0:01.50 /bin/sh -c sleep 30\n"

// psRead is a fake read that answers with text or err and records the call readProcs made.
type psRead struct {
	timeout time.Duration
	name    string
	args    []string
	text    string
	err     error
}

func (r *psRead) read(timeout time.Duration, name string, args ...string) (string, error) {
	r.timeout, r.name, r.args = timeout, name, args
	return r.text, r.err
}

// TestHostCPUReadProcs reads every process in the ps listing, keeping a command's spaces.
func TestHostCPUReadProcs(t *testing.T) {
	t.Parallel()
	r := &psRead{text: procsSample}
	procs, err := readProcs(r.read)
	if err != nil {
		t.Fatalf("readProcs: %v", err)
	}
	want := []hostload.Proc{
		{PID: 1023, CPUTime: 125.25, Command: scannerPath},
		{PID: 42, CPUTime: 1.5, Command: "/bin/sh -c sleep 30"},
	}
	if !reflect.DeepEqual(procs, want) {
		t.Errorf("readProcs = %+v: want %+v", procs, want)
	}
}

// TestHostCPUReadProcsRefuses names ps for a failed read, an empty listing and
// a listing ParseProcs cannot parse, and returns no processes for any of them.
func TestHostCPUReadProcsRefuses(t *testing.T) {
	t.Parallel()
	boom := errors.New("exit status 1")
	for _, c := range []struct {
		why  string
		read error
		text string
	}{
		{"failed read", boom, ""},
		{"empty listing", nil, ""},
		{"unparsable", nil, "1023 103474.27 " + scannerPath + "\n"},
	} {
		r := &psRead{text: c.text, err: c.read}
		procs, err := readProcs(r.read)
		if procs != nil || err == nil || !strings.HasPrefix(err.Error(), "ps: ") || (c.read != nil && !errors.Is(err, c.read)) {
			t.Errorf("%s: readProcs = %+v, %v: want no processes and an error starting \"ps: \"", c.why, procs, err)
		}
	}
}

// TestHostCPUReadProcsRecordsCall: ps runs as ps -axo pid=,time=,comm= under a 10 s timeout.
func TestHostCPUReadProcsRecordsCall(t *testing.T) {
	t.Parallel()
	r := &psRead{text: procsSample}
	if _, err := readProcs(r.read); err != nil {
		t.Fatalf("readProcs: %v", err)
	}
	if r.timeout != 10*time.Second || r.name != "ps" || !reflect.DeepEqual(r.args, []string{"-axo", "pid=,time=,comm="}) {
		t.Errorf("read called with timeout %v, %q %q; want 10s, ps -axo pid=,time=,comm=", r.timeout, r.name, r.args)
	}
}

// TestHostCPUReadProcsFindsScanner: the scanner in the listing readProcs returns is found with its seconds.
func TestHostCPUReadProcsFindsScanner(t *testing.T) {
	t.Parallel()
	r := &psRead{text: procsSample}
	procs, err := readProcs(r.read)
	if err != nil {
		t.Fatalf("readProcs: %v", err)
	}
	if sec, found := scannerSeconds(procs); sec != 125.25 || !found {
		t.Errorf("scannerSeconds = %v, %v: want 125.25, true", sec, found)
	}
}

// TestHostCPUReadProcsScanner sums the seconds of each scanner daemon, and
// reports not found when no process ends /BDLDaemon, including a failed read's nil list.
func TestHostCPUReadProcsScanner(t *testing.T) {
	t.Parallel()
	shell := hostload.Proc{PID: 42, CPUTime: 1.5, Command: "/bin/sh -c sleep 30"}
	one := hostload.Proc{PID: 1023, CPUTime: 10.5, Command: scannerPath}
	two := hostload.Proc{PID: 1024, CPUTime: 62.25, Command: scannerPath}
	for _, c := range []struct {
		why   string
		procs []hostload.Proc
		want  float64
		found bool
	}{
		{"one scanner", []hostload.Proc{shell, one}, 10.5, true},
		{"two scanners add", []hostload.Proc{one, shell, two}, 72.75, true},
		{"no scanner", []hostload.Proc{shell}, 0, false},
		{"failed read leaves no list", nil, 0, false},
		{"name without the slash", []hostload.Proc{{PID: 7, CPUTime: 5, Command: "BDLDaemon"}}, 0, false},
	} {
		got, found := scannerSeconds(c.procs)
		if got != c.want || found != c.found {
			t.Errorf("%s: scannerSeconds = %v, %v: want %v, %v", c.why, got, found, c.want, c.found)
		}
	}
}

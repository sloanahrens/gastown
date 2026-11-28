package hostload

import (
	"strings"
	"testing"
)

// procsSample holds the real ps line, whose 1724:34.27 is 103474.27 seconds, and a command with spaces.
const procsSample = ` 1023 1724:34.27 /Applications/Bitdefender/CoreSecurity.app/Contents/Daemons/BDLDaemon.app/Contents/MacOS/BDLDaemon
  42 0:01.50 /bin/sh -c sleep 30
`

// TestHostCPUParseProcs reads the sample, keeping a command's spaces and the minutes of the time.
func TestHostCPUParseProcs(t *testing.T) {
	t.Parallel()
	want := []Proc{
		{PID: 1023, CPUTime: 103474.27, Command: "/Applications/Bitdefender/CoreSecurity.app/Contents/Daemons/BDLDaemon.app/Contents/MacOS/BDLDaemon"},
		{PID: 42, CPUTime: 1.5, Command: "/bin/sh -c sleep 30"},
	}
	procs, err := ParseProcs(procsSample)
	if err != nil || len(procs) != len(want) {
		t.Fatalf("ParseProcs = %+v, %v: want %d processes", procs, err, len(want))
	}
	for i, w := range want {
		if procs[i] != w {
			t.Errorf("ParseProcs[%d] = %+v: want %+v", i, procs[i], w)
		}
	}
}

// TestHostCPUParseProcsRefuses holds one row for each refusal ParseProcs makes.
func TestHostCPUParseProcsRefuses(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ text, want string }{
		{"", "empty"},
		{"1023 1:02.50 /bin/sleep\n\n1024 1:02.50 /bin/sleep\n", "pid, time and command"},
		{"1023 1:02.50\n", "pid, time and command"},
		{"abc 1:02.50 /bin/sleep\n", "pid is not a number"},
		{"1023 103474.27 /bin/sleep\n", "minutes:seconds"},
		{"1023 1:02:03.00 /bin/sleep\n", "minutes:seconds"},
		{"1023 x:02.50 /bin/sleep\n", "minutes:seconds"},
		{"1023 1:x /bin/sleep\n", "minutes:seconds"},
		{"1023 -5:30.00 /bin/sleep\n", "negative"},
		{"1023 5:-30.00 /bin/sleep\n", "negative"},
		{"1023 5:NaN /bin/sleep\n", "not finite"},
		{"1023 5:+Inf /bin/sleep\n", "not finite"},
	} {
		if _, err := ParseProcs(c.text); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("ParseProcs(%q) = %v: want an error naming %q", c.text, err, c.want)
		}
	}
}

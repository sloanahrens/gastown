package doltserver

import (
	"strconv"
	"strings"
	"testing"
)

func TestCheckPortAvailable_Free(t *testing.T) {
	t.Parallel()
	if err := newFakeHost().host().checkPortAvailable(4530); err != nil {
		t.Errorf("checkPortAvailable(4530) returned error for free port: %v", err)
	}
}

// A busy port is refused with advice, and the holder named when one can be.
func TestCheckPortAvailable_InUse(t *testing.T) {
	t.Parallel()
	f := newFakeHost()
	holder := f.spawn(fakeProc{args: dockerArgs, port: 4531})

	err := f.host().checkPortAvailable(4531)
	if err == nil {
		t.Fatal("checkPortAvailable(4531) returned nil for in-use port")
	}
	for _, want := range []string{"port 4531 is already in use", "Port is held by PID " + strconv.Itoa(holder), "gt config set dolt.port"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
}

// FindFreePort skips busy ports.
func TestFindFreePort(t *testing.T) {
	t.Parallel()
	f := newFakeHost()
	f.spawn(fakeProc{port: 4532})
	f.spawn(fakeProc{port: 4533})
	if got := f.host().FindFreePort(4532); got != 4534 {
		t.Errorf("FindFreePort(4532) = %d, want 4534", got)
	}
}

package main

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

// fakeLsof answers lsof with text and err and records the call it saw.
type fakeLsof struct {
	text, name string
	err        error
	timeout    time.Duration
	args       []string
}

func (f *fakeLsof) read(timeout time.Duration, name string, args ...string) (string, error) {
	f.timeout, f.name, f.args = timeout, name, args
	return f.text, f.err
}

// TestHostCPUVMName names the VM an lsof output shows, or none, with no error.
func TestHostCPUVMName(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ text, want string }{
		{"... /Users/me/.colima/_lima/colima-gt-sbx/disk", "sbx"},
		{"... /usr/lib/dyld", ""},
	} {
		got, err := lsofVM(4242, (&fakeLsof{text: c.text}).read)
		if err != nil || got != c.want {
			t.Errorf("lsofVM(text %q) = %q, %v; want %q, nil", c.text, got, err, c.want)
		}
	}
}

// TestHostCPUVMNameFailedLsof: a failed lsof names the pid in its error.
func TestHostCPUVMNameFailedLsof(t *testing.T) {
	t.Parallel()
	got, err := lsofVM(4242, (&fakeLsof{err: errors.New("exit status 1")}).read)
	if got != "" || err == nil || !strings.Contains(err.Error(), "4242") {
		t.Errorf("lsofVM(failed) = %q, %v; want empty name and an error naming pid 4242", got, err)
	}
}

// TestHostCPUVMNameRecordsCall: lsof runs as lsof -p pid under a 10 s timeout.
func TestHostCPUVMNameRecordsCall(t *testing.T) {
	t.Parallel()
	f := &fakeLsof{}
	if _, err := lsofVM(4242, f.read); err != nil {
		t.Fatalf("lsofVM: %v", err)
	}
	if f.timeout != 10*time.Second || f.name != "lsof" || !reflect.DeepEqual(f.args, []string{"-p", "4242"}) {
		t.Errorf("read called with timeout %v, %q %q; want 10s, lsof -p 4242", f.timeout, f.name, f.args)
	}
}

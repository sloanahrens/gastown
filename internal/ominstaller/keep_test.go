package ominstaller

import (
	"errors"
	"io/fs"
	"reflect"
	"testing"
)

const (
	installedOM = "/home/op/go/bin/om"
	keptPrev    = "/home/op/go/bin/om.prev"
	keepTmp     = "/home/op/go/bin/.om.prev.tmp"
	installed   = "installed om bytes"
	oldPrev     = "previous om bytes"
)

func TestKeepPreviousCopiesAtomically(t *testing.T) {
	t.Parallel()
	files := newFakeFiles()
	files.files[installedOM] = []byte(installed)
	files.modes[installedOM] = 0o755
	logs := &logRecorder{}
	cfg := Config{Files: files, Logf: logs.logf}

	kept, err := keepPrevious(cfg, installedOM)
	if err != nil {
		t.Fatalf("keepPrevious = %v, want nil", err)
	}
	if !kept {
		t.Error("keepPrevious kept = false, want true: this run kept the om it replaces")
	}
	if got := string(files.files[keptPrev]); got != installed {
		t.Errorf("om.prev = %q, want the installed om's bytes", got)
	}
	if mode := files.modes[keptPrev]; mode != 0o755 {
		t.Errorf("om.prev mode = %#o, want the installed om's 0755", mode)
	}
	if _, ok := files.files[keepTmp]; ok {
		t.Error("the temp copy is still on disk after the rename")
	}
	// The copy lands at the temp name and is renamed to om.prev; nothing writes
	// om.prev in place, so a reader never sees a partial om.prev.
	if want := []string{keepTmp}; !reflect.DeepEqual(files.writes, want) {
		t.Errorf("writes = %q, want %q", files.writes, want)
	}
	if want := []string{keepTmp + " -> " + keptPrev}; !reflect.DeepEqual(files.renames, want) {
		t.Errorf("renames = %q, want %q", files.renames, want)
	}
	if len(logs.lines) != 1 || logs.lines[0] != "[install-om] kept the previous om as "+keptPrev {
		t.Errorf("log = %q, want the kept notice", logs.lines)
	}
}

func TestKeepPreviousFailedCopyLeavesNoPartialOM(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		setup func(f *fakeFiles)
	}{
		{
			name: "write dies mid-copy",
			setup: func(f *fakeFiles) {
				f.writeErr = errBoom
				f.partial = []byte("instal")
			},
		},
		{
			name:  "chmod fails",
			setup: func(f *fakeFiles) { f.chmodErr = errBoom },
		},
		{
			name:  "rename fails",
			setup: func(f *fakeFiles) { f.renameErr = errBoom },
		},
		{
			name:  "installed om cannot be read",
			setup: func(f *fakeFiles) { f.readErr[installedOM] = errBoom },
		},
		{
			name:  "installed om cannot be looked at",
			setup: func(f *fakeFiles) { f.statErr[installedOM] = errBoom },
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			files := newFakeFiles()
			files.files[installedOM] = []byte(installed)
			files.modes[installedOM] = 0o755
			tc.setup(files)
			cfg := Config{Files: files}

			kept, err := keepPrevious(cfg, installedOM)
			if !errors.Is(err, errKeepPrevious) {
				t.Fatalf("keepPrevious = %v, want errKeepPrevious", err)
			}
			if kept {
				t.Error("keepPrevious kept = true after a failed copy, want false")
			}
			if _, ok := files.files[keptPrev]; ok {
				t.Errorf("om.prev = %q after a failed copy, want none", files.files[keptPrev])
			}
			if _, ok := files.files[keepTmp]; ok {
				t.Error("a failed copy left its temp file behind")
			}
		})
	}
}

func TestKeepPreviousFailedCopyKeepsTheEarlierPrev(t *testing.T) {
	t.Parallel()
	files := newFakeFiles()
	files.files[installedOM] = []byte(installed)
	files.files[keptPrev] = []byte(oldPrev)
	files.writeErr = errBoom
	files.partial = []byte("instal")

	if _, err := keepPrevious(Config{Files: files}, installedOM); !errors.Is(err, errKeepPrevious) {
		t.Fatalf("keepPrevious = %v, want errKeepPrevious", err)
	}
	if got := string(files.files[keptPrev]); got != oldPrev {
		t.Errorf("om.prev = %q after a failed copy, want the earlier om.prev %q untouched", got, oldPrev)
	}
}

func TestKeepPreviousFirstInstallKeepsNothing(t *testing.T) {
	t.Parallel()
	files := newFakeFiles()
	files.statErr[installedOM] = &fs.PathError{Op: "stat", Path: installedOM, Err: fs.ErrNotExist}
	logs := &logRecorder{}
	cfg := Config{Files: files, Logf: logs.logf}

	kept, err := keepPrevious(cfg, installedOM)
	if err != nil {
		t.Fatalf("keepPrevious = %v, want nil on a first install", err)
	}
	if kept {
		t.Error("keepPrevious kept = true on a first install, want false")
	}
	if len(files.writes) != 0 || len(files.renames) != 0 {
		t.Errorf("wrote %q and renamed %q on a first install, want nothing", files.writes, files.renames)
	}
	if len(logs.lines) != 1 || logs.lines[0] != "[install-om] no om at "+installedOM+" to keep: first install, so there is nothing to roll back to" {
		t.Errorf("log = %q, want the first-install notice", logs.lines)
	}
}

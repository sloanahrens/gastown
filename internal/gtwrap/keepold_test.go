package gtwrap

import (
	"bytes"
	"errors"
	"io/fs"
	"strings"
	"syscall"
	"testing"
)

func TestInstallKeepOld(t *testing.T) {
	t.Parallel()
	keep := "/Users/operator/go/bin/om-native"
	withKeep := func(path string) []Target {
		targets := fourTargets()
		targets[2].KeepOld = path
		return targets
	}

	t.Run("gives the old file a second name that survives rollback", func(t *testing.T) {
		t.Parallel()
		targets := withKeep(keep)
		om := targets[2]
		files := cutoverFiles(targets)
		var out bytes.Buffer
		if err := Install(installConfig(files, &fakeDoctor{}, &out, targets)); err != nil {
			t.Fatalf("Install: %v\n%s", err, out.String())
		}
		if got := files.data(keep); got != "old om" {
			t.Errorf("%s holds %q, want the old om", keep, got)
		}
		if want := dated(om, testStamp) + " " + keep; len(files.links) != 1 || files.links[0] != want {
			t.Errorf("links = %q, want only %q", files.links, want)
		}
		if err := Rollback(RollbackConfig{Stamp: testStamp, Targets: targets, Files: files, Out: &out}); err != nil {
			t.Fatalf("Rollback: %v\n%s", err, out.String())
		}
		if got := files.data(keep); got != "old om" {
			t.Errorf("%s holds %q after rollback, want the old om", keep, got)
		}
		if got := files.data(om.Path); got != "old om" {
			t.Errorf("%s holds %q after rollback, want the old om", om.Path, got)
		}
	})

	refusals := map[string]struct {
		path string
		prep func(*fakeFiles)
		want string
	}{
		"a name that exists":    {path: keep, prep: func(f *fakeFiles) { f.add(keep, "other", 0o755) }, want: "already exists"},
		"another directory":     {path: "/elsewhere/om-native", want: "is not in /Users/operator/go/bin"},
		"another target's name": {path: "/Users/operator/go/bin/hm", want: "a name install or rollback uses"},
		"the dated name":        {path: "/Users/operator/go/bin/om.pre-wrap-" + testStamp, want: "a name install or rollback uses"},
		"the tool's own path":   {path: "/Users/operator/go/bin/om", want: "a name install or rollback uses"},
		"the second name unreadable": {path: keep, prep: func(f *fakeFiles) {
			f.errFor = func(op, p string) error { return nilIf(op == "stat" && p == keep, permission(p)) }
		}, want: "look for " + keep},
	}
	for name, tc := range refusals {
		t.Run("refuses "+name, func(t *testing.T) {
			t.Parallel()
			targets := withKeep(tc.path)
			files := cutoverFiles(targets)
			if tc.prep != nil {
				tc.prep(files)
			}
			var out bytes.Buffer
			err := Install(installConfig(files, &fakeDoctor{}, &out, targets))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Install = %v, want a refusal containing %q", err, tc.want)
			}
			if len(files.ops) != 0 || len(files.links) != 0 {
				t.Errorf("a refused install changed files: ops %q, links %q", files.ops, files.links)
			}
		})
	}

	// One directory holds every tool (--dir), so a second name can land on
	// another tool's dated name. Either order of the targets must refuse before
	// any change: otherwise one tool's swap takes the name and the other's link
	// fails, or its rename replaces the link.
	gtDated := "/Users/operator/bin/gt.pre-wrap-" + testStamp
	overlaps := map[string]struct {
		order   []int
		keepOf  string
		keep    string
		message string
	}{
		"another tool's dated name, gt first": {order: []int{0, 1}, keepOf: "om", keep: gtDated},
		"another tool's dated name, om first": {order: []int{1, 0}, keepOf: "om", keep: gtDated},
		"another tool's aside name":           {order: []int{0, 1}, keepOf: "om", keep: "/Users/operator/bin/gt.wrapper-" + testStamp},
	}
	for name, tc := range overlaps {
		t.Run("refuses "+name, func(t *testing.T) {
			t.Parallel()
			targets := []Target{{Name: "gt", Path: "/Users/operator/bin/gt"}, {Name: "om", Path: "/Users/operator/bin/om"}}
			for i := range targets {
				if targets[i].Name == tc.keepOf {
					targets[i].KeepOld = tc.keep
				}
			}
			ordered := []Target{targets[tc.order[0]], targets[tc.order[1]]}
			files := cutoverFiles(ordered)
			var out bytes.Buffer
			err := Install(installConfig(files, &fakeDoctor{}, &out, ordered))
			if err == nil || !strings.Contains(err.Error(), "--keep-old") {
				t.Fatalf("Install = %v, want a --keep-old refusal", err)
			}
			if len(files.ops) != 0 {
				t.Errorf("a refused install changed files: %q", files.ops)
			}
		})
	}

	t.Run("refuses one name for two tools", func(t *testing.T) {
		t.Parallel()
		targets := []Target{
			{Name: "gt", Path: "/Users/operator/bin/gt", KeepOld: "/Users/operator/bin/keep"},
			{Name: "om", Path: "/Users/operator/bin/om", KeepOld: "/Users/operator/bin/keep"},
		}
		files := cutoverFiles(targets)
		var out bytes.Buffer
		err := Install(installConfig(files, &fakeDoctor{}, &out, targets))
		if err == nil || !strings.Contains(err.Error(), "for both gt and om") {
			t.Fatalf("Install = %v, want a refusal naming both tools", err)
		}
		if len(files.ops) != 0 {
			t.Errorf("a refused install changed files: %q", files.ops)
		}
	})

	t.Run("a failed link puts the old file back", func(t *testing.T) {
		t.Parallel()
		targets := withKeep(keep)
		om := targets[2]
		files := cutoverFiles(targets)
		files.errFor = func(op, _ string) error { return nilIf(op == "link", errors.New("fake failure")) }
		var out bytes.Buffer
		if err := Install(installConfig(files, &fakeDoctor{}, &out, targets)); err == nil {
			t.Fatalf("Install succeeded, want the link's failure:\n%s", out.String())
		}
		if got := files.data(om.Path); got != "old om" || files.has(keep) || files.has(dated(om, testStamp)) {
			t.Errorf("%s holds %q, second name present %t, dated present %t; want the old om back and nothing left", om.Path, got, files.has(keep), files.has(dated(om, testStamp)))
		}
	})

	t.Run("a failed install removes the name it made", func(t *testing.T) {
		t.Parallel()
		targets := withKeep(keep)
		om := targets[2]
		files := cutoverFiles(targets)
		doctor := &fakeDoctor{installed: map[string]error{"om": errors.New("exit status 1")}}
		var out bytes.Buffer
		if err := Install(installConfig(files, doctor, &out, targets)); err == nil {
			t.Fatalf("Install succeeded, want the doctor's failure:\n%s", out.String())
		}
		if files.has(keep) {
			t.Errorf("%s was left behind", keep)
		}
		if got := files.data(om.Path); got != "old om" {
			t.Errorf("%s holds %q, want the old om", om.Path, got)
		}
	})
}

// nilIf is err when ok, else nil: a one-line errFor.
func nilIf(ok bool, err error) error {
	if ok {
		return err
	}
	return nil
}

func TestInstallExplainsAPermissionRefusalOfTheFirstRename(t *testing.T) {
	t.Parallel()
	targets := fourTargets()
	gt := targets[0]
	files := cutoverFiles(targets)
	files.errFor = func(op, path string) error {
		if op == "rename" && path == gt.Path+" -> "+dated(gt, testStamp) {
			return &fs.PathError{Op: "rename", Path: gt.Path, Err: syscall.EPERM}
		}
		return nil
	}
	var out bytes.Buffer
	err := Install(installConfig(files, &fakeDoctor{}, &out, targets))
	if err == nil {
		t.Fatalf("Install succeeded, want the refusal:\n%s", out.String())
	}
	for _, want := range []string{"immutable flag", "ls -lO", "chflags nouchg '" + gt.Path + "'", "nothing was changed for gt"} {
		if !strings.Contains(err.Error()+out.String(), want) {
			t.Errorf("the error and output lack %q:\n%v\n%s", want, err, out.String())
		}
	}
	for _, target := range targets {
		if got := files.data(target.Path); got != "old "+target.Name {
			t.Errorf("%s holds %q, want it untouched", target.Path, got)
		}
	}
}

func TestInstallOtherRenameFailuresGetNoFlagAdvice(t *testing.T) {
	t.Parallel()
	targets := fourTargets()
	gt := targets[0]
	files := cutoverFiles(targets)
	files.errFor = func(op, path string) error {
		return nilIf(op == "rename" && path == gt.Path+" -> "+dated(gt, testStamp), &fs.PathError{Op: "rename", Path: gt.Path, Err: syscall.EIO})
	}
	var out bytes.Buffer
	err := Install(installConfig(files, &fakeDoctor{}, &out, targets))
	if err == nil || strings.Contains(err.Error()+out.String(), "chflags") {
		t.Fatalf("Install = %v, want a failure with no chflags advice for a non-permission error\n%s", err, out.String())
	}
}

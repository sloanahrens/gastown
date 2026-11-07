package gtwrap

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

const (
	testStamp   = "20261106-1430"
	wrapperData = "the wrapper binary\n"
)

// fakeFiles is an in-memory Files that records every operation in order. Its
// Rename refuses a destination that already exists, so an install or rollback
// that succeeded also proves no step wrote over an existing path.
type fakeFiles struct {
	files map[string]*fakeFile
	dirs  map[string]bool
	ops   []string
	// replaced is each path a rename replaced, in order.
	replaced []string
	next     int
	// errFor makes an operation fail: op is "stat", "read", "create",
	// "rename", "remove", "write", "chmod" or "close", and path is what it
	// names, with a rename naming "old -> new". nil fails nothing.
	errFor func(op, path string) error
}

// err is the error the operation returns, or nil.
func (f *fakeFiles) err(op, path string) error {
	if f.errFor == nil {
		return nil
	}
	return f.errFor(op, path)
}

type fakeFile struct {
	data []byte
	mode fs.FileMode
}

func newFakeFiles() *fakeFiles {
	return &fakeFiles{files: map[string]*fakeFile{}, dirs: map[string]bool{}}
}

func (f *fakeFiles) add(path, data string, mode fs.FileMode) {
	f.files[path] = &fakeFile{data: []byte(data), mode: mode}
}

func (f *fakeFiles) addDir(path string) { f.dirs[path] = true }

func (f *fakeFiles) Stat(path string) (fs.FileInfo, error) {
	if err := f.err("stat", path); err != nil {
		return nil, err
	}
	if file, ok := f.files[path]; ok {
		return fakeInfo{name: filepath.Base(path), mode: file.mode, size: int64(len(file.data))}, nil
	}
	if f.dirs[path] {
		return fakeInfo{name: filepath.Base(path), mode: fs.ModeDir | 0o755}, nil
	}
	return nil, &fs.PathError{Op: "stat", Path: path, Err: fs.ErrNotExist}
}

func (f *fakeFiles) ReadFile(path string) ([]byte, error) {
	if err := f.err("read", path); err != nil {
		return nil, err
	}
	file, ok := f.files[path]
	if !ok {
		return nil, &fs.PathError{Op: "open", Path: path, Err: fs.ErrNotExist}
	}
	return file.data, nil
}

func (f *fakeFiles) CreateTemp(dir, pattern string) (TempFile, error) {
	if err := f.err("create", dir); err != nil {
		return nil, err
	}
	f.next++
	name := filepath.Join(dir, fmt.Sprintf(".gt-wrap-%d", f.next))
	f.files[name] = &fakeFile{mode: 0o600}
	f.ops = append(f.ops, "create "+name)
	return &fakeTemp{files: f, name: name, mode: 0o600}, nil
}

// Rename replaces an existing destination, as os.Rename does on POSIX, and
// records each replacement in replaced. A test asserts which paths a run replaced:
// install replaces none while it places a tool, and the undo replaces only the
// wrappers it put back.
func (f *fakeFiles) Rename(oldpath, newpath string) error {
	if err := f.err("rename", oldpath+" -> "+newpath); err != nil {
		return err
	}
	f.ops = append(f.ops, "rename "+oldpath+" "+newpath)
	file, ok := f.files[oldpath]
	if !ok {
		return &fs.PathError{Op: "rename", Path: oldpath, Err: fs.ErrNotExist}
	}
	if _, exists := f.files[newpath]; exists {
		f.replaced = append(f.replaced, newpath)
	}
	delete(f.files, oldpath)
	f.files[newpath] = file
	return nil
}

func (f *fakeFiles) Remove(path string) error {
	if err := f.err("remove", path); err != nil {
		return err
	}
	f.ops = append(f.ops, "remove "+path)
	if _, ok := f.files[path]; !ok {
		return &fs.PathError{Op: "remove", Path: path, Err: fs.ErrNotExist}
	}
	delete(f.files, path)
	return nil
}

// data is a file's contents, or "" when the file is absent.
func (f *fakeFiles) data(path string) string {
	file, ok := f.files[path]
	if !ok {
		return ""
	}
	return string(file.data)
}

func (f *fakeFiles) has(path string) bool {
	_, ok := f.files[path]
	return ok
}

// fakeInfo is the fs.FileInfo the fake files answer with.
type fakeInfo struct {
	name string
	mode fs.FileMode
	size int64
}

func (i fakeInfo) Name() string       { return i.name }
func (i fakeInfo) Size() int64        { return i.size }
func (i fakeInfo) Mode() fs.FileMode  { return i.mode }
func (i fakeInfo) ModTime() time.Time { return time.Time{} }
func (i fakeInfo) IsDir() bool        { return i.mode.IsDir() }
func (i fakeInfo) Sys() any           { return nil }

// fakeTemp is the new file place writes the wrapper into.
type fakeTemp struct {
	files *fakeFiles
	name  string
	mode  fs.FileMode
	data  bytes.Buffer
}

func (t *fakeTemp) Write(p []byte) (int, error) {
	if err := t.files.err("write", t.name); err != nil {
		return 0, err
	}
	return t.data.Write(p)
}

func (t *fakeTemp) Name() string { return t.name }

func (t *fakeTemp) Chmod(mode fs.FileMode) error {
	if err := t.files.err("chmod", t.name); err != nil {
		return err
	}
	t.mode = mode
	return nil
}

func (t *fakeTemp) Close() error {
	if err := t.files.err("close", t.name); err != nil {
		return err
	}
	t.files.files[t.name] = &fakeFile{data: t.data.Bytes(), mode: t.mode}
	return nil
}

// fakeDoctor answers install's two checks and records every question.
type fakeDoctor struct {
	inImage   map[string]error
	installed map[string]error
	asked     []string
	confirmed []string
}

func (d *fakeDoctor) InImage(name string) error {
	d.asked = append(d.asked, name)
	return d.inImage[name]
}

func (d *fakeDoctor) Installed(name, path string) error {
	d.confirmed = append(d.confirmed, name+"="+path)
	return d.installed[name]
}

// cutoverFiles is a fake filesystem holding one old tool per target, a copy of
// the wrapper, and one directory per tool path.
func cutoverFiles(targets []Target) *fakeFiles {
	files := newFakeFiles()
	files.add("/src/gt-wrap", wrapperData, 0o755)
	for _, target := range targets {
		files.addDir(filepath.Dir(target.Path))
		files.add(target.Path, "old "+target.Name, 0o755)
	}
	return files
}

// named returns the target for name.
func named(targets []Target, name string) Target {
	for _, target := range targets {
		if target.Name == name {
			return target
		}
	}
	return Target{}
}

// fourTargets are the four tools in the two directories the operator names
// today: gt and bd in ~/.local/bin, om and hm in ~/go/bin.
func fourTargets() []Target {
	return []Target{
		{Name: "gt", Path: "/Users/operator/.local/bin/gt"},
		{Name: "bd", Path: "/Users/operator/.local/bin/bd"},
		{Name: "om", Path: "/Users/operator/go/bin/om"},
		{Name: "hm", Path: "/Users/operator/go/bin/hm"},
	}
}

func installConfig(files *fakeFiles, doctor Doctor, out *bytes.Buffer, targets []Target) InstallConfig {
	return InstallConfig{
		Source:  "/src/gt-wrap",
		Stamp:   testStamp,
		Targets: targets,
		Files:   files,
		Doctor:  doctor,
		Out:     out,
	}
}

func TestInstallRenamesAsideThenPlacesANewInode(t *testing.T) {
	t.Parallel()
	targets := fourTargets()
	files := cutoverFiles(targets)
	doctor := &fakeDoctor{}
	var out bytes.Buffer
	if err := Install(installConfig(files, doctor, &out, targets)); err != nil {
		t.Fatalf("Install: %v\n%s", err, out.String())
	}

	for _, target := range targets {
		if got := files.data(target.Path); got != wrapperData {
			t.Errorf("%s holds %q, want the wrapper", target.Path, got)
		}
		kept := dated(target, testStamp)
		if got := files.data(kept); got != "old "+target.Name {
			t.Errorf("%s holds %q, want the file it replaced", kept, got)
		}
		if mode := files.files[target.Path].mode; mode != 0o755 {
			t.Errorf("%s has mode %v, want the wrapper's 0755", target.Path, mode)
		}
	}
	if got := files.data("/src/gt-wrap"); got != wrapperData {
		t.Errorf("the source wrapper changed to %q", got)
	}

	// Every tool is staged before any is touched: a new file in each target's own
	// directory, then for each tool its old file renamed aside and its staged file
	// renamed into the emptied path. A placement replaces nothing.
	if len(files.ops) != len(targets)*3 {
		t.Fatalf("install ran %d file operations, want three per tool: %q", len(files.ops), files.ops)
	}
	created := make([]string, len(targets))
	for i, target := range targets {
		create := files.ops[i]
		dir := filepath.Dir(target.Path)
		if !strings.HasPrefix(create, "create "+dir+string(filepath.Separator)) {
			t.Errorf("%s was written to %q, want a new file in %s", target.Name, create, dir)
		}
		created[i] = strings.TrimPrefix(create, "create ")
	}
	for i, target := range targets {
		if got, want := files.ops[len(targets)+2*i], "rename "+target.Path+" "+dated(target, testStamp); got != want {
			t.Errorf("operation %d = %q, want the old file renamed aside as %q", len(targets)+2*i, got, want)
		}
		if got, want := files.ops[len(targets)+2*i+1], "rename "+created[i]+" "+target.Path; got != want {
			t.Errorf("operation %d = %q, want the staged file renamed into place as %q", len(targets)+2*i+1, got, want)
		}
	}
	if len(files.replaced) != 0 {
		t.Errorf("placing the tools replaced %q, want no path replaced", files.replaced)
	}

	if want := "rollback: gt-wrap rollback --stamp '" + testStamp + "' --target 'gt=" + targets[0].Path + "' --target 'bd=" + targets[1].Path + "'"; !strings.Contains(out.String(), want) {
		t.Errorf("install did not print the rollback command %q:\n%s", want, out.String())
	}
	for _, target := range targets {
		if !strings.Contains(out.String(), dated(target, testStamp)) {
			t.Errorf("install did not list the dated file %s:\n%s", dated(target, testStamp), out.String())
		}
	}
}

func TestRollbackRestoresTheDatedFiles(t *testing.T) {
	t.Parallel()

	t.Run("restores every dated file and keeps the wrapper aside", func(t *testing.T) {
		t.Parallel()
		targets := fourTargets()[:2]
		files := newFakeFiles()
		for _, target := range targets {
			files.add(target.Path, wrapperData, 0o755)
			files.add(dated(target, testStamp), "old "+target.Name, 0o755)
		}
		var out bytes.Buffer
		err := Rollback(RollbackConfig{Stamp: testStamp, Targets: targets, Files: files, Out: &out})
		if err != nil {
			t.Fatalf("Rollback: %v\n%s", err, out.String())
		}
		for _, target := range targets {
			if got := files.data(target.Path); got != "old "+target.Name {
				t.Errorf("%s holds %q, want the restored file", target.Path, got)
			}
			if got := files.data(aside(target, testStamp)); got != wrapperData {
				t.Errorf("%s holds %q, want the wrapper kept aside", aside(target, testStamp), got)
			}
			if files.has(dated(target, testStamp)) {
				t.Errorf("%s was left behind after the restore", dated(target, testStamp))
			}
			if want := "restored " + target.Path; !strings.Contains(out.String(), want) {
				t.Errorf("output does not say it restored %s:\n%s", target.Path, out.String())
			}
		}
	})

	t.Run("refuses a tool whose dated file is missing", func(t *testing.T) {
		t.Parallel()
		targets := fourTargets()[:3]
		files := newFakeFiles()
		for _, target := range targets[:2] {
			files.add(target.Path, wrapperData, 0o755)
			files.add(dated(target, testStamp), "old "+target.Name, 0o755)
		}
		files.add(targets[2].Path, wrapperData, 0o755)
		var out bytes.Buffer
		err := Rollback(RollbackConfig{Stamp: testStamp, Targets: targets, Files: files, Out: &out})
		if err == nil {
			t.Fatalf("Rollback returned no error; output:\n%s", out.String())
		}
		missing := dated(targets[2], testStamp)
		if !strings.Contains(err.Error(), missing) {
			t.Errorf("error %q does not name the missing file %s", err, missing)
		}
		if got := files.data(targets[0].Path); got != "old gt" {
			t.Errorf("gt holds %q, want the restored file", got)
		}
		if got := files.data(targets[1].Path); got != "old bd" {
			t.Errorf("bd holds %q, want the restored file", got)
		}
		if got := files.data(targets[2].Path); got != wrapperData {
			t.Errorf("om holds %q, want it left as the wrapper", got)
		}
		if files.has(aside(targets[2], testStamp)) {
			t.Errorf("om's wrapper was moved aside with no dated file to put back")
		}
		for _, op := range files.ops {
			if strings.Contains(op, targets[2].Path) {
				t.Errorf("rollback changed om's file although its dated file was missing: %q", op)
			}
		}
	})

	t.Run("refuses a second install with the same stamp", func(t *testing.T) {
		t.Parallel()
		targets := fourTargets()
		files := cutoverFiles(targets)
		var out bytes.Buffer
		if err := Install(installConfig(files, &fakeDoctor{}, &out, targets)); err != nil {
			t.Fatalf("the first Install: %v", err)
		}
		changed := len(files.ops)
		err := Install(installConfig(files, &fakeDoctor{}, &out, targets))
		if err == nil {
			t.Fatal("the second Install returned no error")
		}
		if want := dated(targets[0], testStamp); !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name the first existing dated file %s", err, want)
		}
		if len(files.ops) != changed {
			t.Errorf("the refused second install changed files: %q", files.ops[changed:])
		}
	})
}

func TestInstallSkipsAToolItsDirectoryDoesNotHold(t *testing.T) {
	t.Parallel()
	dir := "/Users/operator/.local/bin"
	files := newFakeFiles()
	files.add("/src/gt-wrap", wrapperData, 0o755)
	files.add(filepath.Join(dir, "gt"), "old gt", 0o755)
	files.add(filepath.Join(dir, "bd"), "old bd", 0o755)
	var out bytes.Buffer
	err := Install(InstallConfig{Source: "/src/gt-wrap", Stamp: testStamp, Dir: dir, Files: files, Doctor: &fakeDoctor{}, Out: &out})
	if err != nil {
		t.Fatalf("Install: %v\n%s", err, out.String())
	}
	for _, name := range []string{"gt", "bd"} {
		if got := files.data(filepath.Join(dir, name)); got != wrapperData {
			t.Errorf("%s holds %q, want the wrapper", name, got)
		}
	}
	for _, name := range []string{"om", "hm"} {
		if !strings.Contains(out.String(), "skipped "+name+": no file at "+filepath.Join(dir, name)) {
			t.Errorf("output does not note the skipped %s:\n%s", name, out.String())
		}
		if files.has(filepath.Join(dir, name)) {
			t.Errorf("install made %s in a directory that held none", name)
		}
	}
}

func TestInstallRefusesATargetThatIsNotAFile(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		setup func(*fakeFiles, Target)
		want  string
	}{
		{
			name:  "a missing file",
			setup: func(*fakeFiles, Target) {},
			want:  "does not exist",
		},
		{
			name:  "a directory",
			setup: func(files *fakeFiles, target Target) { files.addDir(target.Path) },
			want:  "is a directory",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			target := Target{Name: "gt", Path: "/Users/operator/.local/bin/gt"}
			files := newFakeFiles()
			files.add("/src/gt-wrap", wrapperData, 0o755)
			tt.setup(files, target)
			var out bytes.Buffer
			err := Install(installConfig(files, &fakeDoctor{}, &out, []Target{target}))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Install = %v, want a refusal naming %q", err, tt.want)
			}
			if len(files.ops) != 0 {
				t.Errorf("a refused run changed files: %q", files.ops)
			}
		})
	}
}

func TestInstallRefusesABadStamp(t *testing.T) {
	t.Parallel()
	for _, stamp := range []string{"", "20261106", "20261106-1430-1", "../20261106-1430", "20261306-1430"} {
		files := cutoverFiles(fourTargets()[:1])
		var out bytes.Buffer
		cfg := installConfig(files, &fakeDoctor{}, &out, fourTargets()[:1])
		cfg.Stamp = stamp
		if err := Install(cfg); err == nil {
			t.Errorf("Install with stamp %q returned no error", stamp)
		}
		if len(files.ops) != 0 {
			t.Errorf("Install with stamp %q changed files: %q", stamp, files.ops)
		}
	}
}

// TestCutoverRunbookNamesEveryPrecheck holds the runbook's pre-check list equal
// to the list install prints, so an edit to either alone fails this test.
func TestCutoverRunbookNamesEveryPrecheck(t *testing.T) {
	t.Parallel()
	doc, err := os.ReadFile(filepath.Join("..", "..", "docs", "design", "gt-in-container-cutover.md"))
	if err != nil {
		t.Fatalf("read the cutover runbook: %v", err)
	}

	var out bytes.Buffer
	target := fourTargets()[:1]
	files := cutoverFiles(target)
	if err := Install(installConfig(files, &fakeDoctor{}, &out, target)); err != nil {
		t.Fatalf("Install: %v\n%s", err, out.String())
	}
	printed := printedPrechecks(out.String())
	documented := runbookPrechecks(string(doc))
	if len(printed) == 0 {
		t.Fatalf("install printed no pre-checks:\n%s", out.String())
	}
	if !slices.Equal(printed, documented) {
		t.Errorf("the runbook lists %q, but install prints %q", documented, printed)
	}
	for _, want := range []string{"one hour", "15 minutes"} {
		if !strings.Contains(string(doc), want) {
			t.Errorf("the runbook does not name %q", want)
		}
	}
}

// printedPrechecks is the bullet list install prints.
func printedPrechecks(out string) []string {
	var checks []string
	for _, line := range strings.Split(out, "\n") {
		if rest, ok := strings.CutPrefix(line, "  - "); ok {
			checks = append(checks, rest)
		}
	}
	return checks
}

// runbookPrechecks is the bullet list under the runbook's Pre-checks heading.
func runbookPrechecks(doc string) []string {
	var checks []string
	in := false
	for _, line := range strings.Split(doc, "\n") {
		switch {
		case strings.HasPrefix(line, "## "):
			in = line == "## Pre-checks"
		case in && strings.HasPrefix(line, "- "):
			checks = append(checks, strings.TrimSpace(strings.TrimPrefix(line, "- ")))
		}
	}
	return checks
}

func TestInstallRefusesBadConfigurationBeforeAnyChange(t *testing.T) {
	t.Parallel()
	missingSource := "/src/missing"
	tests := []struct {
		name  string
		build func(*fakeFiles) InstallConfig
		want  string
	}{
		{
			name: "no doctor",
			build: func(files *fakeFiles) InstallConfig {
				cfg := installConfig(files, nil, &bytes.Buffer{}, fourTargets()[:1])
				return cfg
			},
			want: "no doctor",
		},
		{
			name: "no source",
			build: func(files *fakeFiles) InstallConfig {
				cfg := installConfig(files, &fakeDoctor{}, &bytes.Buffer{}, fourTargets()[:1])
				cfg.Source = ""
				return cfg
			},
			want: "no wrapper binary",
		},
		{
			name: "a source that is not there",
			build: func(files *fakeFiles) InstallConfig {
				cfg := installConfig(files, &fakeDoctor{}, &bytes.Buffer{}, fourTargets()[:1])
				cfg.Source = missingSource
				return cfg
			},
			want: "read the wrapper",
		},
		{
			name: "a source that is a directory",
			build: func(files *fakeFiles) InstallConfig {
				cfg := installConfig(files, &fakeDoctor{}, &bytes.Buffer{}, fourTargets()[:1])
				files.addDir("/src")
				cfg.Source = "/src"
				return cfg
			},
			want: "not a regular file",
		},
		{
			name: "no directory and no target",
			build: func(files *fakeFiles) InstallConfig {
				cfg := installConfig(files, &fakeDoctor{}, &bytes.Buffer{}, nil)
				cfg.Targets = nil
				return cfg
			},
			want: "no directory and no target",
		},
		{
			name: "a directory and targets",
			build: func(files *fakeFiles) InstallConfig {
				cfg := installConfig(files, &fakeDoctor{}, &bytes.Buffer{}, fourTargets()[:1])
				cfg.Dir = "/Users/operator/.local/bin"
				return cfg
			},
			want: "both a directory and targets",
		},
		{
			name: "a directory holding none of the four",
			build: func(files *fakeFiles) InstallConfig {
				cfg := installConfig(files, &fakeDoctor{}, &bytes.Buffer{}, nil)
				cfg.Dir = "/nowhere"
				cfg.Targets = nil
				return cfg
			},
			want: "holds none of",
		},
		{
			name: "a tool the wrapper does not answer as",
			build: func(files *fakeFiles) InstallConfig {
				return installConfig(files, &fakeDoctor{}, &bytes.Buffer{}, []Target{{Name: "bash", Path: "/bin/bash"}})
			},
			want: "gt, bd, om and hm",
		},
		{
			name: "a target naming no file",
			build: func(files *fakeFiles) InstallConfig {
				return installConfig(files, &fakeDoctor{}, &bytes.Buffer{}, []Target{{Name: "gt"}})
			},
			want: "names no file",
		},
		{
			name: "a tool named twice",
			build: func(files *fakeFiles) InstallConfig {
				targets := fourTargets()
				return installConfig(files, &fakeDoctor{}, &bytes.Buffer{}, append(targets, targets[0]))
			},
			want: "named twice",
		},
		{
			// The same path would be caught by the path check, so only a second
			// path for gt reaches the name check (gt-xmomer).
			name: "one tool named twice at two paths",
			build: func(files *fakeFiles) InstallConfig {
				targets := fourTargets()
				// Every path exists, so the name check is the only refusal left.
				for _, t := range targets[1:] {
					files.addDir(filepath.Dir(t.Path))
					files.add(t.Path, "old "+t.Name, 0o755)
				}
				files.addDir("/Users/operator/go/bin")
				files.add("/Users/operator/go/bin/gt", "old gt", 0o755)
				return installConfig(files, &fakeDoctor{}, &bytes.Buffer{}, append(targets, Target{Name: "gt", Path: "/Users/operator/go/bin/gt"}))
			},
			want: "gt is named twice",
		},
		{
			name: "one path twice",
			build: func(files *fakeFiles) InstallConfig {
				targets := fourTargets()
				return installConfig(files, &fakeDoctor{}, &bytes.Buffer{}, []Target{targets[0], {Name: "bd", Path: targets[0].Path}})
			},
			want: "named twice",
		},
		{
			name: "a path that is another tool's dated name",
			build: func(files *fakeFiles) InstallConfig {
				gt := fourTargets()[0]
				return installConfig(files, &fakeDoctor{}, &bytes.Buffer{}, []Target{gt, {Name: "bd", Path: dated(gt, testStamp)}})
			},
			want: "another target replaces",
		},
		{
			name: "a target that is not a regular file",
			build: func(files *fakeFiles) InstallConfig {
				target := Target{Name: "gt", Path: "/Users/operator/.local/bin/gt"}
				files.files[target.Path] = &fakeFile{mode: fs.ModeNamedPipe}
				return installConfig(files, &fakeDoctor{}, &bytes.Buffer{}, []Target{target})
			},
			want: "not a regular file",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			files := cutoverFiles(fourTargets()[:1])
			cfg := tt.build(files)
			err := Install(cfg)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Install = %v, want a refusal naming %q", err, tt.want)
			}
			if len(files.ops) != 0 {
				t.Errorf("a refused run changed files: %q", files.ops)
			}
		})
	}
}

func TestRollbackRefusesBadConfigurationBeforeAnyChange(t *testing.T) {
	t.Parallel()
	target := Target{Name: "gt", Path: "/Users/operator/.local/bin/gt"}
	tests := []struct {
		name  string
		build func(*fakeFiles) RollbackConfig
		want  string
	}{
		{
			name: "no stamp",
			build: func(files *fakeFiles) RollbackConfig {
				return RollbackConfig{Targets: []Target{target}, Files: files, Out: &bytes.Buffer{}}
			},
			want: "no stamp",
		},
		{
			name: "a stamp that is not a day and minute",
			build: func(files *fakeFiles) RollbackConfig {
				return RollbackConfig{Stamp: "2026-11-06", Targets: []Target{target}, Files: files, Out: &bytes.Buffer{}}
			},
			want: "yyyymmdd-hhmm",
		},
		{
			name: "no target",
			build: func(files *fakeFiles) RollbackConfig {
				return RollbackConfig{Stamp: testStamp, Files: files, Out: &bytes.Buffer{}}
			},
			want: "no directory and no target",
		},
		{
			name: "a dated file that is not a regular file",
			build: func(files *fakeFiles) RollbackConfig {
				files.add(target.Path, wrapperData, 0o755)
				files.files[dated(target, testStamp)] = &fakeFile{mode: fs.ModeDir | 0o755}
				return RollbackConfig{Stamp: testStamp, Targets: []Target{target}, Files: files, Out: &bytes.Buffer{}}
			},
			want: "not a regular file",
		},
		{
			name: "the wrapper at the target is missing",
			build: func(files *fakeFiles) RollbackConfig {
				files.add(dated(target, testStamp), "old gt", 0o755)
				return RollbackConfig{Stamp: testStamp, Targets: []Target{target}, Files: files, Out: &bytes.Buffer{}}
			},
			want: "no wrapper to keep aside",
		},
		{
			name: "the wrapper at the target is a directory",
			build: func(files *fakeFiles) RollbackConfig {
				files.addDir(target.Path)
				files.add(dated(target, testStamp), "old gt", 0o755)
				return RollbackConfig{Stamp: testStamp, Targets: []Target{target}, Files: files, Out: &bytes.Buffer{}}
			},
			want: "not a regular file",
		},
		{
			name: "a wrapper already kept aside",
			build: func(files *fakeFiles) RollbackConfig {
				files.add(target.Path, wrapperData, 0o755)
				files.add(dated(target, testStamp), "old gt", 0o755)
				files.add(aside(target, testStamp), "an earlier wrapper", 0o755)
				return RollbackConfig{Stamp: testStamp, Targets: []Target{target}, Files: files, Out: &bytes.Buffer{}}
			},
			want: "already exists",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			files := newFakeFiles()
			cfg := tt.build(files)
			err := Rollback(cfg)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Rollback = %v, want a refusal naming %q", err, tt.want)
			}
			if len(files.ops) != 0 {
				t.Errorf("a refused run changed files: %q", files.ops)
			}
		})
	}
}

// permission is the error a failing fake operation returns.
func permission(path string) error {
	return &fs.PathError{Op: "fake", Path: path, Err: fs.ErrPermission}
}

func TestInstallReportsAPlacementStepThatFails(t *testing.T) {
	t.Parallel()
	target := Target{Name: "gt", Path: "/Users/operator/.local/bin/gt"}
	kept := dated(target, testStamp)
	temp := "/Users/operator/.local/bin/.gt-wrap-1"
	placeRename := temp + " -> " + target.Path
	backRename := kept + " -> " + target.Path
	tests := []struct {
		name string
		fail func(op, path string) bool
		want string
		// holds is the file the target path holds after the failure.
		holds string
	}{
		{
			name:  "the temporary file cannot be made",
			fail:  func(op, path string) bool { return op == "create" },
			want:  "make a temporary file",
			holds: "old gt",
		},
		{
			name:  "the wrapper cannot be written",
			fail:  func(op, path string) bool { return op == "write" },
			want:  "write the wrapper beside",
			holds: "old gt",
		},
		{
			name:  "the wrapper's mode cannot be set",
			fail:  func(op, path string) bool { return op == "chmod" },
			want:  "set the wrapper's mode",
			holds: "old gt",
		},
		{
			name:  "the temporary file cannot be finished",
			fail:  func(op, path string) bool { return op == "close" },
			want:  "finish the wrapper beside",
			holds: "old gt",
		},
		{
			name:  "the old file cannot be kept aside",
			fail:  func(op, path string) bool { return op == "rename" && path == target.Path+" -> "+kept },
			want:  "keep " + target.Path,
			holds: "old gt",
		},
		{
			name:  "the wrapper cannot be renamed into place",
			fail:  func(op, path string) bool { return op == "rename" && path == placeRename },
			want:  "rename the wrapper into place at",
			holds: "old gt",
		},
		{
			name: "the wrapper cannot be renamed into place, nor the old file put back",
			fail: func(op, path string) bool {
				return op == "rename" && (path == placeRename || path == backRename)
			},
			want:  "could not be put back",
			holds: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			files := cutoverFiles([]Target{target})
			files.errFor = func(op, path string) error {
				if tt.fail(op, path) {
					return permission(path)
				}
				return nil
			}
			err := Install(installConfig(files, &fakeDoctor{}, &bytes.Buffer{}, []Target{target}))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Install = %v, want a refusal naming %q", err, tt.want)
			}
			if files.has(temp) {
				t.Errorf("the temporary file was left behind")
			}
			if got := files.data(target.Path); got != tt.holds {
				t.Errorf("%s holds %q, want %q", target.Path, got, tt.holds)
			}
			if tt.holds == "" {
				if got := files.data(kept); got != "old gt" {
					t.Errorf("%s holds %q, want the old file kept", kept, got)
				}
			} else if files.has(kept) {
				t.Errorf("%s exists although the old file never moved", kept)
			}
		})
	}
}

func TestRollbackReportsARestoreStepThatFails(t *testing.T) {
	t.Parallel()
	target := Target{Name: "gt", Path: "/Users/operator/.local/bin/gt"}
	kept := dated(target, testStamp)
	moved := aside(target, testStamp)
	moveAside := target.Path + " -> " + moved
	moveBack := kept + " -> " + target.Path
	tests := []struct {
		name  string
		fail  func(op, path string) bool
		want  string
		holds string
		// aside is the file the kept wrapper should be at after the failure.
		aside string
	}{
		{
			name:  "the wrapper cannot move aside",
			fail:  func(op, path string) bool { return op == "rename" && path == moveAside },
			want:  "move " + target.Path + " aside",
			holds: wrapperData,
		},
		{
			name:  "the dated file cannot move back",
			fail:  func(op, path string) bool { return op == "rename" && path == moveBack },
			want:  "rename " + kept + " back",
			holds: wrapperData,
		},
		{
			name: "neither rename holds",
			fail: func(op, path string) bool {
				return op == "rename" && (path == moveBack || path == moved+" -> "+target.Path)
			},
			want:  "putting the wrapper back",
			holds: "",
			aside: wrapperData,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			files := newFakeFiles()
			files.add(target.Path, wrapperData, 0o755)
			files.add(kept, "old gt", 0o755)
			files.errFor = func(op, path string) error {
				if tt.fail(op, path) {
					return permission(path)
				}
				return nil
			}
			err := Rollback(RollbackConfig{Stamp: testStamp, Targets: []Target{target}, Files: files, Out: &bytes.Buffer{}})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Rollback = %v, want a refusal naming %q", err, tt.want)
			}
			if got := files.data(target.Path); got != tt.holds {
				t.Errorf("%s holds %q, want %q", target.Path, got, tt.holds)
			}
			if got := files.data(kept); got != "old gt" {
				t.Errorf("%s holds %q, want the dated file kept", kept, got)
			}
			if got := files.data(moved); got != tt.aside {
				t.Errorf("%s holds %q, want %q", moved, got, tt.aside)
			}
		})
	}
}

// TestRollbackRestoresTheOtherToolsWhenOneFails: a refused tool must not stop
// the others, and the error counts both.
func TestRollbackRestoresTheOtherToolsWhenOneFails(t *testing.T) {
	t.Parallel()
	targets := fourTargets()[:2]
	files := newFakeFiles()
	for _, target := range targets {
		files.add(target.Path, wrapperData, 0o755)
		files.add(dated(target, testStamp), "old "+target.Name, 0o755)
	}
	moveAside := targets[0].Path + " -> " + aside(targets[0], testStamp)
	files.errFor = func(op, path string) error {
		if op == "rename" && path == moveAside {
			return permission(path)
		}
		return nil
	}
	err := Rollback(RollbackConfig{Stamp: testStamp, Targets: targets, Files: files, Out: &bytes.Buffer{}})
	if err == nil {
		t.Fatal("Rollback returned no error")
	}
	for _, want := range []string{"restored 1 of 2", "gt"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
	if got := files.data(targets[1].Path); got != "old bd" {
		t.Errorf("bd holds %q, want the restored file", got)
	}
}

func TestInstallRefusesWhenAFileCannotBeRead(t *testing.T) {
	t.Parallel()
	target := Target{Name: "gt", Path: "/Users/operator/.local/bin/gt"}
	tests := []struct {
		name string
		cfg  func(*fakeFiles) InstallConfig
		fail func(op, path string) bool
		want string
	}{
		{
			name: "the wrapper source cannot be read",
			cfg: func(files *fakeFiles) InstallConfig {
				return installConfig(files, &fakeDoctor{}, &bytes.Buffer{}, []Target{target})
			},
			fail: func(op, path string) bool { return op == "read" && path == "/src/gt-wrap" },
			want: "read the wrapper",
		},
		{
			name: "a target cannot be read",
			cfg: func(files *fakeFiles) InstallConfig {
				return installConfig(files, &fakeDoctor{}, &bytes.Buffer{}, []Target{target})
			},
			fail: func(op, path string) bool { return op == "stat" && path == target.Path },
			want: "read " + target.Path,
		},
		{
			name: "a dated file cannot be read",
			cfg: func(files *fakeFiles) InstallConfig {
				return installConfig(files, &fakeDoctor{}, &bytes.Buffer{}, []Target{target})
			},
			fail: func(op, path string) bool { return op == "stat" && path == dated(target, testStamp) },
			want: "look for " + dated(target, testStamp),
		},
		{
			name: "a directory cannot be listed",
			cfg: func(files *fakeFiles) InstallConfig {
				return InstallConfig{Source: "/src/gt-wrap", Stamp: testStamp, Dir: "/Users/operator/.local/bin", Files: files, Doctor: &fakeDoctor{}, Out: &bytes.Buffer{}}
			},
			fail: func(op, path string) bool { return op == "stat" && path == "/Users/operator/.local/bin/gt" },
			want: "look for /Users/operator/.local/bin/gt",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			files := cutoverFiles([]Target{target})
			files.errFor = func(op, path string) error {
				if tt.fail(op, path) {
					return permission(path)
				}
				return nil
			}
			err := Install(tt.cfg(files))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Install = %v, want a refusal naming %q", err, tt.want)
			}
			if len(files.ops) != 0 {
				t.Errorf("a refused run changed files: %q", files.ops)
			}
		})
	}
}

func TestRollbackRefusesWhenAFileCannotBeRead(t *testing.T) {
	t.Parallel()
	target := Target{Name: "gt", Path: "/Users/operator/.local/bin/gt"}
	tests := []struct {
		name string
		fail func(op, path string) bool
		want string
	}{
		{
			name: "the dated file cannot be read",
			fail: func(op, path string) bool { return op == "stat" && path == dated(target, testStamp) },
			want: "read " + dated(target, testStamp),
		},
		{
			name: "the aside name cannot be read",
			fail: func(op, path string) bool { return op == "stat" && path == aside(target, testStamp) },
			want: "look for " + aside(target, testStamp),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			files := newFakeFiles()
			files.add(target.Path, wrapperData, 0o755)
			files.add(dated(target, testStamp), "old gt", 0o755)
			files.errFor = func(op, path string) error {
				if tt.fail(op, path) {
					return permission(path)
				}
				return nil
			}
			err := Rollback(RollbackConfig{Stamp: testStamp, Targets: []Target{target}, Files: files, Out: &bytes.Buffer{}})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Rollback = %v, want a refusal naming %q", err, tt.want)
			}
			if len(files.ops) != 0 {
				t.Errorf("a refused run changed files: %q", files.ops)
			}
		})
	}
}

// TestInstallNamesATemporaryFileItCannotRemove: when a placement step fails and
// the temporary file cannot be removed either, the error names both, so the
// file left beside the tool is not silent.
func TestInstallNamesATemporaryFileItCannotRemove(t *testing.T) {
	t.Parallel()
	target := Target{Name: "gt", Path: "/Users/operator/.local/bin/gt"}
	kept := dated(target, testStamp)
	tests := []struct {
		name string
		// fail is the step that fails before the temporary file is removed.
		fail func(op, path string) bool
		want string
	}{
		{
			name: "the write fails",
			fail: func(op, path string) bool { return op == "write" },
			want: "write the wrapper beside",
		},
		{
			name: "the old file cannot be kept aside",
			fail: func(op, path string) bool { return op == "rename" && path == target.Path+" -> "+kept },
			want: "keep " + target.Path,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			files := cutoverFiles([]Target{target})
			files.errFor = func(op, path string) error {
				if op == "remove" || tt.fail(op, path) {
					return permission(path)
				}
				return nil
			}
			err := Install(installConfig(files, &fakeDoctor{}, &bytes.Buffer{}, []Target{target}))
			for _, want := range []string{tt.want, "remove the temporary file"} {
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Fatalf("Install = %v, want it to name %q", err, want)
				}
			}
		})
	}
}

// stagedNames are the files install staged beside a target and left behind.
func stagedNames(files *fakeFiles) []string {
	var names []string
	for path := range files.files {
		if strings.Contains(filepath.Base(path), ".gt-wrap-") {
			names = append(names, path)
		}
	}
	slices.Sort(names)
	return names
}

// TestInstallIsTransactional: a placement or doctor failure at tool N puts back
// every tool install changed, in reverse order, so each tool holds its original
// bytes, nothing stays staged, and the output says what became of each tool. The
// tool whose placement failed had its old file moved aside only if the failure
// came after that rename, so it is put back only then.
func TestInstallIsTransactional(t *testing.T) {
	t.Parallel()
	for _, placement := range []bool{true, false} {
		for at := range fourTargets() {
			kind := "a doctor failure"
			if placement {
				kind = "a placement failure"
			}
			name := fmt.Sprintf("%s at tool %d", kind, at+1)
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				targets := fourTargets()
				failing := targets[at]
				files := cutoverFiles(targets)
				doctor := &fakeDoctor{}
				if placement {
					files.errFor = func(op, path string) error {
						if op == "rename" && strings.Contains(path, ".gt-wrap-") && strings.HasSuffix(path, " -> "+failing.Path) {
							return permission(path)
						}
						return nil
					}
				} else {
					doctor.installed = map[string]error{failing.Name: errors.New("exit status 1")}
				}
				var out bytes.Buffer
				err := Install(installConfig(files, doctor, &out, targets))
				if err == nil || !strings.Contains(err.Error(), "install stopped at "+failing.Name) || !strings.Contains(err.Error(), "every tool it changed was put back") {
					t.Fatalf("Install = %v, want a stop at %s with every changed tool put back", err, failing.Name)
				}

				for i, target := range targets {
					if got := files.data(target.Path); got != "old "+target.Name {
						t.Errorf("%s holds %q, want its original bytes", target.Path, got)
					}
					if files.has(dated(target, testStamp)) {
						t.Errorf("%s was left behind after the undo", dated(target, testStamp))
					}
					var want string
					switch {
					case i < at:
						want = "restored " + target.Name + " at " + target.Path + " from " + dated(target, testStamp)
					case i == at:
						want = "failed " + target.Name + " at " + target.Path + ": "
					default:
						want = "untouched " + target.Name + " at " + target.Path
					}
					if !strings.Contains(out.String(), want) {
						t.Errorf("output does not say %q:\n%s", want, out.String())
					}
				}
				if got := stagedNames(files); len(got) != 0 {
					t.Errorf("staged files were left behind: %q", got)
				}

				// The undo replaces the wrappers it puts back, last placed first, and
				// nothing else: the placement itself replaces no path.
				last := at - 1
				if !placement {
					last = at
				}
				var wantReplaced []string
				for i := last; i >= 0; i-- {
					wantReplaced = append(wantReplaced, targets[i].Path)
				}
				if !slices.Equal(files.replaced, wantReplaced) {
					t.Errorf("the run replaced %q, want %q", files.replaced, wantReplaced)
				}
			})
		}
	}
}

// TestInstallRefusesAnUnusableRungBeforeAnyChange: a tool that does not answer
// in the image refuses the whole run before any target is touched.
func TestInstallRefusesAnUnusableRungBeforeAnyChange(t *testing.T) {
	t.Parallel()
	targets := fourTargets()
	files := cutoverFiles(targets)
	doctor := &fakeDoctor{inImage: map[string]error{"bd": errors.New("Docker is down")}}
	var out bytes.Buffer
	err := Install(installConfig(files, doctor, &out, targets))
	if err == nil {
		t.Fatalf("Install returned no error; output:\n%s", out.String())
	}
	for _, want := range []string{"bd", "Docker is down"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
	if len(files.ops) != 0 {
		t.Errorf("a refused run changed files: %q", files.ops)
	}
	for _, target := range targets {
		if got := files.data(target.Path); got != "old "+target.Name {
			t.Errorf("%s holds %q, want the file it had before the run", target.Path, got)
		}
	}
}

// TestInstallStagesEverythingBeforeTouchingATarget: every tool is checked and
// staged before any target is renamed, so a staging failure, or a refusal found
// while checking, changes no target and leaves no staged file behind.
func TestInstallStagesEverythingBeforeTouchingATarget(t *testing.T) {
	t.Parallel()
	targets := fourTargets()
	om := named(targets, "om")
	tests := []struct {
		name  string
		build func(*fakeFiles)
		// want is the error install returns.
		want string
	}{
		{
			name: "a directory cannot take a staged file",
			build: func(files *fakeFiles) {
				files.errFor = func(op, path string) error {
					if op == "create" && path == filepath.Dir(om.Path) {
						return permission(path)
					}
					return nil
				}
			},
			want: "make a temporary file beside " + om.Path,
		},
		{
			name: "a dated name is taken",
			build: func(files *fakeFiles) {
				files.add(dated(named(targets, "hm"), testStamp), "an earlier backup", 0o755)
			},
			want: dated(named(targets, "hm"), testStamp) + " already exists",
		},
		{
			name: "a target is a directory",
			build: func(files *fakeFiles) {
				delete(files.files, named(targets, "hm").Path)
				files.addDir(named(targets, "hm").Path)
			},
			want: named(targets, "hm").Path + " is a directory",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			files := cutoverFiles(targets)
			tt.build(files)
			var out bytes.Buffer
			err := Install(installConfig(files, &fakeDoctor{}, &out, targets))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Install = %v, want it to name %q", err, tt.want)
			}
			for _, op := range files.ops {
				if strings.HasPrefix(op, "rename ") {
					t.Errorf("a refused run renamed a file: %q", op)
				}
			}
			if got := stagedNames(files); len(got) != 0 {
				t.Errorf("staged files were left behind: %q", got)
			}
			for _, target := range targets {
				if target.Name == "hm" && tt.name == "a target is a directory" {
					continue
				}
				if got := files.data(target.Path); got != "old "+target.Name {
					t.Errorf("%s holds %q, want the file it had before the run", target.Path, got)
				}
			}
		})
	}
}

// TestInstallRestoreFailurePrintsTheManualCommand: when a tool cannot be put
// back, install names it, prints the command that puts its original file back by
// hand and the rollback command for it, and returns an error. The other tools are
// still put back.
func TestInstallRestoreFailurePrintsTheManualCommand(t *testing.T) {
	t.Parallel()
	targets := fourTargets()
	gt, bd := targets[0], targets[1]
	files := cutoverFiles(targets)
	files.errFor = func(op, path string) error {
		if op == "rename" && path == dated(gt, testStamp)+" -> "+gt.Path {
			return permission(path)
		}
		return nil
	}
	doctor := &fakeDoctor{installed: map[string]error{bd.Name: errors.New("exit status 1")}}
	var out bytes.Buffer
	err := Install(installConfig(files, doctor, &out, targets))
	if err == nil || !strings.Contains(err.Error(), "gt could not be put back") {
		t.Fatalf("Install = %v, want gt named as not put back", err)
	}
	manual := "mv -f '" + dated(gt, testStamp) + "' '" + gt.Path + "'"
	if !strings.Contains(out.String(), "could not restore gt at "+gt.Path) || !strings.Contains(out.String(), manual) {
		t.Errorf("output does not print the manual command %q:\n%s", manual, out.String())
	}
	if want := "rollback: gt-wrap rollback --stamp '" + testStamp + "' --target 'gt=" + gt.Path + "'"; !strings.Contains(out.String(), want) {
		t.Errorf("output does not print the rollback command for gt %q:\n%s", want, out.String())
	}
	if got := files.data(bd.Path); got != "old bd" {
		t.Errorf("bd holds %q, want it put back", got)
	}
	if got := files.data(gt.Path); got != wrapperData {
		t.Errorf("gt holds %q, want the wrapper it still holds", got)
	}
	if got := files.data(dated(gt, testStamp)); got != "old gt" {
		t.Errorf("%s holds %q, want the original file kept for the manual restore", dated(gt, testStamp), got)
	}
}

// TestRollbackChecksThePathType: a wrapper path that is not a regular file is
// refused, naming the path and the type found, and changes nothing for that tool.
// A regular file is rolled back. os.Stat follows a symlink, so a symlink to a
// regular file is judged as that file and is rolled back too.
func TestRollbackChecksThePathType(t *testing.T) {
	t.Parallel()
	target := Target{Name: "gt", Path: "/Users/operator/.local/bin/gt"}
	kept := dated(target, testStamp)
	tests := []struct {
		name  string
		build func(*fakeFiles)
		// want names the type or the missing file the refusal reports; "" when
		// the tool is rolled back.
		want string
	}{
		{
			name: "the wrapper is a directory",
			build: func(f *fakeFiles) {
				f.addDir(target.Path)
				f.add(kept, "old gt", 0o755)
			},
			want: "is a directory",
		},
		{
			name: "the wrapper is a socket",
			build: func(f *fakeFiles) {
				f.add(target.Path, wrapperData, fs.ModeSocket|0o755)
				f.add(kept, "old gt", 0o755)
			},
			want: "is a socket",
		},
		{
			name: "the wrapper is missing",
			build: func(f *fakeFiles) {
				f.add(kept, "old gt", 0o755)
			},
			want: "is missing",
		},
		{
			name: "the dated file is missing",
			build: func(f *fakeFiles) {
				f.add(target.Path, wrapperData, 0o755)
			},
			want: kept + " does not exist",
		},
		{
			name: "the wrapper is a regular file",
			build: func(f *fakeFiles) {
				f.add(target.Path, wrapperData, 0o755)
				f.add(kept, "old gt", 0o755)
			},
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			files := newFakeFiles()
			tt.build(files)
			err := Rollback(RollbackConfig{Stamp: testStamp, Targets: []Target{target}, Files: files, Out: &bytes.Buffer{}})
			if tt.want == "" {
				if err != nil {
					t.Fatalf("Rollback: %v", err)
				}
				if got := files.data(target.Path); got != "old gt" {
					t.Errorf("%s holds %q, want the restored file", target.Path, got)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Rollback = %v, want a refusal naming %q", err, tt.want)
			}
			if len(files.ops) != 0 {
				t.Errorf("a refused run changed files: %q", files.ops)
			}
		})
	}
}

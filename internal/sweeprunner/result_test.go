package sweeprunner

import (
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// daemonTierSweepPath is the daemon source this package's summary expression
// and TierResult shape must agree with. The tests read it rather than restating
// it in a copy, because a copy only proves this package agrees with itself: a
// change in the daemon would leave both sides of the test untouched and green
// (gt-u00wo). The daemon declares both names unexported and will import this
// package for the conversion (gt-z4ppb), so an import is a cycle and an
// exported seam there would be the daemon's change to make: reading the
// declaration is the only check this side can make.
const daemonTierSweepPath = "../daemon/tier_sweep.go"

// daemonField is one field of the daemon's tierSweepTierResult, as declared.
type daemonField struct {
	Name string
	Type string
	Tag  string
}

// daemonTierShape reads the daemon's summary expression and its tier result
// struct out of the daemon's own source.
func daemonTierShape(t *testing.T) (pattern string, fields []daemonField) {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), daemonTierSweepPath, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse the daemon's tier sweep source %s: %v", daemonTierSweepPath, err)
	}
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok {
			continue
		}
		for _, spec := range gen.Specs {
			switch s := spec.(type) {
			case *ast.ValueSpec:
				for _, name := range s.Names {
					if name.Name == "tierSweepSummaryRE" {
						pattern = regexpCallPattern(t, s)
					}
				}
			case *ast.TypeSpec:
				if s.Name.Name == "tierSweepTierResult" {
					fields = structFields(t, s)
				}
			}
		}
	}
	if pattern == "" {
		t.Fatalf("%s no longer declares tierSweepSummaryRE as a regexp.MustCompile literal; the parity this package relies on must be re-established by hand", daemonTierSweepPath)
	}
	if len(fields) == 0 {
		t.Fatalf("%s no longer declares tierSweepTierResult; the parity this package relies on must be re-established by hand", daemonTierSweepPath)
	}
	return pattern, fields
}

// regexpCallPattern returns the literal a ValueSpec's regexp.MustCompile call
// compiles.
func regexpCallPattern(t *testing.T, spec *ast.ValueSpec) string {
	t.Helper()
	for _, value := range spec.Values {
		call, ok := value.(*ast.CallExpr)
		if !ok {
			continue
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "MustCompile" || len(call.Args) != 1 {
			continue
		}
		lit, ok := call.Args[0].(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			continue
		}
		pattern, err := strconv.Unquote(lit.Value)
		if err != nil {
			t.Fatalf("unquote the daemon's summary expression: %v", err)
		}
		return pattern
	}
	t.Fatalf("tierSweepSummaryRE is not a regexp.MustCompile of a string literal")
	return ""
}

// structFields returns a struct type's fields as written.
func structFields(t *testing.T, spec *ast.TypeSpec) []daemonField {
	t.Helper()
	st, ok := spec.Type.(*ast.StructType)
	if !ok {
		t.Fatalf("%s is not a struct", spec.Name.Name)
	}
	var fields []daemonField
	for _, f := range st.Fields.List {
		if len(f.Names) != 1 {
			t.Fatalf("%s has a field declared with %d names; one field per verdict key is what the parity test reads", spec.Name.Name, len(f.Names))
		}
		if f.Tag == nil {
			t.Fatalf("%s.%s has no tag; the verdict keys are its json tags", spec.Name.Name, f.Names[0].Name)
		}
		tag, err := strconv.Unquote(f.Tag.Value)
		if err != nil {
			t.Fatalf("unquote the tag of %s.%s: %v", spec.Name.Name, f.Names[0].Name, err)
		}
		fields = append(fields, daemonField{Name: f.Names[0].Name, Type: types.ExprString(f.Type), Tag: tag})
	}
	return fields
}

// daemonTagName returns the json key a daemon field marshals to, and whether
// the daemon's tag lets the key be absent.
func daemonTagName(t *testing.T, field daemonField) (name string, optional bool) {
	t.Helper()
	tag := reflect.StructTag(field.Tag)
	key, opts, ok := strings.Cut(tag.Get("json"), ",")
	if !ok && tag.Get("json") == "" {
		t.Fatalf("the daemon's field %s has no json tag: %q", field.Name, field.Tag)
	}
	return key, strings.Contains(opts, "omitempty")
}

const sampleSHA = "0123456789abcdef0123456789abcdef01234567"

func sampleResult() Result {
	started := time.Date(2026, 10, 30, 18, 45, 11, 0, time.UTC)
	return Result{
		Rig:      "gastown",
		SHA:      sampleSHA,
		Started:  started,
		Finished: started.Add(28*time.Minute + 50*time.Second),
		Runner:   Runner{Backend: "docker", Image: "gastown-polecat:dev", CPUs: 8},
		Tiers: []Tier{
			{
				Tier:    "shell",
				Summary: "tier-sweep: shell GREEN passed=5 failed=0 skipped=0 (logs /out/logs/shell) in 45s",
				Command: "make shell-test",
			},
			{
				Tier:    "integration",
				Summary: "tier-sweep: integration RED passed=65 failed=1 skipped=4 failed: ./internal/daemon (logs /out/logs/integration) in 28m50s",
				Command: "go test ./...",
			},
		},
	}
}

// TestResultRoundTrip: a written result reads back equal, a second write
// replaces the first whole, a write that cannot publish leaves neither a
// partial result nor a temp file behind, and a reader polling through a rewrite
// never sees a mixture.
func TestResultRoundTrip(t *testing.T) {
	t.Parallel()

	t.Run("a written result reads back", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		want := sampleResult()
		if err := Write(dir, want); err != nil {
			t.Fatalf("Write: %v", err)
		}
		want.Version = ResultVersion

		got, err := Read(dir, want.Rig, want.SHA)
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("Read returned\n%+v\nwant\n%+v", got, want)
		}
		assertOnlyResultFile(t, dir)
	})

	t.Run("a second write replaces the first", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		first := sampleResult()
		if err := Write(dir, first); err != nil {
			t.Fatalf("Write first: %v", err)
		}
		path := filepath.Join(dir, ResultFileName)
		before, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat the first result: %v", err)
		}
		second := first
		second.Finished = first.Finished.Add(time.Minute)
		second.Runner.CPUs = 4
		if err := Write(dir, second); err != nil {
			t.Fatalf("Write second: %v", err)
		}

		got, err := Read(dir, second.Rig, second.SHA)
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		if got.Runner.CPUs != 4 || !got.Finished.Equal(second.Finished) {
			t.Fatalf("Read returned the first result: %+v", got)
		}
		// The publish is a rename, so the second write leaves a different file
		// at the name: a write in place would truncate the one a reader has
		// open, which is exactly the partial result this test refuses.
		after, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat the second result: %v", err)
		}
		if os.SameFile(before, after) {
			t.Fatal("the second write reused the first file instead of publishing a new one by rename")
		}
		assertOnlyResultFile(t, dir)
	})

	// The rename is what publishes a result, so a rename that fails must leave
	// the directory as it was: no result.json and no temp file a reader could
	// catch half written.
	t.Run("a failed write leaves no file behind", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		target := filepath.Join(dir, ResultFileName)
		if err := os.MkdirAll(filepath.Join(target, "keep"), 0o755); err != nil {
			t.Fatal(err)
		}

		if err := Write(dir, sampleResult()); err == nil {
			t.Fatal("Write over a directory succeeded")
		}
		assertOnlyResultFile(t, dir)
		if _, err := os.Stat(filepath.Join(target, "keep")); err != nil {
			t.Fatalf("the failed write touched what was already at the result's name: %v", err)
		}
	})

	// A write that fails before the rename, because the output directory is
	// not there, leaves no temp file either: the temp is created in that
	// directory, and nothing else is written.
	t.Run("a write into a directory that is not there leaves nothing", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		if err := Write(filepath.Join(dir, "not-there"), sampleResult()); err == nil {
			t.Fatal("Write into a missing directory succeeded")
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 0 {
			var names []string
			for _, e := range entries {
				names = append(names, e.Name())
			}
			t.Fatalf("the failed write left %v behind", names)
		}
	})

	// The daemon reads while the runner rewrites, so a read that lands mid-write
	// must still return one whole result: either the old one or the new one.
	t.Run("a reader never sees a partial result", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		first := sampleResult()
		second := first
		second.Finished = first.Finished.Add(time.Minute)
		second.Runner.CPUs = 4
		if err := Write(dir, first); err != nil {
			t.Fatalf("Write: %v", err)
		}
		first.Version = ResultVersion
		second.Version = ResultVersion

		stop := make(chan struct{})
		var writing sync.WaitGroup
		writing.Add(1)
		go func() {
			defer writing.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				r := first
				if i%2 == 1 {
					r = second
				}
				if err := Write(dir, r); err != nil {
					t.Errorf("Write during reads: %v", err)
					return
				}
			}
		}()
		defer func() {
			close(stop)
			writing.Wait()
		}()

		for i := 0; i < 50; i++ {
			got, err := Read(dir, first.Rig, first.SHA)
			if err != nil {
				t.Fatalf("Read %d during a write: %v", i, err)
			}
			want := first
			if got.Runner.CPUs == second.Runner.CPUs {
				want = second
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("Read %d returned a mixture\n%+v\nneither first\n%+v\nnor second\n%+v", i, got, first, second)
			}
		}
	})
}

// assertOnlyResultFile fails when dir holds anything but the result file, which
// is how a test sees a temp file the write should have removed.
func assertOnlyResultFile(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != ResultFileName {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("directory holds %v, want only %s", names, ResultFileName)
	}
}

// TestResultReadRefusals: each unusable file is refused with an error naming
// what was wrong with it.
func TestResultReadRefusals(t *testing.T) {
	t.Parallel()
	valid := sampleResult()
	writeRaw := func(body string) func(*testing.T, string) {
		return func(t *testing.T, dir string) {
			t.Helper()
			if err := os.WriteFile(filepath.Join(dir, ResultFileName), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	writeJSON := func(v any) func(*testing.T, string) {
		return func(t *testing.T, dir string) {
			t.Helper()
			data, err := json.Marshal(v)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, ResultFileName), data, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	writeValid := func(r Result) func(*testing.T, string) {
		return func(t *testing.T, dir string) {
			t.Helper()
			if err := Write(dir, r); err != nil {
				t.Fatalf("Write: %v", err)
			}
		}
	}

	unknownVersion := valid
	unknownVersion.Version = ResultVersion + 1
	otherRig := valid
	otherRig.Rig = "beads"
	finishedFirst := valid
	finishedFirst.Version = ResultVersion
	finishedFirst.Finished = valid.Started.Add(-time.Minute)
	duplicateTier := valid
	duplicateTier.Version = ResultVersion
	duplicateTier.Tiers = append(slices.Clone(valid.Tiers), valid.Tiers[0])
	noTiers := valid
	noTiers.Version = ResultVersion
	noTiers.Tiers = nil

	cases := []struct {
		name string
		rig  string
		sha  string
		file func(*testing.T, string)
		want string
	}{
		{
			name: "missing file",
			rig:  valid.Rig,
			sha:  valid.SHA,
			file: func(*testing.T, string) {},
			want: "no result file",
		},
		{
			name: "torn JSON",
			rig:  valid.Rig,
			sha:  valid.SHA,
			file: writeRaw(`{"version":1,"rig":"gastown","sha":"` + valid.SHA),
			want: "not a complete JSON document",
		},
		{
			name: "unknown version",
			rig:  valid.Rig,
			sha:  valid.SHA,
			file: writeJSON(unknownVersion),
			want: "version 2, want 1",
		},
		{
			// A later format may change the type of a field. The version is
			// what the caller must hear about, not a torn document.
			name: "a later version whose fields changed type",
			rig:  valid.Rig,
			sha:  valid.SHA,
			file: writeRaw(`{"version":2,"started":12345,"runner":"docker","tiers":"all"}`),
			want: "version 2, want 1",
		},
		{
			// A file stamped with the version this package writes, but holding
			// a field of another type, is corrupt rather than newer.
			name: "the current version with a field of the wrong type",
			rig:  valid.Rig,
			sha:  valid.SHA,
			file: writeRaw(`{"version":1,"rig":"gastown","sha":"` + valid.SHA + `","started":"yesterday"}`),
			want: "version 1 does not decode",
		},
		{
			name: "wrong rig",
			rig:  valid.Rig,
			sha:  valid.SHA,
			file: writeValid(otherRig),
			want: `rig "beads", want "gastown"`,
		},
		{
			name: "wrong sha",
			rig:  valid.Rig,
			sha:  "fedcba9876543210fedcba9876543210fedcba98",
			file: writeValid(valid),
			want: `want "fedcba9876543210fedcba9876543210fedcba98"`,
		},
		{
			name: "finished before started",
			rig:  valid.Rig,
			sha:  valid.SHA,
			file: writeJSON(finishedFirst),
			want: "is before started",
		},
		{
			name: "two tiers of one name",
			rig:  valid.Rig,
			sha:  valid.SHA,
			file: writeJSON(duplicateTier),
			want: `tier "shell" appears twice`,
		},
		{
			name: "a result that tested no tier",
			rig:  valid.Rig,
			sha:  valid.SHA,
			file: writeJSON(noTiers),
			want: "names no tiers",
		},
		{
			name: "a file larger than a result",
			rig:  valid.Rig,
			sha:  valid.SHA,
			file: writeRaw(strings.Repeat("x", maxResultBytes+1)),
			want: "larger than",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			tc.file(t, dir)
			_, err := Read(dir, tc.rig, tc.sha)
			if err == nil {
				t.Fatal("Read accepted the file")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not name the cause %q", err, tc.want)
			}
		})
	}

	// The daemon polls for the file, so a missing one and an unreadable one
	// must stay tellable apart by errors.Is.
	t.Run("a missing file is distinguishable", func(t *testing.T) {
		t.Parallel()
		_, err := Read(t.TempDir(), valid.Rig, valid.SHA)
		if !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("Read's missing-file error %v does not match fs.ErrNotExist", err)
		}
	})

	// A result path that exists but cannot be read is an error, not a result
	// that has not been written yet.
	t.Run("an unreadable result path is an error", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, ResultFileName, "keep"), 0o755); err != nil {
			t.Fatal(err)
		}
		_, err := Read(dir, valid.Rig, valid.SHA)
		if err == nil {
			t.Fatal("Read accepted a directory as the result")
		}
		if errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("Read's unreadable-path error %v reads as a missing file", err)
		}
		if !strings.Contains(err.Error(), "read result") {
			t.Fatalf("error %q does not name the read it failed", err)
		}
	})

	// A result path the kernel refuses to open, for a reason other than the file
	// being absent, is an error and not a result still to come.
	t.Run("a result path that cannot be opened is an error", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		if err := os.Symlink(ResultFileName, filepath.Join(dir, ResultFileName)); err != nil {
			t.Fatalf("create a self-referencing link: %v", err)
		}
		_, err := Read(dir, valid.Rig, valid.SHA)
		if err == nil {
			t.Fatal("Read accepted a link that cannot be followed")
		}
		if errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("Read's unopenable-path error %v reads as a missing file", err)
		}
		if !strings.Contains(err.Error(), "read result") {
			t.Fatalf("error %q does not name the read it failed", err)
		}
	})
}

// TestResultWriteRefusals: Write refuses the results Read would refuse, so a
// written file always reads back.
func TestResultWriteRefusals(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		change func(*Result)
		want   string
	}{
		{"no rig", func(r *Result) { r.Rig = "" }, "rig"},
		{"rig is a path", func(r *Result) { r.Rig = "a/b" }, "rig"},
		{"short sha", func(r *Result) { r.SHA = "abc123" }, "sha"},
		{"no started", func(r *Result) { r.Started = time.Time{} }, "started is missing"},
		{"no finished", func(r *Result) { r.Finished = time.Time{} }, "finished is missing"},
		{"finished before started", func(r *Result) { r.Finished = r.Started.Add(-time.Second) }, "is before started"},
		{"no backend", func(r *Result) { r.Runner.Backend = "" }, "runner.backend"},
		{"no image", func(r *Result) { r.Runner.Image = "" }, "runner.image"},
		{"no cpus", func(r *Result) { r.Runner.CPUs = 0 }, "runner.cpus"},
		{"no tiers", func(r *Result) { r.Tiers = nil }, "names no tiers"},
		{"tier without a name", func(r *Result) { r.Tiers[0].Tier = "" }, "has no name"},
		{
			"a tier name that is not one segment",
			func(r *Result) {
				r.Tiers[0].Tier = "shell/unit"
				r.Tiers[0].Summary = "tier-sweep: shell/unit GREEN passed=5 failed=0 skipped=0"
			},
			"is not a name",
		},
		{
			"a command with a control character",
			func(r *Result) { r.Tiers[0].Command = "make test\x00" },
			"command holds a control character",
		},
		{
			// The failing names reach the dashboard's attention list, and the
			// log directory in this line is clean, so only the line's own check
			// can refuse it.
			"a summary line with a control character in a failing name",
			func(r *Result) {
				r.Tiers[0].Summary = "tier-sweep: shell RED passed=0 failed=1 skipped=0 failed: ./internal/cm\x01d (logs /out/logs/shell) in 4s"
			},
			"summary line holds a control character",
		},
		{"tier line that does not parse", func(r *Result) { r.Tiers[0].Summary = "tier-sweep: shell GREEN" }, "does not parse"},
		{"tier line naming another tier", func(r *Result) { r.Tiers[0].Tier = "race" }, `names "shell"`},
		{"two tiers of one name", func(r *Result) { r.Tiers = append(r.Tiers, r.Tiers[0]) }, "appears twice"},
		{
			"a tier green with failing tests",
			func(r *Result) {
				r.Tiers[0].Summary = "tier-sweep: shell GREEN passed=5 failed=2 skipped=0 failed: ./internal/cmd"
			},
			"GREEN with 2 failed",
		},
		{
			"a relative log directory",
			func(r *Result) {
				r.Tiers[0].Summary = "tier-sweep: shell GREEN passed=5 failed=0 skipped=0 (logs logs/shell)"
			},
			"is not absolute",
		},
		{
			"a log directory that climbs out of the run",
			func(r *Result) {
				r.Tiers[0].Summary = "tier-sweep: shell GREEN passed=5 failed=0 skipped=0 (logs /out/../etc)"
			},
			"is not a clean path",
		},
		{
			"a log directory with a control character",
			func(r *Result) {
				r.Tiers[0].Summary = "tier-sweep: shell GREEN passed=5 failed=0 skipped=0 (logs /out/lo\x01gs)"
			},
			"control character",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := sampleResult()
			tc.change(&r)
			err := Write(t.TempDir(), r)
			if err == nil {
				t.Fatal("Write accepted the result")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not name the cause %q", err, tc.want)
			}
		})
	}
}

// TestTierResultCarriesTheDaemonShape: this package's summary expression and
// its TierResult fields are the daemon's, read from the daemon's source, so a
// change there that is not followed here fails.
func TestTierResultCarriesTheDaemonShape(t *testing.T) {
	t.Parallel()
	pattern, fields := daemonTierShape(t)

	if got := tierSummaryRE.String(); got != pattern {
		t.Fatalf("this package parses summary lines with\n%s\nthe daemon with\n%s", got, pattern)
	}

	typ := reflect.TypeOf(TierResult{})
	if typ.NumField() != len(fields) {
		t.Fatalf("TierResult has %d fields, the daemon's tierSweepTierResult has %d", typ.NumField(), len(fields))
	}
	for i, want := range fields {
		got := typ.Field(i)
		if got.Name != want.Name || got.Type.String() != want.Type || string(got.Tag) != want.Tag {
			t.Fatalf("TierResult field %d is %s %s %q, the daemon's is %s %s %q",
				i, got.Name, got.Type, got.Tag, want.Name, want.Type, want.Tag)
		}
	}
}

// TestToTierResultsMatchesTheDaemonShape: the conversion marshals to the
// daemon's tier keys and values, the sweep's real summary lines parse to the
// counts, names and log directory the daemon reads from them, and a tier that
// does not convert is refused rather than dropped.
func TestToTierResultsMatchesTheDaemonShape(t *testing.T) {
	t.Parallel()

	t.Run("same keys and values as the daemon's struct", func(t *testing.T) {
		t.Parallel()
		_, fields := daemonTierShape(t)
		r := sampleResult()
		converted, err := ToTierResults(r)
		if err != nil {
			t.Fatalf("ToTierResults: %v", err)
		}
		if len(converted) != len(r.Tiers) {
			t.Fatalf("converted %d tiers, the result names %d", len(converted), len(r.Tiers))
		}

		data, err := json.Marshal(converted["integration"])
		if err != nil {
			t.Fatalf("marshal the converted integration tier: %v", err)
		}
		var got map[string]any
		if err := json.Unmarshal(data, &got); err != nil {
			t.Fatalf("unmarshal the converted integration tier: %v", err)
		}
		want := map[string]any{
			"verdict":      "RED",
			"passed":       float64(65),
			"failed":       float64(1),
			"failed_names": []any{"./internal/daemon"},
			"log":          "/out/logs/integration",
			"log_dir":      "/out/logs/integration",
			"command":      "go test ./...",
			"sha":          r.SHA,
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("converted tier\n%v\nwant\n%v", got, want)
		}

		// Every key it writes is one the daemon's struct declares, and every
		// key the daemon always writes is there. The daemon's optional keys may
		// be absent at their zero value, which is what slot_wait_seconds is for
		// a runner that holds no gate slot.
		for _, field := range fields {
			name, optional := daemonTagName(t, field)
			_, present := got[name]
			if !optional && !present {
				t.Fatalf("the converted tier omits %q, which the daemon's struct always writes", name)
			}
		}
		for k := range got {
			known := false
			for _, field := range fields {
				if name, _ := daemonTagName(t, field); name == k {
					known = true
					break
				}
			}
			if !known {
				t.Fatalf("the converted tier writes key %q, which the daemon's struct does not declare", k)
			}
		}
	})

	t.Run("the sweep's real summary lines", func(t *testing.T) {
		t.Parallel()
		pattern, _ := daemonTierShape(t)
		daemonRE := regexp.MustCompile(pattern)
		cases := []struct {
			name    string
			line    string
			tier    string
			verdict string
			passed  int
			failed  int
			skipped int
			names   []string
			logDir  string
			elapsed string
		}{
			{
				name:    "the integration tier that failed internal/daemon",
				line:    "tier-sweep: integration RED passed=65 failed=1 skipped=4 failed: ./internal/daemon (logs /out/logs/integration) in 28m50s",
				tier:    "integration",
				verdict: "RED",
				passed:  65,
				failed:  1,
				skipped: 4,
				names:   []string{"./internal/daemon"},
				logDir:  "/out/logs/integration",
				elapsed: "28m50s",
			},
			{
				name:    "a green shell tier",
				line:    "tier-sweep: shell GREEN passed=5 failed=0 skipped=0 (logs /out/logs/shell) in 45s",
				tier:    "shell",
				verdict: "GREEN",
				passed:  5,
				logDir:  "/out/logs/shell",
				elapsed: "45s",
			},
			{
				name:    "a red race tier naming two packages",
				line:    "tier-sweep: race RED passed=2 failed=2 skipped=1 failed: ./internal/cmd ./internal/daemon (logs /out/logs/race) in 1h1m1s",
				tier:    "race",
				verdict: "RED",
				passed:  2,
				failed:  2,
				skipped: 1,
				names:   []string{"./internal/cmd", "./internal/daemon"},
				logDir:  "/out/logs/race",
				elapsed: "1h1m1s",
			},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				m := daemonRE.FindStringSubmatch(tc.line)
				if m == nil {
					t.Fatalf("the daemon's expression does not match %q", tc.line)
				}
				if m[1] != tc.tier || m[2] != tc.verdict || m[8] != tc.elapsed {
					t.Fatalf("the daemon reads %v, want tier %s verdict %s elapsed %s", m, tc.tier, tc.verdict, tc.elapsed)
				}
				if atoi(t, m[3]) != tc.passed || atoi(t, m[4]) != tc.failed || atoi(t, m[5]) != tc.skipped {
					t.Fatalf("the daemon reads counts %s/%s/%s, want %d/%d/%d", m[3], m[4], m[5], tc.passed, tc.failed, tc.skipped)
				}

				tier, got, ok := parseTierSummary(tc.line)
				if !ok {
					t.Fatalf("parseTierSummary refused %q", tc.line)
				}
				if tier != tc.tier {
					t.Fatalf("parseTierSummary named the tier %q, want %q", tier, tc.tier)
				}
				if got.Verdict != tc.verdict || got.Passed != tc.passed || got.Failed != tc.failed ||
					got.Log != tc.logDir || got.LogDir != tc.logDir {
					t.Fatalf("parseTierSummary returned %+v, want verdict %s passed %d failed %d log dir %s",
						got, tc.verdict, tc.passed, tc.failed, tc.logDir)
				}
				if !slices.Equal(got.FailedNames, tc.names) {
					t.Fatalf("parseTierSummary named failures %v, want %v", got.FailedNames, tc.names)
				}
			})
		}
	})

	// A tier left out of the map reads to the daemon as a tier that never ran,
	// which is a wrong answer: the conversion must say it cannot convert.
	t.Run("a tier it cannot convert is refused, not dropped", func(t *testing.T) {
		t.Parallel()
		cases := []struct {
			name   string
			change func(*Result)
			want   string
		}{
			{
				"a summary line that does not parse",
				func(r *Result) { r.Tiers = append(r.Tiers, Tier{Tier: "race", Summary: "tier-sweep: race GREEN"}) },
				`tier "race": summary line does not parse`,
			},
			{
				"a green verdict with failing tests",
				func(r *Result) {
					r.Tiers[0].Summary = "tier-sweep: shell GREEN passed=5 failed=2 skipped=0 failed: ./internal/cmd"
				},
				`tier "shell" is GREEN with 2 failed`,
			},
			{
				"a log directory outside the run",
				func(r *Result) {
					r.Tiers[0].Summary = "tier-sweep: shell GREEN passed=5 failed=0 skipped=0 (logs logs/shell)"
				},
				"is not absolute",
			},
			{
				"two tiers of one name",
				func(r *Result) { r.Tiers = append(r.Tiers, r.Tiers[0]) },
				`tier "shell" appears twice`,
			},
			{
				"a result that tested no tier",
				func(r *Result) { r.Tiers = nil },
				"names no tiers",
			},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				r := sampleResult()
				tc.change(&r)

				converted, err := ToTierResults(r)
				if err == nil {
					t.Fatalf("ToTierResults converted %v and dropped the tier it could not convert", converted)
				}
				if converted != nil {
					t.Fatalf("ToTierResults returned %v beside its error; a partial conversion is a wrong answer", converted)
				}
				if !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("error %q does not name the cause %q", err, tc.want)
				}
			})
		}
	})
}

func atoi(t *testing.T, s string) int {
	t.Helper()
	n, err := strconv.Atoi(s)
	if err != nil {
		t.Fatalf("atoi %q: %v", s, err)
	}
	return n
}

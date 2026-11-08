package imagevalues_test

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/steveyegge/gastown/internal/imagevalues"
)

const (
	testCommit = "0123456789abcdef0123456789abcdef01234567"
	testDigest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
)

// testRegistry is the registry the fixtures build refs in: a value no fork
// names, since the command is what supplies a town's own.
var testRegistry = imagevalues.Registry{Scheme: "https", Host: "registry.test:5000", Owner: "town"}

// testValues is a valid pair at the fixture commit.
func testValues() imagevalues.Values {
	return imagevalues.Values{
		Daemon:  testRegistry.Ref(imagevalues.DaemonImage, testDigest),
		Polecat: testRegistry.Ref(imagevalues.PolecatImage, testDigest),
		Commit:  testCommit,
	}
}

// workflowEnv reads the images workflow's env block: the operator's one
// spelling of the town's Forgejo host and owner and the two image names
// (deploy/ci/images.workflow.yml).
func workflowEnv(t *testing.T) map[string]string {
	t.Helper()
	const path = "../../deploy/ci/images.workflow.yml"
	raw, err := os.ReadFile(filepath.FromSlash(path))
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	var doc struct {
		Env map[string]string `yaml:"env"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("%s is not valid YAML: %v", path, err)
	}
	return doc.Env
}

// TestImageValuesHoldsNoForkLocalNames holds the passed-in rule: the registry
// host and owner are the command's to read from the town, so this package may
// not repeat the fork's own values. A constant that named either would make
// the shared package wrong for the next town (gt-gg9hv.63).
func TestImageValuesHoldsNoForkLocalNames(t *testing.T) {
	t.Parallel()
	env := workflowEnv(t)
	forkNames := map[string]string{
		"FORGEJO_HOST":   env["FORGEJO_HOST"],
		"REGISTRY_OWNER": env["REGISTRY_OWNER"],
	}
	for key, name := range forkNames {
		if name == "" {
			t.Fatalf("the workflow names no %s, so this test cannot tell a fork-local value from an empty one", key)
		}
	}

	t.Run("the package's own sources hold neither name", func(t *testing.T) {
		hits, scanned := forkLocalHits(t, ".", forkNames)
		for _, hit := range hits {
			t.Errorf("%s; the command reads it from the town and passes it in", hit)
		}
		if scanned == 0 {
			t.Fatal("no package source was scanned, so this test proves nothing")
		}
	})

	t.Run("a source holding a fork name is reported", func(t *testing.T) {
		dir := t.TempDir()
		for key, name := range forkNames {
			path := filepath.Join(dir, strings.ToLower(key)+".go")
			if err := os.WriteFile(path, []byte("package fixture\n\nconst here = \""+name+"\"\n"), 0o600); err != nil {
				t.Fatalf("writing %s: %v", path, err)
			}
		}
		// A clean source and a test source holding the name: the first is not a
		// hit, and the second is skipped rather than read.
		for name, body := range map[string]string{
			"clean.go":      "package fixture\n",
			"clean_test.go": "package fixture\n\nconst here = \"" + env["FORGEJO_HOST"] + "\"\n",
		} {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
				t.Fatalf("writing %s: %v", name, err)
			}
		}

		hits, scanned := forkLocalHits(t, dir, forkNames)
		if want := len(forkNames) + 1; scanned != want {
			t.Errorf("scanned %d sources, want %d (test sources are skipped)", scanned, want)
		}
		if len(hits) != len(forkNames) {
			t.Fatalf("hits = %q, want one per fork name", hits)
		}
		for key, name := range forkNames {
			reported := false
			for _, hit := range hits {
				if strings.Contains(hit, key) && strings.Contains(hit, name) {
					reported = true
				}
			}
			if !reported {
				t.Errorf("the source holding %s %q was not reported; hits = %q", key, name, hits)
			}
		}
	})
}

// forkLocalHits reads every non-test .go source directly in dir and reports
// each one that holds a fork name, plus how many sources it read.
func forkLocalHits(t *testing.T, dir string, forkNames map[string]string) (hits []string, scanned int) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		scanned++
		for key, forkName := range forkNames {
			if strings.Contains(string(data), forkName) {
				hits = append(hits, fmt.Sprintf("%s holds the fork's %s %q", name, key, forkName))
			}
		}
	}
	return hits, scanned
}

// TestImageValuesRoundTrips holds the writer and reader together: what Write
// records, Load answers whole.
func TestImageValuesRoundTrips(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "values", "images.yaml")
	want := testValues()
	if err := imagevalues.Write(path, want); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := imagevalues.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got != want {
		t.Errorf("Load = %+v, want %+v", got, want)
	}
}

// TestImageValuesLoadRefusesAMalformedFile holds the reader's refusals: a file
// that does not parse, or one whose pair or commit is not the shape a deploy
// can use, is an error rather than a partial pair.
func TestImageValuesLoadRefusesAMalformedFile(t *testing.T) {
	t.Parallel()
	good := testValues()
	rows := []struct {
		name string
		file string
		want string
	}{
		{"not YAML", "daemon: [", "does not parse"},
		{"no daemon line", "polecat: " + good.Polecat + "\ncommit: " + good.Commit + "\n", "no daemon line"},
		{"no polecat line", "daemon: " + good.Daemon + "\ncommit: " + good.Commit + "\n", "no polecat line"},
		{"no commit line", "daemon: " + good.Daemon + "\npolecat: " + good.Polecat + "\n", "no commit line"},
		{"a tag instead of a digest", "daemon: gastown-daemon:0123456789ab\npolecat: " + good.Polecat + "\ncommit: " + good.Commit + "\n", "daemon"},
		{"an uppercase digest", "daemon: " + strings.Replace(good.Daemon, "sha256:1111", "sha256:111A", 1) + "\npolecat: " + good.Polecat + "\ncommit: " + good.Commit + "\n", "daemon"},
		{"a short commit", "daemon: " + good.Daemon + "\npolecat: " + good.Polecat + "\ncommit: 0123456\n", "commit"},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "images.yaml")
			if err := os.WriteFile(path, []byte(row.file), 0o600); err != nil {
				t.Fatalf("writing the fixture: %v", err)
			}
			got, err := imagevalues.Load(path)
			if err == nil {
				t.Fatalf("Load = %+v, want an error", got)
			}
			if !strings.Contains(err.Error(), row.want) {
				t.Errorf("Load error = %q, want it to name %q", err, row.want)
			}
		})
	}
}

// TestImageValuesWriteRefusesAPairItCannotRecord holds the writer's refusals
// before any file is touched: an invalid pair, a directory it cannot create,
// and a target it cannot replace.
func TestImageValuesWriteRefusesAPairItCannotRecord(t *testing.T) {
	t.Parallel()

	t.Run("an invalid pair", func(t *testing.T) {
		t.Parallel()
		err := imagevalues.Write(filepath.Join(t.TempDir(), "images.yaml"), imagevalues.Values{})
		if err == nil || !strings.Contains(err.Error(), "no daemon line") {
			t.Fatalf("Write = %v, want a refusal naming the missing daemon line", err)
		}
	})

	t.Run("a directory that cannot be created", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		file := filepath.Join(dir, "not-a-directory")
		if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
			t.Fatalf("writing the fixture: %v", err)
		}
		err := imagevalues.Write(filepath.Join(file, "images.yaml"), testValues())
		if err == nil || !strings.Contains(err.Error(), "create") {
			t.Fatalf("Write below a regular file = %v, want a create refusal", err)
		}
	})

	t.Run("a target that cannot be replaced", func(t *testing.T) {
		t.Parallel()
		err := imagevalues.Write(t.TempDir(), testValues())
		if err == nil || !strings.Contains(err.Error(), "write") {
			t.Fatalf("Write onto a directory = %v, want a write refusal", err)
		}
	})
}

// TestImageValuesLoadKeepsAMissingFileDistinct holds the absence rule: a file
// that is not there reports fs.ErrNotExist, so the caller can tell a first run
// from a file it could not read.
func TestImageValuesLoadKeepsAMissingFileDistinct(t *testing.T) {
	t.Parallel()
	if _, err := imagevalues.Load(filepath.Join(t.TempDir(), "absent.yaml")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Load of a missing file = %v, want fs.ErrNotExist", err)
	}
}

// TestImageValuesRegistryRefusesAPartThatWouldNotReachAValue holds the
// registry's refusals: a scheme, host or owner that a URL path or a values
// line could not carry as typed is named rather than passed to a request.
func TestImageValuesRegistryRefusesAPartThatWouldNotReachAValue(t *testing.T) {
	t.Parallel()
	rows := []struct {
		name string
		reg  imagevalues.Registry
		want string
	}{
		{"no scheme", imagevalues.Registry{Host: "registry.test", Owner: "town"}, "scheme"},
		{"another scheme", imagevalues.Registry{Scheme: "ssh", Host: "registry.test", Owner: "town"}, "scheme"},
		{"a host with a path", imagevalues.Registry{Scheme: "https", Host: "registry.test/v2", Owner: "town"}, "host"},
		{"an uppercase host", imagevalues.Registry{Scheme: "https", Host: "Registry.test", Owner: "town"}, "host"},
		{"a bracketed IPv6 address with a port", imagevalues.Registry{Scheme: "https", Host: "[::1]:5000", Owner: "town"}, "host"},
		{"no owner", imagevalues.Registry{Scheme: "https", Host: "registry.test", Owner: ""}, "owner"},
		{"an owner with a path", imagevalues.Registry{Scheme: "https", Host: "registry.test", Owner: "a/b"}, "owner"},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			err := row.reg.Validate()
			switch {
			case row.want == "":
				if err != nil {
					t.Fatalf("Validate = %v, want the registry accepted", err)
				}
			case err == nil:
				t.Fatalf("Validate = nil, want a refusal naming %q", row.want)
			case !strings.Contains(err.Error(), row.want):
				t.Errorf("Validate = %q, want it to name %q", err, row.want)
			}
		})
	}
}

// TestImageValuesValidateMatchesTheWriter holds Validate and Write to one
// answer: every registry Validate accepts can name a pair the writer records,
// and a host the writer could not emit (a bracketed IPv6 literal, whose ref
// would start a YAML flow sequence) is refused by Validate with the reason.
func TestImageValuesValidateMatchesTheWriter(t *testing.T) {
	t.Parallel()
	for _, reg := range []imagevalues.Registry{
		{Scheme: "https", Host: "registry.test", Owner: "town"},
		{Scheme: "http", Host: "forgejo:3000", Owner: "town"},
		{Scheme: "https", Host: "registry.test:5000", Owner: "town"},
	} {
		if err := reg.Validate(); err != nil {
			t.Fatalf("Validate(%+v) = %v, want the registry accepted", reg, err)
		}
		v := imagevalues.Values{
			Daemon:  reg.Ref(imagevalues.DaemonImage, testDigest),
			Polecat: reg.Ref(imagevalues.PolecatImage, testDigest),
			Commit:  testCommit,
		}
		if err := imagevalues.Write(filepath.Join(t.TempDir(), "images.yaml"), v); err != nil {
			t.Errorf("Write of a pair from accepted registry %+v = %v; Validate accepts a value the writer cannot write", reg, err)
		}
	}

	bracketed := imagevalues.Registry{Scheme: "https", Host: "[::1]:5000", Owner: "town"}
	err := bracketed.Validate()
	if err == nil || !strings.Contains(err.Error(), "bracketed IPv6") {
		t.Fatalf("Validate of a bracketed host = %v, want a refusal naming the bracketed literal and why", err)
	}
}

// TestImageValuesValidators holds the shapes the command and the reader share.
func TestImageValuesValidators(t *testing.T) {
	t.Parallel()
	commitRows := map[string]bool{
		testCommit: true,
		"0123456789abcdef0123456789abcdef0123456":    false,
		"0123456789abcdef0123456789abcdef0123456789": false,
		"0123456789ABCDEF0123456789abcdef01234567":   false,
	}
	for value, want := range commitRows {
		if got := imagevalues.ValidCommit(value); got != want {
			t.Errorf("ValidCommit(%q) = %v, want %v", value, got, want)
		}
	}
	digestRows := map[string]bool{
		testDigest:                          true,
		"sha256:" + strings.Repeat("a", 63): false,
		"sha512:" + strings.Repeat("a", 64): false,
		"sha256:" + strings.Repeat("A", 64): false,
	}
	for value, want := range digestRows {
		if got := imagevalues.ValidDigest(value); got != want {
			t.Errorf("ValidDigest(%q) = %v, want %v", value, got, want)
		}
	}
}

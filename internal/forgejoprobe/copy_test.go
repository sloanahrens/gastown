package forgejoprobe

import (
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// gitCall is one Run the code under test made, as the fake recorded it.
type gitCall struct {
	dir  string
	args []string
}

// gitAnswer is what the fake returns for one argv.
type gitAnswer struct {
	out string
	err error
}

// fakeGit answers each git argv from a table keyed by the argv joined with
// spaces. An argv the table does not name fails, so a test sees every call the
// code makes, including one it should not.
type fakeGit struct {
	answers map[string]gitAnswer
	calls   []gitCall
}

func (f *fakeGit) Run(dir string, _ []string, args ...string) (string, error) {
	f.calls = append(f.calls, gitCall{dir: dir, args: slices.Clone(args)})
	a, ok := f.answers[strings.Join(args, " ")]
	if !ok {
		return "", errors.New("fakeGit: no answer for " + strings.Join(args, " "))
	}
	return a.out, a.err
}

// argvs returns the argv of every call, in order.
func (f *fakeGit) argvs() [][]string {
	out := make([][]string, 0, len(f.calls))
	for _, c := range f.calls {
		out = append(out, c.args)
	}
	return out
}

const testGitHubURL = "https://github.com/acme/rig.git"

// gateWorkflow is a gate file in the shape the probe's script test used: one
// workflow, one job.
const gateWorkflow = "name: ci\non: push\njobs:\n  gate:\n    runs-on: ubuntu-latest\n"

// tokenPath is a 40-character token a forge can carry as a path segment.
const tokenPath = "0123456789abcdef0123456789abcdef01234567"

// redactRow is one URL and the error git echoes for it. echo names the URL in a
// spelling redaction must catch; wantEcho is echo once redacted; secret is what
// must not reach the error; header is how a push error names the URL itself.
type redactRow struct {
	name     string
	rawURL   string
	echo     string
	secret   string
	wantEcho string
	header   string
}

// redactRows covers the spellings git prints a URL in: with its password, with a
// token in the path, without its scheme, and a URL that does not parse.
var redactRows = []redactRow{
	{
		name:     "a password in the URL and in git's echo",
		rawURL:   "https://ci:s3cret@forgejo.example/acme/rig.git",
		echo:     "fatal: unable to access 'https://ci:s3cret@forgejo.example/acme/rig.git/': refused",
		secret:   "s3cret",
		wantEcho: "fatal: unable to access '<repository URL>/': refused",
		header:   "https://ci:xxxxx@forgejo.example/acme/rig.git",
	},
	{
		name:     "a token carried as a path segment",
		rawURL:   "https://forgejo.example/" + tokenPath + "/rig.git",
		echo:     "fatal: unable to access 'https://forgejo.example/" + tokenPath + "/rig.git/': refused",
		secret:   tokenPath,
		wantEcho: "fatal: unable to access '<repository URL>/': refused",
		header:   "<repository URL>",
	},
	{
		name:     "the URL echoed without its scheme",
		rawURL:   "https://forgejo.example/acme/rig.git",
		echo:     "fatal: 'forgejo.example/acme/rig.git/' not found",
		wantEcho: "fatal: '<repository URL>/' not found",
		header:   "https://forgejo.example/acme/rig.git",
	},
	{
		name:     "a URL that does not parse",
		rawURL:   "https://ci:s3cret@forgejo.example/%zz/rig.git",
		echo:     "fatal: unable to access 'https://ci:s3cret@forgejo.example/%zz/rig.git/': refused",
		secret:   "s3cret",
		wantEcho: "fatal: unable to access '<repository URL>/': refused",
		header:   "<repository URL>",
	},
	{
		// A username with no password is not a secret: the word that spells
		// it inside "specific" must survive.
		name:     "a username with no password",
		rawURL:   "https://ci@forgejo.example/acme/rig.git",
		echo:     "fatal: unable to access 'https://ci@forgejo.example/acme/rig.git/': specific refusal",
		wantEcho: "fatal: unable to access '<repository URL>/': specific refusal",
		header:   "https://ci@forgejo.example/acme/rig.git",
	},
}

// TestCloneCopyRedactsEveryForm pins the redaction of a failing clone: each
// spelling of the URL that git echoes reaches the error redacted, and no secret
// the URL carried does.
func TestCloneCopyRedactsEveryForm(t *testing.T) {
	t.Parallel()
	for _, row := range redactRows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			g := &fakeGit{answers: map[string]gitAnswer{
				"clone --quiet -- " + row.rawURL + " dest/copy": {err: errors.New(row.echo)},
			}}
			err := CloneCopy(g, row.rawURL, "dest/copy")
			if want := "git clone failed: " + row.wantEcho; err == nil || err.Error() != want {
				t.Fatalf("CloneCopy = %v, want %q", err, want)
			}
			if row.secret != "" && strings.Contains(err.Error(), row.secret) {
				t.Fatalf("error carries a secret the URL held")
			}
		})
	}
}

// TestCloneCopyIsFull pins the clone to git clone --quiet -- URL DEST, with no
// --depth: Forgejo refuses a shallow push, and the -- keeps a URL from being
// read as an option. A failing clone's error carries git's own output, and no
// credential from the URL.
func TestCloneCopyIsFull(t *testing.T) {
	t.Parallel()
	t.Run("the argv is a full quiet clone", func(t *testing.T) {
		t.Parallel()
		g := &fakeGit{answers: map[string]gitAnswer{
			"clone --quiet -- " + testGitHubURL + " dest/copy": {},
		}}
		if err := CloneCopy(g, testGitHubURL, "dest/copy"); err != nil {
			t.Fatalf("CloneCopy: %v", err)
		}
		want := [][]string{{"clone", "--quiet", "--", testGitHubURL, "dest/copy"}}
		if got := g.argvs(); !reflect.DeepEqual(got, want) {
			t.Fatalf("git argv = %q, want %q", got, want)
		}
	})
	t.Run("a failing clone carries git's output", func(t *testing.T) {
		t.Parallel()
		g := &fakeGit{answers: map[string]gitAnswer{
			"clone --quiet -- " + testGitHubURL + " dest/copy": {err: errors.New("fatal: repository 'acme/rig' not found")},
		}}
		err := CloneCopy(g, testGitHubURL, "dest/copy")
		if err == nil {
			t.Fatal("CloneCopy succeeded, want the clone's failure")
		}
		want := "git clone failed: fatal: repository 'acme/rig' not found"
		if err.Error() != want {
			t.Fatalf("error = %q, want %q", err.Error(), want)
		}
	})
	t.Run("a failing clone does not echo the URL's credentials", func(t *testing.T) {
		t.Parallel()
		// git echoes a credentialed form of the URL, not the URL as passed.
		g := &fakeGit{answers: map[string]gitAnswer{
			"clone --quiet -- " + testGitHubURL + " dest/copy": {err: errors.New(
				"fatal: unable to access 'https://ci:s3cret@github.com/acme/rig.git/': timed out")},
		}}
		err := CloneCopy(g, testGitHubURL, "dest/copy")
		if err == nil {
			t.Fatal("CloneCopy succeeded, want the clone's failure")
		}
		if strings.Contains(err.Error(), "s3cret") || strings.Contains(err.Error(), "ci:") {
			t.Fatalf("error %q carries the URL's credentials", err.Error())
		}
		// The host and path are redacted with the credentials: the spelling
		// without a scheme is one of the forms git prints.
		if want := "git clone failed: fatal: unable to access 'https://<redacted>@<repository URL>/': timed out"; err.Error() != want {
			t.Fatalf("error = %q, want %q", err.Error(), want)
		}
	})
	t.Run("a URL git prints whole is replaced by its name", func(t *testing.T) {
		t.Parallel()
		const withToken = "https://github.com/acme/rig.git?token=abc123"
		g := &fakeGit{answers: map[string]gitAnswer{
			"clone --quiet -- " + withToken + " dest/copy": {err: errors.New("fatal: repository '" + withToken + "' not found")},
		}}
		err := CloneCopy(g, withToken, "dest/copy")
		if err == nil {
			t.Fatal("CloneCopy succeeded, want the clone's failure")
		}
		if strings.Contains(err.Error(), "abc123") {
			t.Fatalf("error %q carries the URL's token", err.Error())
		}
		if want := "git clone failed: fatal: repository '<repository URL>' not found"; err.Error() != want {
			t.Fatalf("error = %q, want %q", err.Error(), want)
		}
	})
}

// TestGateIdentityDerivesTheContext reads the gate file from the ref the probe
// pushes, derives the identity with land.ParseGateWorkflow and returns the
// commit the ref names. The job reports under its name: when it sets one, so
// the probe waits on the context the landing worker waits on.
func TestGateIdentityDerivesTheContext(t *testing.T) {
	t.Parallel()
	const copyDir = "work/copy"
	cases := []struct {
		name        string
		workflow    string
		wantContext string
	}{
		{name: "the script test's gate file", workflow: gateWorkflow, wantContext: "ci / gate (push)"},
		{
			name:        "a workflow named build with job check",
			workflow:    "name: build\non: push\njobs:\n  check:\n    runs-on: ubuntu-latest\n",
			wantContext: "build / check (push)",
		},
		{
			name:        "a job name overrides the key",
			workflow:    "name: ci\non: push\njobs:\n  gate:\n    name: verdict\n    runs-on: ubuntu-latest\n",
			wantContext: "ci / verdict (push)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			g := &fakeGit{answers: map[string]gitAnswer{
				"show refs/remotes/origin/main:.forgejo/workflows/gate.yml": {out: tc.workflow},
				"rev-parse --verify refs/remotes/origin/main^{commit}":      {out: "0123abcd\n"},
			}}
			wf, sha, err := GateIdentity(g, copyDir, "main")
			if err != nil {
				t.Fatalf("GateIdentity: %v", err)
			}
			if got := wf.Context(); got != tc.wantContext {
				t.Errorf("context = %q, want %q", got, tc.wantContext)
			}
			if sha != "0123abcd" {
				t.Errorf("sha = %q, want 0123abcd", sha)
			}
		})
	}
}

// TestGateIdentityRefusals refuses, before any SHA is read, a copy whose ref
// has no gate file and a gate with two jobs or an unnamed job. The missing-file
// refusal reads the file from the ref, never from the work tree, and names the
// repository from the origin remote. A missing ref is its own refusal, not a
// missing file.
func TestGateIdentityRefusals(t *testing.T) {
	t.Parallel()
	const showRef = "show refs/remotes/origin/main:.forgejo/workflows/gate.yml"
	const origin = "config --get remote.origin.url"
	const revParse = "rev-parse --verify refs/remotes/origin/main^{commit}"
	cases := []struct {
		name    string
		answers map[string]gitAnswer
		want    string
	}{
		{
			name: "a missing gate file",
			answers: map[string]gitAnswer{
				showRef:  {err: errors.New("fatal: path does not exist")},
				revParse: {out: "0123abcd\n"},
				origin:   {out: testGitHubURL + "\n"},
			},
			want: "refusing: the copy of acme/rig's main has no .forgejo/workflows/gate.yml, so Forgejo would run the repo's .github/workflows and nothing would gate",
		},
		{
			name: "a missing gate file with no origin to name",
			answers: map[string]gitAnswer{
				showRef:  {err: errors.New("fatal: path does not exist")},
				revParse: {out: "0123abcd\n"},
				origin:   {err: errors.New("exit status 1")},
			},
			want: "refusing: the copy of the repository's main has no .forgejo/workflows/gate.yml, so Forgejo would run the repo's .github/workflows and nothing would gate",
		},
		{
			name: "a missing gate file with an origin that names no owner",
			answers: map[string]gitAnswer{
				showRef:  {err: errors.New("fatal: path does not exist")},
				revParse: {out: "0123abcd\n"},
				origin:   {out: "rig.git\n"},
			},
			want: "refusing: the copy of the repository's main has no .forgejo/workflows/gate.yml, so Forgejo would run the repo's .github/workflows and nothing would gate",
		},
		{
			name: "a missing ref is not a missing file",
			answers: map[string]gitAnswer{
				showRef:  {err: errors.New("fatal: invalid object name")},
				revParse: {err: errors.New("fatal: Needed a single revision")},
			},
			want: "the copy has no refs/remotes/origin/main",
		},
		{
			name: "two jobs",
			answers: map[string]gitAnswer{
				showRef: {out: "name: ci\non: push\njobs:\n  a:\n    runs-on: x\n  b:\n    runs-on: x\n"},
			},
			want: "the gate workflow has 2 jobs, want exactly one",
		},
		{
			name: "a job with no name",
			answers: map[string]gitAnswer{
				showRef: {out: "name: ci\non: push\njobs:\n  \"\":\n    runs-on: x\n"},
			},
			want: "the gate workflow's job has no name",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			g := &fakeGit{answers: tc.answers}
			wf, sha, err := GateIdentity(g, "work/copy", "main")
			if err == nil {
				t.Fatalf("GateIdentity accepted the copy as %q at %q, want a refusal", wf.Context(), sha)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want it to contain %q", err.Error(), tc.want)
			}
			if got := g.calls[0]; got.dir != "work/copy" || !slices.Equal(got.args, []string{"show", "refs/remotes/origin/main:.forgejo/workflows/gate.yml"}) {
				t.Errorf("first git call = dir %q args %q, want show of the ref in the copy", got.dir, got.args)
			}
			// A copy whose file reads is refused on its gate, so no ref is
			// read. A failed show only asks whether the ref exists.
			if tc.answers[showRef].err == nil {
				for _, c := range g.calls {
					if slices.Contains(c.args, "rev-parse") {
						t.Errorf("rev-parse ran on a refused copy: %q", c.args)
					}
				}
			}
		})
	}
}

// TestGateIdentityNeedsTheRef refuses a copy whose gate file reads but whose
// ref names no commit: the probe has no SHA to push, so there is no run to
// start.
func TestGateIdentityNeedsTheRef(t *testing.T) {
	t.Parallel()
	g := &fakeGit{answers: map[string]gitAnswer{
		"show refs/remotes/origin/main:.forgejo/workflows/gate.yml": {out: gateWorkflow},
		"rev-parse --verify refs/remotes/origin/main^{commit}":      {err: errors.New("fatal: Needed a single revision")},
	}}
	wf, sha, err := GateIdentity(g, "work/copy", "main")
	if err == nil || err.Error() != "the copy has no refs/remotes/origin/main" {
		t.Fatalf("GateIdentity = %q at %q, %v; want the copy has no refs/remotes/origin/main", wf.Context(), sha, err)
	}
	if sha != "" {
		t.Errorf("sha = %q on a refused copy, want none", sha)
	}
}

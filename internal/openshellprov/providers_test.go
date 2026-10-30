package openshellprov

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"slices"
	"strings"
	"testing"
)

const (
	deepseekSecret = "sk-deepseek-test-zqxvjkcd"
	forgejoSecret  = "forgejo-test-token-zqxvjkcd"
)

// createRunner answers provider list with listing, and every provider create with
// printed on both streams and failErr. It records each argv and the environment
// the last create ran with.
type createRunner struct {
	listing   string
	listErr   error
	printed   string
	failErr   error
	argv      [][]string
	createEnv []string
}

func (c *createRunner) Run(name string, args []string, env []string, _ io.Reader) ([]byte, []byte, error) {
	c.argv = append(c.argv, append([]string{name}, args...))
	if slices.Contains(args, "create") {
		c.createEnv = env
		return []byte(c.printed), []byte(c.printed), c.failErr
	}
	return []byte(c.listing), nil, c.listErr
}

// firstWriteOnly takes one write and refuses every later one, so a failure that
// only shows when a held tail is flushed in Close is reached.
type firstWriteOnly struct{ taken bool }

func (w *firstWriteOnly) Write(p []byte) (int, error) {
	if w.taken {
		return 0, errors.New("disk full")
	}
	w.taken = true
	return len(p), nil
}

// creates counts the provider create calls the runner saw.
func (c *createRunner) creates() int {
	n := 0
	for _, argv := range c.argv {
		if slices.Contains(argv, "create") {
			n++
		}
	}
	return n
}

// newCreateRun builds a run that creates providers through runner and reads key
// files from files, with its two streams in the returned buffers.
func newCreateRun(runner *createRunner, files *memFiles) (*Run, *bytes.Buffer, *bytes.Buffer) {
	out, errw := &bytes.Buffer{}, &bytes.Buffer{}
	return &Run{
		Client: Client{Run: runner},
		Files:  files,
		Env:    []string{"PATH=/usr/bin"},
		Out:    out,
		Err:    errw,
	}, out, errw
}

// The secret reaches openshell only as the credential's environment variable: argv
// names the variable, the child's environment holds the value, and every copy of
// the value in what create prints is masked, on both streams.
func TestCreateProviderSecretNeverInArgvOrOutput(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		secret  string
		printed string
		wantOut string
	}{
		{
			name:    "a long key",
			secret:  deepseekSecret,
			printed: "created provider deepseek with value=" + deepseekSecret + "\n",
			wantOut: "created provider deepseek with value=***\n",
		},
		{
			// q is in no argv token the create call builds, so a substring match on
			// it means the secret leaked into argv.
			name:    "a one-character key",
			secret:  "q",
			printed: "value=q\n",
			wantOut: "value=***\n",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			runner := &createRunner{listing: "NAME TYPE\n", printed: tc.printed}
			files := newMemFiles()
			files.contents[testConfig+"/deepseek.env"] = []byte("DEEPSEEK_API_KEY=" + tc.secret + "\n")
			run, out, errw := newCreateRun(runner, files)

			if err := run.CreateProvider("deepseek", "deepseek-claude", testConfig+"/deepseek.env", "DEEPSEEK_API_KEY", "ANTHROPIC_AUTH_TOKEN"); err != nil {
				t.Fatalf("CreateProvider: %v", err)
			}
			var create []string
			for _, argv := range runner.argv {
				if slices.Contains(argv, "create") {
					create = argv
				}
				for _, arg := range argv {
					if strings.Contains(arg, tc.secret) {
						t.Errorf("argv %q carries the secret as an argument", argv)
					}
				}
			}
			if i := slices.Index(create, "--credential"); i < 0 || i+1 >= len(create) || create[i+1] != "ANTHROPIC_AUTH_TOKEN" {
				t.Errorf("create argv %q does not name the credential ANTHROPIC_AUTH_TOKEN", create)
			}
			if !slices.Contains(runner.createEnv, "ANTHROPIC_AUTH_TOKEN="+tc.secret) {
				t.Errorf("create environment does not hold the secret under the credential name")
			}
			if !slices.Contains(runner.createEnv, "PATH=/usr/bin") {
				t.Errorf("create environment = %q, want the run's environment inherited", runner.createEnv)
			}
			if got := out.String(); got != tc.wantOut {
				t.Errorf("stdout = %q, want %q", got, tc.wantOut)
			}
			if got := errw.String(); got != tc.wantOut {
				t.Errorf("stderr = %q, want %q", got, tc.wantOut)
			}
		})
	}
}

// The three refusals before a create: an existing provider is left alone and its
// key file is never read, an unreadable key file and a key file without the key
// each stop the run with their message and create nothing.
func TestCreateProviderExistingUnreadableAndMissingKey(t *testing.T) {
	t.Parallel()
	const file = testConfig + "/deepseek.env"
	tests := []struct {
		name    string
		listing string
		listErr error
		setup   func(files *memFiles)
		wantOut string
		wantErr string
	}{
		{
			name:    "an existing provider is left alone and its key file is never read",
			listing: "NAME TYPE\ndeepseek deepseek-claude\n",
			setup:   func(files *memFiles) { files.readErr = fs.ErrPermission },
			wantOut: "openshell-providers: provider deepseek exists\n",
		},
		{
			name:    "a failed provider list stops the create before any key file is read",
			listErr: errors.New("gateway down"),
			setup:   func(files *memFiles) { files.readErr = fs.ErrPermission },
			wantErr: "openshell-providers: openshell provider list failed, so whether provider deepseek exists is unknown",
		},
		{
			name:    "an unreadable key file stops the create",
			setup:   func(files *memFiles) { files.readErr = fs.ErrPermission },
			wantErr: "openshell-providers: " + file + " is unreadable, so provider deepseek cannot be created",
		},
		{
			name:    "a key file without the key stops the create",
			setup:   func(files *memFiles) { files.contents[file] = []byte("OTHER=1\n") },
			wantErr: "openshell-providers: " + file + " does not set DEEPSEEK_API_KEY",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			runner := &createRunner{listing: tc.listing, listErr: tc.listErr}
			files := newMemFiles()
			tc.setup(files)
			run, out, _ := newCreateRun(runner, files)

			err := run.CreateProvider("deepseek", "deepseek-claude", file, "DEEPSEEK_API_KEY", "ANTHROPIC_AUTH_TOKEN")
			switch {
			case tc.wantErr != "" && (err == nil || err.Error() != tc.wantErr):
				t.Errorf("CreateProvider = %v, want %q", err, tc.wantErr)
			case tc.wantErr == "" && err != nil:
				t.Errorf("CreateProvider: %v", err)
			}
			if got := out.String(); got != tc.wantOut {
				t.Errorf("stdout = %q, want %q", got, tc.wantOut)
			}
			if n := runner.creates(); n != 0 {
				t.Errorf("provider create ran %d times, want none", n)
			}
		})
	}
}

// A create that fails names the command and not its output, which may hold the
// secret; what create printed still reaches the terminal, masked.
func TestCreateProviderFailureNamesTheCommandNotItsOutput(t *testing.T) {
	t.Parallel()
	runner := &createRunner{
		listing: "NAME TYPE\n",
		printed: "error: rejected " + deepseekSecret + "\n",
		failErr: errors.New("exit status 1"),
	}
	files := newMemFiles()
	files.contents[testConfig+"/deepseek.env"] = []byte("DEEPSEEK_API_KEY=" + deepseekSecret + "\n")
	run, out, errw := newCreateRun(runner, files)

	err := run.CreateProvider("deepseek", "deepseek-claude", testConfig+"/deepseek.env", "DEEPSEEK_API_KEY", "ANTHROPIC_AUTH_TOKEN")
	if err == nil {
		t.Fatal("CreateProvider = nil, want the failed create")
	}
	if got, want := err.Error(), "openshell-providers: openshell provider create --name deepseek failed"; got != want {
		t.Errorf("error = %q, want %q", got, want)
	}
	const want = "error: rejected ***\n"
	if out.String() != want || errw.String() != want {
		t.Errorf("stdout = %q, stderr = %q, want both %q", out.String(), errw.String(), want)
	}
}

// A stream that cannot take what create printed stops the create with an error
// that names the stream's output and not the output itself. The flush case is a
// partial secret held at the end of the output, which Close must still write.
func TestCreateProviderWriteFailureStopsTheRun(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		printed string
		out     func() io.Writer
		errw    func() io.Writer
	}{
		{
			name:    "stdout refuses the write",
			printed: "value=x\n",
			out:     func() io.Writer { return failingWriter{} },
			errw:    func() io.Writer { return &bytes.Buffer{} },
		},
		{
			name:    "stderr refuses the write",
			printed: "value=x\n",
			out:     func() io.Writer { return &bytes.Buffer{} },
			errw:    func() io.Writer { return failingWriter{} },
		},
		{
			name:    "stdout refuses the flush of a partial secret",
			printed: "abc " + deepseekSecret[:6],
			out:     func() io.Writer { return &firstWriteOnly{} },
			errw:    func() io.Writer { return &bytes.Buffer{} },
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			runner := &createRunner{listing: "NAME TYPE\n", printed: tc.printed}
			files := newMemFiles()
			files.contents[testConfig+"/deepseek.env"] = []byte("DEEPSEEK_API_KEY=" + deepseekSecret + "\n")
			run, _, _ := newCreateRun(runner, files)
			run.Out = tc.out()
			run.Err = tc.errw()

			err := run.CreateProvider("deepseek", "deepseek-claude", testConfig+"/deepseek.env", "DEEPSEEK_API_KEY", "ANTHROPIC_AUTH_TOKEN")
			if err == nil || !strings.Contains(err.Error(), "writing openshell's output") {
				t.Fatalf("CreateProvider = %v, want the stream's write failure", err)
			}
			if strings.Contains(err.Error(), deepseekSecret[:6]) || strings.Contains(err.Error(), tc.printed) {
				t.Errorf("error %q carries output", err)
			}
		})
	}
}

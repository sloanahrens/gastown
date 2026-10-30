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
	testRoot     = "/src/gt"
	testProfiles = testRoot + "/deploy/polecat/profiles"
	testWork     = "/scratch/run"
)

// scriptRunner records each command and answers it through reply, so no process
// starts. A nil reply succeeds on every command with no output.
type scriptRunner struct {
	calls []string
	reply func(cmd string) ([]byte, error)
}

func (s *scriptRunner) Run(name string, args []string, _ []string, _ io.Reader) ([]byte, []byte, error) {
	cmd := strings.Join(append([]string{name}, args...), " ")
	s.calls = append(s.calls, cmd)
	if s.reply == nil {
		return nil, nil, nil
	}
	out, err := s.reply(cmd)
	return out, nil, err
}

// answer gives the first command containing match the output and error given.
type answer struct {
	match string
	out   string
	err   error
}

func answerWith(answers ...answer) func(string) ([]byte, error) {
	return func(cmd string) ([]byte, error) {
		for _, a := range answers {
			if strings.Contains(cmd, a.match) {
				return []byte(a.out), a.err
			}
		}
		return nil, nil
	}
}

// memFiles is an in-memory Files. The profiles it lists are the files it was
// built with under testProfiles; written files are kept by name, so a test reads
// what a run wrote and what it removed.
type memFiles struct {
	entries   []fs.DirEntry
	contents  map[string][]byte
	written   map[string]*bytes.Buffer
	removed   []string
	listErr   error
	readErr   error
	createErr error
	closeErr  error
	tempErr   error
	diskFull  bool
}

// profileFile is one profile under testProfiles: its file name and its body.
type profileFile struct{ name, body string }

// fakeEntry is a directory entry that is only a name.
type fakeEntry string

func (e fakeEntry) Name() string             { return string(e) }
func (fakeEntry) IsDir() bool                { return false }
func (fakeEntry) Type() fs.FileMode          { return 0 }
func (fakeEntry) Info() (fs.FileInfo, error) { return nil, nil }

func newMemFiles(profiles ...profileFile) *memFiles {
	m := &memFiles{contents: map[string][]byte{}, written: map[string]*bytes.Buffer{}}
	for _, p := range profiles {
		m.entries = append(m.entries, fakeEntry(p.name))
		m.contents[testProfiles+"/"+p.name] = []byte(p.body)
	}
	return m
}

func (m *memFiles) ReadDir(string) ([]fs.DirEntry, error) {
	if m.listErr != nil {
		return nil, m.listErr
	}
	return m.entries, nil
}

func (m *memFiles) ReadFile(name string) ([]byte, error) {
	if m.readErr != nil {
		return nil, m.readErr
	}
	data, ok := m.contents[name]
	if !ok {
		return nil, fs.ErrNotExist
	}
	return data, nil
}

// memWriter keeps what is written and reports closeErr from Close.
type memWriter struct {
	*bytes.Buffer
	closeErr error
}

func (w memWriter) Close() error { return w.closeErr }

// fullWriter is a file on a full disk: every write fails and Close is clean.
type fullWriter struct{ failingWriter }

func (fullWriter) Close() error { return nil }

func (m *memFiles) Create(name string) (io.WriteCloser, error) {
	if m.createErr != nil {
		return nil, m.createErr
	}
	if m.diskFull {
		return fullWriter{}, nil
	}
	buf := &bytes.Buffer{}
	m.written[name] = buf
	return memWriter{buf, m.closeErr}, nil
}

func (m *memFiles) MkdirTemp() (string, error) {
	if m.tempErr != nil {
		return "", m.tempErr
	}
	return testWork, nil
}

func (m *memFiles) RemoveAll(path string) error {
	m.removed = append(m.removed, path)
	return nil
}

// newTestRun builds a Run over runner and files, with its log lines in the
// returned buffer.
func newTestRun(runner *scriptRunner, files *memFiles) (*Run, *bytes.Buffer) {
	out := &bytes.Buffer{}
	return &Run{
		Client: Client{Run: runner},
		Gt:     Gt{Run: runner, Root: testRoot},
		Paths:  Paths{Root: testRoot, Profiles: testProfiles},
		Files:  files,
		Out:    out,
		Err:    io.Discard,
	}, out
}

// lastCall is the last command the runner saw, or "" when it saw none.
func lastCall(calls []string) string {
	if len(calls) == 0 {
		return ""
	}
	return calls[len(calls)-1]
}

const (
	listCall   = "openshell profile list --global"
	exportCall = "openshell profile export --global -- forgejo-polecat"
	checkCall  = "gt sandbox endpoint --gateway openshell --root /src/gt --check " + testWork + "/forgejo-polecat.export.yaml"
	// renderCall renders without an export; the stale path adds --from-export.
	renderCall     = "gt sandbox endpoint --gateway openshell --root /src/gt --render " + testProfiles + "/forgejo-polecat.yaml"
	renderFromCall = renderCall + " --from-export " + testWork + "/forgejo-polecat.export.yaml"
	lintRendered   = "openshell profile lint -f " + testWork + "/forgejo-polecat.yaml"
	updateCall     = "openshell profile update --global -f " + testWork + "/forgejo-polecat.yaml forgejo-polecat"
	importRendered = "openshell profile import -f " + testWork + "/forgejo-polecat.yaml --global"
)

func TestImportProfilesFreshImportsLintedFiles(t *testing.T) {
	t.Parallel()
	runner := &scriptRunner{}
	files := newMemFiles(profileFile{"a.yaml", "id: a\n"}, profileFile{"b.yaml", "id: b\n"})
	run, _ := newTestRun(runner, files)
	if err := run.ImportProfiles(); err != nil {
		t.Fatalf("ImportProfiles: %v", err)
	}
	want := []string{
		listCall,
		"openshell profile lint -f " + testProfiles + "/a.yaml",
		"openshell profile import -f " + testProfiles + "/a.yaml --global",
		listCall,
		"openshell profile lint -f " + testProfiles + "/b.yaml",
		"openshell profile import -f " + testProfiles + "/b.yaml --global",
	}
	if !slices.Equal(runner.calls, want) {
		t.Errorf("calls = %q, want %q", runner.calls, want)
	}
}

func TestImportProfilesLeavesExistingAlone(t *testing.T) {
	t.Parallel()
	runner := &scriptRunner{reply: answerWith(answer{match: "profile list", out: "a\nb\n"})}
	files := newMemFiles(profileFile{"a.yaml", "id: a\n"}, profileFile{"b.yaml", "id: b\n"})
	run, out := newTestRun(runner, files)
	if err := run.ImportProfiles(); err != nil {
		t.Fatalf("ImportProfiles: %v", err)
	}
	want := []string{listCall, listCall}
	if !slices.Equal(runner.calls, want) {
		t.Errorf("calls = %q, want only the two listings %q", runner.calls, want)
	}
	const wantOut = "openshell-providers: profile a exists\nopenshell-providers: profile b exists\n"
	if out.String() != wantOut {
		t.Errorf("output = %q, want %q", out.String(), wantOut)
	}
}

func TestImportProfilesNoProfilesIsNotAnError(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		listErr error
	}{
		{name: "empty dir"},
		{name: "missing dir", listErr: fs.ErrNotExist},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			runner := &scriptRunner{}
			files := newMemFiles()
			files.listErr = tc.listErr
			run, out := newTestRun(runner, files)
			if err := run.ImportProfiles(); err != nil {
				t.Fatalf("ImportProfiles: %v", err)
			}
			const wantOut = "openshell-providers: no profiles under deploy/polecat/profiles, nothing to import\n"
			if out.String() != wantOut {
				t.Errorf("output = %q, want %q", out.String(), wantOut)
			}
			if len(runner.calls) != 0 {
				t.Errorf("calls = %q, want none", runner.calls)
			}
		})
	}
}

// TestImportProfilesSkipsFilesThatAreNotYAML is the listing's filter: only *.yaml
// entries are profiles, so a README beside them is never linted or imported.
func TestImportProfilesSkipsFilesThatAreNotYAML(t *testing.T) {
	t.Parallel()
	runner := &scriptRunner{}
	files := newMemFiles(profileFile{"README.md", "not a profile\n"}, profileFile{"a.yaml", "id: a\n"})
	run, _ := newTestRun(runner, files)
	if err := run.ImportProfiles(); err != nil {
		t.Fatalf("ImportProfiles: %v", err)
	}
	for _, c := range runner.calls {
		if strings.Contains(c, "README.md") {
			t.Errorf("call %q touches the README, which is not a profile", c)
		}
	}
	if len(runner.calls) != 3 {
		t.Errorf("calls = %q, want the listing, lint and import of a.yaml only", runner.calls)
	}
}

// TestImportProfilesStopsWhenOpenshellOutputCannotBeWritten covers the two writes
// that pass an openshell call's output on: a run whose log cannot take it stops at
// that call rather than go on importing with its output lost.
func TestImportProfilesStopsWhenOpenshellOutputCannotBeWritten(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		out  func(run *Run)
	}{
		{"stdout", func(run *Run) { run.Out = failingWriter{} }},
		{"stderr", func(run *Run) { run.Err = failingWriter{} }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			runner := &scriptRunner{}
			files := newMemFiles(profileFile{"a.yaml", "id: a\n"})
			run, _ := newTestRun(runner, files)
			tc.out(run)
			if err := run.ImportProfiles(); err == nil {
				t.Fatal("ImportProfiles = nil, want the unwritable output")
			}
			if want := "openshell profile lint -f " + testProfiles + "/a.yaml"; lastCall(runner.calls) != want {
				t.Errorf("last call = %q, want the run to stop at %q", lastCall(runner.calls), want)
			}
		})
	}
}

func TestImportProfilesListFailureStopsTheRun(t *testing.T) {
	t.Parallel()
	runner := &scriptRunner{reply: answerWith(answer{match: "profile list", err: errors.New("gateway unreachable")})}
	files := newMemFiles(profileFile{"a.yaml", "id: a\n"}, profileFile{"b.yaml", "id: b\n"})
	run, out := newTestRun(runner, files)
	if err := run.ImportProfiles(); err == nil {
		t.Fatal("ImportProfiles = nil, want the failed list")
	}
	if want := []string{listCall}; !slices.Equal(runner.calls, want) {
		t.Errorf("calls = %q, want the run to stop at the first listing %q", runner.calls, want)
	}
	if out.Len() != 0 {
		t.Errorf("output = %q, want none: nothing was imported", out.String())
	}
	if !slices.Equal(files.removed, []string{testWork}) {
		t.Errorf("removed = %q, want the run's temp dir %q", files.removed, testWork)
	}
}

func TestImportProfilesForgejoPolecatRenderedForTheGateway(t *testing.T) {
	t.Parallel()
	runner := &scriptRunner{reply: answerWith(answer{match: "--render", out: "rendered for the gateway\n"})}
	files := newMemFiles(profileFile{"forgejo-polecat.yaml", "id: forgejo-polecat\nhost: docker-alias\n"})
	run, _ := newTestRun(runner, files)
	if err := run.ImportProfiles(); err != nil {
		t.Fatalf("ImportProfiles: %v", err)
	}
	want := []string{listCall, renderCall, "openshell profile lint -f " + testWork + "/forgejo-polecat.yaml", importRendered}
	if !slices.Equal(runner.calls, want) {
		t.Errorf("calls = %q, want %q", runner.calls, want)
	}
	if got := files.written[testWork+"/forgejo-polecat.yaml"].String(); got != "rendered for the gateway\n" {
		t.Errorf("imported file = %q, want the rendered profile", got)
	}
}

func TestImportProfilesForgejoPolecatStaleIsUpdatedAndBoundIsKept(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		check     string
		want      []string
		wantOut   string
		wantWrite string
	}{
		{
			name:  "stale",
			check: "stale: endpoint host differs\n",
			want: []string{
				listCall,
				exportCall,
				checkCall,
				renderFromCall,
				lintRendered,
				updateCall,
			},
			wantOut:   "openshell-providers: profile forgejo-polecat is stale (endpoint host differs), updating it\n",
			wantWrite: "rendered\n",
		},
		{
			name:    "bound",
			check:   "bound\n",
			want:    []string{listCall, exportCall, checkCall},
			wantOut: "openshell-providers: profile forgejo-polecat exists\n",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			runner := &scriptRunner{reply: answerWith(
				answer{match: "profile list", out: "forgejo-polecat\n"},
				answer{match: "profile export", out: "exported\n"},
				answer{match: "--check", out: tc.check},
				answer{match: "--render", out: "rendered\n"},
			)}
			files := newMemFiles(profileFile{"forgejo-polecat.yaml", "id: forgejo-polecat\n"})
			run, out := newTestRun(runner, files)
			if err := run.ImportProfiles(); err != nil {
				t.Fatalf("ImportProfiles: %v", err)
			}
			if !slices.Equal(runner.calls, tc.want) {
				t.Errorf("calls = %q, want %q", runner.calls, tc.want)
			}
			if out.String() != tc.wantOut {
				t.Errorf("output = %q, want %q", out.String(), tc.wantOut)
			}
			if got := files.written[testWork+"/forgejo-polecat.export.yaml"].String(); got != "exported\n" {
				t.Errorf("export = %q, want the gateway's profile", got)
			}
		})
	}
}

func TestImportProfilesCleansItsTempDir(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		answers []answer
		wantErr bool
	}{
		{name: "on success"},
		{name: "on a refusal", answers: []answer{{match: "profile lint", err: errors.New("refused")}}, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			runner := &scriptRunner{reply: answerWith(tc.answers...)}
			files := newMemFiles(profileFile{"a.yaml", "id: a\n"})
			run, _ := newTestRun(runner, files)
			if err := run.ImportProfiles(); (err != nil) != tc.wantErr {
				t.Fatalf("ImportProfiles error = %v, want error %v", err, tc.wantErr)
			}
			if !slices.Equal(files.removed, []string{testWork}) {
				t.Errorf("removed = %q, want the run's temp dir %q", files.removed, testWork)
			}
		})
	}
}

func TestImportProfilesRefusalsStopTheRun(t *testing.T) {
	t.Parallel()
	forgejo := profileFile{"forgejo-polecat.yaml", "id: forgejo-polecat\n"}
	tests := []struct {
		name        string
		profiles    []profileFile
		mutate      func(*memFiles)
		answers     []answer
		wantLast    string
		wantRemoved []string
	}{
		{
			name:        "listing the profiles fails",
			profiles:    []profileFile{{"a.yaml", "id: a\n"}},
			mutate:      func(m *memFiles) { m.listErr = errors.New("unreadable dir") },
			wantRemoved: nil,
		},
		{
			name:        "the temp dir cannot be made",
			profiles:    []profileFile{{"a.yaml", "id: a\n"}},
			mutate:      func(m *memFiles) { m.tempErr = errors.New("no space") },
			wantRemoved: nil,
		},
		{
			name:        "a profile cannot be read",
			profiles:    []profileFile{{"a.yaml", "id: a\n"}},
			mutate:      func(m *memFiles) { m.readErr = errors.New("permission denied") },
			wantRemoved: []string{testWork},
		},
		{
			name:        "lint refuses a profile",
			profiles:    []profileFile{{"a.yaml", "id: a\n"}, {"b.yaml", "id: b\n"}},
			answers:     []answer{{match: "profile lint", err: errors.New("bad profile")}},
			wantLast:    "openshell profile lint -f " + testProfiles + "/a.yaml",
			wantRemoved: []string{testWork},
		},
		{
			name:        "import refuses a profile",
			profiles:    []profileFile{{"a.yaml", "id: a\n"}, {"b.yaml", "id: b\n"}},
			answers:     []answer{{match: "profile import", err: errors.New("clash")}},
			wantLast:    "openshell profile import -f " + testProfiles + "/a.yaml --global",
			wantRemoved: []string{testWork},
		},
		{
			name:     "the export of forgejo-polecat fails",
			profiles: []profileFile{forgejo},
			answers: []answer{
				{match: "profile list", out: "forgejo-polecat\n"},
				{match: "profile export", err: errors.New("no such profile")},
			},
			wantLast:    exportCall,
			wantRemoved: []string{testWork},
		},
		{
			name:     "the endpoint check fails",
			profiles: []profileFile{forgejo},
			answers: []answer{
				{match: "profile list", out: "forgejo-polecat\n"},
				{match: "--check", err: errors.New("no profile names the gateway")},
			},
			wantLast:    checkCall,
			wantRemoved: []string{testWork},
		},
		{
			name:        "the export file cannot be created",
			profiles:    []profileFile{forgejo},
			answers:     []answer{{match: "profile list", out: "forgejo-polecat\n"}},
			mutate:      func(m *memFiles) { m.createErr = errors.New("read-only temp dir") },
			wantLast:    listCall,
			wantRemoved: []string{testWork},
		},
		{
			name:     "the export cannot be written",
			profiles: []profileFile{forgejo},
			answers: []answer{
				{match: "profile list", out: "forgejo-polecat\n"},
				{match: "profile export", out: "exported\n"},
			},
			mutate:      func(m *memFiles) { m.diskFull = true },
			wantLast:    exportCall,
			wantRemoved: []string{testWork},
		},
		{
			name:     "the export file cannot be closed",
			profiles: []profileFile{forgejo},
			answers: []answer{
				{match: "profile list", out: "forgejo-polecat\n"},
				{match: "profile export", out: "exported\n"},
			},
			mutate:      func(m *memFiles) { m.closeErr = errors.New("disk full") },
			wantLast:    exportCall,
			wantRemoved: []string{testWork},
		},
		{
			name:        "the render of a missing forgejo-polecat fails",
			profiles:    []profileFile{forgejo},
			answers:     []answer{{match: "--render", err: errors.New("no Forgejo rule")}},
			wantLast:    renderCall,
			wantRemoved: []string{testWork},
		},
		{
			name:     "the render of a stale forgejo-polecat fails",
			profiles: []profileFile{forgejo},
			answers: []answer{
				{match: "profile list", out: "forgejo-polecat\n"},
				{match: "--check", out: "stale: host differs\n"},
				{match: "--render", err: errors.New("no Forgejo rule")},
			},
			wantLast:    renderFromCall,
			wantRemoved: []string{testWork},
		},
		{
			name:     "lint refuses the rendered stale forgejo-polecat",
			profiles: []profileFile{forgejo},
			answers: []answer{
				{match: "profile list", out: "forgejo-polecat\n"},
				{match: "--check", out: "stale: host differs\n"},
				{match: "profile lint", err: errors.New("bad profile")},
			},
			wantLast:    lintRendered,
			wantRemoved: []string{testWork},
		},
		{
			name:        "a rendered file cannot be created",
			profiles:    []profileFile{forgejo},
			mutate:      func(m *memFiles) { m.createErr = errors.New("read-only temp dir") },
			wantLast:    listCall,
			wantRemoved: []string{testWork},
		},
		{
			name:        "a rendered file cannot be closed",
			profiles:    []profileFile{forgejo},
			mutate:      func(m *memFiles) { m.closeErr = errors.New("disk full") },
			wantLast:    renderCall,
			wantRemoved: []string{testWork},
		},
		{
			name:     "the update of a stale forgejo-polecat fails",
			profiles: []profileFile{forgejo},
			answers: []answer{
				{match: "profile list", out: "forgejo-polecat\n"},
				{match: "--check", out: "stale: host differs\n"},
				{match: "profile update", err: errors.New("refused")},
			},
			wantLast:    updateCall,
			wantRemoved: []string{testWork},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			runner := &scriptRunner{reply: answerWith(tc.answers...)}
			files := newMemFiles(tc.profiles...)
			if tc.mutate != nil {
				tc.mutate(files)
			}
			run, _ := newTestRun(runner, files)
			if err := run.ImportProfiles(); err == nil {
				t.Fatal("ImportProfiles = nil, want the refusal")
			}
			if got := lastCall(runner.calls); got != tc.wantLast {
				t.Errorf("last call = %q, want the run to stop at %q", got, tc.wantLast)
			}
			if !slices.Equal(files.removed, tc.wantRemoved) {
				t.Errorf("removed = %q, want %q", files.removed, tc.wantRemoved)
			}
		})
	}
}

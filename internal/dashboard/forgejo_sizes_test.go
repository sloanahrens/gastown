package dashboard

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/steveyegge/gastown/internal/forgejo"
	"github.com/steveyegge/gastown/internal/workspaceid"
)

// sizeTownRoot writes a town whose registry maps two rigs to two workspaces,
// so a report can tell a mapped repository from the town default.
func sizeTownRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeSizeTownFile(t, root, "mayor/rigs.json", `{"version":1,"rigs":{
  "gastown":{"git_url":"http://forgejo.test/sloan/gastown.git","added_at":"2026-10-31T00:00:00Z","beads":{"repo":"local","prefix":"gt"}},
  "beads":{"git_url":"http://forgejo.test/sloan/beads.git","added_at":"2026-10-31T00:00:00Z","beads":{"repo":"local","prefix":"bd"}}
}}`)
	writeSizeTownFile(t, root, "settings/workspaces.json", `{"default":"customer-zero","rigs":{"gt":"workspace-a","bd":"workspace-b"}}`)
	return root
}

func writeSizeTownFile(t *testing.T, root, rel, body string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(body), 0o644))
}

// forgejoReposStub answers the repository list with a canned reply and nothing
// else, so the client's own parsing of the reply runs in the test. A status
// answers the call with that failure instead.
type forgejoReposStub struct {
	body   string
	status int
}

func (s forgejoReposStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/api/v1/user/repos" {
		http.NotFound(w, r)
		return
	}
	if s.status != 0 {
		w.WriteHeader(s.status)
		_, _ = io.WriteString(w, `{"message":"boom"}`)
		return
	}
	// The client pages the list to its end: answer the call with the canned
	// page, then the empty page that says there is no more (gt-nlrpk).
	if r.URL.Query().Get("page") != "1" {
		_, _ = io.WriteString(w, `[]`)
		return
	}
	_, _ = io.WriteString(w, s.body)
}

// newForgejoSizeWriter wires a writer to a stub Forgejo and the fixed clock the
// day file's name and rows are read from.
func newForgejoSizeWriter(t *testing.T, reply string) *ForgejoSizeWriter {
	t.Helper()
	return newForgejoSizeWriterAt(t, sizeTownRoot(t), forgejoReposStub{body: reply})
}

func newForgejoSizeWriterAt(t *testing.T, root string, stub forgejoReposStub) *ForgejoSizeWriter {
	t.Helper()
	client, err := forgejo.NewClient("viewer",
		forgejo.WithToken("test-token"),
		forgejo.WithBaseURL("http://forgejo.test/api/v1"),
		forgejo.WithHTTPClient(&http.Client{Transport: stubTransport{stub}}),
	)
	require.NoError(t, err)
	w := NewForgejoSizeWriter(client, root)
	w.now = func() time.Time { return time.Date(2026, 11, 1, 3, 0, 0, 0, time.UTC) }
	return w
}

// sizeDayLines reads the day's report back as one string per line.
func sizeDayLines(t *testing.T, root string) []string {
	t.Helper()
	path := filepath.Join(ForgejoSizesDir(root), "2026-11-01.jsonl")
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	body := strings.TrimSuffix(string(data), "\n")
	if body == "" {
		return nil
	}
	return strings.Split(body, "\n")
}

func TestForgejoRepoSizesParse(t *testing.T) {
	t.Parallel()
	w := newForgejoSizeWriter(t, `[
	  {"id":12,"name":"gastown","full_name":"sloan/gastown","size":1280},
	  {"id":7,"name":"beads","full_name":"sloan/beads","size":4711}
	]`)
	rows, err := w.Write(context.Background())
	require.NoError(t, err)

	require.Len(t, rows, 2)
	sizes := map[string]int64{}
	for _, row := range rows {
		require.NotNil(t, row.SizeKB, "the reply carried a size for %s", row.Repo)
		sizes[row.Repo] = *row.SizeKB
	}
	assert.Equal(t, map[string]int64{"sloan/gastown": 1280, "sloan/beads": 4711}, sizes)
}

func TestForgejoSizeLinesWritten(t *testing.T) {
	t.Parallel()
	w := newForgejoSizeWriter(t, `[
	  {"id":12,"name":"gastown","full_name":"sloan/gastown","size":1280},
	  {"id":7,"name":"beads","full_name":"sloan/beads","size":4711}
	]`)
	_, err := w.Write(context.Background())
	require.NoError(t, err)

	lines := sizeDayLines(t, w.townRoot)
	require.Len(t, lines, 2, "one line per repository")
	// Field order is the reader's contract, so a struct that grows a field
	// ahead of the size is a change the monthly export sees.
	assert.True(t, strings.HasPrefix(lines[0], `{"ts":`), "a row starts with ts: %s", lines[0])

	var first, second map[string]any
	require.NoError(t, json.Unmarshal([]byte(lines[0]), &first))
	require.NoError(t, json.Unmarshal([]byte(lines[1]), &second))
	assert.Equal(t, map[string]any{
		"ts":        "2026-11-01T03:00:00Z",
		"workspace": "workspace-a",
		"repo":      "sloan/gastown",
		"size_kb":   float64(1280),
	}, first)
	assert.Equal(t, map[string]any{
		"ts":        "2026-11-01T03:00:00Z",
		"workspace": "workspace-b",
		"repo":      "sloan/beads",
		"size_kb":   float64(4711),
	}, second)
}

func TestForgejoSizeMissingFieldIsNull(t *testing.T) {
	t.Parallel()
	w := newForgejoSizeWriter(t, `[{"id":12,"name":"gastown","full_name":"sloan/gastown"}]`)
	_, err := w.Write(context.Background())
	require.NoError(t, err)

	lines := sizeDayLines(t, w.townRoot)
	require.Len(t, lines, 1)

	var row map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(lines[0]), &row))
	size, ok := row["size_kb"]
	require.True(t, ok, "the size field is written even when the reply has none")
	assert.Equal(t, "null", string(size), "an unmeasured size is null, never zero")
}

// A repository whose name the registry does not know would have to be charged
// to some workspace; writing the town default would bill customer zero for it.
func TestForgejoSizeUnmappedRepoWritesNothing(t *testing.T) {
	t.Parallel()
	w := newForgejoSizeWriter(t, `[{"id":1,"name":"stranger","full_name":"sloan/stranger","size":9}]`)
	_, err := w.Write(context.Background())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "sloan/stranger")
	_, statErr := os.Stat(filepath.Join(ForgejoSizesDir(w.townRoot), "2026-11-01.jsonl"))
	assert.True(t, os.IsNotExist(statErr), "a refused report leaves no file")
}

// A town config that does not read is an unknown bill, not a default bill.
func TestForgejoSizeUnreadableTownConfigWritesNothing(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		breakTown func(t *testing.T, root string)
		want      string
	}{
		{"no workspace map default", func(t *testing.T, root string) {
			writeSizeTownFile(t, root, "settings/workspaces.json", `{"rigs":{"gt":"workspace-a"}}`)
		}, "no default workspace id"},
		{"no rig registry", func(t *testing.T, root string) {
			require.NoError(t, os.Remove(filepath.Join(root, "mayor", "rigs.json")))
		}, "rigs.json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := sizeTownRoot(t)
			tc.breakTown(t, root)
			w := newForgejoSizeWriterAt(t, root, forgejoReposStub{body: `[{"full_name":"sloan/gastown","size":1280}]`})
			_, err := w.Write(context.Background())

			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
			_, statErr := os.Stat(filepath.Join(ForgejoSizesDir(root), "2026-11-01.jsonl"))
			assert.True(t, os.IsNotExist(statErr), "a refused report leaves no file")
		})
	}
}

// A repository list the client cannot read is an outage, never a report of no
// repositories: an empty file would read as a town with nothing stored.
func TestForgejoSizeListFailureWritesNothing(t *testing.T) {
	t.Parallel()
	w := newForgejoSizeWriterAt(t, sizeTownRoot(t), forgejoReposStub{status: http.StatusInternalServerError})
	_, err := w.Write(context.Background())

	require.Error(t, err)
	_, statErr := os.Stat(filepath.Join(ForgejoSizesDir(w.townRoot), "2026-11-01.jsonl"))
	assert.True(t, os.IsNotExist(statErr), "a refused report leaves no file")
}

func TestForgejoSizeNoTownRootFails(t *testing.T) {
	t.Parallel()
	w := newForgejoSizeWriterAt(t, "", forgejoReposStub{body: `[{"full_name":"sloan/gastown","size":1280}]`})
	_, err := w.Write(context.Background())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "no town root")
}

// A repository that is not owner/name names no rig to read, so it is refused as
// the operator's mistake rather than reported under a made-up name.
func TestForgejoSizeRepoWithoutOwnerFails(t *testing.T) {
	t.Parallel()
	w := newForgejoSizeWriter(t, `[{"full_name":"gastown","size":1280}]`)
	_, err := w.Write(context.Background())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "repo is not owner/name")
}

// A rig the registry holds without a beads prefix names no workspace, so its
// repositories are refused rather than charged to the town default.
func TestForgejoSizePrefixlessRigIsUnmapped(t *testing.T) {
	t.Parallel()
	root := sizeTownRoot(t)
	writeSizeTownFile(t, root, "mayor/rigs.json", `{"version":1,"rigs":{
  "bare":{"git_url":"http://forgejo.test/sloan/bare.git","added_at":"2026-10-31T00:00:00Z"}
}}`)
	w := newForgejoSizeWriterAt(t, root, forgejoReposStub{body: `[{"full_name":"sloan/bare","size":7}]`})
	_, err := w.Write(context.Background())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "sloan/bare")
	_, statErr := os.Stat(filepath.Join(ForgejoSizesDir(root), "2026-11-01.jsonl"))
	assert.True(t, os.IsNotExist(statErr), "a refused report leaves no file")
}

// A rig whose workspace lookup fails for any reason other than its missing
// prefix fails the run: dropping the rig would leave its repositories out of
// the bill without saying so.
func TestForgejoSizeWorkspaceLookupFailureFails(t *testing.T) {
	t.Parallel()
	w := newForgejoSizeWriter(t, `[{"full_name":"sloan/gastown","size":1280}]`)
	w.forRig = func(*workspaceid.Map, string) (string, error) { return "", errors.New("workspace map is unreadable") }
	_, err := w.Write(context.Background())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "workspace map is unreadable")
	_, statErr := os.Stat(sizeDayPath(w.townRoot))
	assert.True(t, os.IsNotExist(statErr), "a refused report leaves no file")
}

// The day file is written through a temporary file, so a run the disk refuses
// is a failed run and never a day file that stops mid-report.
func TestForgejoSizeUnopenableTempFileFails(t *testing.T) {
	t.Parallel()
	w := newForgejoSizeWriter(t, `[{"full_name":"sloan/gastown","size":1280}]`)
	w.openTemp = func(string) (io.WriteCloser, error) { return nil, errors.New("day file is locked") }
	_, err := w.Write(context.Background())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "day file is locked")
}

// A write that fails leaves the run failed, so the caller retries instead of
// trusting a file that stops mid-report.
func TestForgejoSizeWriteFailureFails(t *testing.T) {
	t.Parallel()
	w := newForgejoSizeWriter(t, `[{"full_name":"sloan/gastown","size":1280}]`)
	sink := &sizeSink{writeErr: errors.New("no space left")}
	w.openTemp = func(string) (io.WriteCloser, error) { return sink, nil }
	_, err := w.Write(context.Background())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "no space left")
	assert.True(t, sink.closed, "a failed write still closes the file")
}

// A close that fails can mean the last bytes never landed, so the run reports
// it rather than a clean night.
func TestForgejoSizeCloseFailureFails(t *testing.T) {
	t.Parallel()
	w := newForgejoSizeWriter(t, `[{"full_name":"sloan/gastown","size":1280}]`)
	sink := &sizeSink{closeErr: errors.New("flush failed")}
	w.openTemp = func(string) (io.WriteCloser, error) { return sink, nil }
	_, err := w.Write(context.Background())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "flush failed")
}

// sizeDayPath is the day file the fixed clock writes for.
func sizeDayPath(root string) string {
	return filepath.Join(ForgejoSizesDir(root), "2026-11-01.jsonl")
}

// twoRepoReply is a reply with one repository per town rig, so a complete run
// writes two lines.
const twoRepoReply = `[
  {"id":12,"name":"gastown","full_name":"sloan/gastown","size":1280},
  {"id":7,"name":"beads","full_name":"sloan/beads","size":4711}
]`

// A rerun that fails partway through must leave the earlier complete day file
// byte-identical and no temporary file behind: the day file is billing data,
// and a half-written snapshot would charge from a truncated record.
func TestForgejoSizeFailedRerunKeepsEarlierDayFile(t *testing.T) {
	t.Parallel()
	w := newForgejoSizeWriter(t, twoRepoReply)
	_, err := w.Write(context.Background())
	require.NoError(t, err)
	before, err := os.ReadFile(sizeDayPath(w.townRoot))
	require.NoError(t, err)

	realOpen := w.openTemp
	w.openTemp = func(path string) (io.WriteCloser, error) {
		f, err := realOpen(path)
		if err != nil {
			return nil, err
		}
		return &sizeFailAfter{f: f.(*os.File), failAt: 2}, nil
	}
	_, err = w.Write(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no space left")

	after, err := os.ReadFile(sizeDayPath(w.townRoot))
	require.NoError(t, err)
	assert.Equal(t, string(before), string(after), "a failed rerun leaves the earlier day file untouched")
	_, statErr := os.Stat(sizeDayPath(w.townRoot) + ".tmp")
	assert.True(t, os.IsNotExist(statErr), "a failed rerun leaves no temporary file")
}

// A rerun after a failed night replaces the day's snapshot, so the monthly
// export reading the day file charges one line per repository, not two sets.
func TestForgejoSizeRerunWritesOneSetPerDay(t *testing.T) {
	t.Parallel()
	w := newForgejoSizeWriter(t, twoRepoReply)
	_, err := w.Write(context.Background())
	require.NoError(t, err)

	realOpen := w.openTemp
	w.openTemp = func(path string) (io.WriteCloser, error) {
		f, err := realOpen(path)
		if err != nil {
			return nil, err
		}
		return &sizeFailAfter{f: f.(*os.File), failAt: 2}, nil
	}
	_, err = w.Write(context.Background())
	require.Error(t, err)

	w.openTemp = realOpen
	_, err = w.Write(context.Background())
	require.NoError(t, err)

	assert.Len(t, sizeDayLines(t, w.townRoot), 2, "one line per repository after the rerun")
}

// A day file that cannot be replaced (a directory holds its name) is a failed
// run that leaves no temporary file behind, so the next run's exclusive create
// is not refused by this one's leftovers.
func TestForgejoSizeRenameFailureFails(t *testing.T) {
	t.Parallel()
	w := newForgejoSizeWriter(t, `[{"full_name":"sloan/gastown","size":1280}]`)
	require.NoError(t, os.MkdirAll(sizeDayPath(w.townRoot), 0o755))
	_, err := w.Write(context.Background())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "replacing")
	_, statErr := os.Stat(sizeDayPath(w.townRoot) + ".tmp")
	assert.True(t, os.IsNotExist(statErr), "a failed replace leaves no temporary file")
}

// A metrics directory that cannot be made is a failed run.
func TestForgejoSizeBlockedMetricsDirFails(t *testing.T) {
	t.Parallel()
	w := newForgejoSizeWriter(t, `[{"full_name":"sloan/gastown","size":1280}]`)
	// A regular file where the directory belongs makes MkdirAll fail.
	writeSizeTownFile(t, w.townRoot, ".runtime/metrics/forgejo-sizes", "not a directory")
	_, err := w.Write(context.Background())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "creating")
}

// sizeSink is the day file a test drives: it records what was written, and
// fails the write or the close on demand.
type sizeSink struct {
	body     bytes.Buffer
	writeErr error
	closeErr error
	closed   bool
}

func (s *sizeSink) Write(p []byte) (int, error) {
	if s.writeErr != nil {
		return 0, s.writeErr
	}
	return s.body.Write(p)
}

func (s *sizeSink) Close() error {
	s.closed = true
	return s.closeErr
}

// sizeFailAfter is a real temporary file whose write fails after failAt rows,
// the mid-run disk failure a test cannot ask a real filesystem for. Each
// encoded row arrives as one Write, so failAt counts rows.
type sizeFailAfter struct {
	f      *os.File
	writes int
	failAt int
}

func (s *sizeFailAfter) Write(p []byte) (int, error) {
	s.writes++
	if s.writes >= s.failAt {
		return 0, errors.New("no space left")
	}
	return s.f.Write(p)
}

func (s *sizeFailAfter) Close() error {
	return s.f.Close()
}

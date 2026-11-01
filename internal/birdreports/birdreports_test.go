package birdreports

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// testNow is the clock every test injects, in the same shape a bird writes.
var testNow = time.Date(2026, 11, 1, 14, 0, 0, 0, time.UTC)

// writeReport writes a sidecar for reporter under root, with written_at the
// given duration before testNow and the given bird field.
func writeReport(t *testing.T, root, reporter, bird string, age time.Duration) {
	t.Helper()
	writeSidecar(t, root, reporter, fmt.Sprintf(
		`{"bird":%q,"written_at":%q,"title":"t","summary":"s","needs_operator":false,"decisions":0}`,
		bird, testNow.Add(-age).Format(time.RFC3339)))
}

// writeSidecar writes body at root/<reporter>/reports/daily.json.
func writeSidecar(t *testing.T, root, reporter, body string) {
	t.Helper()
	reports := filepath.Join(root, reporter, ReportsSubdir)
	if err := os.MkdirAll(reports, 0o755); err != nil {
		t.Fatalf("making %s: %v", reports, err)
	}
	if err := os.WriteFile(filepath.Join(reports, SidecarName), []byte(body), 0o644); err != nil {
		t.Fatalf("writing the sidecar: %v", err)
	}
}

func TestReadFreshStaleAndMissing(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		age    time.Duration
		write  bool
		status Status
	}{
		{name: "inside the day", age: 25*time.Hour + 59*time.Minute, write: true, status: Fresh},
		{name: "exactly maxAge is fresh", age: MaxAge, write: true, status: Fresh},
		{name: "one minute past maxAge is stale", age: MaxAge + time.Minute, write: true, status: Stale},
		{name: "no sidecar is missing", status: Missing},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			if tt.write {
				writeReport(t, root, "dunlin", "dunlin", tt.age)
			}

			got := Read(root, "dunlin", testNow, MaxAge)
			if got.Status != tt.status {
				t.Fatalf("status = %q (%s), want %q", got.Status, got.Note, tt.status)
			}
			if got.Reporter != "dunlin" {
				t.Errorf("reporter = %q, want dunlin", got.Reporter)
			}
			if tt.write {
				if want := testNow.Add(-tt.age); !got.Written.Equal(want) {
					t.Errorf("written = %v, want %v", got.Written, want)
				}
				if got.Age != tt.age {
					t.Errorf("age = %v, want %v", got.Age, tt.age)
				}
			}
			if due := got.Due(); due != (tt.status != Fresh) {
				t.Errorf("due = %v, status %q", due, tt.status)
			}
		})
	}
}

func TestReadRefusesWhatItCannotUse(t *testing.T) {
	t.Parallel()
	const reporter = "dunlin"
	good := func(bird string, age time.Duration) string {
		return fmt.Sprintf(`{"bird":%q,"written_at":%q}`, bird, testNow.Add(-age).Format(time.RFC3339))
	}
	tests := []struct {
		name     string
		reporter string
		setup    func(t *testing.T, root string)
		note     string
	}{
		{
			name:     "not a reporter name",
			reporter: "Dunlin",
			setup:    func(t *testing.T, root string) {},
			note:     "not a reporter name",
		},
		{
			name: "invalid JSON",
			setup: func(t *testing.T, root string) {
				t.Helper()
				writeSidecar(t, root, reporter, "{")
			},
			note: "daily.json is not valid JSON",
		},
		{
			name: "another reporter's sidecar",
			setup: func(t *testing.T, root string) {
				t.Helper()
				writeSidecar(t, root, reporter, good("owl", time.Hour))
			},
			note: "daily.json names a different reporter",
		},
		{
			name: "written_at is not an instant",
			setup: func(t *testing.T, root string) {
				t.Helper()
				writeSidecar(t, root, reporter, `{"bird":"dunlin","written_at":"yesterday"}`)
			},
			note: "written_at is not an RFC 3339 instant",
		},
		{
			name: "written_at is in the future",
			setup: func(t *testing.T, root string) {
				t.Helper()
				writeSidecar(t, root, reporter, good(reporter, -10*time.Minute))
			},
			note: "written_at is in the future",
		},
		{
			name: "a symlinked sidecar",
			setup: func(t *testing.T, root string) {
				t.Helper()
				real := filepath.Join(root, "real.json")
				if err := os.WriteFile(real, []byte(good(reporter, time.Hour)), 0o644); err != nil {
					t.Fatalf("writing the target: %v", err)
				}
				reports := filepath.Join(root, reporter, ReportsSubdir)
				if err := os.MkdirAll(reports, 0o755); err != nil {
					t.Fatalf("making %s: %v", reports, err)
				}
				if err := os.Symlink(real, filepath.Join(reports, SidecarName)); err != nil {
					t.Fatalf("linking the sidecar: %v", err)
				}
			},
			note: "daily.json is not a regular file",
		},
		{
			name: "a directory named daily.json",
			setup: func(t *testing.T, root string) {
				t.Helper()
				reports := filepath.Join(root, reporter, ReportsSubdir)
				if err := os.MkdirAll(filepath.Join(reports, SidecarName), 0o755); err != nil {
					t.Fatalf("making the directory: %v", err)
				}
			},
			note: "daily.json is not a regular file",
		},
		{
			name: "larger than the cap",
			setup: func(t *testing.T, root string) {
				t.Helper()
				writeSidecar(t, root, reporter, strings.Repeat(" ", SidecarMaxBytes+1))
			},
			note: "daily.json is larger than 16 KiB",
		},
		{
			name: "a reporter that is a file",
			setup: func(t *testing.T, root string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(root, reporter), []byte("x"), 0o644); err != nil {
					t.Fatalf("writing the file: %v", err)
				}
			},
			note: "reporter directory is not a real directory",
		},
		{
			name: "a symlinked reporter directory",
			setup: func(t *testing.T, root string) {
				t.Helper()
				writeSidecar(t, root, "elsewhere", good(reporter, time.Hour))
				if err := os.Symlink("elsewhere", filepath.Join(root, reporter)); err != nil {
					t.Fatalf("linking the reporter directory: %v", err)
				}
			},
			note: "reporter directory is not a real directory",
		},
		{
			name: "a symlinked reports directory",
			setup: func(t *testing.T, root string) {
				t.Helper()
				writeSidecar(t, root, "elsewhere", good(reporter, time.Hour))
				if err := os.MkdirAll(filepath.Join(root, reporter), 0o755); err != nil {
					t.Fatalf("making the reporter directory: %v", err)
				}
				link := filepath.Join(root, reporter, ReportsSubdir)
				if err := os.Symlink(filepath.Join("..", "elsewhere", ReportsSubdir), link); err != nil {
					t.Fatalf("linking the reports directory: %v", err)
				}
			},
			note: "reports directory is not a real directory",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			tt.setup(t, root)
			name := tt.reporter
			if name == "" {
				name = reporter
			}

			got := Read(root, name, testNow, MaxAge)
			if got.Status != Unreadable {
				t.Fatalf("status = %q, want unreadable", got.Status)
			}
			if got.Note != tt.note {
				t.Errorf("note = %q, want %q", got.Note, tt.note)
			}
			if !got.Due() {
				t.Error("an unreadable report must be due")
			}
			if !got.Written.IsZero() {
				t.Errorf("written = %v on an unreadable report, want zero", got.Written)
			}
		})
	}
}

func TestReadNamesAPathItCannotRead(t *testing.T) {
	t.Parallel()
	runtimeDir := filepath.Join(t.TempDir(), "runtime")
	if err := os.WriteFile(runtimeDir, []byte("x"), 0o644); err != nil {
		t.Fatalf("writing the runtime path: %v", err)
	}

	got := Read(runtimeDir, "dunlin", testNow, MaxAge)
	if got.Status != Unreadable {
		t.Fatalf("status = %q (%s), want unreadable: a runtime path that is a file says nothing about the report", got.Status, got.Note)
	}
	if want := "reporter directory could not be read"; got.Note != want {
		t.Errorf("note = %q, want %q", got.Note, want)
	}
}

func TestReadAllNamesTheLateReportersInOrder(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeReport(t, root, "owl", "owl", 2*time.Hour)
	writeReport(t, root, "dunlin", "dunlin", MaxAge+time.Hour)

	got := ReadAll(root, []string{"owl", "merlin", "dunlin"}, testNow, MaxAge)
	want := []struct {
		reporter string
		status   Status
	}{
		{"dunlin", Stale},
		{"merlin", Missing},
		{"owl", Fresh},
	}
	if len(got) != len(want) {
		t.Fatalf("ReadAll returned %d reports, want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i].Reporter != w.reporter || got[i].Status != w.status {
			t.Errorf("report %d = %s %q, want %s %q", i, got[i].Reporter, got[i].Status, w.reporter, w.status)
		}
		if got[i].Due() != (w.status != Fresh) {
			t.Errorf("%s due = %v", got[i].Reporter, got[i].Due())
		}
	}
}

func TestEmbeddedRequestIsTheDocsCopy(t *testing.T) {
	t.Parallel()
	docsPath := filepath.Join("..", "..", "docs", "birds", "daily-request.md")
	docs, err := os.ReadFile(docsPath)
	if err != nil {
		t.Fatalf("reading the source, %s: %v", docsPath, err)
	}
	if strings.TrimSpace(string(docs)) == "" {
		t.Fatalf("%s is empty; the request the command prints lives there", docsPath)
	}
	request := copyBody(t)
	if string(docs) != request {
		t.Errorf("%s and internal/birdreports/daily-request.md differ below its pointer header; copy the docs file over the embedded one", docsPath)
	}

	if got := len(numberedSection.FindAllStringIndex(request, -1)); got != 8 {
		t.Errorf("the request has %d numbered sections, want the DAILY profile's eight", got)
	}
	for _, field := range []string{"bird", "written_at", "title", "summary", "needs_operator", "decisions"} {
		if !strings.Contains(request, `"`+field+`"`) {
			t.Errorf("the request does not name the daily.json field %q", field)
		}
	}
	if !strings.Contains(request, "slug") {
		t.Error("the request does not name the fixed slug the artifact is published under")
	}
	if !strings.Contains(request, "<NAME>") {
		t.Error("the request has no <NAME> placeholder to fill in")
	}
	if !strings.Contains(request, "<RUNTIME_DIR>") {
		t.Error("the request has no <RUNTIME_DIR> placeholder to fill in")
	}
	if strings.Contains(request, "~/gt/.runtime") {
		t.Error("the request hard-codes ~/gt/.runtime instead of the resolved runtime dir")
	}
}

// numberedSection is one of the DAILY profile's eight numbered sections.
var numberedSection = regexp.MustCompile(`(?m)^[1-8]\. `)

// copyBody is the embedded copy without its pointer header, as Request cuts it.
func copyBody(t *testing.T) string {
	t.Helper()
	i := strings.Index(dailyRequest, pointerClose)
	if i < 0 {
		t.Fatalf("internal/birdreports/daily-request.md has no pointer header closed by %q", pointerClose)
	}
	return dailyRequest[i+len(pointerClose):]
}

func TestEmbeddedCopyPointsAtTheDocsSource(t *testing.T) {
	t.Parallel()
	i := strings.Index(dailyRequest, pointerClose)
	if i < 0 {
		t.Fatalf("internal/birdreports/daily-request.md does not open with a pointer comment closed by %q; a reader of the copy has no source to go to", pointerClose)
	}
	header := dailyRequest[:i]
	if !strings.HasPrefix(header, "<!--") {
		t.Errorf("the copy opens with %q, want an HTML comment, which renders out of a markdown view", header)
	}
	if !strings.Contains(header, "docs/birds/daily-request.md") {
		t.Errorf("the copy's pointer header does not name docs/birds/daily-request.md, the source: %q", header)
	}
}

func TestRequestIsTheDocsTextWithTheNameFilled(t *testing.T) {
	t.Parallel()
	docsPath := filepath.Join("..", "..", "docs", "birds", "daily-request.md")
	docs, err := os.ReadFile(docsPath)
	if err != nil {
		t.Fatalf("reading the source, %s: %v", docsPath, err)
	}
	want := strings.NewReplacer("<NAME>", "dunlin", "<RUNTIME_DIR>", "/town/.runtime").Replace(string(docs))
	if got := Request("dunlin", "/town/.runtime"); got != want {
		t.Errorf("Request(\"dunlin\") is not %s with the placeholders filled in; the copy's pointer header must not reach the request", docsPath)
	}
}

func TestRequestNamesTheReporter(t *testing.T) {
	t.Parallel()
	got := Request("dunlin", "/town/.runtime")
	if strings.Contains(got, "<NAME>") {
		t.Error("Request left a <NAME> placeholder in the text")
	}
	if !strings.Contains(got, "dunlin") {
		t.Error("Request does not name the reporter")
	}
	if got == dailyRequest {
		t.Error("Request returned the unfilled text")
	}
}

func TestRequestNamesTheRuntimeDirectoryItWasGiven(t *testing.T) {
	t.Parallel()
	runtimeDir := filepath.Join(t.TempDir(), ".runtime")
	got := Request("dunlin", runtimeDir)
	if strings.Contains(got, "<RUNTIME_DIR>") {
		t.Error("Request left a <RUNTIME_DIR> placeholder in the text")
	}
	if !strings.Contains(got, runtimeDir) {
		t.Errorf("Request does not name the runtime dir %q:\n%s", runtimeDir, got)
	}
	if strings.Contains(got, "~/gt/.runtime") {
		t.Error("Request kept the hard-coded ~/gt/.runtime path")
	}
}

func TestDefaultReportersNamesTheTenReporters(t *testing.T) {
	t.Parallel()
	got := DefaultReporters()
	for _, want := range []string{"dunlin", "dipper", "eagle", "goose", "peacock", "egret", "seagull", "owl", "merlin", "overseer"} {
		found := false
		for _, name := range got {
			if name == want {
				found = true
			}
		}
		if !found {
			t.Errorf("DefaultReporters() has no %q", want)
		}
	}
	if len(got) != 10 {
		t.Errorf("DefaultReporters() has %d names, want 10", len(got))
	}
	// The slice is the caller's: changing it must not change the default.
	got[0] = "changed"
	if DefaultReporters()[0] == "changed" {
		t.Error("DefaultReporters() returns a shared slice")
	}
}

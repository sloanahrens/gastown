package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/birdreports"
	"github.com/steveyegge/gastown/internal/reportdir"
)

// reportPublishNow is the clock every publish test injects, in the shape a
// bird writes.
var reportPublishNow = time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)

// reportPublishClock returns the clock the command is handed.
func reportPublishClock() time.Time { return reportPublishNow }

// reportPublishDir is the directory gt report publish writes for bird, as
// internal/reportdir resolves it, so the test names no layout itself.
func reportPublishDir(t *testing.T, runtimeDir, bird string) string {
	t.Helper()
	dir, err := reportdir.Published(runtimeDir, bird)
	if err != nil {
		t.Fatalf("resolving the published directory for %s: %v", bird, err)
	}
	return dir
}

// reportPublishDocument builds the one JSON document the command reads.
func reportPublishDocument(t *testing.T, bird, page string, sidecar json.RawMessage) string {
	t.Helper()
	doc := map[string]any{"bird": bird, "page": page}
	if sidecar != nil {
		doc["sidecar"] = sidecar
	}
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("building the document: %v", err)
	}
	return string(data)
}

// reportPublishSidecar is a sidecar the reader accepts, written age before the
// test clock.
func reportPublishSidecar(bird string, age time.Duration) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(
		`{"bird":%q,"written_at":%q,"title":"t","summary":"s","needs_operator":false,"decisions":0}`,
		bird, reportPublishNow.Add(-age).Format(time.RFC3339)))
}

// runPublish runs the command's body over doc and returns its output.
func runPublish(t *testing.T, runtimeDir, bird, doc string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := runReportPublish(strings.NewReader(doc), &out, runtimeDir, bird, reportPublishClock)
	return out.String(), err
}

// readReportPublishFile reads one file of a published report.
func readReportPublishFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(data)
}

// assertReportPublishDirHolds asserts the published directory holds exactly
// the named files, in name order.
func assertReportPublishDirHolds(t *testing.T, dir string, want ...string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	got := make([]string, 0, len(entries))
	for _, entry := range entries {
		got = append(got, entry.Name())
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("%s holds %v, want %v", dir, got, want)
	}
}

// TestReportPublishWritesAtomically drives a publish, then a second one over
// it: the page, the sidecar and the previous pair are each written by rename,
// and the pair the second publish replaces is kept beside the new one.
func TestReportPublishWritesAtomically(t *testing.T) {
	t.Parallel()
	runtimeDir := t.TempDir()
	dir := reportPublishDir(t, runtimeDir, "dunlin")

	firstPage := "<html><body>first report</body></html>"
	firstSidecar := reportPublishSidecar("dunlin", time.Hour)
	out, err := runPublish(t, runtimeDir, "dunlin",
		reportPublishDocument(t, "dunlin", firstPage, firstSidecar))
	if err != nil {
		t.Fatalf("the first publish = %v", err)
	}
	pagePath := filepath.Join(dir, reportPublishPageName)
	sidecarPath := filepath.Join(dir, reportdir.SidecarName)
	for _, want := range []string{pagePath, sidecarPath, fmt.Sprintf("%d bytes", len(firstPage))} {
		if !strings.Contains(out, want) {
			t.Errorf("the receipt does not name %q:\n%s", want, out)
		}
	}
	if got := readReportPublishFile(t, pagePath); got != firstPage {
		t.Errorf("daily.html = %q, want the page as given", got)
	}
	if got := readReportPublishFile(t, sidecarPath); got != string(firstSidecar) {
		t.Errorf("daily.json = %q, want the sidecar as given", got)
	}
	assertReportPublishDirHolds(t, dir, reportPublishPageName, reportdir.SidecarName)
	for _, path := range []string{pagePath, sidecarPath} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", path, err)
		}
		if want := os.FileMode(0o644); info.Mode().Perm() != want {
			t.Errorf("%s is %o, want %o", path, info.Mode().Perm(), want)
		}
	}

	secondPage := "<html><body>second report</body></html>"
	secondSidecar := reportPublishSidecar("dunlin", 0)
	if _, err := runPublish(t, runtimeDir, "dunlin",
		reportPublishDocument(t, "dunlin", secondPage, secondSidecar)); err != nil {
		t.Fatalf("the second publish = %v", err)
	}
	if got := readReportPublishFile(t, pagePath); got != secondPage {
		t.Errorf("daily.html = %q, want the second page", got)
	}
	if got := readReportPublishFile(t, sidecarPath); got != string(secondSidecar) {
		t.Errorf("daily.json = %q, want the second sidecar", got)
	}
	prevPath := filepath.Join(dir, reportPublishPrevPageName)
	if got := readReportPublishFile(t, prevPath); got != firstPage {
		t.Errorf("daily.prev.html = %q, want the page it replaced", got)
	}
	prevSidecarPath := filepath.Join(dir, reportdir.PrevSidecarName)
	if got := readReportPublishFile(t, prevSidecarPath); got != string(firstSidecar) {
		t.Errorf("daily.prev.json = %q, want the sidecar it replaced", got)
	}
	assertReportPublishDirHolds(t, dir, reportPublishPageName, reportdir.SidecarName,
		reportPublishPrevPageName, reportdir.PrevSidecarName)
}

// TestReportPublishPublishesAControlCharacterPageAtTheCap asserts a page at the
// page cap is judged by the page, not the document: a control character is one
// byte in the page and six in the JSON text, so the document is over four times
// the page and a document cap set from the page's own cap would refuse it as a
// document (gt-fvleo0).
func TestReportPublishPublishesAControlCharacterPageAtTheCap(t *testing.T) {
	t.Parallel()
	page := strings.Repeat("\x01", reportPublishPageMax)
	doc := reportPublishDocument(t, "dunlin", page, reportPublishSidecar("dunlin", time.Hour))
	if len(doc) <= 4*reportPublishPageMax {
		t.Fatalf("the document is %d bytes, want a page at the cap to take over four times its cap", len(doc))
	}
	runtimeDir := t.TempDir()
	if _, err := runPublish(t, runtimeDir, "dunlin", doc); err != nil {
		t.Fatalf("publishing a page of control characters at the cap: %v", err)
	}
	dir := reportPublishDir(t, runtimeDir, "dunlin")
	if got := readReportPublishFile(t, filepath.Join(dir, reportPublishPageName)); got != page {
		t.Errorf("the published page is %d bytes, want the page read", len(got))
	}
}

// TestReportHelpLeavesTheLayoutToReportdir asserts the report commands' help
// points at internal/reportdir for the report's layout instead of restating it:
// no help names the runtime path or the files a report is written as
// (gt-fvleo0).
func TestReportHelpLeavesTheLayoutToReportdir(t *testing.T) {
	t.Parallel()
	helps := []struct {
		name string
		long string
	}{
		{"gt report publish", reportPublishCmd.Long},
		{"gt birds", birdsCmd.Long},
		{"gt birds due", birdsDueCmd.Long},
	}
	restated := []string{
		".runtime/",
		reportdir.PageName,
		reportdir.SidecarName,
		reportdir.PrevPageName,
		reportdir.PrevSidecarName,
	}
	for _, help := range helps {
		if !strings.Contains(help.long, "internal/reportdir") {
			t.Errorf("%s help does not point at internal/reportdir for the layout:\n%s", help.name, help.long)
		}
		for _, layout := range restated {
			if strings.Contains(help.long, layout) {
				t.Errorf("%s help restates the layout with %q:\n%s", help.name, layout, help.long)
			}
		}
	}
}

// TestReportPublishRollsBackAPartialRename fails the rename that puts the new
// sidecar in place, after the new page and the previous pair have been renamed:
// the publish fails, the old daily.html and daily.json are exactly as they
// were, and no temp file is left behind.
func TestReportPublishRollsBackAPartialRename(t *testing.T) {
	t.Parallel()
	const bird = "dunlin"
	runtimeDir := t.TempDir()
	dir := reportPublishDir(t, runtimeDir, bird)

	firstPage := []byte("<html>first report</html>")
	firstSidecar := reportPublishSidecar(bird, time.Hour)
	if _, err := writeReportPublish(osReportPublishFS(), runtimeDir, bird, firstPage, firstSidecar); err != nil {
		t.Fatalf("the first publish = %v", err)
	}
	pagePath := filepath.Join(dir, reportPublishPageName)
	sidecarPath := filepath.Join(dir, reportdir.SidecarName)

	fsys := osReportPublishFS()
	fsys.rename = func(oldpath, newpath string) error {
		// The sidecar lands last; failing it leaves the page already replaced.
		if newpath == sidecarPath {
			return errors.New("scripted rename failure")
		}
		return os.Rename(oldpath, newpath)
	}
	secondPage := []byte("<html>second report</html>")
	if _, err := writeReportPublish(fsys, runtimeDir, bird, secondPage, reportPublishSidecar(bird, 0)); err == nil {
		t.Fatal("the publish succeeded with the sidecar rename failing")
	}

	if got := readReportPublishFile(t, pagePath); got != string(firstPage) {
		t.Errorf("daily.html = %q after a failed rename, want the first page back", got)
	}
	if got := readReportPublishFile(t, sidecarPath); got != string(firstSidecar) {
		t.Errorf("daily.json = %q after a failed rename, want the first sidecar", got)
	}
	// The pair had no previous version before this publish, so the rotation the
	// failed publish began is undone rather than left behind.
	assertReportPublishDirHolds(t, dir, reportPublishPageName, reportdir.SidecarName)
}

// TestReportPublishRefusesBadInput drives each refusal of a document before
// anything is written: the field is named and the runtime directory is left as
// it was.
func TestReportPublishRefusesBadInput(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		bird   string
		doc    func(t *testing.T) string
		want   string
		absent string
	}{
		{
			name: "a malformed document",
			bird: "dunlin",
			doc:  func(t *testing.T) string { return "{" },
			want: "document",
		},
		{
			name: "a document over the input cap",
			bird: "dunlin",
			doc:  func(t *testing.T) string { return strings.Repeat(" ", reportPublishInputMax+1) },
			want: "document",
		},
		{
			name: "a page over the cap",
			bird: "dunlin",
			doc: func(t *testing.T) string {
				return reportPublishDocument(t, "dunlin", strings.Repeat("x", reportPublishPageMax+1),
					reportPublishSidecar("dunlin", time.Hour))
			},
			want: "page",
		},
		{
			name: "a sidecar over the cap",
			bird: "dunlin",
			doc: func(t *testing.T) string {
				sidecar := json.RawMessage(`"` + strings.Repeat("x", birdreports.SidecarMaxBytes+1) + `"`)
				return reportPublishDocument(t, "dunlin", "<html></html>", sidecar)
			},
			want: "sidecar",
		},
		{
			name: "a sidecar that is not an object",
			bird: "dunlin",
			doc: func(t *testing.T) string {
				return reportPublishDocument(t, "dunlin", "<html></html>", json.RawMessage(`42`))
			},
			want: "sidecar",
		},
		{
			name: "a sidecar naming another bird",
			bird: "dunlin",
			doc: func(t *testing.T) string {
				return reportPublishDocument(t, "dunlin", "<html></html>", reportPublishSidecar("owl", time.Hour))
			},
			want: "sidecar",
		},
		{
			name: "a sidecar with no written_at",
			bird: "dunlin",
			doc: func(t *testing.T) string {
				return reportPublishDocument(t, "dunlin", "<html></html>", json.RawMessage(`{"bird":"dunlin"}`))
			},
			want: "sidecar",
		},
		{
			name: "a sidecar written in the future",
			bird: "dunlin",
			doc: func(t *testing.T) string {
				return reportPublishDocument(t, "dunlin", "<html></html>", reportPublishSidecar("dunlin", -time.Hour))
			},
			want: "sidecar",
		},
		{
			name: "no sidecar",
			bird: "dunlin",
			doc: func(t *testing.T) string {
				return reportPublishDocument(t, "dunlin", "<html></html>", nil)
			},
			want: "sidecar",
		},
		{
			name: "a bird name that is not one",
			bird: "Dunlin",
			doc: func(t *testing.T) string {
				return reportPublishDocument(t, "Dunlin", "<html></html>", reportPublishSidecar("Dunlin", time.Hour))
			},
			want: "bird",
		},
		{
			name: "a document naming another bird",
			bird: "dunlin",
			doc: func(t *testing.T) string {
				return reportPublishDocument(t, "owl", "<html></html>", reportPublishSidecar("owl", time.Hour))
			},
			want: "bird",
		},
		{
			name: "a document whose bird field is never echoed",
			bird: "dunlin",
			doc: func(t *testing.T) string {
				return reportPublishDocument(t, "MARKER-document-bird", "<html></html>", reportPublishSidecar("dunlin", time.Hour))
			},
			want:   "bird",
			absent: "MARKER-document-bird",
		},
		{
			name: "a bird name equal to a daemon subsystem directory",
			bird: "agents",
			doc: func(t *testing.T) string {
				return reportPublishDocument(t, "agents", "<html></html>", reportPublishSidecar("agents", time.Hour))
			},
			want: "bird",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			runtimeDir := t.TempDir()
			_, err := runPublish(t, runtimeDir, tt.bird, tt.doc(t))
			if err == nil {
				t.Fatalf("the document was accepted, want a refusal naming %s", tt.want)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %q, want it to name %s", err, tt.want)
			}
			if tt.absent != "" && strings.Contains(err.Error(), tt.absent) {
				t.Errorf("error = %q, want the document's text absent from it", err)
			}
			entries, readErr := os.ReadDir(runtimeDir)
			if readErr != nil {
				t.Fatalf("reading the runtime directory: %v", readErr)
			}
			if len(entries) != 0 {
				t.Errorf("a refused document left %d entries in the runtime directory", len(entries))
			}
		})
	}
}

// TestReportPublishRefusesAPathItCannotUse drives the write side's refusals: a
// runtime directory that is not there, a published directory or bird directory
// that is a file, and a destination that is a directory.
func TestReportPublishRefusesAPathItCannotUse(t *testing.T) {
	t.Parallel()
	validDoc := func(t *testing.T, bird string) string {
		return reportPublishDocument(t, bird, "<html></html>", reportPublishSidecar(bird, time.Hour))
	}

	t.Run("a runtime directory that is not there", func(t *testing.T) {
		t.Parallel()
		_, err := runPublish(t, filepath.Join(t.TempDir(), "missing"), "dunlin", validDoc(t, "dunlin"))
		if err == nil || !strings.Contains(err.Error(), "missing") {
			t.Fatalf("error = %v, want the missing runtime directory named", err)
		}
	})

	t.Run("a runtime directory that is a file", func(t *testing.T) {
		t.Parallel()
		runtimeDir := filepath.Join(t.TempDir(), "runtime")
		if err := os.WriteFile(runtimeDir, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := runPublish(t, runtimeDir, "dunlin", validDoc(t, "dunlin"))
		if err == nil || !strings.Contains(err.Error(), "not a real directory") {
			t.Fatalf("error = %v, want the runtime path refused as not a directory", err)
		}
	})

	t.Run("a published directory that is a file", func(t *testing.T) {
		t.Parallel()
		runtimeDir := t.TempDir()
		if err := os.WriteFile(filepath.Join(runtimeDir, reportdir.PublishedDirName), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := runPublish(t, runtimeDir, "dunlin", validDoc(t, "dunlin")); err == nil {
			t.Fatal("a file where the published directory belongs was accepted")
		}
	})

	t.Run("a bird directory that is a file", func(t *testing.T) {
		t.Parallel()
		runtimeDir := t.TempDir()
		dir := reportPublishDir(t, runtimeDir, "dunlin")
		if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dir, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := runPublish(t, runtimeDir, "dunlin", validDoc(t, "dunlin")); err == nil {
			t.Fatal("a file where the bird's directory belongs was accepted")
		}
	})

	t.Run("a destination that is a directory", func(t *testing.T) {
		t.Parallel()
		runtimeDir := t.TempDir()
		dir := reportPublishDir(t, runtimeDir, "dunlin")
		if err := os.MkdirAll(filepath.Join(dir, reportdir.SidecarName), 0o755); err != nil {
			t.Fatal(err)
		}
		out, err := runPublish(t, runtimeDir, "dunlin", validDoc(t, "dunlin"))
		if err == nil {
			t.Fatal("a directory where daily.json belongs was accepted")
		}
		if strings.Contains(out, "<html>") {
			t.Errorf("a refused publish printed the page:\n%s", out)
		}
	})

	t.Run("a bird name the resolver refuses", func(t *testing.T) {
		t.Parallel()
		// The writer resolves its own destination, so a name the resolver
		// refuses is refused there too rather than joined into a path.
		_, err := writeReportPublish(osReportPublishFS(), t.TempDir(), "../dunlin",
			[]byte("<html></html>"), []byte(`{"bird":"dunlin","written_at":"2026-11-08T11:00:00Z"}`))
		if !errors.Is(err, reportdir.ErrBadName) {
			t.Fatalf("writeReportPublish error = %v, want ErrBadName", err)
		}
	})
}

// TestReportPublishNeverFollowsASymlink drives a publish at a destination that
// is a symlink: the command refuses, the link's target is untouched, and the
// page is never printed.
func TestReportPublishNeverFollowsASymlink(t *testing.T) {
	t.Parallel()
	const marker = "page-that-must-not-be-printed"
	doc := reportPublishDocument(t, "dunlin", "<html>"+marker+"</html>", reportPublishSidecar("dunlin", time.Hour))

	t.Run("a symlinked page", func(t *testing.T) {
		t.Parallel()
		runtimeDir := t.TempDir()
		dir := reportPublishDir(t, runtimeDir, "dunlin")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(t.TempDir(), "target.html")
		if err := os.WriteFile(target, []byte("<html>target</html>"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(dir, reportPublishPageName)); err != nil {
			t.Fatal(err)
		}

		out, err := runPublish(t, runtimeDir, "dunlin", doc)
		if err == nil {
			t.Fatal("a symlinked daily.html was accepted")
		}
		if strings.Contains(out, marker) {
			t.Errorf("the refused publish printed the page:\n%s", out)
		}
		if got := readReportPublishFile(t, target); got != "<html>target</html>" {
			t.Errorf("the link's target = %q, want it untouched", got)
		}
	})

	t.Run("a symlinked published reports directory", func(t *testing.T) {
		t.Parallel()
		runtimeDir := t.TempDir()
		// The link points at a real directory holding a report for another
		// bird: a publish through it would write outside the runtime tree.
		elsewhere := t.TempDir()
		if err := os.Symlink(elsewhere, filepath.Join(runtimeDir, reportdir.PublishedDirName)); err != nil {
			t.Fatal(err)
		}

		out, err := runPublish(t, runtimeDir, "dunlin", doc)
		if err == nil {
			t.Fatal("a symlinked published reports directory was accepted")
		}
		if strings.Contains(out, marker) {
			t.Errorf("the refused publish printed the page:\n%s", out)
		}
		if entries, readErr := os.ReadDir(elsewhere); readErr != nil || len(entries) != 0 {
			t.Errorf("the link's target holds %d entries (err %v), want nothing written through it", len(entries), readErr)
		}
	})
}

// TestReportPublishReportsAFailedRead fails the command when standard input
// cannot be read.
func TestReportPublishReportsAFailedRead(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	err := runReportPublish(reportPublishFailReader{}, &out, t.TempDir(), "dunlin", reportPublishClock)
	if err == nil {
		t.Fatal("an unreadable standard input did not fail the command")
	}
	if !strings.Contains(err.Error(), "standard input") {
		t.Errorf("error = %q, want it to name standard input", err)
	}
}

// TestReportPublishReportsAFailedReceipt fails the command when its receipt
// cannot be printed: a report whose destination the caller never learns is a
// failed publish.
func TestReportPublishReportsAFailedReceipt(t *testing.T) {
	t.Parallel()
	doc := reportPublishDocument(t, "dunlin", "<html></html>", reportPublishSidecar("dunlin", time.Hour))
	err := runReportPublish(strings.NewReader(doc), reportPublishFailWriter{}, t.TempDir(), "dunlin", reportPublishClock)
	if err == nil {
		t.Fatal("a failed receipt write did not fail the command")
	}
}

// TestReportPublishCommandReturnsAFailedTownLookup fails the command when the
// town root cannot be found.
func TestReportPublishCommandReturnsAFailedTownLookup(t *testing.T) {
	t.Parallel()
	want := errors.New("not in a town")
	err := runReportPublishCommand(&cobra.Command{}, func() (string, error) { return "", want }, reportPublishClock)
	if !errors.Is(err, want) {
		t.Fatalf("runReportPublishCommand = %v, want the lookup's error", err)
	}
}

// scriptedTemp is a staged file whose named operation fails, so a test drives
// a write, a chmod, a sync and a close a healthy filesystem will not refuse.
type scriptedTemp struct {
	file *os.File
	fail string
}

func (t scriptedTemp) Write(p []byte) (int, error) {
	if t.fail == "write" {
		return 0, errors.New("scripted write failure")
	}
	return t.file.Write(p)
}

func (t scriptedTemp) Name() string { return t.file.Name() }

func (t scriptedTemp) Chmod(mode os.FileMode) error {
	if t.fail == "chmod" {
		return errors.New("scripted chmod failure")
	}
	return t.file.Chmod(mode)
}

func (t scriptedTemp) Sync() error {
	if t.fail == "sync" {
		return errors.New("scripted sync failure")
	}
	return t.file.Sync()
}

func (t scriptedTemp) Close() error {
	err := t.file.Close()
	if t.fail == "close" {
		return errors.New("scripted close failure")
	}
	return err
}

// TestReportPublishReportsAFailureOnEveryWritePath drives each filesystem
// operation the publish makes to its failure and asserts the publish fails
// instead of reporting a report it did not write.
func TestReportPublishReportsAFailureOnEveryWritePath(t *testing.T) {
	t.Parallel()
	const bird = "dunlin"
	page := []byte("<html></html>")
	sidecar := []byte(reportPublishSidecar(bird, time.Hour))

	tests := []struct {
		name   string
		script func(t *testing.T, fsys *reportPublishFS, dir string)
	}{
		{
			name: "a destination that cannot be examined",
			script: func(t *testing.T, fsys *reportPublishFS, dir string) {
				fsys.lstat = func(path string) (os.FileInfo, error) {
					if path == filepath.Join(dir, reportPublishPageName) {
						return nil, errors.New("scripted lstat failure")
					}
					return os.Lstat(path)
				}
			},
		},
		{
			name: "a directory that cannot be examined",
			script: func(t *testing.T, fsys *reportPublishFS, dir string) {
				reportsDir := filepath.Dir(dir)
				fsys.lstat = func(path string) (os.FileInfo, error) {
					if path == reportsDir {
						return nil, errors.New("scripted lstat failure")
					}
					return os.Lstat(path)
				}
			},
		},
		{
			name: "a directory that cannot be made",
			script: func(t *testing.T, fsys *reportPublishFS, dir string) {
				fsys.mkdir = func(string, os.FileMode) error { return errors.New("scripted mkdir failure") }
			},
		},
		{
			name: "a previous page that cannot be read",
			script: func(t *testing.T, fsys *reportPublishFS, dir string) {
				t.Helper()
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, reportPublishPageName), []byte("<html>yesterday</html>"), 0o644); err != nil {
					t.Fatal(err)
				}
				fsys.readFile = func(string) ([]byte, error) { return nil, errors.New("scripted read failure") }
			},
		},
		{
			name: "a rotated page that cannot be read",
			script: func(t *testing.T, fsys *reportPublishFS, dir string) {
				t.Helper()
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, reportPublishPrevPageName), []byte("<html>older</html>"), 0o644); err != nil {
					t.Fatal(err)
				}
				fsys.readFile = func(string) ([]byte, error) { return nil, errors.New("scripted read failure") }
			},
		},
		{
			name: "a rotated sidecar that cannot be read",
			script: func(t *testing.T, fsys *reportPublishFS, dir string) {
				t.Helper()
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, reportdir.PrevSidecarName), []byte(`{"bird":"dunlin"}`), 0o644); err != nil {
					t.Fatal(err)
				}
				fsys.readFile = func(string) ([]byte, error) { return nil, errors.New("scripted read failure") }
			},
		},
		{
			name: "a temp file that cannot be made",
			script: func(t *testing.T, fsys *reportPublishFS, dir string) {
				fsys.createTemp = func(string, string) (reportPublishTemp, error) {
					return nil, errors.New("scripted create failure")
				}
			},
		},
		{
			name: "a temp file that cannot be written",
			script: func(t *testing.T, fsys *reportPublishFS, dir string) {
				fsys.createTemp = scriptedCreateTemp(t, "write")
			},
		},
		{
			name: "a temp file that cannot be chmodded",
			script: func(t *testing.T, fsys *reportPublishFS, dir string) {
				fsys.createTemp = scriptedCreateTemp(t, "chmod")
			},
		},
		{
			name: "a temp file that cannot be synced",
			script: func(t *testing.T, fsys *reportPublishFS, dir string) {
				fsys.createTemp = scriptedCreateTemp(t, "sync")
			},
		},
		{
			name: "a temp file that cannot be closed",
			script: func(t *testing.T, fsys *reportPublishFS, dir string) {
				fsys.createTemp = scriptedCreateTemp(t, "close")
			},
		},
		{
			name: "a temp file that cannot be renamed",
			script: func(t *testing.T, fsys *reportPublishFS, dir string) {
				fsys.rename = func(string, string) error { return errors.New("scripted rename failure") }
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			runtimeDir := t.TempDir()
			dir := reportPublishDir(t, runtimeDir, bird)
			fsys := osReportPublishFS()
			tt.script(t, &fsys, dir)

			if _, err := writeReportPublish(fsys, runtimeDir, bird, page, sidecar); err == nil {
				t.Fatalf("the publish succeeded with %s failing", tt.name)
			}
		})
	}
}

// scriptedCreateTemp returns a createTemp that makes a real temp file whose
// named operation fails.
func scriptedCreateTemp(t *testing.T, fail string) func(string, string) (reportPublishTemp, error) {
	t.Helper()
	return func(dir, pattern string) (reportPublishTemp, error) {
		f, err := os.CreateTemp(dir, pattern)
		if err != nil {
			return nil, err
		}
		return scriptedTemp{file: f, fail: fail}, nil
	}
}

// reportPublishFailReader is a reader that always fails.
type reportPublishFailReader struct{}

func (reportPublishFailReader) Read([]byte) (int, error) { return 0, errors.New("broken pipe") }

// reportPublishFailWriter is a writer that always fails.
type reportPublishFailWriter struct{}

func (reportPublishFailWriter) Write([]byte) (int, error) { return 0, errors.New("broken pipe") }

// TestReportPublishReportsAFailedRestore drives a rollback that cannot finish:
// the sidecar rename fails after the page was replaced, and the page cannot be
// put back. The command must fail and say the previous report was not restored,
// rather than report a rollback it did not make.
func TestReportPublishReportsAFailedRestore(t *testing.T) {
	t.Parallel()
	const bird = "dunlin"
	runtimeDir := t.TempDir()
	dir := reportPublishDir(t, runtimeDir, bird)

	if _, err := writeReportPublish(osReportPublishFS(), runtimeDir, bird,
		[]byte("<html>first report</html>"), reportPublishSidecar(bird, time.Hour)); err != nil {
		t.Fatalf("the first publish = %v", err)
	}
	pagePath := filepath.Join(dir, reportPublishPageName)
	sidecarPath := filepath.Join(dir, reportdir.SidecarName)

	// The sidecar rename fails after the page is in place, and the page cannot
	// be renamed back: the first page rename is the publish's, the second is the
	// restore's.
	pageRenames := 0
	fsys := osReportPublishFS()
	fsys.rename = func(oldpath, newpath string) error {
		if newpath == sidecarPath {
			return errors.New("scripted rename failure")
		}
		if newpath == pagePath {
			pageRenames++
			if pageRenames > 1 {
				return errors.New("scripted restore failure")
			}
		}
		return os.Rename(oldpath, newpath)
	}
	_, err := writeReportPublish(fsys, runtimeDir, bird,
		[]byte("<html>second report</html>"), reportPublishSidecar(bird, 0))
	if err == nil {
		t.Fatal("the publish succeeded with both the sidecar and the page rename failing")
	}
	if !strings.Contains(err.Error(), "restoring the previous report also failed") {
		t.Errorf("error = %q, want the failed restore named", err)
	}
}

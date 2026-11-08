package reportdir

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeReportDir makes dir and writes the named files in it.
func writeReportDir(t *testing.T, dir string, files ...string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("making %s: %v", dir, err)
	}
	for _, name := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
	}
}

// TestReadsBothLocations covers the two layouts a town holds while its birds
// switch to gt report publish: the published directory wins when it holds a
// report, the old one is read when it is the only one, and a name in neither
// falls back to the old path so the reader that asks can report it missing.
func TestReadsBothLocations(t *testing.T) {
	t.Parallel()
	const reporter = "dunlin"

	t.Run("the published location wins when both hold a report", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		writeReportDir(t, filepath.Join(root, PublishedDirName, reporter), SidecarName)
		writeReportDir(t, filepath.Join(root, reporter, ReportsSubdir), SidecarName)

		loc, err := Resolve(root, reporter)
		if err != nil {
			t.Fatal(err)
		}
		if want := filepath.Join(root, PublishedDirName, reporter); loc.Dir != want || !loc.Published {
			t.Errorf("Resolve = %+v, want the published directory %s", loc, want)
		}
	})

	t.Run("a published page with no sidecar still wins", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		writeReportDir(t, filepath.Join(root, PublishedDirName, reporter), PageName)
		writeReportDir(t, filepath.Join(root, reporter, ReportsSubdir), SidecarName)

		if got, err := Dir(root, reporter); err != nil || got != filepath.Join(root, PublishedDirName, reporter) {
			t.Errorf("Dir = %q (%v), want the published directory", got, err)
		}
	})

	t.Run("the old location is read when it is the only one", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		writeReportDir(t, filepath.Join(root, reporter, ReportsSubdir), SidecarName)

		loc, err := Resolve(root, reporter)
		if err != nil {
			t.Fatal(err)
		}
		if want := filepath.Join(root, reporter, ReportsSubdir); loc.Dir != want || loc.Published {
			t.Errorf("Resolve = %+v, want the old directory %s", loc, want)
		}
	})

	t.Run("a report in neither location falls back to the old path", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()

		if got, err := Dir(root, reporter); err != nil || got != filepath.Join(root, reporter, ReportsSubdir) {
			t.Errorf("Dir = %q (%v), want the old directory to be named", got, err)
		}
	})

	t.Run("a linked published directory holds no report", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		real := t.TempDir()
		if err := os.MkdirAll(filepath.Join(real, reporter), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(real, reporter, SidecarName), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(root, PublishedDirName), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(real, reporter), filepath.Join(root, PublishedDirName, reporter)); err != nil {
			t.Fatal(err)
		}
		writeReportDir(t, filepath.Join(root, reporter, ReportsSubdir), SidecarName)

		loc, err := Resolve(root, reporter)
		if err != nil {
			t.Fatal(err)
		}
		if loc.Published {
			t.Errorf("Resolve followed a linked published directory: %+v", loc)
		}
	})
}

// TestReportDirRefusesBadNames drives the one name check: every reader that
// asks the resolver for a directory gets the refusal rather than a path.
func TestReportDirRefusesBadNames(t *testing.T) {
	t.Parallel()
	runtimeDir := t.TempDir()
	bad := []string{"", "d", "../x", "a/b", "Dunlin", "dun lin", "abcdefghijklmnopqrstuvwxy"}
	for _, name := range bad {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got, err := Dir(runtimeDir, name); !errors.Is(err, ErrBadName) || got != "" {
				t.Errorf("Dir(%q) = %q (%v), want ErrBadName and no path", name, got, err)
			}
			if got, err := Published(runtimeDir, name); !errors.Is(err, ErrBadName) || got != "" {
				t.Errorf("Published(%q) = %q (%v), want ErrBadName and no path", name, got, err)
			}
			if _, err := TopicPages(runtimeDir, name, 1<<20); !errors.Is(err, ErrBadName) {
				t.Errorf("TopicPages(%q) = %v, want ErrBadName", name, err)
			}
			if _, err := PagePath(runtimeDir, name, PageName); !errors.Is(err, ErrBadName) {
				t.Errorf("PagePath(%q) = %v, want ErrBadName", name, err)
			}
		})
	}
}

// TestTopicPagesMergesBothLocations covers the union of the two locations: a
// page in both is listed once from the published location, and the pages of
// the old location survive a publish.
func TestTopicPagesMergesBothLocations(t *testing.T) {
	t.Parallel()
	const reporter = "dunlin"
	root := t.TempDir()
	publishedDir := filepath.Join(root, PublishedDirName, reporter)
	legacyDir := filepath.Join(root, reporter, ReportsSubdir)

	now := time.Now().UTC().Truncate(time.Second)
	write := func(dir, name string, mtime time.Time) {
		t.Helper()
		writeReportDir(t, dir, name)
		if err := os.Chtimes(filepath.Join(dir, name), mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
	write(publishedDir, "both.html", now.Add(-time.Hour))
	write(publishedDir, "new-only.html", now.Add(-2*time.Hour))
	write(legacyDir, "both.html", now.Add(-10*time.Hour))
	write(legacyDir, "old-only.html", now.Add(-3*time.Hour))
	// Not topic pages: the daily page and a file of another shape.
	write(publishedDir, PageName, now)
	write(legacyDir, "notes.txt", now)

	pages, err := TopicPages(root, reporter, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, page := range pages {
		got = append(got, page.Name)
	}
	want := []string{"both.html", "new-only.html", "old-only.html"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("TopicPages = %v, want %v newest first", got, want)
	}
	if pages[0].Dir != publishedDir {
		t.Errorf("the page in both locations was listed from %s, want the published %s", pages[0].Dir, publishedDir)
	}
	if pages[2].Dir != legacyDir {
		t.Errorf("the old page was listed from %s, want %s", pages[2].Dir, legacyDir)
	}
}

// TestPagePathFindsATopicPageInEitherLocation drives the route's lookup: a
// daily page comes from the resolved directory, and a topic page is published
// first and old second.
func TestPagePathFindsATopicPageInEitherLocation(t *testing.T) {
	t.Parallel()
	const reporter = "dunlin"
	root := t.TempDir()
	publishedDir := filepath.Join(root, PublishedDirName, reporter)
	legacyDir := filepath.Join(root, reporter, ReportsSubdir)
	writeReportDir(t, publishedDir, SidecarName, "new-only.html")
	writeReportDir(t, legacyDir, SidecarName, "old-only.html")

	for _, c := range []struct{ file, want string }{
		{PageName, filepath.Join(publishedDir, PageName)},
		{"new-only.html", filepath.Join(publishedDir, "new-only.html")},
		{"old-only.html", filepath.Join(legacyDir, "old-only.html")},
	} {
		got, err := PagePath(root, reporter, c.file)
		if err != nil {
			t.Errorf("PagePath(%q) = %v", c.file, err)
			continue
		}
		if got != c.want {
			t.Errorf("PagePath(%q) = %q, want %q", c.file, got, c.want)
		}
	}
}

// TestPagePathRefusesAPageName drives the page check: a name a bird does not
// write is refused before a path is built from it.
func TestPagePathRefusesAPageName(t *testing.T) {
	t.Parallel()
	for _, file := range []string{"daily.json", "../escape.html", "Notes.txt", "a/b.html", "daily.prev.js"} {
		if _, err := PagePath(t.TempDir(), "dunlin", file); !errors.Is(err, ErrBadPage) {
			t.Errorf("PagePath(%q) = %v, want ErrBadPage", file, err)
		}
	}
}

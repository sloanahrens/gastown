package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/birdreports"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/townstate"
	"github.com/steveyegge/gastown/internal/workspace"
)

const (
	// reportPublishPageMax is the largest page the command writes; a page over
	// it is refused. A report is one page of HTML, not an archive.
	reportPublishPageMax = 1 << 20

	// reportPublishPageName and reportPublishPrevPageName are the page this
	// command writes and the one it keeps in its place.
	reportPublishPageName     = "daily.html"
	reportPublishPrevPageName = "daily.prev.html"

	// reportPublishInputMax bounds the document read from standard input. It
	// leaves room to decode a page over the cap so the refusal can name the
	// field rather than the document.
	reportPublishInputMax = 4*reportPublishPageMax + birdreports.SidecarMaxBytes

	// reportPublishNameShape is the whole name a bird may publish under:
	// lowercase letters, 2 to 24, which is every name the pool hands out.
	reportPublishNameShape = `^[a-z]{2,24}$`
)

var (
	reportPublishBird   string
	reportPublishNameRe = regexp.MustCompile(reportPublishNameShape)
)

var reportPublishCmd = &cobra.Command{
	Use:   "publish",
	Short: "Publish a bird's daily report from standard input",
	Long: `Publish one bird's daily report into the town's runtime directory.

Reads one JSON document from standard input:

  {"bird":"<name>","page":"<the daily.html text>","sidecar":{<the daily.json object>}}

and writes <town>/.runtime/reports/<bird>/daily.html and daily.json there, each
as a temp file renamed into place, keeping the page it replaces as
daily.prev.html. The document is refused, with the field named and nothing
written, when it is malformed, the page is over 1 MiB, the sidecar is over 16
KiB or fails the reader's own validation (internal/birdreports), the document's
bird is not --bird, or the name is a daemon subsystem directory under .runtime.

The command prints the two paths and their sizes, and never prints the page.

The daily request in docs/birds/daily-request.md builds the sidecar with jq and
pipes the document here.

Examples:
  jq -n --rawfile page daily.html --slurpfile sidecar sidecar.json \
    '{bird:"dunlin",page:$page,sidecar:$sidecar[0]}' | gt report publish --bird dunlin`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return runReportPublishCommand(cmd, workspace.FindFromCwdOrError, time.Now)
	},
}

// runReportPublishCommand is gt report publish's body. The town lookup and the
// clock are parameters so a test drives the failure paths without a town on
// this machine.
func runReportPublishCommand(cmd *cobra.Command, findTownRoot func() (string, error), now func() time.Time) error {
	townRoot, err := findTownRoot()
	if err != nil {
		return err
	}
	return runReportPublish(cmd.InOrStdin(), cmd.OutOrStdout(),
		constants.TownRuntimePath(townRoot), reportPublishBird, now)
}

func init() {
	reportPublishCmd.Flags().StringVar(&reportPublishBird, "bird", "", "The bird whose report this is (required)")
	_ = reportPublishCmd.MarkFlagRequired("bird")
	reportCmd.AddCommand(reportPublishCmd)
}

// reportPublishDoc is the one JSON document the command reads: the page as
// text and the sidecar as the object the bird built.
type reportPublishDoc struct {
	Bird    string          `json:"bird"`
	Page    string          `json:"page"`
	Sidecar json.RawMessage `json:"sidecar"`
}

// reportPublishReceipt is what the command prints: the paths it wrote and
// their sizes.
type reportPublishReceipt struct {
	pagePath     string
	pageBytes    int
	sidecarPath  string
	sidecarBytes int
}

// runReportPublish reads one document from stdin and publishes its report
// under runtimeDir. The reader, the writer, the runtime directory and the
// clock are parameters so a test drives every refusal without a town on this
// machine; the command wires the process's stdin and stdout, the town's
// .runtime and time.Now.
func runReportPublish(stdin io.Reader, out io.Writer, runtimeDir, bird string, now func() time.Time) error {
	doc, err := readReportPublishDoc(stdin)
	if err != nil {
		return err
	}
	page := []byte(doc.Page)
	if err := validateReportPublish(bird, doc, page, now()); err != nil {
		return err
	}
	receipt, err := writeReportPublish(osReportPublishFS(), runtimeDir, bird, page, []byte(doc.Sidecar))
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "published %s: %s (%d bytes), %s (%d bytes)\n",
		bird, receipt.pagePath, receipt.pageBytes, receipt.sidecarPath, receipt.sidecarBytes)
	return err
}

// readReportPublishDoc reads the one document from stdin, refusing input past
// the cap instead of reading an unbounded stream into memory.
func readReportPublishDoc(in io.Reader) (reportPublishDoc, error) {
	var doc reportPublishDoc
	data, err := io.ReadAll(io.LimitReader(in, reportPublishInputMax+1))
	if err != nil {
		return doc, fmt.Errorf("reading standard input: %w", err)
	}
	if len(data) > reportPublishInputMax {
		return doc, fmt.Errorf("document: larger than %d bytes", reportPublishInputMax)
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		// The parser's own sentence can carry a character of the page, so the
		// refusal names the document and nothing in it (gt-yjzepk.3).
		return doc, errors.New("document: not the JSON object with bird, page and sidecar")
	}
	return doc, nil
}

// validateReportPublish refuses a document the report may not be written from.
// Every refusal names the field, and the whole document is validated before
// the first byte is written, so a refused document leaves the directory as it
// was.
func validateReportPublish(bird string, doc reportPublishDoc, page []byte, now time.Time) error {
	if !reportPublishNameRe.MatchString(bird) {
		return fmt.Errorf("bird: %q is not a bird name (lowercase letters, 2 to 24)", bird)
	}
	if doc.Bird != bird {
		return fmt.Errorf("bird: the document names %q, --bird is %q", doc.Bird, bird)
	}
	if reservedRuntimeNames()[bird] {
		return fmt.Errorf("bird: %q is a daemon subsystem directory under .runtime", bird)
	}
	if len(page) > reportPublishPageMax {
		return fmt.Errorf("page: %d bytes is over the %d-byte cap", len(page), reportPublishPageMax)
	}
	if len(doc.Sidecar) == 0 {
		return errors.New("sidecar: missing")
	}
	if len(doc.Sidecar) > birdreports.SidecarMaxBytes {
		return fmt.Errorf("sidecar: %d bytes is over the %d-byte cap",
			len(doc.Sidecar), birdreports.SidecarMaxBytes)
	}
	if _, err := birdreports.ParseSidecar(bird, doc.Sidecar, now); err != nil {
		return fmt.Errorf("sidecar: %w", err)
	}
	return nil
}

// reservedRuntimeNames is the set of the daemon's directories directly under
// .runtime. A report published under one of those names would share it with a
// daemon subsystem, and the split under .runtime is read by name.
func reservedRuntimeNames() map[string]bool {
	names := townstate.Default().DaemonRuntimeNames()
	reserved := make(map[string]bool, len(names))
	for _, name := range names {
		reserved[name] = true
	}
	return reserved
}

// reportPublishTemp is one file of a report being written before its rename.
type reportPublishTemp interface {
	io.Writer
	Name() string
	Chmod(os.FileMode) error
	Sync() error
	Close() error
}

// reportPublishFS is the filesystem operations a publish makes. The command
// uses osReportPublishFS; a test hands a copy with one operation failing, to
// drive a failure a healthy filesystem does not produce on demand.
type reportPublishFS struct {
	lstat      func(string) (os.FileInfo, error)
	readFile   func(string) ([]byte, error)
	mkdir      func(string, os.FileMode) error
	createTemp func(string, string) (reportPublishTemp, error)
	rename     func(string, string) error
}

// osReportPublishFS returns the real filesystem.
func osReportPublishFS() reportPublishFS {
	return reportPublishFS{
		lstat:    os.Lstat,
		readFile: os.ReadFile,
		mkdir:    os.Mkdir,
		createTemp: func(dir, pattern string) (reportPublishTemp, error) {
			return os.CreateTemp(dir, pattern)
		},
		rename: os.Rename,
	}
}

// writeReportPublish writes the page and sidecar into the bird's published
// directory, keeping the page it replaces as daily.prev.html. Every file is
// staged as a temp file and renamed only after all of them are written, so a
// write that fails leaves the files the directory held untouched. The renamed
// page is never a symlink the write follows: the destination is refused when it
// is not a regular file, and a rename replaces the name itself.
func writeReportPublish(fsys reportPublishFS, runtimeDir, bird string, page, sidecar []byte) (reportPublishReceipt, error) {
	dir := birdreports.PublishedReportDir(runtimeDir, bird)
	if err := ensurePublishedDir(fsys, runtimeDir, dir); err != nil {
		return reportPublishReceipt{}, err
	}

	pagePath := filepath.Join(dir, reportPublishPageName)
	sidecarPath := filepath.Join(dir, birdreports.SidecarName)
	prevPath := filepath.Join(dir, reportPublishPrevPageName)
	for _, path := range []string{pagePath, sidecarPath, prevPath} {
		if err := refuseNonRegular(fsys, path); err != nil {
			return reportPublishReceipt{}, err
		}
	}
	prev, havePrev, err := previousPage(fsys, pagePath)
	if err != nil {
		return reportPublishReceipt{}, err
	}

	// The previous page is renamed first, so it is in its place before the
	// page it came from is replaced; the sidecar lands last, after both pages.
	files := make([]reportFile, 0, 3)
	if havePrev {
		files = append(files, reportFile{path: prevPath, data: prev})
	}
	files = append(files,
		reportFile{path: pagePath, data: page},
		reportFile{path: sidecarPath, data: sidecar})

	staged := make([]stagedReportFile, 0, len(files))
	discard := func() {
		for _, s := range staged {
			_ = os.Remove(s.tmp)
		}
	}
	for _, f := range files {
		s, err := stageReportFile(fsys, f.path, f.data)
		if err != nil {
			discard()
			return reportPublishReceipt{}, err
		}
		staged = append(staged, s)
	}
	for _, s := range staged {
		if err := fsys.rename(s.tmp, s.path); err != nil {
			discard()
			return reportPublishReceipt{}, fmt.Errorf("renaming %s into place: %w", s.path, err)
		}
	}
	return reportPublishReceipt{
		pagePath:     pagePath,
		pageBytes:    len(page),
		sidecarPath:  sidecarPath,
		sidecarBytes: len(sidecar),
	}, nil
}

// reportFile is one file of a report and the bytes it is written from.
type reportFile struct {
	path string
	data []byte
}

// stagedReportFile is a temp file waiting for its rename.
type stagedReportFile struct {
	tmp  string
	path string
}

// stageReportFile writes data to a temp file beside path, at the mode the
// report's reader expects. It is renamed into place only once every file of
// the report is written.
func stageReportFile(fsys reportPublishFS, path string, data []byte) (stagedReportFile, error) {
	f, err := fsys.createTemp(filepath.Dir(path), filepath.Base(path)+".tmp.*")
	if err != nil {
		return stagedReportFile{}, fmt.Errorf("writing %s: %w", path, err)
	}
	tmp := f.Name()
	fail := func(err error) (stagedReportFile, error) {
		_ = f.Close()
		_ = os.Remove(tmp)
		return stagedReportFile{}, fmt.Errorf("writing %s: %w", path, err)
	}
	if _, err := f.Write(data); err != nil {
		return fail(err)
	}
	if err := f.Chmod(0o644); err != nil {
		return fail(err)
	}
	if err := f.Sync(); err != nil {
		return fail(err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return stagedReportFile{}, fmt.Errorf("writing %s: %w", path, err)
	}
	return stagedReportFile{tmp: tmp, path: path}, nil
}

// ensurePublishedDir makes the directory the bird's report is published in,
// refusing a path that exists as anything else, a link included: the report
// must land in the directory its reader looks at.
func ensurePublishedDir(fsys reportPublishFS, runtimeDir, dir string) error {
	if err := requireRealDir(fsys, runtimeDir); err != nil {
		return err
	}
	for _, path := range []string{filepath.Join(runtimeDir, birdreports.PublishedDirName), dir} {
		if err := ensureRealDir(fsys, path); err != nil {
			return err
		}
	}
	return nil
}

// requireRealDir refuses a path that is not an existing directory. The runtime
// directory is the daemon's, so this command never makes it.
func requireRealDir(fsys reportPublishFS, path string) error {
	fi, err := fsys.lstat(path)
	switch {
	case err != nil:
		return err
	case !fi.IsDir():
		return fmt.Errorf("%s is not a real directory", path)
	}
	return nil
}

// ensureRealDir makes path a directory when it is absent, and refuses it when
// it is anything else.
func ensureRealDir(fsys reportPublishFS, path string) error {
	fi, err := fsys.lstat(path)
	switch {
	case err == nil && fi.IsDir():
		return nil
	case err == nil:
		return fmt.Errorf("%s is not a real directory", path)
	case !errors.Is(err, fs.ErrNotExist):
		return err
	}
	if err := fsys.mkdir(path, 0o755); err != nil {
		if !errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("creating %s: %w", path, err)
		}
		// Another publisher made it between the check and the mkdir; it must
		// still be the real directory.
		return requireRealDir(fsys, path)
	}
	return nil
}

// refuseNonRegular refuses a destination that exists as something other than a
// regular file, so a link or a directory where a report belongs is reported
// rather than replaced.
func refuseNonRegular(fsys reportPublishFS, path string) error {
	fi, err := fsys.lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return err
	case fi.Mode().IsRegular():
		return nil
	}
	return fmt.Errorf("%s exists and is not a regular file", path)
}

// previousPage returns the page the directory holds now, which the publish
// keeps as daily.prev.html, and whether there was one.
func previousPage(fsys reportPublishFS, path string) ([]byte, bool, error) {
	data, err := fsys.readFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, false, nil
	case err != nil:
		return nil, false, fmt.Errorf("reading the previous page: %w", err)
	}
	return data, true, nil
}

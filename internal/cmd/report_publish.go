package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/birdreports"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/reportdir"
	"github.com/steveyegge/gastown/internal/townstate"
	"github.com/steveyegge/gastown/internal/workspace"
)

const (
	// reportPublishPageMax is the largest page the command writes; a page over
	// it is refused. A report is one page of HTML, not an archive.
	reportPublishPageMax = 1 << 20

	// reportPublishPageName and reportPublishPrevPageName are the page this
	// command writes and the one it keeps in its place.
	reportPublishPageName     = reportdir.PageName
	reportPublishPrevPageName = reportdir.PrevPageName

	// reportPublishPageWireMax is the most JSON text a page at
	// reportPublishPageMax takes: an ASCII control character is one byte and is
	// written as \u00XX, six.
	reportPublishPageWireMax = 6 * reportPublishPageMax

	// reportPublishInputSlack is room in the document for its keys, the bird's
	// name and ordinary spacing around them.
	reportPublishInputSlack = 4 << 10

	// reportPublishInputMax bounds the document read from standard input, so an
	// unbounded stream is never read into memory. A page at its cap takes up to
	// six times its length in JSON text and a sidecar its own cap, and this
	// leaves room for the document's keys and the bird's name, so such a
	// document is decoded and judged by the page's cap, naming the page, rather
	// than refused as a document.
	reportPublishInputMax = reportPublishPageWireMax + birdreports.SidecarMaxBytes + reportPublishInputSlack
)

var reportPublishBird string

var reportPublishCmd = &cobra.Command{
	Use:   "publish",
	Short: "Publish a bird's daily report from standard input",
	Long: `Publish one bird's daily report into the town's runtime directory.

Reads one JSON document from standard input:

  {"bird":"<name>","page":"<the report page>","sidecar":{<the sidecar object>}}

The internal/reportdir package comment states where the page and the sidecar are
written and the names of the pair kept beside them; this command asks that
package for the directory rather than laying a path out here. Each file is a temp
file renamed into place, and a rename that fails puts the previous pair back. The
document is refused, with the field named and nothing written, when it is
malformed, the page is over 1 MiB, the sidecar is over 16 KiB or fails the
reader's own validation (internal/birdreports), the document's bird is not
--bird, or the name is a daemon subsystem directory under .runtime.

The command prints the two paths and their sizes, and never prints the page.

The daily request in docs/birds/daily-request.md builds the sidecar with jq and
pipes the document here.

A Mac caller does not publish with its own gt, which writes under the Mac's
.runtime, where the dashboard does not read. It pipes the same document into the
daemon container through this final step, with the pod found by its label:

  ... | kubectl --context colima-gt-control -n gt exec -i "$(kubectl --context colima-gt-control -n gt get pods -l app.kubernetes.io/name=daemon --field-selector=status.phase=Running -o jsonpath='{.items[0].metadata.name}')" -c daemon -- gt report publish --bird <NAME>

The -i is required: without it the pod reads an empty document and writes nothing.

Examples:
  jq -n --rawfile page page.html --slurpfile sidecar sidecar.json \
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
	if !reportdir.ValidName(bird) {
		return fmt.Errorf("bird: %q is not a bird name (lowercase letters, 2 to 24)", bird)
	}
	if doc.Bird != bird {
		// The document's bird field is the bird's own text, so the refusal
		// names the field and never repeats its value (gt-yjzepk.3).
		return errors.New("bird: the document's bird is not the one --bird names")
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
// directory, rotating the pair it replaces to daily.prev.html and
// daily.prev.json. Every file is staged as a temp file and renamed only after
// all of them are written. If a rename fails, every file already renamed is put
// back as it was and the temp files are removed, so a failed publish leaves the
// old daily.html and daily.json intact rather than a torn pair. The renamed
// page is never a symlink the write follows: a destination that is not a
// regular file is refused, and a rename replaces the name itself.
func writeReportPublish(fsys reportPublishFS, runtimeDir, bird string, page, sidecar []byte) (reportPublishReceipt, error) {
	dir, err := reportdir.Published(runtimeDir, bird)
	if err != nil {
		return reportPublishReceipt{}, err
	}
	if err := ensurePublishedDir(fsys, runtimeDir, dir); err != nil {
		return reportPublishReceipt{}, err
	}

	pagePath := filepath.Join(dir, reportPublishPageName)
	sidecarPath := filepath.Join(dir, reportdir.SidecarName)
	prevPath := filepath.Join(dir, reportPublishPrevPageName)
	prevSidecarPath := filepath.Join(dir, reportdir.PrevSidecarName)

	// What each destination holds now, so a failed rename can put it back.
	pagePrior, hadPage, err := priorFile(fsys, pagePath)
	if err != nil {
		return reportPublishReceipt{}, err
	}
	sidecarPrior, hadSidecar, err := priorFile(fsys, sidecarPath)
	if err != nil {
		return reportPublishReceipt{}, err
	}
	prevPrior, hadPrev, err := priorFile(fsys, prevPath)
	if err != nil {
		return reportPublishReceipt{}, err
	}
	prevSidecarPrior, hadPrevSidecar, err := priorFile(fsys, prevSidecarPath)
	if err != nil {
		return reportPublishReceipt{}, err
	}

	// The previous pair is renamed first, so it is in place before the files it
	// came from are replaced; the new page and sidecar land last.
	files := make([]reportFile, 0, 4)
	if hadPage {
		files = append(files, reportFile{path: prevPath, data: pagePrior, prior: prevPrior, hadPrior: hadPrev})
	}
	if hadSidecar {
		files = append(files, reportFile{path: prevSidecarPath, data: sidecarPrior, prior: prevSidecarPrior, hadPrior: hadPrevSidecar})
	}
	files = append(files,
		reportFile{path: pagePath, data: page, prior: pagePrior, hadPrior: hadPage},
		reportFile{path: sidecarPath, data: sidecar, prior: sidecarPrior, hadPrior: hadSidecar})

	staged := make([]stagedReportFile, 0, len(files))
	for _, f := range files {
		s, err := stageReportFile(fsys, f)
		if err != nil {
			discardStaged(staged)
			return reportPublishReceipt{}, err
		}
		staged = append(staged, s)
	}

	renamed := make([]stagedReportFile, 0, len(staged))
	for i, s := range staged {
		if err := fsys.rename(s.tmp, s.path); err != nil {
			restoreErr := restoreStagedReport(fsys, renamed)
			discardStaged(staged[i:])
			if restoreErr != nil {
				return reportPublishReceipt{}, fmt.Errorf("renaming %s into place: %w (restoring the previous report also failed: %v)", s.path, err, restoreErr)
			}
			return reportPublishReceipt{}, fmt.Errorf("renaming %s into place: %w", s.path, err)
		}
		renamed = append(renamed, s)
	}
	return reportPublishReceipt{
		pagePath:     pagePath,
		pageBytes:    len(page),
		sidecarPath:  sidecarPath,
		sidecarBytes: len(sidecar),
	}, nil
}

// reportFile is one file of a report: the bytes this publish writes to it, and
// the bytes it replaces so a failure can put them back.
type reportFile struct {
	path     string
	data     []byte
	prior    []byte
	hadPrior bool
}

// stagedReportFile is a temp file waiting for its rename, with what the
// destination held before it.
type stagedReportFile struct {
	tmp      string
	path     string
	prior    []byte
	hadPrior bool
}

// stageReportFile writes f.data to a temp file beside f.path, at the mode the
// report's reader expects. It is renamed into place only once every file of the
// report is written.
func stageReportFile(fsys reportPublishFS, f reportFile) (stagedReportFile, error) {
	file, err := fsys.createTemp(filepath.Dir(f.path), filepath.Base(f.path)+".tmp.*")
	if err != nil {
		return stagedReportFile{}, fmt.Errorf("writing %s: %w", f.path, err)
	}
	tmp := file.Name()
	fail := func(err error) (stagedReportFile, error) {
		_ = file.Close()
		_ = os.Remove(tmp)
		return stagedReportFile{}, fmt.Errorf("writing %s: %w", f.path, err)
	}
	if _, err := file.Write(f.data); err != nil {
		return fail(err)
	}
	if err := file.Chmod(0o644); err != nil {
		return fail(err)
	}
	if err := file.Sync(); err != nil {
		return fail(err)
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(tmp)
		return stagedReportFile{}, fmt.Errorf("writing %s: %w", f.path, err)
	}
	return stagedReportFile{tmp: tmp, path: f.path, prior: f.prior, hadPrior: f.hadPrior}, nil
}

// discardStaged removes the temp files that were never renamed.
func discardStaged(staged []stagedReportFile) {
	for _, s := range staged {
		_ = os.Remove(s.tmp)
	}
}

// restoreStagedReport puts the files a failed publish already renamed back as
// they were, newest first. A destination that did not exist before is removed;
// one that did is written to a temp file and renamed back, so a reader never
// sees a half-written file. Every destination is tried; the failures are
// returned together, so a restore that could not finish is not reported as a
// clean rollback.
func restoreStagedReport(fsys reportPublishFS, renamed []stagedReportFile) error {
	var errs []error
	for i := len(renamed) - 1; i >= 0; i-- {
		s := renamed[i]
		if !s.hadPrior {
			if err := os.Remove(s.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
				errs = append(errs, fmt.Errorf("removing %s: %w", s.path, err))
			}
			continue
		}
		restored, err := stageReportFile(fsys, reportFile{path: s.path, data: s.prior})
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if err := fsys.rename(restored.tmp, s.path); err != nil {
			_ = os.Remove(restored.tmp)
			errs = append(errs, fmt.Errorf("restoring %s: %w", s.path, err))
		}
	}
	return errors.Join(errs...)
}

// ensurePublishedDir makes the directory the bird's report is published in,
// refusing a path that exists as anything else, a link included: the report
// must land in the directory its reader looks at.
func ensurePublishedDir(fsys reportPublishFS, runtimeDir, dir string) error {
	if err := requireRealDir(fsys, runtimeDir); err != nil {
		return err
	}
	for _, path := range []string{reportdir.PublishedReportsDir(runtimeDir), dir} {
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

// priorFile returns the bytes path holds now, so a failed publish can put them
// back, and whether it held a regular file. A destination that exists as
// anything else — a link or a directory where a report belongs — is refused
// rather than replaced.
func priorFile(fsys reportPublishFS, path string) ([]byte, bool, error) {
	fi, err := fsys.lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, false, nil
	case err != nil:
		return nil, false, err
	case !fi.Mode().IsRegular():
		return nil, false, fmt.Errorf("%s exists and is not a regular file", path)
	}
	data, err := fsys.readFile(path)
	if err != nil {
		return nil, false, fmt.Errorf("reading %s: %w", path, err)
	}
	return data, true, nil
}

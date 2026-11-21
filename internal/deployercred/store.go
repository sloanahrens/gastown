package deployercred

import (
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	// dirMode is the one mode the credential directory may have: the deployer
	// user alone may list or enter it.
	dirMode fs.FileMode = 0o700
	// fileMode is the one mode a credential file may have.
	fileMode fs.FileMode = 0o600
	// adminContext is the town's admin kubectl context; a deployer context of
	// that name would shadow the operator's own.
	adminContext = "colima-gt-control"
)

// Info is one path as Store or the approval-record reader checks it: its kind and
// permission bits, the uid that owns it, and its group's gid. Owner is -1
// when the platform reports no owner, which Store refuses rather than reading as
// uid 0; Group is -1 when the platform reports no group. Store and Check do not
// test Group: the reader refuses a file outside the deployer's group (gt-31vzri).
type Info struct {
	Mode  fs.FileMode
	Owner int
	Group int
}

// FS is the filesystem Store writes the credentials through: Disk puts the
// host's behind it, and a test puts an in-memory fake.
type FS interface {
	Lstat(name string) (Info, error)
	MkdirAll(dir string, perm fs.FileMode) error
	// CreateExclusive opens name for writing with perm, failing when name
	// already exists. The caller relies on the atomicity of that check: a
	// separate existence test would let another process put a file in the way.
	CreateExclusive(name string, perm fs.FileMode) (File, error)
	Chmod(name string, mode fs.FileMode) error
	Rename(oldpath, newpath string) error
	Remove(name string) error
}

// File is a file Store is writing the credential bytes into.
type File interface {
	Write(p []byte) (int, error)
	Sync() error
	Close() error
}

// Disk is FS on the host's filesystem.
type Disk struct{}

// Lstat is the host's lstat, with the owner read from the platform's stat.
func (Disk) Lstat(name string) (Info, error) {
	fi, err := os.Lstat(name)
	if err != nil {
		return Info{}, err
	}
	return infoOf(fi), nil
}

// MkdirAll is os.MkdirAll.
func (Disk) MkdirAll(dir string, perm fs.FileMode) error { return os.MkdirAll(dir, perm) }

// CreateExclusive is os.OpenFile with O_CREATE|O_EXCL: the file is created in
// perm, and an existing path is an error rather than a file to write through.
func (Disk) CreateExclusive(name string, perm fs.FileMode) (File, error) {
	return os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
}

// Chmod is os.Chmod.
func (Disk) Chmod(name string, mode fs.FileMode) error { return os.Chmod(name, mode) }

// Rename is os.Rename.
func (Disk) Rename(oldpath, newpath string) error { return os.Rename(oldpath, newpath) }

// Remove is os.Remove.
func (Disk) Remove(name string) error { return os.Remove(name) }

// Disk is the FS the deployer command runs on the host; a drift from the
// interface fails to compile here rather than at the command.
var _ FS = Disk{}

// ReadFS is the filesystem the approval-record reader reads a directory and a
// file through. A Stat on an open handle describes the bytes its Read returns,
// not a path checked a moment earlier (gt-31vzri).
type ReadFS interface {
	Lstat(name string) (Info, error)
	OpenDir(name string) (Dir, error)
}

// Dir is an open directory handle: Stat reports the handle's attributes, and
// Open opens a name inside it rather than along the path it was opened by.
type Dir interface {
	Stat() (Info, error)
	Open(name string) (ReadFile, error)
	Close() error
}

// ReadFile is a file opened inside a Dir: Stat reports the opened descriptor,
// so it describes the bytes Read returns.
type ReadFile interface {
	Stat() (Info, error)
	Read(p []byte) (int, error)
	Close() error
}

// OpenDir is os.OpenRoot: every later name resolves inside the directory the
// handle was opened on, so a symlink swapped onto the path after the open
// cannot redirect a read through the handle.
func (Disk) OpenDir(name string) (Dir, error) {
	root, err := os.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	return diskDir{root: root}, nil
}

// diskDir is Dir through a directory descriptor, which outlives a rename of the
// path it was opened by.
type diskDir struct{ root *os.Root }

func (d diskDir) Stat() (Info, error) {
	fi, err := d.root.Stat(".")
	if err != nil {
		return Info{}, err
	}
	return infoOf(fi), nil
}

func (d diskDir) Open(name string) (ReadFile, error) {
	f, err := d.root.Open(name)
	if err != nil {
		return nil, err
	}
	return diskFile{f: f}, nil
}

func (d diskDir) Close() error { return d.root.Close() }

// diskFile is ReadFile through the opened descriptor, so a Stat on it describes
// the bytes Read returns.
type diskFile struct{ f *os.File }

func (d diskFile) Stat() (Info, error) {
	fi, err := d.f.Stat()
	if err != nil {
		return Info{}, err
	}
	return infoOf(fi), nil
}

func (d diskFile) Read(p []byte) (int, error) { return d.f.Read(p) }

func (d diskFile) Close() error { return d.f.Close() }

// Disk is the read side the deployer command runs on the host; a drift from the
// interface fails to compile here rather than at the command.
var _ ReadFS = Disk{}

// Store writes files into dir, a directory in exactly 0700 held by uid, and
// refuses anything it cannot verify: a symlink at dir or on the way to it, a
// directory with another mode or owner, a name that is not one plain file, a
// target that is or ends in .kube/config, and an existing file that is not 0600
// and uid's. Each file is created exclusively under a temporary name in the
// directory, written and synced, verified 0600 and uid's, and only then renamed
// onto its target, so a reader of the target opens either no file or the whole
// one; a failure at any step removes the temporary file, and a removal that also
// fails is reported with the cause. The files are not written as one unit: a
// failure after the first rename leaves the files already renamed in place,
// which Check refuses. The bytes are never returned or printed; an error names
// the path, not the content.
func Store(fsys FS, uid int, dir string, files map[string][]byte) error {
	if fsys == nil {
		return errors.New("the store has no filesystem to write through")
	}
	if uid < 0 {
		return fmt.Errorf("the deployer uid is %d, and an owner is never read from a negative uid", uid)
	}
	if err := checkDirPath(dir); err != nil {
		return err
	}
	if len(files) == 0 {
		return fmt.Errorf("there are no files to store in %s", dir)
	}
	names := make([]string, 0, len(files))
	for name := range files {
		if err := checkName(name); err != nil {
			return err
		}
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := checkTarget(dir, name); err != nil {
			return err
		}
	}

	info, exists, err := checkChain(fsys, dir)
	if err != nil {
		return err
	}
	if !exists {
		if err := fsys.MkdirAll(dir, dirMode); err != nil {
			return fmt.Errorf("could not create the deployer directory %s: %w", dir, err)
		}
		info, err = fsys.Lstat(dir)
		if err != nil {
			return fmt.Errorf("could not read the deployer directory %s it just created: %w", dir, err)
		}
	}
	if err := checkDir(dir, info, uid); err != nil {
		return err
	}

	// Every target is checked before the first write, so a file Store refuses
	// leaves the directory as it found it.
	for _, name := range names {
		if err := checkExisting(fsys, uid, filepath.Join(dir, name)); err != nil {
			return err
		}
	}
	for _, name := range names {
		if err := writeFile(fsys, uid, dir, name, files[name]); err != nil {
			return err
		}
	}
	return nil
}

// Check refuses a credential set Store cannot vouch for: it returns an error
// unless dir still passes Store's directory checks and every named file exists,
// is 0600 and owned by uid. It reads no file contents.
func Check(fsys FS, uid int, dir string, names []string) error {
	if fsys == nil {
		return errors.New("the check has no filesystem to read through")
	}
	if uid < 0 {
		return fmt.Errorf("the deployer uid is %d, and an owner is never read from a negative uid", uid)
	}
	if err := checkDirPath(dir); err != nil {
		return err
	}
	if len(names) == 0 {
		return fmt.Errorf("there are no files to check in %s", dir)
	}
	info, exists, err := checkChain(fsys, dir)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("the deployer directory %s does not exist", dir)
	}
	if err := checkDir(dir, info, uid); err != nil {
		return err
	}
	for _, name := range names {
		if err := checkName(name); err != nil {
			return err
		}
		if err := checkPresent(fsys, uid, filepath.Join(dir, name)); err != nil {
			return err
		}
	}
	return nil
}

// checkDirPath refuses a directory path Store cannot verify as one place: an
// empty, relative or unclean path names a different directory than the one the
// Lstat walk checks.
func checkDirPath(dir string) error {
	switch {
	case dir == "":
		return errors.New("the deployer directory is empty")
	case !filepath.IsAbs(dir):
		return fmt.Errorf("the deployer directory %q is not an absolute path", dir)
	case filepath.Clean(dir) != dir:
		return fmt.Errorf("the deployer directory %q is not a clean path", dir)
	}
	return nil
}

// checkName refuses a name that is not one plain file in dir: a separator would
// put the file in a subdirectory Store never verified.
func checkName(name string) error {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\`) {
		return fmt.Errorf("the credential name %q is not one plain file name in the deployer directory", name)
	}
	return nil
}

// checkTarget refuses the admin kubeconfig's path: writing the deployer
// credential there would replace the operator's own kubectl credential.
func checkTarget(dir, name string) error {
	target := filepath.Join(dir, name)
	sep := string(filepath.Separator)
	if strings.HasSuffix(target, sep+".kube"+sep+"config") {
		return fmt.Errorf("the target %s is a .kube/config, where the deployer credential is never written", target)
	}
	return nil
}

// checkChain refuses a symlink at dir or at any parent above it, and reports the
// directory's Info when it already exists. Every ancestor is checked because
// Store is given no home to stop at, and a symlink anywhere above the write
// would move the credential somewhere nobody verified. A missing component is
// not an error here: MkdirAll creates it inside a chain already checked above
// it. A component that cannot be read is an error, never treated as absent.
func checkChain(fsys FS, dir string) (Info, bool, error) {
	var dirInfo Info
	exists := false
	for p := dir; ; p = filepath.Dir(p) {
		info, err := fsys.Lstat(p)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			// Keep climbing: the parents of a missing component still need the
			// check.
		case err != nil:
			return Info{}, false, fmt.Errorf("could not read %s on the way to the deployer directory %s: %w", p, dir, err)
		case info.Mode&fs.ModeSymlink != 0:
			return Info{}, false, fmt.Errorf("%s is a symlink, and the deployer credential is written only into a real directory", p)
		case p == dir:
			dirInfo, exists = info, true
		case !info.Mode.IsDir():
			return Info{}, false, fmt.Errorf("%s is not a directory, so the deployer directory %s cannot be reached", p, dir)
		}
		if parent := filepath.Dir(p); parent == p {
			return dirInfo, exists, nil
		}
	}
}

// checkDir refuses a directory Store does not hold exclusively: a real
// directory in exactly 0700, owned by uid.
func checkDir(dir string, info Info, uid int) error {
	if !info.Mode.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}
	if info.Mode != fs.ModeDir|dirMode {
		return fmt.Errorf("the deployer directory %s has mode %04o, not 0700", dir, info.Mode.Perm())
	}
	return checkOwner("the deployer directory "+dir, info, uid)
}

// checkOwner refuses a path whose owner is not uid's. An owner the platform does
// not report is an error, never read as uid 0.
func checkOwner(what string, info Info, uid int) error {
	if info.Owner < 0 {
		return fmt.Errorf("the owner of %s is not reported on this platform, so it cannot be verified as uid %d's", what, uid)
	}
	if info.Owner != uid {
		return fmt.Errorf("%s is owned by uid %d, not %d", what, info.Owner, uid)
	}
	return nil
}

// checkExisting refuses an existing target Store would replace: a symlink, a
// file that is not regular, or one that is not already 0600 and uid's.
func checkExisting(fsys FS, uid int, target string) error {
	info, err := fsys.Lstat(target)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("could not read the existing credential %s: %w", target, err)
	}
	return checkFileInfo(uid, target, info)
}

// checkPresent refuses a credential Check cannot vouch for: an absent file, a
// symlink, a file that is not regular, or one that is not 0600 and uid's.
func checkPresent(fsys FS, uid int, target string) error {
	info, err := fsys.Lstat(target)
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("the credential %s does not exist", target)
	}
	if err != nil {
		return fmt.Errorf("could not read the credential %s: %w", target, err)
	}
	return checkFileInfo(uid, target, info)
}

// checkFileInfo refuses a path that is not one credential file: a symlink, a
// directory, a wider mode, or another owner.
func checkFileInfo(uid int, target string, info Info) error {
	switch {
	case info.Mode&fs.ModeSymlink != 0:
		return fmt.Errorf("the credential %s is a symlink, and a credential is never a link", target)
	case !info.Mode.IsRegular():
		return fmt.Errorf("the credential %s is not a regular file", target)
	case info.Mode != fileMode:
		return fmt.Errorf("the credential %s has mode %04o, not 0600", target, info.Mode.Perm())
	}
	return checkOwner("the credential "+target, info, uid)
}

// writeFile writes one credential through a temporary file in dir and renames it
// onto the target only after its mode and owner are verified, so neither a
// failed write nor a filesystem that dropped a permission bit leaves anything
// usable at the target. A temporary path that already exists is refused by the
// exclusive create, never written over.
func writeFile(fsys FS, uid int, dir, name string, data []byte) error {
	target := filepath.Join(dir, name)
	temp := tempName(dir, name)
	f, err := fsys.CreateExclusive(temp, fileMode)
	if err != nil {
		return fmt.Errorf("could not create a temporary file for %s: %w", target, err)
	}
	// CreateExclusive's mode is subject to umask, so Chmod makes 0600 the mode
	// the temporary file holds before its bytes reach the disk.
	if err := fsys.Chmod(temp, fileMode); err != nil {
		return finishTemp(fsys, f, temp, fmt.Errorf("could not set the credential %s to 0600: %w", target, err))
	}
	if err := writeAndSync(f, data); err != nil {
		return finishTemp(fsys, f, temp, fmt.Errorf("could not write the credential %s: %w", target, err))
	}
	if err := f.Close(); err != nil {
		return finishTemp(fsys, nil, temp, fmt.Errorf("could not close the credential %s: %w", target, err))
	}
	if err := verifyTemp(fsys, uid, temp, target); err != nil {
		return finishTemp(fsys, nil, temp, err)
	}
	if err := fsys.Rename(temp, target); err != nil {
		return finishTemp(fsys, nil, temp, fmt.Errorf("could not move the credential into %s: %w", target, err))
	}
	return nil
}

// writeAndSync writes the credential bytes and flushes them, so a power loss
// after the rename leaves the whole file, not a prefix of it.
func writeAndSync(f File, data []byte) error {
	if _, err := f.Write(data); err != nil {
		return err
	}
	return f.Sync()
}

// finishTemp removes the temporary file after a failure before the rename, so no
// key bytes stay in the directory, and returns the cause. A close or removal
// that also fails is joined to the cause, never dropped: a temporary file that
// outlives its removal still holds the credential.
func finishTemp(fsys FS, f File, temp string, cause error) error {
	errs := []error{cause}
	if f != nil {
		if err := f.Close(); err != nil {
			errs = append(errs, fmt.Errorf("the temporary file %s could not be closed: %w", temp, err))
		}
	}
	if err := fsys.Remove(temp); err != nil && !errors.Is(err, fs.ErrNotExist) {
		errs = append(errs, fmt.Errorf("the temporary file %s could not be removed and may hold the credential: %w", temp, err))
	}
	return errors.Join(errs...)
}

// tempName is the name a credential is written under before its rename; the pid
// keeps two runs apart, so one neither overwrites nor blocks the other's
// temporary file.
func tempName(dir, name string) string {
	return filepath.Join(dir, "."+name+"."+strconv.Itoa(os.Getpid())+".tmp")
}

// verifyTemp checks the mode and owner the temporary file holds before the
// rename, so a filesystem that dropped a permission bit refuses instead of
// landing a world-readable credential.
func verifyTemp(fsys FS, uid int, temp, target string) error {
	info, err := fsys.Lstat(temp)
	if err != nil {
		return fmt.Errorf("could not read back the temporary file %s for %s: %w", temp, target, err)
	}
	if info.Mode != fileMode {
		return fmt.Errorf("the temporary file %s for %s holds mode %04o, not 0600, so it is never moved into place", temp, target, info.Mode.Perm())
	}
	return checkOwner("the temporary file "+temp+" for "+target, info, uid)
}

// kubeconfigObject is the kubeconfig as kubectl reads it; only the fields the
// deployer's context needs are set.
type kubeconfigObject struct {
	APIVersion     string        `yaml:"apiVersion"`
	Kind           string        `yaml:"kind"`
	Clusters       []kubeCluster `yaml:"clusters"`
	Users          []kubeUser    `yaml:"users"`
	Contexts       []kubeContext `yaml:"contexts"`
	CurrentContext string        `yaml:"current-context"`
}

type kubeCluster struct {
	Name    string            `yaml:"name"`
	Cluster kubeClusterConfig `yaml:"cluster"`
}

type kubeClusterConfig struct {
	Server                   string `yaml:"server"`
	CertificateAuthorityData string `yaml:"certificate-authority-data"`
}

type kubeUser struct {
	Name string         `yaml:"name"`
	User kubeUserConfig `yaml:"user"`
}

type kubeUserConfig struct {
	ClientCertificate string `yaml:"client-certificate"`
	ClientKey         string `yaml:"client-key"`
}

type kubeContext struct {
	Name    string            `yaml:"name"`
	Context kubeContextConfig `yaml:"context"`
}

type kubeContextConfig struct {
	Cluster string `yaml:"cluster"`
	User    string `yaml:"user"`
}

// Kubeconfig returns the deployer's kubeconfig: the cluster at server with caPEM
// embedded, and a user that references the certificate and key by path, so the
// private key is never copied into a second file. The server must be https on a
// loopback address, the CA must hold a certificate that parses, both paths must
// be absolute, and contextName may not be the admin context.
func Kubeconfig(server string, caPEM []byte, certPath, keyPath, contextName string) ([]byte, error) {
	return kubeconfigDocument(server, caPEM, certPath, keyPath, contextName, yaml.Marshal)
}

// kubeconfigDocument is Kubeconfig over the one call that can fail, so a test
// can drive the marshal failure.
func kubeconfigDocument(server string, caPEM []byte, certPath, keyPath, contextName string, marshal func(any) ([]byte, error)) ([]byte, error) {
	if contextName == "" {
		return nil, errors.New("the kubeconfig context name is empty")
	}
	if contextName == adminContext {
		return nil, fmt.Errorf("the context name %q is the town's admin context, and a deployer kubeconfig never shadows it", contextName)
	}
	if err := checkServer(server); err != nil {
		return nil, err
	}
	if err := checkCA(caPEM); err != nil {
		return nil, err
	}
	if err := checkCredentialPath("certificate", certPath); err != nil {
		return nil, err
	}
	if err := checkCredentialPath("key", keyPath); err != nil {
		return nil, err
	}

	out, err := marshal(kubeconfigObject{
		APIVersion: "v1",
		Kind:       "Config",
		Clusters: []kubeCluster{{
			Name: contextName,
			Cluster: kubeClusterConfig{
				Server:                   server,
				CertificateAuthorityData: base64.StdEncoding.EncodeToString(caPEM),
			},
		}},
		Users: []kubeUser{{
			Name: contextName,
			User: kubeUserConfig{ClientCertificate: certPath, ClientKey: keyPath},
		}},
		Contexts: []kubeContext{{
			Name:    contextName,
			Context: kubeContextConfig{Cluster: contextName, User: contextName},
		}},
		CurrentContext: contextName,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal the %s kubeconfig: %w", contextName, err)
	}
	return out, nil
}

// checkServer refuses a server that is not https on a loopback address: the
// deployer's client certificate reaches only the cluster on this host, over TLS.
func checkServer(server string) error {
	u, err := url.Parse(server)
	if err != nil {
		return fmt.Errorf("the cluster server %q does not parse: %w", server, err)
	}
	if u.Scheme != "https" {
		return fmt.Errorf("the cluster server %q is not an https URL, and the deployer kubeconfig sends its credential only over TLS", server)
	}
	if u.User != nil {
		return fmt.Errorf("the cluster server %q carries user info, and the deployer kubeconfig names no credential in its URL", server)
	}
	host := u.Hostname()
	if host == "localhost" {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return nil
	}
	return fmt.Errorf("the cluster server %q is not a loopback address, and the deployer credential reaches only the cluster on this host", server)
}

// checkCA refuses a CA that is empty, holds no CERTIFICATE block, or holds one
// that does not parse: a kubeconfig that could not verify the server must not be
// written as if it could.
func checkCA(caPEM []byte) error {
	if len(caPEM) == 0 {
		return errors.New("the cluster CA is empty, so the kubeconfig could not verify the server")
	}
	rest := caPEM
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			return errors.New("the cluster CA holds no CERTIFICATE PEM block, so the kubeconfig could not verify the server")
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		if _, err := x509.ParseCertificate(block.Bytes); err != nil {
			return fmt.Errorf("the cluster CA's CERTIFICATE block does not parse as a certificate, so the kubeconfig could not verify the server: %w", err)
		}
		return nil
	}
}

// checkCredentialPath refuses a path that is not absolute: kubectl resolves a
// relative one against whatever directory it runs in, which is not the
// deployer's credential directory.
func checkCredentialPath(what, path string) error {
	switch {
	case path == "":
		return fmt.Errorf("the %s path is empty", what)
	case !filepath.IsAbs(path):
		return fmt.Errorf("the %s path %q is not absolute, and kubectl resolves a relative one against its own directory", what, path)
	}
	return nil
}

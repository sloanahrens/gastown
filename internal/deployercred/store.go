package deployercred

import (
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
	// that name would shadow the operator's own (Q13).
	adminContext = "colima-gt-control"
	// kubeconfigTarget is the file a credential is never written to: the
	// deployer's kubeconfig lives in its own 0700 directory (Q13).
	kubeconfigTarget = ".kube/config"
)

// Info is one path as Store checks it: its kind and permission bits, and the
// uid that owns it. Owner is -1 when the platform reports no owner, which Store
// refuses rather than reading as uid 0.
type Info struct {
	Mode  fs.FileMode
	Owner int
}

// FS is the filesystem Store writes the credentials through: Disk puts the
// host's behind it, and a test puts an in-memory fake.
type FS interface {
	Lstat(name string) (Info, error)
	MkdirAll(dir string, perm fs.FileMode) error
	WriteFile(name string, data []byte, perm fs.FileMode) error
	Chmod(name string, mode fs.FileMode) error
	Rename(oldpath, newpath string) error
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

// WriteFile is os.WriteFile.
func (Disk) WriteFile(name string, data []byte, perm fs.FileMode) error {
	return os.WriteFile(name, data, perm)
}

// Chmod is os.Chmod.
func (Disk) Chmod(name string, mode fs.FileMode) error { return os.Chmod(name, mode) }

// Rename is os.Rename.
func (Disk) Rename(oldpath, newpath string) error { return os.Rename(oldpath, newpath) }

// Disk is the FS the deployer command runs on the host; a drift from the
// interface fails to compile here rather than at the command.
var _ FS = Disk{}

// Store writes files into dir, a directory in exactly 0700 held by uid, and
// refuses anything it cannot verify: a symlink at dir or on the way to it, a
// directory with another mode or owner, a name that is not one plain file, a
// target that is or ends in .kube/config, and an existing file that is not 0600
// and uid's. Each file is written 0600 under a temporary name and renamed into
// place, so a reader opens either no credential or a whole one. The bytes are
// never returned or printed; an error names the path, not the content.
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

// checkTarget refuses the admin kubeconfig's path: the deployer credential is
// written into its own 0700 directory, never over ~/.kube/config (Q13).
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
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return fmt.Errorf("could not read the existing credential %s: %w", target, err)
	}
	switch {
	case info.Mode&fs.ModeSymlink != 0:
		return fmt.Errorf("the credential %s is a symlink, and Store never writes through one", target)
	case !info.Mode.IsRegular():
		return fmt.Errorf("the credential %s is not a regular file", target)
	case info.Mode != fileMode:
		return fmt.Errorf("the existing credential %s has mode %04o, not 0600", target, info.Mode.Perm())
	}
	return checkOwner("the credential "+target, info, uid)
}

// writeFile writes one credential under a temporary name in dir and renames it
// into place, so a reader of the target opens either no file or the whole one.
// WriteFile's mode is subject to umask, so Chmod repeats 0600 before the rename.
// A temporary path that already exists is refused: Store writes only over a file
// it created.
func writeFile(fsys FS, uid int, dir, name string, data []byte) error {
	target := filepath.Join(dir, name)
	temp := tempName(dir, name)
	_, err := fsys.Lstat(temp)
	switch {
	case err == nil:
		return fmt.Errorf("the temporary file %s already exists, and Store never writes over a file it did not create", temp)
	case !errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("could not read the temporary path %s: %w", temp, err)
	}
	if err := fsys.WriteFile(temp, data, fileMode); err != nil {
		return fmt.Errorf("could not write the credential %s: %w", target, err)
	}
	if err := fsys.Chmod(temp, fileMode); err != nil {
		return fmt.Errorf("could not set the credential %s to 0600 before moving it into place: %w", target, err)
	}
	if err := fsys.Rename(temp, target); err != nil {
		return fmt.Errorf("could not move the credential into %s: %w", target, err)
	}
	return verifyWritten(fsys, uid, target)
}

// tempName is the name a credential is written under before its rename; the pid
// keeps two runs apart, so one neither overwrites nor blocks the other's
// temporary file.
func tempName(dir, name string) string {
	return filepath.Join(dir, "."+name+"."+strconv.Itoa(os.Getpid())+".tmp")
}

// verifyWritten checks the mode and owner the finished file landed with, so a
// filesystem that dropped a permission bit is a refusal, not a credential.
func verifyWritten(fsys FS, uid int, target string) error {
	info, err := fsys.Lstat(target)
	if err != nil {
		return fmt.Errorf("could not read back the credential %s: %w", target, err)
	}
	if info.Mode != fileMode {
		return fmt.Errorf("the credential %s has mode %04o after the write, not 0600", target, info.Mode.Perm())
	}
	return checkOwner("the credential "+target, info, uid)
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
// loopback address, the CA must hold a certificate, both paths must be
// absolute, and contextName may not be the admin context (Q13).
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

// checkCA refuses a CA that is empty or holds no certificate: a kubeconfig that
// could not verify the server must not be written as if it could.
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
		if block.Type == "CERTIFICATE" {
			return nil
		}
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

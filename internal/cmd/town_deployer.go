package cmd

import (
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/deployercred"
)

// townDeployerCmd holds the credential commands the deployer user runs on the
// host; each one refuses a pod and a running user other than --as-user
// (gt-5wyd9y).
var townDeployerCmd = &cobra.Command{
	Use:   "deployer",
	Short: "Make the deployer's cluster credential as the deployer user",
	RunE:  requireSubcommand,
}

// maxRequestedDays bounds --days before it becomes a time.Duration: the apply
// identity lives at most 30 days (deployercred.MaxLifetime), and the bound also
// keeps the multiplication below from overflowing.
const maxRequestedDays = 30

var townDeployerCSRCmd = &cobra.Command{
	Use:   "csr",
	Short: "Write the deployer client key and print its signing request",
	Long: `Write a P-256 client key for the named deployer identity into --dir, and
print the CertificateSigningRequest YAML the operator submits for signing. The
key is written only into a directory deployercred verifies as 0700 and owned by
the deployer user, and the command never prints it.

The command runs only as the deployer user named by --as-user, and never inside
a pod: the deployer credential is made by the deployer user on the host.

--name is gt-deployer-apply (at most 30 days) or gt-deployer-policy (at most
one day). The key is written to <dir>/<name>.key.

Examples:
  gt town deployer csr --as-user deployer --name gt-deployer-apply --days 30 --dir ~deployer/.gt/deployer --out-csr apply.csr.yaml
  gt town deployer csr --as-user deployer --name gt-deployer-policy --days 1 --dir ~deployer/.gt/deployer`,
	Args: cobra.NoArgs,
	RunE: runTownDeployerCSR,
}

var townDeployerKubeconfigCmd = &cobra.Command{
	Use:   "kubeconfig",
	Short: "Store the signed certificate and write the deployer's kubeconfig",
	Long: `Store the signed certificate from --cert beside the key csr wrote, and
write the deployer's kubeconfig into --dir.

The kubeconfig names <dir>/<name>.crt and <dir>/<name>.key by path, so the
private key is never copied out of the directory csr wrote it into, and embeds
the CA at --ca so the deployer reaches only the cluster on this host. --name is
also the context name, and the town's admin context name is refused so the
deployer's kubeconfig never shadows the operator's own.

The command writes only into the directory deployercred verifies as 0700 and
owned by --as-user, at 0600. A --ca or --cert file that cannot be read, or
holds no certificate that parses, is an error naming the file.

The command runs only as the deployer user named by --as-user, and never inside
a pod.

Examples:
  gt town deployer kubeconfig --as-user deployer --name gt-deployer-apply --dir ~deployer/.gt/deployer --server https://127.0.0.1:64459 --ca ca.crt --cert gt-deployer-apply.crt`,
	Args: cobra.NoArgs,
	RunE: runTownDeployerKubeconfig,
}

func init() {
	townCmd.AddCommand(townDeployerCmd)
	townDeployerCmd.AddCommand(townDeployerCSRCmd)
	townDeployerCSRCmd.Flags().String("as-user", "", "the deployer user the command must run as (required)")
	townDeployerCSRCmd.Flags().String("name", "", "the deployer identity: gt-deployer-apply or gt-deployer-policy (required)")
	townDeployerCSRCmd.Flags().Int("days", 0, "the certificate lifetime in days (required)")
	townDeployerCSRCmd.Flags().String("dir", "", "the deployer credential directory, verified 0700 and owned by --as-user (required)")
	townDeployerCSRCmd.Flags().String("out-csr", "", "write the signing request here instead of standard output (optional)")
	_ = townDeployerCSRCmd.MarkFlagRequired("as-user")
	_ = townDeployerCSRCmd.MarkFlagRequired("name")
	_ = townDeployerCSRCmd.MarkFlagRequired("days")
	_ = townDeployerCSRCmd.MarkFlagRequired("dir")

	townDeployerCmd.AddCommand(townDeployerKubeconfigCmd)
	townDeployerKubeconfigCmd.Flags().String("as-user", "", "the deployer user the command must run as (required)")
	townDeployerKubeconfigCmd.Flags().String("name", "", "the deployer identity, and the kubeconfig's context name (required)")
	townDeployerKubeconfigCmd.Flags().String("dir", "", "the deployer credential directory the key is in, verified 0700 and owned by --as-user (required)")
	townDeployerKubeconfigCmd.Flags().String("server", "", "the control cluster's address as https on loopback, e.g. https://127.0.0.1:64459 (required)")
	townDeployerKubeconfigCmd.Flags().String("ca", "", "the cluster CA certificate file, embedded in the kubeconfig (required)")
	townDeployerKubeconfigCmd.Flags().String("cert", "", "the signed client certificate file for --name (required)")
	_ = townDeployerKubeconfigCmd.MarkFlagRequired("as-user")
	_ = townDeployerKubeconfigCmd.MarkFlagRequired("name")
	_ = townDeployerKubeconfigCmd.MarkFlagRequired("dir")
	_ = townDeployerKubeconfigCmd.MarkFlagRequired("server")
	_ = townDeployerKubeconfigCmd.MarkFlagRequired("ca")
	_ = townDeployerKubeconfigCmd.MarkFlagRequired("cert")
}

// deployerEnv is what a deployer command reads from the host: the running user,
// the filesystem and the environment. Production fills it from os and os/user;
// a test fills it with a fixed user and a fake filesystem, so no test reads the
// developer's own identity.
type deployerEnv struct {
	// command names the subcommand in a refusal.
	command string
	asUser  string
	getenv  func(string) string
	current func() (*user.User, error)
	fs      deployercred.FS
	// readFile is os.ReadFile for the files the operator hands the command,
	// the CA and the signed certificate; they are inputs, so they do not go
	// through deployercred.Store.
	readFile func(string) ([]byte, error)
	// write is os.WriteFile for the CSR; every file that lands in the
	// credential directory goes through deployercred.Store alone.
	write func(string, []byte, fs.FileMode) error
	out   io.Writer
}

// deployerUserUID refuses unless the process runs as asUser, and returns that
// user's uid: Store verifies the credential directory and files by owner, so a
// uid different from the running user's would refuse every write.
func deployerUserUID(env deployerEnv) (int, error) {
	if env.asUser == "" {
		return 0, fmt.Errorf("gt town deployer %s needs --as-user: the deployer credential is made by the deployer user only", env.command)
	}
	if env.getenv(podEnv) != "" {
		return 0, fmt.Errorf("gt town deployer %s refuses inside a pod, where %s is set: the deployer credential is made by the deployer user only", env.command, podEnv)
	}
	u, err := env.current()
	if err != nil {
		return 0, fmt.Errorf("reading the user gt town deployer %s runs as: %w", env.command, err)
	}
	if u.Username != env.asUser {
		return 0, fmt.Errorf("gt town deployer %s runs as %q, not --as-user %q: the deployer credential is made by the deployer user only", env.command, u.Username, env.asUser)
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return 0, fmt.Errorf("the uid %q of user %q is not a number, so the credential's owner cannot be checked: %w", u.Uid, u.Username, err)
	}
	return uid, nil
}

// townDeployerCSREnv is what `gt town deployer csr` reads from its session.
type townDeployerCSREnv struct {
	deployerEnv
	name   string
	days   int
	dir    string
	outCSR string
}

func runTownDeployerCSR(cmd *cobra.Command, _ []string) error {
	asUser, err := cmd.Flags().GetString("as-user")
	if err != nil {
		return fmt.Errorf("reading --as-user: %w", err)
	}
	name, err := cmd.Flags().GetString("name")
	if err != nil {
		return fmt.Errorf("reading --name: %w", err)
	}
	days, err := cmd.Flags().GetInt("days")
	if err != nil {
		return fmt.Errorf("reading --days: %w", err)
	}
	dir, err := cmd.Flags().GetString("dir")
	if err != nil {
		return fmt.Errorf("reading --dir: %w", err)
	}
	outCSR, err := cmd.Flags().GetString("out-csr")
	if err != nil {
		return fmt.Errorf("reading --out-csr: %w", err)
	}
	return runTownDeployerCSRIn(townDeployerCSREnv{
		deployerEnv: deployerEnv{
			command:  "csr",
			asUser:   asUser,
			getenv:   os.Getenv,
			current:  user.Current,
			fs:       deployercred.Disk{},
			readFile: os.ReadFile,
			write:    os.WriteFile,
			out:      cmd.OutOrStdout(),
		},
		name:   name,
		days:   days,
		dir:    dir,
		outCSR: outCSR,
	})
}

// runTownDeployerCSRIn is what `gt town deployer csr` runs.
func runTownDeployerCSRIn(env townDeployerCSREnv) error {
	uid, err := deployerUserUID(env.deployerEnv)
	if err != nil {
		return err
	}
	if env.days < 1 || env.days > maxRequestedDays {
		return fmt.Errorf("--days %d is not between 1 and %d", env.days, maxRequestedDays)
	}

	id := deployercred.Identity(env.name)
	keyPEM, csrYAML, err := deployercred.NewRequest(id, time.Duration(env.days)*24*time.Hour, rand.Reader)
	if err != nil {
		return fmt.Errorf("making the %s signing request: %w", env.name, err)
	}

	// The key is the only secret here, and Store is the one writer that
	// verifies the directory and the file's mode and owner before the rename.
	if err := deployercred.Store(env.fs, uid, env.dir, map[string][]byte{deployerNamedKey(id): keyPEM}); err != nil {
		return fmt.Errorf("writing the %s key: %w", env.name, err)
	}

	if env.outCSR != "" {
		if err := env.write(env.outCSR, csrYAML, 0o644); err != nil {
			return fmt.Errorf("writing the %s signing request to %s: %w", env.name, env.outCSR, err)
		}
		return nil
	}
	if _, err := env.out.Write(csrYAML); err != nil {
		return fmt.Errorf("printing the %s signing request: %w", env.name, err)
	}
	return nil
}

// deployerNamedKey is the file name Store writes the identity's key under; the
// kubeconfig command references the same path.
func deployerNamedKey(id deployercred.Identity) string {
	return string(id) + ".key"
}

// deployerNamedCert is the file name the signed certificate is stored under;
// the kubeconfig references the same path.
func deployerNamedCert(id deployercred.Identity) string {
	return string(id) + ".crt"
}

// deployerNamedKubeconfig is the file name the identity's kubeconfig is
// written under.
func deployerNamedKubeconfig(id deployercred.Identity) string {
	return string(id) + ".kubeconfig"
}

// townDeployerKubeconfigEnv is what `gt town deployer kubeconfig` reads from its
// session.
type townDeployerKubeconfigEnv struct {
	deployerEnv
	name   string
	dir    string
	server string
	ca     string
	cert   string
}

func runTownDeployerKubeconfig(cmd *cobra.Command, _ []string) error {
	flags := make(map[string]string, 6)
	for _, name := range []string{"as-user", "name", "dir", "server", "ca", "cert"} {
		v, err := cmd.Flags().GetString(name)
		if err != nil {
			return fmt.Errorf("reading --%s: %w", name, err)
		}
		flags[name] = v
	}
	return runTownDeployerKubeconfigIn(townDeployerKubeconfigEnv{
		deployerEnv: deployerEnv{
			command:  "kubeconfig",
			asUser:   flags["as-user"],
			getenv:   os.Getenv,
			current:  user.Current,
			fs:       deployercred.Disk{},
			readFile: os.ReadFile,
			write:    os.WriteFile,
			out:      cmd.OutOrStdout(),
		},
		name:   flags["name"],
		dir:    flags["dir"],
		server: flags["server"],
		ca:     flags["ca"],
		cert:   flags["cert"],
	})
}

// runTownDeployerKubeconfigIn is what `gt town deployer kubeconfig` runs.
func runTownDeployerKubeconfigIn(env townDeployerKubeconfigEnv) error {
	uid, err := deployerUserUID(env.deployerEnv)
	if err != nil {
		return err
	}
	caPEM, err := env.readFile(env.ca)
	if err != nil {
		return fmt.Errorf("reading --ca %s: %w", env.ca, err)
	}
	certPEM, err := env.readFile(env.cert)
	if err != nil {
		return fmt.Errorf("reading --cert %s: %w", env.cert, err)
	}
	if err := checkDeployerCertificate(env.cert, certPEM); err != nil {
		return err
	}

	id := deployercred.Identity(env.name)
	kubeconfig, err := deployercred.Kubeconfig(env.server, caPEM, filepath.Join(env.dir, deployerNamedCert(id)), filepath.Join(env.dir, deployerNamedKey(id)), env.name)
	if err != nil {
		return fmt.Errorf("building the %s kubeconfig: %w", env.name, err)
	}

	// The certificate is stored beside the key it was issued for, so the two
	// paths the kubeconfig names are both in the verified directory.
	files := map[string][]byte{
		deployerNamedCert(id):       certPEM,
		deployerNamedKubeconfig(id): kubeconfig,
	}
	if err := deployercred.Store(env.fs, uid, env.dir, files); err != nil {
		return fmt.Errorf("writing the %s certificate and kubeconfig: %w", env.name, err)
	}
	return nil
}

// checkDeployerCertificate refuses a --cert file with no CERTIFICATE PEM block
// that parses. The bytes are stored as the credential kubectl presents to the
// cluster, so a file the cluster cannot read must not be written as one; the
// error names the file, never its content.
func checkDeployerCertificate(path string, certPEM []byte) error {
	rest := certPEM
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			return fmt.Errorf("--cert %s holds no CERTIFICATE PEM block, so the kubeconfig would name a credential the cluster cannot read", path)
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		if _, err := x509.ParseCertificate(block.Bytes); err != nil {
			return fmt.Errorf("--cert %s holds a CERTIFICATE block that does not parse, so the kubeconfig would name a credential the cluster cannot read: %w", path, err)
		}
		return nil
	}
}

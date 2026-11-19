package cmd

import (
	"crypto/rand"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/user"
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
	// write is os.WriteFile for the non-secret outputs, the CSR and later the
	// kubeconfig; the key goes through deployercred.Store alone.
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
			command: "csr",
			asUser:  asUser,
			getenv:  os.Getenv,
			current: user.Current,
			fs:      deployercred.Disk{},
			write:   os.WriteFile,
			out:     cmd.OutOrStdout(),
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

package doctor

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/url"
	"os"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// AdminKubeconfigCheckName is the name 'gt doctor --check' takes for AdminKubeconfigCheck.
const AdminKubeconfigCheckName = "admin-kubeconfig"

// adminKubeconfigControlContext is the name of the control cluster's admin
// context, so a context with that name counts as the control cluster even when
// its server names another host.
const adminKubeconfigControlContext = "colima-gt-control"

// AdminKubeconfigCheck finds a kubeconfig context that reaches the control
// cluster with a credential stored in the file, which the sealed-admin
// convention forbids: the admin entry is removed and the deployer's kubeconfig
// references its key by path. A context reaches the control cluster when its
// name is the admin context's or its cluster server is a loopback https
// address, the address the control API listens on.
//
// A finding names only the context and user, never a credential value, a
// certificate or a port. Register it only for the named check, because the
// kubeconfig it reads is the Mac's, not the pod's; it never writes.
type AdminKubeconfigCheck struct {
	BaseCheck

	// path is the kubeconfig the check reads.
	path string
}

// NewAdminKubeconfigCheck creates the check over the kubeconfig at path.
func NewAdminKubeconfigCheck(path string) *AdminKubeconfigCheck {
	return &AdminKubeconfigCheck{
		BaseCheck: BaseCheck{
			CheckName:        AdminKubeconfigCheckName,
			CheckDescription: "Find a kubeconfig context that reaches the control cluster with an embedded credential",
			CheckCategory:    CategoryConfig,
		},
		path: path,
	}
}

// Run reports an error for each control-cluster context whose user carries an
// embedded credential.
func (c *AdminKubeconfigCheck) Run(ctx *CheckContext) *CheckResult {
	if strings.TrimSpace(c.path) == "" {
		return c.result(StatusError, "No kubeconfig path was given, so no file was checked", nil,
			"Pass the kubeconfig path to the check")
	}
	data, err := os.ReadFile(c.path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		// No file was read, so nothing about the convention was proved: a
		// wrong path must not look like a clean town.
		return c.result(StatusSkipped, "No kubeconfig exists at "+c.path+", so no file was checked", nil,
			"Pass the path of the kubeconfig the convention covers, then re-run the check")
	case err != nil:
		return c.result(StatusError, "Cannot read the kubeconfig "+c.path, []string{err.Error()},
			"Make the kubeconfig a readable regular file and re-run the check")
	}

	findings, problem := adminKubeconfigFindings(data)
	if problem != "" {
		return c.result(StatusError, "Cannot parse the kubeconfig "+c.path, []string{problem},
			"Repair the kubeconfig so it parses as YAML, then re-run the check")
	}
	if len(findings) > 0 {
		return c.result(StatusError,
			fmt.Sprintf("%d kubeconfig context(s) reach the control cluster with an embedded credential", len(findings)),
			findings,
			"Remove the credential from each entry or remove the entry; the deployer's kubeconfig references its key by path")
	}
	return c.result(StatusOK, "No kubeconfig context reaches the control cluster with an embedded credential", nil, "")
}

func (c *AdminKubeconfigCheck) result(status CheckStatus, msg string, details []string, hint string) *CheckResult {
	return &CheckResult{
		Name:     c.Name(),
		Status:   status,
		Message:  msg,
		Details:  details,
		FixHint:  hint,
		Category: c.Category(),
	}
}

// adminKubeconfig holds the kubeconfig fields the check reads. Every other
// field, and any extension the file carries, is ignored.
type adminKubeconfig struct {
	Clusters []adminKubeconfigCluster `yaml:"clusters"`
	Contexts []adminKubeconfigContext `yaml:"contexts"`
	Users    []adminKubeconfigUser    `yaml:"users"`
}

type adminKubeconfigCluster struct {
	Name    string                     `yaml:"name"`
	Cluster adminKubeconfigClusterData `yaml:"cluster"`
}

type adminKubeconfigClusterData struct {
	Server string `yaml:"server"`
}

type adminKubeconfigContext struct {
	Name    string                     `yaml:"name"`
	Context adminKubeconfigContextData `yaml:"context"`
}

type adminKubeconfigContextData struct {
	Cluster string `yaml:"cluster"`
	User    string `yaml:"user"`
}

// adminKubeconfigUser holds the user record's fields by name, so presence
// alone decides the answer and no field value is copied into a result. The
// yaml library applies merge keys to a map, so a credential inherited through
// `<<` is present.
type adminKubeconfigUser struct {
	Name string               `yaml:"name"`
	User map[string]yaml.Node `yaml:"user"`
}

// adminKubeconfigFindings returns one line per control-cluster context whose
// user carries an embedded credential, and the problem that stopped a parse
// (empty when the file parsed).
func adminKubeconfigFindings(data []byte) (findings []string, problem string) {
	var cfg adminKubeconfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, adminKubeconfigParseProblem(err)
	}

	// A name may repeat, so a context is matched to every record with its name
	// and a credential on any of them is reported.
	servers := make(map[string][]string, len(cfg.Clusters))
	for _, cluster := range cfg.Clusters {
		servers[cluster.Name] = append(servers[cluster.Name], cluster.Cluster.Server)
	}
	users := make(map[string][]map[string]yaml.Node, len(cfg.Users))
	for _, user := range cfg.Users {
		users[user.Name] = append(users[user.Name], user.User)
	}

	for _, context := range cfg.Contexts {
		if context.Context.User == "" {
			// A context with no user stores no credential.
			continue
		}
		if !adminKubeconfigReachesControl(context, servers[context.Context.Cluster]) {
			continue
		}
		for _, user := range users[context.Context.User] {
			if kind := adminKubeconfigEmbeddedCredential(user); kind != "" {
				findings = append(findings, fmt.Sprintf("context %q uses user %q, which carries %s",
					context.Name, context.Context.User, kind))
			}
		}
	}
	return findings, ""
}

// adminKubeconfigReachesControl reports whether a context names the control
// cluster or points at a loopback https server, the address the control API
// listens on.
func adminKubeconfigReachesControl(context adminKubeconfigContext, servers []string) bool {
	if context.Name == adminKubeconfigControlContext {
		return true
	}
	for _, server := range servers {
		if adminKubeconfigLoopbackHTTPS(server) {
			return true
		}
	}
	return false
}

// adminKubeconfigLoopbackHTTPS reports whether server is an https URL whose
// host is a loopback name or address.
func adminKubeconfigLoopbackHTTPS(server string) bool {
	u, err := url.Parse(server)
	if err != nil || !strings.EqualFold(u.Scheme, "https") {
		return false
	}
	host := strings.TrimSuffix(u.Hostname(), ".")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// adminKubeconfigEmbeddedCredential names the credential forms a user record
// carries, or returns "" when it carries none. Only the field names are read:
// a credential field present with a null or empty value still counts, because
// the convention removes the field, not only its bytes.
func adminKubeconfigEmbeddedCredential(user map[string]yaml.Node) string {
	_, hasCertificate := user["client-certificate-data"]
	_, hasKey := user["client-key-data"]
	_, hasToken := user["token"]
	_, hasExec := user["exec"]

	var forms []string
	if hasCertificate || hasKey {
		forms = append(forms, "an embedded client certificate")
	}
	if hasToken {
		forms = append(forms, "an embedded token")
	}
	if hasExec {
		forms = append(forms, "an exec credential command")
	}
	return strings.Join(forms, " and ")
}

var (
	// adminKubeconfigTypeErrorLineRE matches the line prefix yaml puts at the
	// start of each entry of a *yaml.TypeError. The prefix is yaml's own text,
	// so a line number cannot come from the quoted scalar that follows it.
	adminKubeconfigTypeErrorLineRE = regexp.MustCompile(`^line (\d+):`)
	// adminKubeconfigSyntaxLineRE matches the prefix of a yaml parser error.
	adminKubeconfigSyntaxLineRE = regexp.MustCompile(`^yaml: line (\d+):`)
)

// adminKubeconfigParseProblem says why the file did not parse without echoing
// the document. A yaml type error quotes a scalar, which can be a token, so
// only the line numbers yaml itself wrote survive; a file naming a bad field
// reads as invalid YAML at that line, never as a clean file.
func adminKubeconfigParseProblem(err error) string {
	var lines []string
	var typeErr *yaml.TypeError
	if errors.As(err, &typeErr) {
		for _, entry := range typeErr.Errors {
			if m := adminKubeconfigTypeErrorLineRE.FindStringSubmatch(entry); m != nil {
				lines = append(lines, m[1])
			}
		}
	} else if m := adminKubeconfigSyntaxLineRE.FindStringSubmatch(err.Error()); m != nil {
		lines = append(lines, m[1])
	}
	if len(lines) == 0 {
		return "the file is not valid YAML"
	}
	return "the file is not valid YAML at line " + strings.Join(lines, ", ")
}

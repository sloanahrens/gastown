package cmd

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"text/template"

	"github.com/spf13/cobra"
)

// forwardLoopbackBind is the only address the forward listens on. The dashboard
// is reached from the Mac itself, so a wider bind would expose it to the network.
const forwardLoopbackBind = "127.0.0.1"

// forwardPATH is the PATH the job runs under. launchd gives a job only the system
// directories, which do not hold a Homebrew kubectl.
const forwardPATH = "/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"

// forwardDNSLabel is an RFC 1123 label. The Service and namespace names reach an
// argv element and a launchd label, so they are checked against it first.
var forwardDNSLabel = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

// forwardXMLEscaper escapes the characters that are special in an XML text node.
// text/template does not escape XML, so each value passes through it.
var forwardXMLEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;")

// forwardPlistTemplate is the job's property list.
const forwardPlistTemplate = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>{{xmlText .Label}}</string>
	<key>ProgramArguments</key>
	<array>
{{- range .Args}}
		<string>{{xmlText .}}</string>
{{- end}}
	</array>
	<key>EnvironmentVariables</key>
	<dict>
		<key>PATH</key>
		<string>{{xmlText .PATH}}</string>
	</dict>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
	<key>ThrottleInterval</key>
	<integer>10</integer>
	<key>StandardOutPath</key>
	<string>{{xmlText .LogOut}}</string>
	<key>StandardErrorPath</key>
	<string>{{xmlText .LogErr}}</string>
</dict>
</plist>
`

var forwardPlistTmpl = template.Must(template.New("plist").
	Funcs(template.FuncMap{"xmlText": forwardXMLEscaper.Replace}).
	Parse(forwardPlistTemplate))

var townForwardCmd = &cobra.Command{
	Use:   "forward",
	Short: "Port-forward a control-cluster Service to a loopback port on the Mac",
	RunE:  requireSubcommand,
}

// forwardPlistFlags are the flags of `gt town forward plist`.
type forwardPlistFlags struct {
	service   string
	context   string
	namespace string
	bind      string
	port      int
}

// forwardPlistView is the data the property list template reads.
type forwardPlistView struct {
	Label  string
	Args   []string
	PATH   string
	LogOut string
	LogErr string
}

func init() {
	townForwardCmd.AddCommand(newForwardPlistCmd(os.UserHomeDir))
	townCmd.AddCommand(townForwardCmd)
}

// newForwardPlistCmd builds `plist`, which prints the launchd job and loads
// nothing. home supplies the directory the job's logs go under; a test passes its own.
func newForwardPlistCmd(home func() (string, error)) *cobra.Command {
	var f forwardPlistFlags
	c := &cobra.Command{
		Use:   "plist",
		Short: "Print the launchd job that port-forwards a Service to a loopback port",
		Long: `Print a launchd property list for a job that runs kubectl port-forward of
--service to 127.0.0.1:--port, restarting it if it exits. The job logs under
~/gt/daemon.

The command only prints. The plist and the dashboard URL go to stdout and stderr;
nothing is loaded into launchd and no cluster is contacted.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := f.validate(); err != nil {
				return err
			}
			dir, err := home()
			if err != nil {
				return fmt.Errorf("HOME cannot be found, so the job's log paths cannot be set: %w", err)
			}
			if err = writeForwardPlist(cmd.OutOrStdout(), dir, f); err != nil {
				return err
			}
			if _, err = fmt.Fprintf(cmd.ErrOrStderr(), "dashboard URL: http://%s:%d/\n", f.bind, f.port); err != nil {
				return fmt.Errorf("writing the URL: %w", err)
			}
			return nil
		},
	}
	c.Flags().StringVar(&f.service, "service", "",
		"the Service to forward, a DNS label (required)")
	c.Flags().StringVar(&f.context, "context", "",
		"the kube context the forward runs against (required; a shell default is not trusted)")
	c.Flags().StringVar(&f.namespace, "namespace", "gt",
		"the namespace the Service lives in, a DNS label")
	c.Flags().StringVar(&f.bind, "bind", forwardLoopbackBind,
		"the address the forward listens on; only 127.0.0.1 is accepted")
	c.Flags().IntVar(&f.port, "port", 0,
		"the local and the Service port, 1 to 65535 (required)")
	return c
}

// validate refuses a flag the job cannot run with, before anything is printed.
func (f forwardPlistFlags) validate() error {
	switch {
	case !forwardDNSLabel.MatchString(f.service):
		return fmt.Errorf("--service %q is not a DNS label: use the Service's name, lowercase letters, digits and hyphens", f.service)
	case strings.TrimSpace(f.context) == "":
		return errors.New("--context is required: name the kube context; a shell default context is not trusted")
	case strings.HasPrefix(f.context, "-"):
		return fmt.Errorf("--context %q starts with -, so it would read as a flag", f.context)
	case !forwardDNSLabel.MatchString(f.namespace):
		return fmt.Errorf("--namespace %q is not a DNS label", f.namespace)
	case f.bind != forwardLoopbackBind:
		return fmt.Errorf("--bind %q is refused: the forward listens on %s only, so the dashboard is not exposed beyond the Mac", f.bind, forwardLoopbackBind)
	case f.port < 1 || f.port > 65535:
		return fmt.Errorf("--port %d is outside 1 to 65535", f.port)
	}
	return nil
}

// writeForwardPlist writes the property list for f to w, with its logs under home.
// The template writes straight to w, so a failed write surfaces here.
func writeForwardPlist(w io.Writer, home string, f forwardPlistFlags) error {
	logBase := filepath.Join(home, "gt", "daemon", "forward-"+f.service)
	view := forwardPlistView{
		Label: "com.gastown.forward-" + f.service,
		Args: []string{
			"kubectl",
			"--context=" + f.context,
			"--namespace=" + f.namespace,
			"port-forward",
			"--address=" + f.bind,
			"service/" + f.service,
			fmt.Sprintf("%d:%d", f.port, f.port),
		},
		PATH:   forwardPATH,
		LogOut: logBase + ".log",
		LogErr: logBase + ".err.log",
	}
	if err := forwardPlistTmpl.Execute(w, view); err != nil {
		return fmt.Errorf("writing the plist: %w", err)
	}
	return nil
}

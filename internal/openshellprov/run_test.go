package openshellprov

import (
	"bytes"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
)

const (
	testConfig = "/cfg/gt"
	// renderedForgejo is the forgejo-polecat profile gt renders for the gateway
	// in these tests, and dockerForgejo the file as the repository names it.
	renderedForgejo = "id: forgejo-polecat\nendpoints:\n  - host: control-plane.openshell.svc.cluster.local\n    port: 3000\n    allowed_ips: [\"192.168.5.2/32\"]\n"
	dockerForgejo   = "id: forgejo-polecat\nendpoints:\n  - host: host.docker.internal\n    port: 3000\n"
)

// gateway is OpenShell and gt as one Runner. It holds the profiles and providers
// the real gateway would list, keeps the content each profile was imported or
// updated with, and records every call. Like the real gateway it refuses to
// import a profile it already lists, and it refuses an update whose file has no
// resource_version, so a run that writes where it should not fails the test.
type gateway struct {
	files      *memFiles
	profiles   map[string]string
	providers  []string
	calls      []string
	refuseGt   bool
	failCreate string
	// verdict, when set, is gt's answer to every --check, in place of a judgement.
	verdict string
}

func newGateway(files *memFiles) *gateway {
	return &gateway{files: files, profiles: map[string]string{}}
}

func (g *gateway) Run(name string, args []string, env []string, _ io.Reader) ([]byte, []byte, error) {
	g.calls = append(g.calls, strings.Join(append([]string{name}, args...), " "))
	if name == "gt" {
		return g.gt(args)
	}
	if len(args) >= 2 && args[0] == "-g" {
		args = args[2:]
	}
	switch cmd := strings.Join(args, " "); {
	case cmd == "provider list":
		return []byte("NAME TYPE\n" + strings.Join(g.providers, "\n") + "\n"), nil, nil
	case strings.HasPrefix(cmd, "provider create "):
		return g.createProvider(args, env)
	case cmd == "profile list --global":
		var b strings.Builder
		b.WriteString("NAME TYPE\n")
		for id := range g.profiles {
			b.WriteString(id + "\n")
		}
		return []byte(b.String()), nil, nil
	case strings.HasPrefix(cmd, "profile lint "):
		return nil, nil, nil
	case strings.HasPrefix(cmd, "profile import "):
		return g.importProfile(args)
	case strings.HasPrefix(cmd, "profile export --global -- "):
		id := args[len(args)-1]
		held, ok := g.profiles[id]
		if !ok {
			return nil, []byte("Error: no custom profile " + id), errors.New("exit status 1")
		}
		return []byte("resource_version: 7\n" + held), nil, nil
	case strings.HasPrefix(cmd, "profile update "):
		return g.updateProfile(args)
	}
	return nil, []byte("unhandled: " + strings.Join(args, " ")), errors.New("exit status 1")
}

// gt answers the endpoint check with nothing, renders every forgejo profile as
// renderedForgejo (stamped with version 7 when it is rendered from an export), and
// judges an exported profile bound when it is renderedForgejo less its versions.
func (g *gateway) gt(args []string) ([]byte, []byte, error) {
	if g.refuseGt {
		reason := "Error: sandbox endpoint: no sandbox profile names gateway \"" + flagValue(args, "--gateway") + "\"\n"
		return nil, []byte(reason), errors.New("exit status 1")
	}
	switch {
	case slices.Contains(args, "--render"):
		out := renderedForgejo
		if slices.Contains(args, "--from-export") {
			out += "resource_version: 7\n"
		}
		return []byte(out), nil, nil
	case slices.Contains(args, "--check"):
		if g.verdict != "" {
			return []byte(g.verdict), nil, nil
		}
		exported := g.body(flagValue(args, "--check"))
		if withoutVersions(exported) == renderedForgejo {
			return []byte("bound\n"), nil, nil
		}
		return []byte("stale: the profile binds another endpoint\n"), nil, nil
	}
	return nil, nil, nil
}

func (g *gateway) importProfile(args []string) ([]byte, []byte, error) {
	id := ProfileID([]byte(g.body(flagValue(args, "-f"))))
	if _, ok := g.profiles[id]; ok {
		return nil, []byte("provider profile import failed: custom provider profile '" + id + "' already exists"), errors.New("exit status 1")
	}
	g.profiles[id] = g.body(flagValue(args, "-f"))
	return []byte("imported profile " + id + "\n"), nil, nil
}

func (g *gateway) updateProfile(args []string) ([]byte, []byte, error) {
	body := g.body(flagValue(args, "-f"))
	if !hasVersion(body) {
		return nil, []byte("custom provider profile update requires a non-zero resource_version"), errors.New("exit status 1")
	}
	g.profiles[args[len(args)-1]] = body
	return nil, nil, nil
}

func (g *gateway) createProvider(args []string, env []string) ([]byte, []byte, error) {
	name := flagValue(args, "--name")
	if name == g.failCreate {
		return nil, []byte("provider create failed"), errors.New("exit status 1")
	}
	secret := envValue(env, flagValue(args, "--credential"))
	g.providers = append(g.providers, name)
	return []byte("created provider " + name + " with value=" + secret + "\n"), nil, nil
}

// body is the content of a file a run wrote, or else one it read.
func (g *gateway) body(name string) string {
	if w, ok := g.files.written[name]; ok {
		return w.String()
	}
	return string(g.files.contents[name])
}

// flagValue is the argument after flag, or "" when there is none.
func flagValue(args []string, flag string) string {
	i := slices.Index(args, flag)
	if i < 0 || i+1 >= len(args) {
		return ""
	}
	return args[i+1]
}

// envValue is the value of name in env, or "" when env does not set it.
func envValue(env []string, name string) string {
	for _, e := range env {
		if value, ok := strings.CutPrefix(e, name+"="); ok {
			return value
		}
	}
	return ""
}

// withoutVersions is s less its resource_version lines.
func withoutVersions(s string) string {
	var kept []string
	for _, line := range strings.SplitAfter(s, "\n") {
		if !strings.HasPrefix(line, "resource_version:") {
			kept = append(kept, line)
		}
	}
	return strings.Join(kept, "")
}

// hasVersion reports whether s carries a non-zero resource_version line.
func hasVersion(s string) bool {
	for _, line := range strings.Split(s, "\n") {
		if v, ok := strings.CutPrefix(line, "resource_version: "); ok && v != "" && v != "0" {
			return true
		}
	}
	return false
}

// newFreshFiles holds the three profiles of a checkout and both key files.
func newFreshFiles() *memFiles {
	files := newMemFiles(
		profileFile{"anthropic-oauth.yaml", "id: anthropic-oauth\n"},
		profileFile{"deepseek-claude.yaml", "id: deepseek-claude\n"},
		profileFile{"forgejo-polecat.yaml", dockerForgejo},
	)
	files.contents[testConfig+"/deepseek.env"] = []byte("DEEPSEEK_API_KEY=" + deepseekSecret + "\n")
	files.contents[testConfig+"/forgejo-polecat.env"] = []byte("FORGEJO_TOKEN=" + forgejoSecret + "\n")
	return files
}

// newGatewayRun builds the run the command makes, over gw and files, for the
// named gateway; "" is the default gateway.
func newGatewayRun(gw *gateway, files *memFiles, gatewayName string) (*Run, *bytes.Buffer, *bytes.Buffer) {
	out, errw := &bytes.Buffer{}, &bytes.Buffer{}
	return &Run{
		Client: Client{Run: gw, Gateway: gatewayName},
		Gt:     Gt{Run: gw, Gateway: gatewayName, Root: testRoot, Err: errw},
		Paths:  Paths{Root: testRoot, Profiles: testProfiles, ConfigDir: testConfig},
		Files:  files,
		Env:    []string{"PATH=/usr/bin"},
		Out:    out,
		Err:    errw,
	}, out, errw
}

// checkNamesGateway asserts every call went to the named gateway: each openshell
// call carries -g NAME and each gt call carries --gateway NAME. "" is the default,
// which takes no -g and names the default gateway to gt.
func checkNamesGateway(t *testing.T, calls []string, gatewayName string) {
	t.Helper()
	for _, c := range calls {
		switch {
		case strings.HasPrefix(c, "gt "):
			want := gatewayName
			if want == "" {
				want = "openshell"
			}
			if !strings.Contains(c, "--gateway "+want+" ") {
				t.Errorf("gt call %q does not name gateway %s", c, want)
			}
		case gatewayName == "" && strings.HasPrefix(c, "openshell -g "):
			t.Errorf("default-gateway call %q passed -g", c)
		case gatewayName != "" && !strings.HasPrefix(c, "openshell -g "+gatewayName+" "):
			t.Errorf("openshell call %q does not name gateway %s", c, gatewayName)
		}
	}
}

// checkNoSecret asserts that neither key appears in the streams or in any call, and
// that provider create's echo of the key reached the terminal masked.
func checkNoSecret(t *testing.T, out, errw *bytes.Buffer, calls []string) {
	t.Helper()
	for _, secret := range []string{deepseekSecret, forgejoSecret} {
		if strings.Contains(out.String(), secret) || strings.Contains(errw.String(), secret) {
			t.Errorf("output carries the secret %q", secret)
		}
		for _, c := range calls {
			if strings.Contains(c, secret) {
				t.Errorf("call %q carries the secret %q", c, secret)
			}
		}
	}
	if !strings.Contains(out.String(), "created provider deepseek with value=***") {
		t.Errorf("stdout = %q, want the masked create line", out.String())
	}
}

// TestRunOrderAndExitCodes is the run's sequence: the endpoint is checked before
// anything is touched, the profiles are imported before the providers are made,
// and a step that fails stops the run with its error.
func TestRunOrderAndExitCodes(t *testing.T) {
	t.Parallel()
	t.Run("a fresh gateway: endpoint, then profiles, then providers", func(t *testing.T) {
		t.Parallel()
		gw := newGateway(newFreshFiles())
		run, out, errw := newGatewayRun(gw, gw.files, "")
		if err := run.Execute(); err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if !strings.Contains(out.String(), "imported profile deepseek-claude\n") {
			t.Errorf("stdout = %q, want openshell's import line passed on", out.String())
		}
		var renders int
		for _, c := range gw.calls {
			if strings.Contains(c, " --render ") {
				renders++
			}
		}
		if renders != 1 {
			t.Errorf("gt rendered the forgejo profile %d times, want once", renders)
		}
		if !strings.HasPrefix(gw.calls[0], "gt sandbox endpoint ") {
			t.Errorf("first call = %q, want the endpoint check", gw.calls[0])
		}
		var imports, creates int
		lastImport, firstCreate := -1, -1
		perProfile := map[string]int{}
		for i, c := range gw.calls {
			switch {
			case strings.Contains(c, " profile import "):
				imports++
				perProfile[flagValue(strings.Fields(c), "-f")]++
				lastImport = i
			case strings.Contains(c, " provider create "):
				creates++
				if firstCreate < 0 {
					firstCreate = i
				}
			}
		}
		if imports != 3 || creates != 2 {
			t.Errorf("imports = %d, creates = %d, want 3 and 2", imports, creates)
		}
		for _, file := range []string{
			testProfiles + "/anthropic-oauth.yaml",
			testProfiles + "/deepseek-claude.yaml",
			testWork + "/forgejo-polecat.yaml",
		} {
			if perProfile[file] != 1 {
				t.Errorf("imported %s %d times, want once", file, perProfile[file])
			}
		}
		if lastImport > firstCreate {
			t.Errorf("a profile import (call %d) came after the first provider create (call %d)", lastImport, firstCreate)
		}
		if !slices.Equal(gw.providers, []string{"deepseek", "forgejo-polecat"}) {
			t.Errorf("providers = %q, want deepseek then forgejo-polecat", gw.providers)
		}
		if got := gw.profiles["forgejo-polecat"]; got != renderedForgejo {
			t.Errorf("forgejo-polecat profile = %q, want gt's rendering %q", got, renderedForgejo)
		}
		checkNamesGateway(t, gw.calls, "")
		checkNoSecret(t, out, errw, gw.calls)
	})

	t.Run("a named gateway: every call names it", func(t *testing.T) {
		t.Parallel()
		gw := newGateway(newFreshFiles())
		run, out, errw := newGatewayRun(gw, gw.files, "k8s")
		if err := run.Execute(); err != nil {
			t.Fatalf("Execute: %v", err)
		}
		checkNamesGateway(t, gw.calls, "k8s")
		checkNoSecret(t, out, errw, gw.calls)
		if !slices.Equal(gw.providers, []string{"deepseek", "forgejo-polecat"}) {
			t.Errorf("providers = %q, want both on k8s", gw.providers)
		}
		if got := gw.profiles["forgejo-polecat"]; got != renderedForgejo {
			t.Errorf("forgejo-polecat profile = %q, want the rendering gt gave for k8s %q", got, renderedForgejo)
		}
		if strings.Contains(gw.profiles["forgejo-polecat"], "host.docker.internal") {
			t.Errorf("forgejo-polecat profile still names the Docker alias")
		}
	})

	t.Run("profiles present, providers missing: no profile is imported", func(t *testing.T) {
		t.Parallel()
		gw := newGateway(newFreshFiles())
		gw.profiles = map[string]string{
			"anthropic-oauth": "id: anthropic-oauth\n",
			"deepseek-claude": "id: deepseek-claude\n",
			"forgejo-polecat": renderedForgejo,
		}
		run, _, _ := newGatewayRun(gw, gw.files, "")
		if err := run.Execute(); err != nil {
			t.Fatalf("Execute: %v", err)
		}
		for _, c := range gw.calls {
			if strings.Contains(c, " profile import ") {
				t.Errorf("call %q imports a profile the gateway already lists", c)
			}
		}
		if !slices.Equal(gw.providers, []string{"deepseek", "forgejo-polecat"}) {
			t.Errorf("providers = %q, want both created", gw.providers)
		}
	})

	t.Run("a gateway gt refuses: no openshell call is made and gt's reason is shown", func(t *testing.T) {
		t.Parallel()
		gw := newGateway(newFreshFiles())
		gw.refuseGt = true
		run, _, errw := newGatewayRun(gw, gw.files, "nope")
		err := run.Execute()
		if err == nil || !strings.Contains(err.Error(), "gt sandbox endpoint --gateway nope failed") {
			t.Fatalf("Execute = %v, want the refusal naming gateway nope", err)
		}
		if !strings.Contains(errw.String(), "no sandbox profile names gateway \"nope\"") {
			t.Errorf("stderr = %q, want gt's own reason", errw.String())
		}
		if strings.Contains(err.Error(), "no sandbox profile") {
			t.Errorf("error = %q carries gt's output, which the error must not", err)
		}
		for _, c := range gw.calls {
			if strings.HasPrefix(c, "openshell") {
				t.Errorf("call %q after gt refused the gateway", c)
			}
		}
	})

	t.Run("a verdict gt cannot read stops the run before any update", func(t *testing.T) {
		t.Parallel()
		gw := newGateway(newFreshFiles())
		gw.profiles["forgejo-polecat"] = dockerForgejo
		gw.verdict = "maybe\n"
		run, _, _ := newGatewayRun(gw, gw.files, "")
		err := run.Execute()
		if err == nil || !strings.Contains(err.Error(), "neither bound nor stale for forgejo-polecat: maybe") {
			t.Fatalf("Execute = %v, want the unreadable verdict named", err)
		}
		for _, c := range gw.calls {
			if strings.Contains(c, " profile update ") || strings.Contains(c, " provider create ") {
				t.Errorf("call %q made after the verdict was unreadable", c)
			}
		}
	})

	t.Run("a failed deepseek create stops the run before forgejo-polecat", func(t *testing.T) {
		t.Parallel()
		gw := newGateway(newFreshFiles())
		gw.failCreate = "deepseek"
		run, _, _ := newGatewayRun(gw, gw.files, "")
		err := run.Execute()
		if err == nil || err.Error() != "openshell-providers: openshell provider create --name deepseek failed" {
			t.Fatalf("Execute = %v, want the failed deepseek create", err)
		}
		for _, c := range gw.calls {
			if strings.Contains(c, "--name forgejo-polecat") {
				t.Errorf("call %q made after deepseek failed", c)
			}
		}
	})
}

// TestRunIsIdempotent runs the import, then twice more: every rerun finds what the
// gateway lists, reports it as existing, and makes no write call, on the default
// gateway and on a named one.
func TestRunIsIdempotent(t *testing.T) {
	t.Parallel()
	for _, gatewayName := range []string{"", "k8s"} {
		t.Run("gateway "+gatewayName, func(t *testing.T) {
			t.Parallel()
			gw := newGateway(newFreshFiles())
			first, _, _ := newGatewayRun(gw, gw.files, gatewayName)
			if err := first.Execute(); err != nil {
				t.Fatalf("first Execute: %v", err)
			}
			for run := 2; run <= 3; run++ {
				gw.calls = nil
				rerun, out, _ := newGatewayRun(gw, gw.files, gatewayName)
				if err := rerun.Execute(); err != nil {
					t.Fatalf("run %d Execute: %v", run, err)
				}
				for _, c := range gw.calls {
					for _, write := range []string{" profile import ", " profile update ", " profile lint ", " provider create "} {
						if strings.Contains(c, write) {
							t.Errorf("run %d made write call %q", run, c)
						}
					}
				}
				for _, want := range []string{
					"openshell-providers: profile anthropic-oauth exists\n",
					"openshell-providers: profile deepseek-claude exists\n",
					"openshell-providers: profile forgejo-polecat exists\n",
					"openshell-providers: provider deepseek exists\n",
					"openshell-providers: provider forgejo-polecat exists\n",
				} {
					if !strings.Contains(out.String(), want) {
						t.Errorf("run %d stdout = %q, want it to contain %q", run, out.String(), want)
					}
				}
			}
		})
	}
}

// TestRunUpdatesAStaleForgejoProfile is the gateway holding a forgejo-polecat that
// binds another host: it is exported, judged stale by gt, rendered with the
// export's version and updated, and it is not imported again.
func TestRunUpdatesAStaleForgejoProfile(t *testing.T) {
	t.Parallel()
	gw := newGateway(newFreshFiles())
	gw.profiles["forgejo-polecat"] = dockerForgejo
	run, out, _ := newGatewayRun(gw, gw.files, "")
	if err := run.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	const stale = "openshell-providers: profile forgejo-polecat is stale (the profile binds another endpoint), updating it\n"
	if !strings.Contains(out.String(), stale) {
		t.Errorf("stdout = %q, want the stale line with gt's reason", out.String())
	}
	if strings.Contains(out.String(), "profile forgejo-polecat exists") {
		t.Errorf("stdout = %q, the stale profile is reported as existing", out.String())
	}
	got := gw.profiles["forgejo-polecat"]
	if !hasVersion(got) || withoutVersions(got) != renderedForgejo {
		t.Errorf("forgejo-polecat profile = %q, want gt's rendering with a resource_version", got)
	}
	if n := strings.Count(got, "resource_version:"); n != 1 {
		t.Errorf("forgejo-polecat profile carries %d resource_version lines, want 1", n)
	}
	if !strings.Contains(got, "resource_version: 7\n") {
		t.Errorf("forgejo-polecat profile = %q, want the gateway's current version 7", got)
	}
	var checked, rendered, updated bool
	for _, c := range gw.calls {
		checked = checked || (strings.Contains(c, " --check ") && strings.HasSuffix(c, "forgejo-polecat.export.yaml"))
		rendered = rendered || (strings.Contains(c, " --render ") && strings.Contains(c, " --from-export "))
		if strings.Contains(c, " profile update --global -f ") && strings.HasSuffix(c, " forgejo-polecat") {
			updated = true
		}
		if strings.Contains(c, " profile import ") && strings.Contains(c, "forgejo") {
			t.Errorf("forgejo-polecat imported again: %q", c)
		}
	}
	if !checked || !rendered || !updated {
		t.Errorf("calls = %q, want gt's check, gt's render from the export, and the update", gw.calls)
	}

	// The updated profile is the gateway's now: a rerun finds it bound and leaves it.
	gw.calls = nil
	rerun, out2, _ := newGatewayRun(gw, gw.files, "")
	if err := rerun.Execute(); err != nil {
		t.Fatalf("rerun Execute: %v", err)
	}
	if !strings.Contains(out2.String(), "openshell-providers: profile forgejo-polecat exists\n") {
		t.Errorf("rerun stdout = %q, want the updated profile reported as existing", out2.String())
	}
	for _, c := range gw.calls {
		if strings.Contains(c, " profile update ") {
			t.Errorf("rerun updated the profile again: %q", c)
		}
	}
}

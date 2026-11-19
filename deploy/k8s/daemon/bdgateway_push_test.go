package daemon

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// testBdGatewayPushURL is the push Service address the push goldens record. It is a
// loopback address: the pusher's trust list widens to the in-cluster Service name in
// a later bead (gt-ozn0j0), and the render only carries the value.
const testBdGatewayPushURL = "http://127.0.0.1:7441"

// bdGatewayPushValues is the gateway workload values with the dashboard on and both
// push URLs set, so one render carries the push Service, the gateway Deployment's
// push port, and the daemon's and dashboard's BD_GATEWAY_PUSH_URL.
func bdGatewayPushValues(t *testing.T) Values {
	t.Helper()
	v := bdGatewayWorkloadValues(t)
	v.Dashboard = true
	v.BdGatewayURL = testBdGatewayURL
	v.BdGatewayPushURL = testBdGatewayPushURL
	v.DaemonBdGatewayURL = "http://127.0.0.1:7440"
	v.DaemonBdGatewayPushURL = testBdGatewayPushURL
	// The push URL renders only with the policy rendered or attested; these
	// fixtures exercise the push render, not the refusal.
	v.BdGatewayServicePolicyApplied = true
	return v
}

// TestBdGatewayPushServiceGolden holds the push Service to its golden and to the
// shape the pusher needs: a ClusterIP Service selecting the gateway's pods, one port
// 7441, and a pod published before its readiness probe passes.
func TestBdGatewayPushServiceGolden(t *testing.T) {
	t.Parallel()
	m := mustBuild(t, bdGatewayWorkloadValues(t))
	compareGolden(t, filepath.Join(packageDir(t), "testdata"), "service-bd-gateway-push.yaml", document(t, m.BdGatewayPushService))

	svc := m.BdGatewayPushService
	if svc.Metadata.Name != BdGatewayPushName {
		t.Errorf("the push Service is named %q, want %q", svc.Metadata.Name, BdGatewayPushName)
	}
	if svc.Spec.Type != "ClusterIP" {
		t.Errorf("the push Service type is %q, want ClusterIP", svc.Spec.Type)
	}
	if want := bdGatewaySelectorLabels(); len(svc.Spec.Selector) != 1 || svc.Spec.Selector["app.kubernetes.io/name"] != want["app.kubernetes.io/name"] {
		t.Errorf("the push Service selects %q, want the gateway's one label pair %q", svc.Spec.Selector, want)
	}
	if len(svc.Spec.Ports) != 1 || svc.Spec.Ports[0].Port != BdGatewayPushPort || svc.Spec.Ports[0].Protocol != "TCP" {
		t.Errorf("the push Service publishes %+v, want one TCP port %d", svc.Spec.Ports, BdGatewayPushPort)
	}
	if !svc.Spec.PublishNotReadyAddresses {
		t.Error("the push Service publishes ready pods only, so a pusher waits out a starting gateway")
	}
}

// TestBdGatewayPushServiceDeployment holds the gateway container's push port and
// flag, and the bd-gateway Service to the render it had before: the exec route keeps
// publishing ready pods only.
func TestBdGatewayPushServiceDeployment(t *testing.T) {
	t.Parallel()
	m := mustBuild(t, bdGatewayWorkloadValues(t))
	c := m.BdGatewayDeployment.Spec.Template.Spec.Containers[0]

	if len(c.Ports) != 1 || c.Ports[0].Name != "push" || c.Ports[0].ContainerPort != BdGatewayPushPort || c.Ports[0].Protocol != "TCP" {
		t.Errorf("the gateway declares ports %+v, want one TCP push port %d", c.Ports, BdGatewayPushPort)
	}
	if !slices.Contains(c.Command, "--push-addr") {
		t.Errorf("the gateway command %q carries no --push-addr", c.Command)
	}
	if i := slices.Index(c.Command, "--push-addr"); i >= 0 && (i+1 >= len(c.Command) || c.Command[i+1] != ":7441") {
		t.Errorf("the gateway command names --push-addr %v, want :7441", c.Command[i+1:])
	}

	compareGolden(t, filepath.Join(packageDir(t), "testdata"), "service-bd-gateway.yaml", document(t, m.BdGatewayService))
	svc := m.BdGatewayService
	if svc.Spec.PublishNotReadyAddresses {
		t.Error("the bd-gateway Service publishes unready pods, so a bd exec reaches a gateway whose workspace is not ready")
	}
	if len(svc.Spec.Ports) != 1 || svc.Spec.Ports[0].Port != BdGatewayPort {
		t.Errorf("the bd-gateway Service publishes %+v, want its one port %d", svc.Spec.Ports, BdGatewayPort)
	}
}

// TestBdGatewayPushServiceEnv holds the two pods' push URL: the daemon and the
// dashboard each carry BD_GATEWAY_PUSH_URL with the value the render was given, once
// per container, and neither carries it when no URL is set. Every refusal row names
// the field it is about.
func TestBdGatewayPushServiceEnv(t *testing.T) {
	t.Parallel()

	t.Run("the daemon and the dashboard carry the push URL", func(t *testing.T) {
		t.Parallel()
		m := mustBuild(t, bdGatewayPushValues(t))
		for _, tc := range []struct {
			name string
			c    Container
		}{
			{"the daemon", m.Deployment.Spec.Template.Spec.Containers[0]},
			{"the dashboard", m.DashboardDeployment.Spec.Template.Spec.Containers[0]},
		} {
			if got := envValuesNamed(tc.c, BdGatewayPushURLEnv); !slices.Equal(got, []string{testBdGatewayPushURL}) {
				t.Errorf("%s names %s = %q, want exactly %q", tc.name, BdGatewayPushURLEnv, got, testBdGatewayPushURL)
			}
		}
		if dup := envNamesNamedTwice(m); len(dup) != 0 {
			t.Errorf("the render names environment variables twice: %q", dup)
		}
	})

	t.Run("neither carries it when unset", func(t *testing.T) {
		t.Parallel()
		v := bdGatewayPushValues(t)
		v.BdGatewayPushURL = ""
		v.DaemonBdGatewayPushURL = ""
		m := mustBuild(t, v)
		for _, tc := range []struct {
			name string
			c    Container
		}{
			{"the daemon", m.Deployment.Spec.Template.Spec.Containers[0]},
			{"the dashboard", m.DashboardDeployment.Spec.Template.Spec.Containers[0]},
		} {
			if got := envValuesNamed(tc.c, BdGatewayPushURLEnv); len(got) != 0 {
				t.Errorf("%s names %s = %q with no push URL set, want none", tc.name, BdGatewayPushURLEnv, got)
			}
		}
	})

	const secret = "push-password-sentinel"
	for _, tc := range []struct {
		name   string
		mutate func(*Values)
		names  string
	}{
		{"the dashboard's push URL without the dashboard", func(v *Values) {
			v.Dashboard = false
			v.BdGatewayURL = ""
			v.DaemonBdGatewayPushURL = ""
		}, "Values.Dashboard"},
		{"the daemon's push URL without the gateway workload", func(v *Values) {
			v.BdGatewayImage = ""
			v.BdGatewayTokenSecret = ""
			v.BdGatewayPushURL = ""
		}, "Values.BdGatewayImage"},
		{"the dashboard's push URL without the gateway workload", func(v *Values) {
			v.BdGatewayImage = ""
			v.BdGatewayTokenSecret = ""
			v.DaemonBdGatewayPushURL = ""
		}, "Values.BdGatewayImage"},
		{"a non-http push URL", func(v *Values) { v.BdGatewayPushURL = "ftp://host:7441" }, "Values.BdGatewayPushURL"},
		{"a push URL with user information", func(v *Values) {
			v.BdGatewayPushURL = "http://gt:" + secret + "@host:7441"
		}, "Values.BdGatewayPushURL"},
		{"a daemon push URL with no scheme", func(v *Values) { v.DaemonBdGatewayPushURL = "host:7441" }, "Values.DaemonBdGatewayPushURL"},
		{"a daemon push URL in a rehearsal", func(v *Values) {
			*v = rehearsalValues(t)
			v.DaemonBdGatewayPushURL = testBdGatewayPushURL
		}, "Values.DaemonBdGatewayPushURL"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			v := bdGatewayPushValues(t)
			tc.mutate(&v)
			m, err := Build(v)
			if err == nil {
				t.Fatalf("Build accepted %s, want a refusal naming %s", tc.name, tc.names)
			}
			if !strings.Contains(err.Error(), tc.names) {
				t.Errorf("the refusal %q does not name %s", err, tc.names)
			}
			if strings.Contains(err.Error(), secret) {
				t.Errorf("the refusal %q echoes the credentials", err)
			}
			if m.BdGatewayPushService.Kind != "" {
				t.Errorf("a refused Build returned a push Service")
			}
		})
	}
}

// TestPushPolicyAttestRefuses holds the push render's refusal: a dashboard push
// URL and a daemon push URL are each refused when neither Values.IncludeStatic
// nor Values.BdGatewayServicePolicyApplied is set, and the message names both
// fields and the policy file, so the operator hears both ways out.
func TestPushPolicyAttestRefuses(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		mutate func(*Values)
		field  string
	}{
		{
			name:   "the dashboard's push URL",
			mutate: func(v *Values) { v.DaemonBdGatewayPushURL = "" },
			field:  "Values.BdGatewayPushURL",
		},
		{
			name:   "the daemon's push URL",
			mutate: func(v *Values) { v.BdGatewayPushURL = "" },
			field:  "Values.DaemonBdGatewayPushURL",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			v := bdGatewayPushValues(t)
			v.BdGatewayServicePolicyApplied = false
			tc.mutate(&v)
			m, err := Build(v)
			if err == nil {
				t.Fatalf("Build accepted %s with neither Values.IncludeStatic nor Values.BdGatewayServicePolicyApplied, want a refusal", tc.name)
			}
			for _, want := range []string{
				tc.field,
				"BdGatewayServicePolicyApplied",
				"IncludeStatic",
				"bd-gateway-service-admission.yaml",
			} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal %q does not name %s", err, want)
				}
			}
			if m.BdGatewayPushService.Kind != "" {
				t.Errorf("a refused Build returned a push Service")
			}
		})
	}
}

// TestPushPolicyAttestAdmits holds the two ways out: each push URL renders with
// Values.IncludeStatic and with Values.BdGatewayServicePolicyApplied, and a values
// file with no push URL renders in all four flag combinations, with the flag
// alone changing nothing.
func TestPushPolicyAttestAdmits(t *testing.T) {
	t.Parallel()

	// pushEnvIs asserts the named container carries exactly the fixture's push URL,
	// so a render that dropped the URL cannot pass as an admit.
	pushEnvIs := func(t *testing.T, name string, c Container) {
		t.Helper()
		if got := envValuesNamed(c, BdGatewayPushURLEnv); !slices.Equal(got, []string{testBdGatewayPushURL}) {
			t.Errorf("%s names %s = %q, want exactly %q", name, BdGatewayPushURLEnv, got, testBdGatewayPushURL)
		}
	}

	for _, tc := range []struct {
		name   string
		mutate func(*Values)
		check  func(t *testing.T, m Manifests)
	}{
		{
			name: "the dashboard's push URL with IncludeStatic",
			mutate: func(v *Values) {
				v.DaemonBdGatewayPushURL = ""
				v.IncludeStatic = true
				v.BdGatewayServicePolicyApplied = false
			},
			check: func(t *testing.T, m Manifests) {
				pushEnvIs(t, "the dashboard", m.DashboardDeployment.Spec.Template.Spec.Containers[0])
			},
		},
		{
			name: "the dashboard's push URL attested",
			mutate: func(v *Values) {
				v.DaemonBdGatewayPushURL = ""
				v.IncludeStatic = false
				v.BdGatewayServicePolicyApplied = true
			},
			check: func(t *testing.T, m Manifests) {
				pushEnvIs(t, "the dashboard", m.DashboardDeployment.Spec.Template.Spec.Containers[0])
			},
		},
		{
			name: "the daemon's push URL with IncludeStatic",
			mutate: func(v *Values) {
				v.BdGatewayPushURL = ""
				v.IncludeStatic = true
				v.BdGatewayServicePolicyApplied = false
			},
			check: func(t *testing.T, m Manifests) {
				pushEnvIs(t, "the daemon", m.Deployment.Spec.Template.Spec.Containers[0])
			},
		},
		{
			name: "the daemon's push URL attested",
			mutate: func(v *Values) {
				v.BdGatewayPushURL = ""
				v.IncludeStatic = false
				v.BdGatewayServicePolicyApplied = true
			},
			check: func(t *testing.T, m Manifests) {
				pushEnvIs(t, "the daemon", m.Deployment.Spec.Template.Spec.Containers[0])
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			v := bdGatewayPushValues(t)
			tc.mutate(&v)
			tc.check(t, mustBuild(t, v))
		})
	}

	t.Run("a values file with no push URL renders in all four flag combinations", func(t *testing.T) {
		t.Parallel()
		base := bdGatewayPushValues(t)
		base.BdGatewayPushURL = ""
		base.DaemonBdGatewayPushURL = ""
		var withoutStatic []byte
		for _, tc := range []struct {
			name    string
			include bool
			applied bool
		}{
			{"neither flag", false, false},
			{"IncludeStatic", true, false},
			{"BdGatewayServicePolicyApplied", false, true},
			{"both flags", true, true},
		} {
			v := base
			v.IncludeStatic = tc.include
			v.BdGatewayServicePolicyApplied = tc.applied
			m := mustBuild(t, v)
			for _, container := range []struct {
				name string
				c    Container
			}{
				{"the daemon", m.Deployment.Spec.Template.Spec.Containers[0]},
				{"the dashboard", m.DashboardDeployment.Spec.Template.Spec.Containers[0]},
			} {
				if got := envValuesNamed(container.c, BdGatewayPushURLEnv); len(got) != 0 {
					t.Errorf("%s with %s names %s = %q, want none with no push URL set", tc.name, container.name, BdGatewayPushURLEnv, got)
				}
			}
			if tc.include {
				continue
			}
			var buf bytes.Buffer
			if err := Render(&buf, m); err != nil {
				t.Fatalf("rendering with %s: %v", tc.name, err)
			}
			if withoutStatic == nil {
				withoutStatic = buf.Bytes()
				continue
			}
			if !bytes.Equal(withoutStatic, buf.Bytes()) {
				t.Errorf("the render with %s differs from the render with neither flag, though no push URL is set", tc.name)
			}
		}
	})
}

// TestPushPolicyAttestDocumented holds the attestation's contract to its word: the
// field's doc comment in daemon.go says it is an attestation, and the architecture
// page carries the refusal beside the paragraph that names the push Service.
func TestPushPolicyAttestDocumented(t *testing.T) {
	t.Parallel()

	t.Run("the field doc says it is an attestation", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(packageDir(t), "daemon.go")
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ParseComments)
		if err != nil {
			t.Fatalf("parsing %s: %v", path, err)
		}
		var doc string
		ast.Inspect(file, func(n ast.Node) bool {
			spec, ok := n.(*ast.TypeSpec)
			if !ok || spec.Name.Name != "Values" {
				return true
			}
			structType, ok := spec.Type.(*ast.StructType)
			if !ok {
				return false
			}
			for _, field := range structType.Fields.List {
				for _, name := range field.Names {
					if name.Name == "BdGatewayServicePolicyApplied" && field.Doc != nil {
						doc = field.Doc.Text()
					}
				}
			}
			return false
		})
		if doc == "" {
			t.Fatal("daemon.go has no doc comment on Values.BdGatewayServicePolicyApplied")
		}
		if !strings.Contains(strings.ToLower(doc), "attestation") {
			t.Errorf("the Values.BdGatewayServicePolicyApplied doc %q does not say it is an attestation", doc)
		}
	})

	t.Run("the architecture page carries the refusal", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(packageDir(t), "..", "..", "..", "docs", "architecture.md")
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		const want = "A push URL is refused at render unless the policy is in the same render or the values file attests it is applied"
		if !strings.Contains(string(body), want) {
			t.Errorf("docs/architecture.md does not carry the refusal sentence %q", want)
		}
	})
}

package bdgateway

import (
	"strings"
	"testing"
)

// TestLocalGatewayHostInClusterService: a plain http gateway URL may name this
// machine or the gateway's own in-cluster Service, and nothing else in the
// cluster: another Service, or the gateway's name in another namespace, would
// receive the bearer token in the clear.
func TestLocalGatewayHostInClusterService(t *testing.T) {
	t.Parallel()
	cases := []struct {
		host string
		want bool
	}{
		{"bd-gateway.gt.svc", true},
		{"BD-Gateway.GT.SVC", true},
		{"bd-gateway.gt.svc.cluster.local", true},
		{"Bd-Gateway.Gt.Svc.Cluster.Local", true},
		{"localhost", true},
		{"host.docker.internal", true},
		{"127.0.0.1", true},
		{"::1", true},
		{"bd-gateway.openshell.svc", false},
		{"bd-gateway.other.svc", false},
		{"bd-gateway.gt-rehearsal.svc", false},
		{"bd-gateway.openshell.svc.cluster.local", false},
		{"dolt.gt.svc", false},
		{"forgejo.gt.svc", false},
		{"tracker.openshell.svc", false},
		{"dolt.gt.svc.cluster.local", false},
		{"bd-gateway-svc.gt.svc", false},
		{"evil-bd-gateway.gt.svc", false},
		{"svc", false},
		{"gt.svc", false},
		{"bd-gateway.svcx.example", false},
		{"a.svc.evil.com", false},
		{"bd-gateway.gt.svc.evil.com", false},
		{"bd-gateway.gt.svc.cluster.local.evil.com", false},
		{"bd-gateway.gt.svc.cluster", false},
		{"bd-gateway..svc", false},
		{".gt.svc", false},
		{"bd_gateway.gt.svc", false},
		{"-bd-gateway.gt.svc", false},
		{"bd-gateway-.gt.svc", false},
		{"a.b.gt.svc", false},
		{"bd-gateway.example.com", false},
		{"10.0.0.7", false},
		{"192.168.5.2", false},
		{"", false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.host, func(t *testing.T) {
			t.Parallel()
			if got := localGatewayHost(tc.host); got != tc.want {
				t.Errorf("localGatewayHost(%q) = %v, want %v", tc.host, got, tc.want)
			}
		})
	}
}

// TestLocalGatewayHostEndpoint: the pusher's endpoint takes the gateway's
// in-cluster Service URL over plain http, and refuses an external host, another
// Service in gt and the gateway's name in another namespace, naming the
// gateway's Service form in the refusal.
func TestLocalGatewayHostEndpoint(t *testing.T) {
	t.Parallel()
	got, err := GatewayPusher{URL: "http://bd-gateway.gt.svc:7440"}.endpoint("/v1/workspace")
	if err != nil {
		t.Fatalf("endpoint(the gateway's in-cluster Service) = %v, want nil", err)
	}
	if want := "http://bd-gateway.gt.svc:7440/v1/workspace"; got != want {
		t.Errorf("endpoint = %q, want %q", got, want)
	}
	for _, url := range []string{
		"http://bd-gateway.example.com:7440",
		"http://dolt.gt.svc:7440",
		"http://bd-gateway.openshell.svc:7440",
	} {
		_, err = GatewayPusher{URL: url}.endpoint("/v1/workspace")
		if err == nil {
			t.Errorf("endpoint(%s over plain http) = nil, want a refusal", url)
			continue
		}
		if !strings.Contains(err.Error(), "bd-gateway.gt.svc") {
			t.Errorf("refusal for %s = %q, want it to name the gateway's Service form", url, err)
		}
	}
}

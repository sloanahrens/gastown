package bdgateway

import (
	"strings"
	"testing"
)

// TestLocalGatewayHostInClusterService: a plain http gateway URL may name this
// machine or an in-cluster Service, and nothing that only looks like one.
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

// TestLocalGatewayHostEndpoint: the pusher's endpoint takes the in-cluster
// Service URL over plain http and still refuses an external host, naming the
// in-cluster form in the refusal.
func TestLocalGatewayHostEndpoint(t *testing.T) {
	t.Parallel()
	got, err := GatewayPusher{URL: "http://bd-gateway.gt.svc:7440"}.endpoint("/v1/workspace")
	if err != nil {
		t.Fatalf("endpoint(in-cluster Service) = %v, want nil", err)
	}
	if want := "http://bd-gateway.gt.svc:7440/v1/workspace"; got != want {
		t.Errorf("endpoint = %q, want %q", got, want)
	}
	_, err = GatewayPusher{URL: "http://bd-gateway.example.com:7440"}.endpoint("/v1/workspace")
	if err == nil {
		t.Fatal("endpoint(external host over plain http) = nil, want a refusal")
	}
	if !strings.Contains(err.Error(), "in-cluster Service name") {
		t.Errorf("refusal = %q, want it to name an in-cluster Service name", err)
	}
}

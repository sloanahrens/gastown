//go:build integration

package makegate

import (
	"strings"
	"testing"
)

// TestIntegrationCIBaseImagePassesThePin checks the image build's seam: the
// recipe hands docker the version `make golangci-lint-version` prints, so the
// image the gate runs on and the gate itself cannot install and require
// different versions (gt-zzo1g2). Mutation: pass a literal, or drop the build
// arg, and the image can drift from the pin `make lint` refuses.
func TestIntegrationCIBaseImagePassesThePin(t *testing.T) {
	g := newGateSandbox(t)
	r := g.runTarget("ci-base-image", nil, "CI_BASE_IMAGE=ci-base:test")
	if r.Code != 0 {
		t.Fatalf("make ci-base-image exit %d, want 0\nstdout:\n%s\nstderr:\n%s\ncalls:\n%s", r.Code, r.Stdout, r.Stderr, r.callLog())
	}
	want := "docker build --build-arg CI_BASE_IMAGE=ci-base:test --build-arg GOLANGCI_LINT_VERSION=" +
		golangciPin(t) + " -t ci-base:gate-tools deploy/ci-base"
	if !r.hasCall(want) {
		t.Fatalf("make ci-base-image ran:\n%s\nwant:\n%s", r.callLog(), want)
	}
}

// TestIntegrationCIBaseImageRefusesAnEmptyBase checks that the recipe names the
// missing base and starts no build, which is what keeps an image from being
// tagged for the gate without extending a runner image (gt-zzo1g2).
// Mutation: default the base and the build runs against whatever the default
// names.
func TestIntegrationCIBaseImageRefusesAnEmptyBase(t *testing.T) {
	g := newGateSandbox(t)
	r := g.runTarget("ci-base-image", nil, "CI_BASE_IMAGE=")
	if r.Code != 2 {
		t.Fatalf("make ci-base-image exit %d, want 2 with an empty CI_BASE_IMAGE\nstdout:\n%s\nstderr:\n%s", r.Code, r.Stdout, r.Stderr)
	}
	if !strings.Contains(r.Stderr, "CI_BASE_IMAGE") {
		t.Fatalf("the refusal does not name CI_BASE_IMAGE\nstderr:\n%s", r.Stderr)
	}
	if r.hasCallWithPrefix("docker ") {
		t.Fatalf("make ci-base-image started a build with no base image named\ncalls:\n%s", r.callLog())
	}
}

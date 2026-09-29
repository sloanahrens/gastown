package doltserver

import (
	"os"
	"testing"
)

func TestDoltHubToken(t *testing.T) {
	// Save and restore original value
	orig := os.Getenv("DOLTHUB_TOKEN")
	defer os.Setenv("DOLTHUB_TOKEN", orig)

	os.Setenv("DOLTHUB_TOKEN", "dhat.v1.test123")
	if got := DoltHubToken(); got != "dhat.v1.test123" {
		t.Errorf("DoltHubToken() = %q, want %q", got, "dhat.v1.test123")
	}

	os.Unsetenv("DOLTHUB_TOKEN")
	if got := DoltHubToken(); got != "" {
		t.Errorf("DoltHubToken() = %q, want empty", got)
	}
}

func TestDoltHubOrg(t *testing.T) {
	orig := os.Getenv("DOLTHUB_ORG")
	defer os.Setenv("DOLTHUB_ORG", orig)

	os.Setenv("DOLTHUB_ORG", "bvts")
	if got := DoltHubOrg(); got != "bvts" {
		t.Errorf("DoltHubOrg() = %q, want %q", got, "bvts")
	}

	os.Unsetenv("DOLTHUB_ORG")
	if got := DoltHubOrg(); got != "" {
		t.Errorf("DoltHubOrg() = %q, want empty", got)
	}
}

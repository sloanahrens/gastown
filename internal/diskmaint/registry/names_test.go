package registry

import "testing"

func TestValidRepo(t *testing.T) {
	t.Parallel()
	ok := []string{"gastown-polecat", "seedtest/polecat", "a/b/c", "repo.a_b-c"}
	bad := []string{"", "../x", "a//b", "A", "a b", "a/", "/a", "a/../b", "a;rm", "a$(x)"}
	for _, s := range ok {
		if !ValidRepo(s) {
			t.Errorf("ValidRepo(%q) = false, want true", s)
		}
	}
	for _, s := range bad {
		if ValidRepo(s) {
			t.Errorf("ValidRepo(%q) = true, want false", s)
		}
	}
}

func TestValidTag(t *testing.T) {
	t.Parallel()
	ok := []string{"dev", "2d5a48dcb", "build-3", repeatByte('a', 128)}
	bad := []string{"", "-x", "a b", "a/b", repeatByte('a', 129)}
	for _, s := range ok {
		if !ValidTag(s) {
			t.Errorf("ValidTag(%q) = false, want true", s)
		}
	}
	for _, s := range bad {
		if ValidTag(s) {
			t.Errorf("ValidTag(%q) = true, want false", s)
		}
	}
}

func TestValidDigest(t *testing.T) {
	t.Parallel()
	ok := []string{
		"sha256:" + repeatByte('a', 64),
		"sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
	}
	bad := []string{
		"",
		"sha256:" + repeatByte('A', 64),
		"sha256:" + repeatByte('a', 63),
		"md5:" + repeatByte('a', 64),
		repeatByte('a', 64),
		"sha256:",
	}
	for _, s := range ok {
		if !ValidDigest(s) {
			t.Errorf("ValidDigest(%q) = false, want true", s)
		}
	}
	for _, s := range bad {
		if ValidDigest(s) {
			t.Errorf("ValidDigest(%q) = true, want false", s)
		}
	}
}

func TestValidContainer(t *testing.T) {
	t.Parallel()
	ok := []string{"gt-registry"}
	bad := []string{"", "-x", "a b", "a;b"}
	for _, s := range ok {
		if !ValidContainer(s) {
			t.Errorf("ValidContainer(%q) = false, want true", s)
		}
	}
	for _, s := range bad {
		if ValidContainer(s) {
			t.Errorf("ValidContainer(%q) = true, want false", s)
		}
	}
}

func repeatByte(b byte, n int) string {
	out := make([]byte, n)
	for i := range out {
		out[i] = b
	}
	return string(out)
}

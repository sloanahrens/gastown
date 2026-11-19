package bdgateway

import (
	"bytes"
	"sync"

	"github.com/steveyegge/gastown/deploy/bd"
)

// embeddedPolicy parses the verbs file a binary carries once, keeping the
// result for every later call. A parse error is kept too and returned on each
// call: a caller that met a broken file must not read the second answer as a
// healthy one.
type embeddedPolicy struct {
	once sync.Once
	raw  []byte
	p    *Policy
	err  error
}

// load returns the parsed policy, parsing raw on the first call. The policy is
// kept only on success, so a failed parse can never hand back a partial
// policy beside its error.
func (h *embeddedPolicy) load() (*Policy, error) {
	h.once.Do(func() {
		p, err := ParsePolicy(bytes.NewReader(h.raw))
		if err != nil {
			h.err = err
			return
		}
		h.p = p
	})
	return h.p, h.err
}

// defaultPolicy is deploy/bd/verbs.json, the policy file the gateway image
// ships (deploy/bd/doc.go), compiled into the binary.
var defaultPolicy = embeddedPolicy{raw: bd.VerbsJSON}

// DefaultPolicy returns the policy embedded in the binary, parsed once per
// process; a file that fails to parse returns its error and no policy.
func DefaultPolicy() (*Policy, error) { return defaultPolicy.load() }

// IsReadCall reports whether argv runs a verb the embedded policy classes as a
// read, which is what lets a caller retry the call after a transport failure
// (gt-vdadv0). It answers false for every doubt — a policy that failed to
// load, a verb the policy does not place, an argv the scan cannot place, a
// refused flag, an empty argv — because a write retried as a read can land
// twice while a read treated as a write only stays unretried.
//
// The class comes from Authorize under the operator token, the highest class:
// the read question is not about who may call, so only the placement and flag
// refusals remain, and an argv they refuse is a call bd would not run as the
// policy describes it.
func IsReadCall(argv []string) bool { return isReadCall(DefaultPolicy, argv) }

// isReadCall is IsReadCall with the policy load injected. Only the failed-load
// answer needs the seam: the embedded file always parses, so a test drives
// that refusal with a loader that fails rather than corrupting the file.
func isReadCall(load func() (*Policy, error), argv []string) bool {
	p, err := load()
	if err != nil {
		return false
	}
	v, err := p.Authorize(TokenOperator, argv)
	if err != nil {
		return false
	}
	return v.Class == ClassRead
}

//go:build integration

package allowtools

import "github.com/steveyegge/gastown/internal/testutil"

var _ = testutil.AllowTools("dolt")

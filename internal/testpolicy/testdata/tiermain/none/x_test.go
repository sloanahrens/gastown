package none

import "testing"

// Main is a local function of the same name as unittier.Main.
func Main(m *testing.M) int { return m.Run() }

func TestX(t *testing.T) { t.Parallel() }

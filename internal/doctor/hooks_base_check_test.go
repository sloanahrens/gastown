package doctor

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/steveyegge/gastown/internal/hooks"
)

// memHooksBase is a hooksBaseStore holding the base config in memory.
type memHooksBase struct {
	cfg   *hooks.HooksConfig
	saves int
}

func (m *memHooksBase) LoadBase() (*hooks.HooksConfig, error) {
	if m.cfg == nil {
		return nil, os.ErrNotExist
	}
	return m.cfg, nil
}

func (m *memHooksBase) SaveBase(cfg *hooks.HooksConfig) error {
	m.cfg = cfg
	m.saves++
	return nil
}

func (m *memHooksBase) BasePath() string { return filepath.Join("home", ".gt", "hooks-base.json") }

func hooksBaseCheckOn(store *memHooksBase) *HooksBaseCheck {
	c := NewHooksBaseCheck()
	c.store = store
	return c
}

func TestHooksBaseCheck_Missing(t *testing.T) {
	t.Parallel()
	result := hooksBaseCheckOn(&memHooksBase{}).Run(&CheckContext{TownRoot: t.TempDir()})

	if result.Status != StatusWarning {
		t.Errorf("expected StatusWarning when hooks-base.json is missing, got %v: %s", result.Status, result.Message)
	}
}

func TestHooksBaseCheck_Present(t *testing.T) {
	t.Parallel()
	check := hooksBaseCheckOn(&memHooksBase{cfg: hooks.DefaultBase()})
	result := check.Run(&CheckContext{TownRoot: t.TempDir()})

	if result.Status != StatusOK {
		t.Errorf("expected StatusOK when hooks-base.json exists, got %v: %s", result.Status, result.Message)
	}
}

func TestHooksBaseCheck_Fix(t *testing.T) {
	t.Parallel()
	store := &memHooksBase{}
	check := hooksBaseCheckOn(store)
	ctx := &CheckContext{TownRoot: t.TempDir()}

	// Should warn before fix
	result := check.Run(ctx)
	if result.Status != StatusWarning {
		t.Fatalf("expected StatusWarning before fix, got %v", result.Status)
	}

	// Fix should save the default base
	if err := check.Fix(ctx); err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if store.saves != 1 || !hooks.HooksEqual(store.cfg, hooks.DefaultBase()) {
		t.Errorf("Fix saved %d times, cfg %+v; want the default base saved once", store.saves, store.cfg)
	}

	// Should now pass
	result = check.Run(ctx)
	if result.Status != StatusOK {
		t.Errorf("expected StatusOK after fix, got %v: %s", result.Status, result.Message)
	}
}

func TestHooksBaseCheck_FixIdempotent(t *testing.T) {
	t.Parallel()
	store := &memHooksBase{cfg: hooks.DefaultBase()}
	check := hooksBaseCheckOn(store)

	// Fix on already-present file should be a no-op
	if err := check.Fix(&CheckContext{TownRoot: t.TempDir()}); err != nil {
		t.Errorf("Fix on existing base should not error: %v", err)
	}
	if store.saves != 0 {
		t.Errorf("Fix saved %d times over an existing base, want 0", store.saves)
	}
}

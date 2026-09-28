package nontestingreceiver

import "testing"

type cfgBuilder struct{}

func (c *cfgBuilder) Setenv(k, v string) {}

func helper(t *cfgBuilder) {
	t.Setenv("A", "b")
}

func TestA(t *testing.T) {
	t.Parallel()
	helper(&cfgBuilder{})
}

package presubmit

import (
	"errors"
	"io"
)

// RedBaseAdapters holds the operations the red-on-base check runs, so a caller supplies the real ones and a test fakes them.
type RedBaseAdapters struct {
	Git    Runner
	Read   FileReader
	Mkdir  RedBaseMkdir
	Write  RedBaseWriter
	GoTest func(dir string, args []string) ([]byte, error)
}

// RedBaseRunCheck runs the red-on-base check over the adapters: the plan from RedBasePlan under base, the tree from RedBaseTree under base and dir, the judge from JudgeRedBase, and the report and removal from RedBaseCheck, whose result and error it returns unchanged. A nil adapter field is an error naming the field, returned before any adapter is called, so a check wired with a missing operation never runs half a pass.
func RedBaseRunCheck(a RedBaseAdapters, base, dir string, out io.Writer) (bool, error) {
	if err := a.checkWired(); err != nil {
		return false, err
	}
	return RedBaseCheck(
		func() (RedBasePlanResult, error) { return RedBasePlan(a.Git, a.Read, base) },
		func(files []string) (string, func() error, error) {
			return RedBaseTree(a.Git, a.Read, a.Mkdir, a.Write, base, dir, files)
		},
		func(plan RedBasePlanResult, tree string) ([]RedBaseFinding, error) {
			return JudgeRedBase(plan, tree, a.GoTest)
		},
		out,
	)
}

// checkWired returns an error naming the first nil adapter field, so RedBaseRunCheck refuses an unwired RedBaseAdapters rather than call a nil function.
func (a RedBaseAdapters) checkWired() error {
	switch {
	case a.Git == nil:
		return errors.New("red-base adapters: nil Git")
	case a.Read == nil:
		return errors.New("red-base adapters: nil Read")
	case a.Mkdir == nil:
		return errors.New("red-base adapters: nil Mkdir")
	case a.Write == nil:
		return errors.New("red-base adapters: nil Write")
	case a.GoTest == nil:
		return errors.New("red-base adapters: nil GoTest")
	}
	return nil
}

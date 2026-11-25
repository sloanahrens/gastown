package done

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// gateTestFiles is one package's test files: TestGoFiles, then XTestGoFiles.
type gateTestFiles struct {
	Dir   string
	Files []string
}

type goListPackage struct {
	ImportPath   string
	Dir          string
	TestGoFiles  []string
	XTestGoFiles []string
	Error        *goListError
}

// goListError is go list's PackageError: why it could not load a package.
type goListError struct {
	Err string
}

// goListTestFiles asks go which test files each package has under tags, building
// and running nothing. go list applies the build constraints, so a file behind
// another tag is not returned (gt-y9uww1). tags is empty when the Gate line names none.
func goListTestFiles(run gateListCommand, env []string, dir, tags string, packages []string) ([]gateTestFiles, error) {
	args := []string{"list", "-e"}
	if tags != "" {
		args = append(args, "-tags", tags)
	}
	args = append(args, "-json=ImportPath,Dir,TestGoFiles,XTestGoFiles,Error")
	out, err := run(dir, env, append(args, packages...)...)
	if err != nil {
		return nil, fmt.Errorf("go list did not complete: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(out))
	var got []gateTestFiles
	for {
		var p goListPackage
		err := dec.Decode(&p)
		if errors.Is(err, io.EOF) {
			return got, nil
		}
		if err != nil {
			return nil, fmt.Errorf("go list output does not decode: %w", err)
		}
		if p.Error != nil {
			return nil, fmt.Errorf("go list cannot load %s: %s", p.ImportPath, p.Error.Err)
		}
		got = append(got, gateTestFiles{Dir: p.Dir, Files: append(append([]string(nil), p.TestGoFiles...), p.XTestGoFiles...)})
	}
}

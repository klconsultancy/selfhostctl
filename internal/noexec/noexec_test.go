// Package noexec guards "no shell-outs" (#1): none of the tool's own packages run an
// external program. Dependencies may (client-go's exec credential plugins do), but the
// tool never asks them to run `docker`, `kubectl`, `clusterctl` or `helm`.
package noexec

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestNoPackageImportsOSExec(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == ".git" || d.Name() == "testdata" || strings.HasPrefix(d.Name(), ".")) && path != root {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, imp := range f.Imports {
			if p, _ := strconv.Unquote(imp.Path.Value); p == "os/exec" {
				rel, _ := filepath.Rel(root, path)
				t.Errorf("%s imports os/exec", rel)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

package gateway

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func TestPackageDoesNotDependOnConcreteRuntimeOrHTTP(t *testing.T) {
	t.Parallel()

	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller could not locate gateway package")
	}
	dir := filepath.Dir(currentFile)
	packages, err := parser.ParseDir(token.NewFileSet(), dir, func(info fs.FileInfo) bool {
		return !strings.HasSuffix(info.Name(), "_test.go")
	}, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parse gateway package: %v", err)
	}

	for _, pkg := range packages {
		for filename, file := range pkg.Files {
			for _, spec := range file.Imports {
				path, err := strconv.Unquote(spec.Path.Value)
				if err != nil {
					t.Fatalf("unquote import %s in %s: %v", spec.Path.Value, filename, err)
				}
				switch {
				case path == "net/http":
					t.Errorf("%s imports transport package net/http", filepath.Base(filename))
				case strings.HasSuffix(path, "/internal/scheduler"):
					t.Errorf("%s imports concrete scheduler package", filepath.Base(filename))
				case strings.HasSuffix(path, "/internal/upstream"):
					t.Errorf("%s imports concrete upstream package", filepath.Base(filename))
				}
			}
		}
	}
}

/*
Copyright 2019 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// This code is directly lifted from the Kubernetes codebase in order to avoid relying on the k8s.io/kubernetes package.
// For reference: https://github.com/kubernetes/kubernetes/blob/release-1.22/cmd/preferredimports/preferredimports.go

// verify that all the imports have our preferred alias(es).
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"go/ast"
	"go/build"
	"go/format"
	"go/parser"
	"go/token"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"golang.org/x/term"
)

var (
	importAliases    = flag.String("import-aliases", "hack/.import-aliases", "json file with import aliases")
	confirm          = flag.Bool("confirm", false, "update file with the preferred aliases for imports")
	includePathRegex = flag.String("include-path", "(test/e2e/|test/e2e_node)", "only files with paths matching this regex is touched")
	excludePathRegex = flag.String("exclude-path", "(testing)", "files with paths matching this regex is ignored")
	isTerminal       = term.IsTerminal(int(os.Stdout.Fd()))
	logPrefix        = ""
	aliases          map[string]string
)

type analyzer struct {
	fset      *token.FileSet // positions are relative to fset
	ctx       build.Context
	failed    bool
	donePaths map[string]any
}

func newAnalyzer() *analyzer {
	ctx := build.Default
	ctx.CgoEnabled = true

	a := &analyzer{
		fset:      token.NewFileSet(),
		ctx:       ctx,
		donePaths: make(map[string]any),
	}

	return a
}

// collect extracts test metadata from a file.
func (a *analyzer) collect(dir string) {
	if _, ok := a.donePaths[dir]; ok {
		return
	}
	a.donePaths[dir] = nil

	// Create the AST by parsing src.
	fs, err := parser.ParseDir(a.fset, dir, nil, parser.AllErrors|parser.ParseComments)

	if err != nil {
		fmt.Fprintln(os.Stderr, "ERROR(syntax)", logPrefix, err)
		a.failed = true
		return
	}

	for _, p := range fs {
		// returns first error, but a.handleError deals with it
		files := a.filterFiles(p.Files)
		for _, file := range files {
			replacements := make(map[string]string)
			pathToFile := a.fset.File(file.Pos()).Name()
			for _, imp := range file.Imports {
				importPath := strings.ReplaceAll(imp.Path.Value, "\"", "")
				pathSegments := strings.Split(importPath, "/")
				importName := pathSegments[len(pathSegments)-1]
				if imp.Name != nil {
					importName = imp.Name.Name
				}
				if alias, ok := aliases[importPath]; ok {
					if alias != importName {
						if !*confirm {
							fmt.Fprintf(os.Stderr, "%sERROR wrong alias for import \"%s\" should be %s in file %s\n", logPrefix, importPath, alias, pathToFile)
							a.failed = true
						}
						replacements[importName] = alias
						if imp.Name != nil {
							imp.Name.Name = alias
						} else {
							imp.Name = ast.NewIdent(alias)
						}
					}
				}
			}

			if len(replacements) > 0 {
				if *confirm {
					fmt.Printf("%sReplacing imports with aliases in file %s\n", logPrefix, pathToFile)
					for key, value := range replacements {
						renameImportUsages(file, key, value)
					}
					ast.SortImports(a.fset, file)
					var buffer bytes.Buffer
					if err = format.Node(&buffer, a.fset, file); err != nil {
						panic(fmt.Sprintf("Error formatting ast node after rewriting import.\n%s\n", err.Error()))
					}

					fileInfo, err := os.Stat(pathToFile)
					if err != nil {
						panic(fmt.Sprintf("Error stat'ing file: %s\n%s\n", pathToFile, err.Error()))
					}

					err = os.WriteFile(pathToFile, buffer.Bytes(), fileInfo.Mode())
					if err != nil {
						panic(fmt.Sprintf("Error writing file: %s\n%s\n", pathToFile, err.Error()))
					}
				}
			}
		}
	}
}

func renameImportUsages(f *ast.File, old, new string) {
	// use this to avoid renaming the package declaration, eg:
	//   given: package foo; import foo "bar"; foo.Baz, rename foo->qux
	//   yield: package foo; import qux "bar"; qux.Baz
	var pkg *ast.Ident

	// Rename top-level old to new, both unresolved names
	// (probably defined in another file) and names that resolve
	// to a declaration we renamed.
	ast.Inspect(f, func(node ast.Node) bool {
		if node == nil {
			return false
		}
		switch id := node.(type) {
		case *ast.File:
			pkg = id.Name
		case *ast.Ident:
			if pkg != nil && id == pkg {
				return false
			}
			if id.Name == old {
				id.Name = new
			}
		}
		return true
	})
}

func (a *analyzer) filterFiles(fs map[string]*ast.File) []*ast.File {
	var files []*ast.File
	for _, f := range fs {
		files = append(files, f)
	}
	return files
}

type collector struct {
	dirs             []string
	includePathRegex *regexp.Regexp
	excludePathRegex *regexp.Regexp
}

// handlePath walks the filesystem recursively, collecting directories,
// ignoring some unneeded directories (hidden/vendored) that are handled
// specially later.
func (c *collector) handlePath(path string, info os.FileInfo, err error) error {
	if err != nil {
		return err
	}
	if info.IsDir() {
		// Ignore hidden directories (.git, .cache, etc)
		if len(path) > 1 && path[0] == '.' ||
			// Staging code is symlinked from vendor/k8s.io, and uses import
			// paths as if it were inside of vendor/. It fails typechecking
			// inside of staging/, but works when typechecked as part of vendor/.
			path == "staging" ||
			// OS-specific vendor code tends to be imported by OS-specific
			// packages. We recursively typecheck imported vendored packages for
			// each OS, but don't typecheck everything for every OS.
			path == "vendor" ||
			path == "_output" ||
			// This is a weird one. /testdata/ is *mostly* ignored by Go,
			// and this translates to kubernetes/vendor not working.
			// edit/record.go doesn't compile without gopkg.in/yaml.v2
			// in $GOSRC/$GOROOT (both typecheck and the shell script).
			path == "pkg/kubectl/cmd/testdata/edit" {
			return filepath.SkipDir
		}
		if c.includePathRegex.MatchString(path) && !c.excludePathRegex.MatchString(path) {
			c.dirs = append(c.dirs, path)
		}
	}
	return nil
}

func main() {
	flag.Parse()
	args := flag.Args()

	if len(args) == 0 {
		args = append(args, ".")
	}

	includePathRegex, err := regexp.Compile(*includePathRegex)
	if err != nil {
		log.Fatalf("Error compiling regex: %v", err)
	}
	excludePathRegex, err := regexp.Compile(*excludePathRegex)
	if err != nil {
		log.Fatalf("Error compiling regex: %v", err)
	}
	c := collector{includePathRegex: includePathRegex, excludePathRegex: excludePathRegex}
	for _, arg := range args {
		err := filepath.Walk(arg, c.handlePath)
		if err != nil {
			log.Fatalf("Error walking: %v", err)
		}
	}
	sort.Strings(c.dirs)

	if len(*importAliases) > 0 {
		bytes, err := os.ReadFile(*importAliases)
		if err != nil {
			log.Fatalf("Error reading import aliases: %v", err)
		}
		err = json.Unmarshal(bytes, &aliases)
		if err != nil {
			log.Fatalf("Error loading aliases: %v", err)
		}
	}
	if isTerminal {
		logPrefix = "\r" // clear status bar when printing
	}
	fmt.Println("checking-imports: ")

	a := newAnalyzer()
	for _, dir := range c.dirs {
		if isTerminal {
			fmt.Printf("\r\033[0m %-80s\n", dir)
		}
		a.collect(dir)
	}
	fmt.Println()
	if a.failed {
		os.Exit(1)
	}
}

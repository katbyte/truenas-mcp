package generator

import (
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/katbyte/pandorest/definitions"
)

// Check verifies that the package in dir, or the packages under it in the grouped layout, have a method on a Client for every
// operation in the definitions (and a Complete method for every list, a To method for a call answering a file), and returns
// how many operations it found. In the grouped layout each operation's method is looked for on the Client of its own group's
// package, as groups.json names it, under the name it is generated with there: a call drops its group's name (c.Pool.Query).
func Check(svc *definitions.Service, dir string) (int, error) {
	byDir, err := clientMethods(dir)
	if err != nil {
		return 0, err
	}
	packages, err := groupPackages(dir)
	if err != nil {
		return 0, err
	}
	has := func(grp *definitions.Group, name string) bool {
		if pkg, ok := packages[grp.Name]; ok {
			return byDir[pkg][name]
		}

		return byDir["."][name]
	}

	var missing []string
	count := 0
	for gi := range svc.Groups {
		grp := &svc.Groups[gi]
		for oi := range grp.Operations {
			o := &grp.Operations[oi]
			count++
			name := o.Name
			if _, grouped := packages[grp.Name]; grouped {
				name = callMethodName(grp, o)
			}
			if !has(grp, name) {
				missing = append(missing, fmt.Sprintf("%s (%s)", name, o.Key()))
			}
			if o.Pageable != nil && !has(grp, name+"Complete") {
				missing = append(missing, fmt.Sprintf("%sComplete (%s)", name, o.Key()))
			}
			if o.Pageable != nil && !has(grp, name+"Each") {
				missing = append(missing, fmt.Sprintf("%sEach (%s)", name, o.Key()))
			}
			if o.Command != "" && !has(grp, name+"AndWait") {
				missing = append(missing, fmt.Sprintf("%sAndWait (%s)", name, o.Key()))
			}
			if o.Commands != nil && !has(grp, "WaitForCommand") {
				missing = append(missing, fmt.Sprintf("WaitForCommand (%s)", o.Key()))
			}
			if o.Call != nil && o.Call.Downloadable && !has(grp, name+"To") {
				missing = append(missing, fmt.Sprintf("%sTo (%s)", name, o.Key()))
			}
		}
	}
	if len(missing) > 0 {
		slices.Sort(missing)
		return count, fmt.Errorf("%s: %d methods missing from %s:\n  %s", svc.Name, len(missing), dir, strings.Join(missing, "\n  "))
	}

	return count, nil
}

// groupPackages is each group's package directory under dir, from the grouped layout's groups.json, or nothing for the flat
// layout, which writes none.
func groupPackages(dir string) (map[string]string, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "groups.json")) //nolint:gosec // the generated package's own file
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var groups struct {
		Groups map[string]struct{ Package string } `json:"Groups"`
	}
	if err := json.Unmarshal(raw, &groups); err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Join(dir, "groups.json"), err)
	}
	packages := map[string]string{}
	for name, g := range groups.Groups {
		packages[name] = g.Package
	}

	return packages, nil
}

// clientMethods returns the names of the methods declared on a type named Client in the non-test Go files of dir and the
// directories under it, by directory relative to dir ("." for dir itself).
func clientMethods(dir string) (map[string]map[string]bool, error) {
	byDir := map[string]map[string]bool{}
	fset := token.NewFileSet()
	err := filepath.WalkDir(dir, func(path string, e os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := e.Name()
		if e.IsDir() || filepath.Ext(name) != ".go" || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, filepath.Dir(path))
		if err != nil {
			return err
		}
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Recv == nil || len(fn.Recv.List) != 1 {
				continue
			}
			recv := fn.Recv.List[0].Type
			if star, ok := recv.(*ast.StarExpr); ok {
				recv = star.X
			}
			if id, ok := recv.(*ast.Ident); ok && id.Name == "Client" {
				if byDir[rel] == nil {
					byDir[rel] = map[string]bool{}
				}
				byDir[rel][fn.Name.Name] = true
			}
		}

		return nil
	})

	return byDir, err
}

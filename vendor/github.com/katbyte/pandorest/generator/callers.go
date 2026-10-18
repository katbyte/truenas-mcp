package generator

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/katbyte/pandorest/definitions"
)

// Uncalled lists the generated methods of a service that no hand-written Go file under root names, with how many methods there
// are: what the repository's own code never reaches, for a reader deciding what no tool wraps. A name counts wherever it is used
// as a selector, so a tool calling c.Items.GetItems counts, and a method of the same name on another type counts too, since the
// parse does not resolve types; a test file does not count, since a test naming a method is not a tool wrapping it. A call whose
// method drops its namespace in a package per namespace counts by its group and short name together, c.Pool.Query, since Query
// alone is every namespace's. Generated files, by their header, are left out, as is what is under skip. A repository with two
// services, where one's GetItems would count for the other's, uses UncalledOn.
func Uncalled(svc *definitions.Service, root string, skip ...string) (uncalled []string, total int, err error) {
	return UncalledOn(svc, root, "", skip...)
}

// UncalledOn is Uncalled counting only what is reached through field, the field or variable the service's client sits on: with
// field "emby", s.emby.GetItems and s.emby.Items.GetItems count and jf.GetItems does not. An empty field is Uncalled.
func UncalledOn(svc *definitions.Service, root, field string, skip ...string) (uncalled []string, total int, err error) {
	used, err := selectorNames(root, skip)
	if err != nil {
		return nil, 0, err
	}
	through := func(name string) string {
		if field == "" {
			return name
		}

		return field + "." + name
	}
	for gi := range svc.Groups {
		grp := &svc.Groups[gi]
		for oi := range grp.Operations {
			o := &grp.Operations[oi]
			names := []string{o.Name}
			if o.Pageable != nil {
				names = append(names, o.Name+"Complete", o.Name+"Each")
			}
			if o.Command != "" {
				names = append(names, o.Name+"AndWait")
			}
			if o.Commands != nil {
				names = append(names, "WaitForCommand")
			}
			for _, name := range names {
				total++
				if !used[through(name)] && !used[through(grp.Name+"."+callMethodName(grp, o))] {
					uncalled = append(uncalled, name)
				}
			}
		}
	}
	slices.Sort(uncalled)

	return uncalled, total, nil
}

// selectorNames is every name used as a selector (x.Name) in the hand-written Go files under root, outside the directories in
// skip, and every run of two or three selectors (s.emby.Pool.Query is emby.Pool.Query and Pool.Query as well as Pool and Query).
func selectorNames(root string, skip []string) (map[string]bool, error) {
	names := map[string]bool{}
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if e.IsDir() {
			if name := e.Name(); path != root && (strings.HasPrefix(name, ".") || name == "vendor" || name == "testdata" || slices.ContainsFunc(skip, func(s string) bool { return s != "" && sameDir(path, s) })) {
				return filepath.SkipDir
			}

			return nil
		}
		if filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(path) //nolint:gosec // the repository's own files
		if err != nil {
			return err
		}
		if Generated(src) {
			return nil
		}
		f, err := parser.ParseFile(fset, path, src, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok {
				names[sel.Sel.Name] = true
				if inner, ok := sel.X.(*ast.SelectorExpr); ok {
					names[inner.Sel.Name+"."+sel.Sel.Name] = true
					if outer, ok := inner.X.(*ast.SelectorExpr); ok {
						names[outer.Sel.Name+"."+inner.Sel.Name+"."+sel.Sel.Name] = true
					}
				}
			}

			return true
		})

		return nil
	})

	return names, err
}

// sameDir reports whether two paths name one directory.
func sameDir(a, b string) bool {
	a, errA := filepath.Abs(a)
	b, errB := filepath.Abs(b)

	return errA == nil && errB == nil && a == b
}

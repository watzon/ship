package provider

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestProvidersListThroughClientList guards the invariant that keeps
// `ship provision plan --refresh` and `ship provision apply` in agreement:
// refresh lists servers with Provider.List (Client.List), so every provider's
// reconcile backend and reconcile preflight must list through that same
// method, with the same scope and filters, and never through a separately
// scoped listing. A provider that lists differently (Thorn QA: Latitude's
// backend filtered by project, refresh did not) fails here.
func TestProvidersListThroughClientList(t *testing.T) {
	dirs, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, dir := range dirs {
		if !dir.IsDir() || dir.Name() == "providers" {
			continue
		}
		files, _ := filepath.Glob(filepath.Join(dir.Name(), "*.go"))
		for _, path := range files {
			if strings.HasSuffix(path, "_test.go") {
				continue
			}
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Recv == nil || len(fn.Recv.List) != 1 || fn.Body == nil {
					continue
				}
				recv := receiverType(fn.Recv.List[0].Type)
				switch {
				case recv == "reconcileBackend" && fn.Name.Name == "List":
					checked++
					if !returnsClientList(fn) {
						t.Errorf("%s: reconcileBackend.List must be exactly `return b.client.List(ctx, project, environment)` so apply lists the same servers as provision plan --refresh", fset.Position(fn.Pos()))
					}
				case recv == "Client" && fn.Name.Name == "Reconcile":
					if callsMethod(fn, "reconcileBackendFor") && !preflightsWithClient(fn) {
						t.Errorf("%s: Reconcile must call provider.PreflightHosts(ctx, c, ...) before reconcileBackendFor", fset.Position(fn.Pos()))
					}
				}
			}
		}
	}
	if checked < 15 {
		t.Fatalf("checked %d reconcile backends; the scan is not finding the providers", checked)
	}
}

func receiverType(expr ast.Expr) string {
	if star, ok := expr.(*ast.StarExpr); ok {
		expr = star.X
	}
	if ident, ok := expr.(*ast.Ident); ok {
		return ident.Name
	}
	return ""
}

func returnsClientList(fn *ast.FuncDecl) bool {
	if len(fn.Body.List) != 1 {
		return false
	}
	ret, ok := fn.Body.List[0].(*ast.ReturnStmt)
	if !ok || len(ret.Results) != 1 {
		return false
	}
	call, ok := ret.Results[0].(*ast.CallExpr)
	if !ok || len(call.Args) != 3 {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "List" {
		return false
	}
	inner, ok := sel.X.(*ast.SelectorExpr)
	if !ok || inner.Sel.Name != "client" {
		return false
	}
	params := fn.Type.Params.List
	var names []string
	for _, field := range params {
		for _, name := range field.Names {
			names = append(names, name.Name)
		}
	}
	for i, arg := range call.Args {
		ident, ok := arg.(*ast.Ident)
		if !ok || i >= len(names) || ident.Name != names[i] {
			return false
		}
	}
	return true
}

func callsMethod(fn *ast.FuncDecl, name string) bool {
	found := false
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok {
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == name {
				found = true
			}
		}
		return !found
	})
	return found
}

func preflightsWithClient(fn *ast.FuncDecl) bool {
	found := false
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "PreflightHosts" || len(call.Args) < 2 {
			return true
		}
		if ident, ok := call.Args[1].(*ast.Ident); ok && ident.Name == "c" {
			found = true
		}
		return !found
	})
	return found
}

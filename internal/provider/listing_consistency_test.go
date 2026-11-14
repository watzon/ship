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

// TestProvidersListThroughClientList guards the invariants that keep
// `ship provision plan --refresh` and `ship provision apply` in agreement:
//
//   - refresh lists servers with Provider.List (Client.List), so every
//     provider's reconcile backend must list through that same method, with
//     the same scope and filters, and never through a separately scoped
//     listing (Thorn QA: Latitude's backend filtered by project, refresh did
//     not);
//   - a server-creating Reconcile (one that calls provider.ReconcileHosts)
//     that does any setup on the provider before ReconcileHosts (networks,
//     firewalls, keys, the reconcile backend) must run provider.PreflightHosts
//     with the provider itself before the first such call. Setup after
//     ReconcileHosts is covered by ReconcileHosts' own preflight.
//
// Provider types are found by their Reconcile method and backend types by
// their List and Create methods, whatever the types and receivers are named.
func TestProvidersListThroughClientList(t *testing.T) {
	dirs, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	backends, reconcilers := 0, 0
	for _, dir := range dirs {
		if !dir.IsDir() || dir.Name() == "providers" {
			continue
		}
		fset := token.NewFileSet()
		methods := map[string]map[string]*ast.FuncDecl{}
		files, _ := filepath.Glob(filepath.Join(dir.Name(), "*.go"))
		for _, path := range files {
			if strings.HasSuffix(path, "_test.go") {
				continue
			}
			file, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			collectMethods(file, methods)
		}
		for typ, byName := range methods {
			if reconcile, ok := byName["Reconcile"]; ok {
				if callsReconcileHosts(reconcile) {
					reconcilers++
				}
				if msg := checkReconcilePreflight(reconcile); msg != "" {
					t.Errorf("%s: %s.Reconcile: %s", fset.Position(reconcile.Pos()), typ, msg)
				}
				continue
			}
			list, hasList := byName["List"]
			if _, hasCreate := byName["Create"]; hasList && hasCreate {
				backends++
				if msg := checkBackendList(list); msg != "" {
					t.Errorf("%s: %s.List: %s", fset.Position(list.Pos()), typ, msg)
				}
			}
		}
	}
	// 19 server-creating providers; proxmox passes its Client to
	// ReconcileHosts directly, so it has no separate backend type.
	if backends < 18 || reconcilers < 19 {
		t.Fatalf("found %d reconcile backends and %d Reconcile methods; the scan is not finding the providers", backends, reconcilers)
	}
}

// TestListingGuardCatchesDrift runs the guard's checks on small sources, so a
// change that weakens them fails here even when every provider is correct.
func TestListingGuardCatchesDrift(t *testing.T) {
	tests := []struct {
		name    string
		src     string
		wantErr bool
	}{
		{name: "backend returns client list", src: `func (b backend) List(ctx context.Context, project, environment string) ([]provider.Host, error) { return b.client.List(ctx, project, environment) }`},
		{name: "backend assigns then returns", src: `func (x backend) List(ctx context.Context, p, e string) ([]provider.Host, error) { hosts, err := x.api.List(ctx, p, e); return hosts, err }`},
		{name: "backend lists another way", src: `func (b backend) List(ctx context.Context, project, environment string) ([]provider.Host, error) { return b.client.ListInProject(ctx, project, environment) }`, wantErr: true},
		{name: "backend changes arguments", src: `func (b backend) List(ctx context.Context, project, environment string) ([]provider.Host, error) { return b.client.List(ctx, project, "other") }`, wantErr: true},
		{name: "backend filters result", src: `func (b backend) List(ctx context.Context, project, environment string) ([]provider.Host, error) { hosts, err := b.client.List(ctx, project, environment); return filter(hosts), err }`, wantErr: true},
		{name: "preflight before setup", src: `func (p Client) Reconcile(ctx context.Context, project, environment string, env config.Environment) (provider.ReconcileResult, error) {
			p = p.forEnvironment(env)
			if err := provider.PreflightHosts(ctx, p, project, environment, nil); err != nil { return provider.ReconcileResult{}, err }
			backend, _ := p.reconcileBackendFor(ctx, project, environment, env)
			return provider.ReconcileHosts(ctx, project, environment, nil, backend) }`},
		{name: "preflight after setup", src: `func (c Client) Reconcile(ctx context.Context, project, environment string, env config.Environment) (provider.ReconcileResult, error) {
			backend, _ := c.reconcileBackendFor(ctx, project, environment, env)
			if err := provider.PreflightHosts(ctx, c, project, environment, nil); err != nil { return provider.ReconcileResult{}, err }
			return provider.ReconcileHosts(ctx, project, environment, nil, backend) }`, wantErr: true},
		{name: "firewall before preflight", src: `func (c Client) Reconcile(ctx context.Context, project, environment string, env config.Environment) (provider.ReconcileResult, error) {
			c.EnsureFirewall(ctx, project, environment)
			if err := provider.PreflightHosts(ctx, c, project, environment, nil); err != nil { return provider.ReconcileResult{}, err }
			return provider.ReconcileHosts(ctx, project, environment, nil, backend{client: c}) }`, wantErr: true},
		{name: "setup without preflight", src: `func (c Client) Reconcile(ctx context.Context, project, environment string, env config.Environment) (provider.ReconcileResult, error) {
			backend, _ := c.reconcileBackendFor(ctx, project, environment, env)
			return provider.ReconcileHosts(ctx, project, environment, nil, backend) }`, wantErr: true},
		{name: "preflight with another lister", src: `func (c Client) Reconcile(ctx context.Context, project, environment string, env config.Environment) (provider.ReconcileResult, error) {
			if err := provider.PreflightHosts(ctx, other, project, environment, nil); err != nil { return provider.ReconcileResult{}, err }
			backend, _ := c.reconcileBackendFor(ctx, project, environment, env)
			return provider.ReconcileHosts(ctx, project, environment, nil, backend) }`, wantErr: true},
		{name: "setup after ReconcileHosts needs no extra preflight", src: `func (c Client) Reconcile(ctx context.Context, project, environment string, env config.Environment) (provider.ReconcileResult, error) {
			result, err := provider.ReconcileHosts(ctx, project, environment, nil, backend{client: c})
			c.PutInstancePublicPorts(ctx, result)
			return result, err }`},
		{name: "inventory provider creates nothing", src: `func (p Provider) Reconcile(ctx context.Context, project, environment string, env config.Environment) (provider.ReconcileResult, error) {
			return p.planAndHosts(ctx, env) }`},
		{name: "no setup needs no extra preflight", src: `func (c Client) Reconcile(ctx context.Context, project, environment string, env config.Environment) (provider.ReconcileResult, error) {
			if c.DryRun { return provider.ReconcileResult{}, nil }
			return provider.ReconcileHosts(ctx, project, environment, nil, backend{client: c}) }`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			file, err := parser.ParseFile(token.NewFileSet(), "guard.go", "package p\n"+tt.src, 0)
			if err != nil {
				t.Fatal(err)
			}
			fn := file.Decls[0].(*ast.FuncDecl)
			check := checkBackendList
			if fn.Name.Name == "Reconcile" {
				check = checkReconcilePreflight
			}
			if msg := check(fn); (msg != "") != tt.wantErr {
				t.Fatalf("check = %q, wantErr %v", msg, tt.wantErr)
			}
		})
	}
}

func collectMethods(file *ast.File, methods map[string]map[string]*ast.FuncDecl) {
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv == nil || len(fn.Recv.List) != 1 || fn.Body == nil {
			continue
		}
		typ := receiverType(fn.Recv.List[0].Type)
		if methods[typ] == nil {
			methods[typ] = map[string]*ast.FuncDecl{}
		}
		methods[typ][fn.Name.Name] = fn
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

func receiverName(fn *ast.FuncDecl) string {
	if names := fn.Recv.List[0].Names; len(names) == 1 {
		return names[0].Name
	}
	return ""
}

func paramNames(fn *ast.FuncDecl) []string {
	var names []string
	for _, field := range fn.Type.Params.List {
		for _, name := range field.Names {
			names = append(names, name.Name)
		}
	}
	return names
}

// checkBackendList requires the backend's List to return the provider's own
// List(ctx, project, environment) unchanged: either `return r.f.List(...)` or
// `a, b := r.f.List(...); return a, b`.
func checkBackendList(fn *ast.FuncDecl) string {
	const want = "must return <receiver>.<client>.List(ctx, project, environment) unchanged, so apply lists the same servers as provision plan --refresh"
	var call ast.Expr
	switch body := fn.Body.List; len(body) {
	case 1:
		ret, ok := body[0].(*ast.ReturnStmt)
		if !ok || len(ret.Results) != 1 {
			return want
		}
		call = ret.Results[0]
	case 2:
		assign, ok := body[0].(*ast.AssignStmt)
		ret, ok2 := body[1].(*ast.ReturnStmt)
		if !ok || !ok2 || len(assign.Rhs) != 1 || len(assign.Lhs) != len(ret.Results) {
			return want
		}
		for i, lhs := range assign.Lhs {
			l, ok := lhs.(*ast.Ident)
			r, ok2 := ret.Results[i].(*ast.Ident)
			if !ok || !ok2 || l.Name != r.Name {
				return want
			}
		}
		call = assign.Rhs[0]
	default:
		return want
	}
	ce, ok := call.(*ast.CallExpr)
	if !ok {
		return want
	}
	sel, ok := ce.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "List" {
		return want
	}
	inner, ok := sel.X.(*ast.SelectorExpr)
	if !ok {
		return want
	}
	if recv, ok := inner.X.(*ast.Ident); !ok || recv.Name != receiverName(fn) {
		return want
	}
	params := paramNames(fn)
	if len(ce.Args) != len(params) {
		return want
	}
	for i, arg := range ce.Args {
		if ident, ok := arg.(*ast.Ident); !ok || ident.Name != params[i] {
			return want
		}
	}
	return ""
}

// readOnlyReceiverCalls are receiver methods a Reconcile may call before the
// preflight because they only derive configuration.
var readOnlyReceiverCalls = map[string]bool{"forEnvironment": true, "PlanHosts": true}

func callsReconcileHosts(fn *ast.FuncDecl) bool {
	found := false
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok {
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "ReconcileHosts" {
				found = true
			}
		}
		return !found
	})
	return found
}

// checkReconcilePreflight requires that, when a server-creating Reconcile does
// setup through the provider before ReconcileHosts,
// provider.PreflightHosts(ctx, <receiver>, ...) comes first in source order.
func checkReconcilePreflight(fn *ast.FuncDecl) string {
	recv := receiverName(fn)
	preflight, firstSetup, reconcileHosts := token.NoPos, token.NoPos, token.NoPos
	setupName := ""
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		switch {
		case sel.Sel.Name == "PreflightHosts":
			if len(call.Args) >= 2 && preflight == token.NoPos {
				if ident, ok := call.Args[1].(*ast.Ident); ok && ident.Name == recv {
					preflight = call.Pos()
				}
			}
		case sel.Sel.Name == "ReconcileHosts":
			if reconcileHosts == token.NoPos {
				reconcileHosts = call.Pos()
			}
		default:
			if ident, ok := sel.X.(*ast.Ident); ok && ident.Name == recv && !readOnlyReceiverCalls[sel.Sel.Name] && firstSetup == token.NoPos {
				firstSetup, setupName = call.Pos(), sel.Sel.Name
			}
		}
		return true
	})
	if reconcileHosts == token.NoPos || firstSetup == token.NoPos || firstSetup > reconcileHosts {
		return ""
	}
	if preflight == token.NoPos {
		return "calls " + setupName + " without provider.PreflightHosts(ctx, " + recv + ", ...) first"
	}
	if preflight > firstSetup {
		return "provider.PreflightHosts must run before " + setupName
	}
	return ""
}

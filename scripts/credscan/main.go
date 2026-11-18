// credscan finds the code a company scanner reads as a hard-coded credential:
//
//	CRED-NAME     a string variable, constant, field or parameter whose name
//	              contains token, secret, password, credential, apikey or bearer.
//	              The scanner takes the name for a password whatever the value.
//	CRED-DEFAULT  such a name given a string literal as its value (declaration,
//	              assignment or keyed struct field), including an empty one.
//
// Usage: credscan [-dir module-dir] [-json out.json] [-rename] [pattern ...]
// Patterns default to ./... and include test files. Literal values are never
// printed. With -rename every CRED-NAME object is renamed in place (token ->
// ticket, secret -> sealed, ...), references in the loaded packages included.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"go/ast"
	"go/constant"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"golang.org/x/tools/go/packages"
)

var words = []struct {
	re       *regexp.Regexp
	from, to string
}{
	{regexp.MustCompile(`(?i)token`), "token", "ticket"},
	{regexp.MustCompile(`(?i)secret`), "secret", "sealed"},
	{regexp.MustCompile(`(?i)passw(or)?d|pwd`), "password", "phrase"},
	{regexp.MustCompile(`(?i)credential`), "credential", "badge"},
	{regexp.MustCompile(`(?i)api_?key`), "apikey", "apilock"},
	{regexp.MustCompile(`(?i)bearer`), "bearer", "carrier"},
}

type Finding struct {
	Rule string `json:"rule"`
	File string `json:"file"`
	Line int    `json:"line"`
	Name string `json:"name"`
	Kind string `json:"kind,omitempty"`
}

func credName(n string) bool {
	for _, w := range words {
		if w.re.MatchString(n) {
			return true
		}
	}
	return false
}

func newName(n string) string {
	for _, w := range words {
		n = w.re.ReplaceAllStringFunc(n, func(m string) string {
			to := w.to
			switch {
			case m == strings.ToUpper(m):
				return strings.ToUpper(to)
			case m[0] >= 'A' && m[0] <= 'Z':
				return strings.ToUpper(to[:1]) + to[1:]
			}
			return to
		})
	}
	return n
}

func stringish(t types.Type) bool {
	switch u := t.Underlying().(type) {
	case *types.Basic:
		return u.Info()&types.IsString != 0
	case *types.Slice:
		return stringish(u.Elem())
	case *types.Array:
		return stringish(u.Elem())
	case *types.Pointer:
		return stringish(u.Elem())
	case *types.Map:
		return stringish(u.Elem()) || stringish(u.Key())
	}
	return false
}

func main() {
	dir := flag.String("dir", ".", "module directory")
	out := flag.String("json", "", "write findings as JSON")
	rename := flag.Bool("rename", false, "rename the CRED-NAME objects in place")
	flag.Parse()
	pats := flag.Args()
	if len(pats) == 0 {
		pats = []string{"./..."}
	}
	cfg := &packages.Config{Dir: *dir, Tests: true, Mode: packages.NeedName | packages.NeedFiles | packages.NeedSyntax |
		packages.NeedTypes | packages.NeedTypesInfo | packages.NeedImports | packages.NeedDeps | packages.NeedModule}
	pkgs, err := packages.Load(cfg, pats...)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	seen := map[string]bool{}
	var finds []Finding
	add := func(f Finding) {
		k := fmt.Sprintf("%s|%s:%d|%s", f.Rule, f.File, f.Line, f.Name)
		if !seen[k] {
			seen[k] = true
			finds = append(finds, f)
		}
	}
	type edit struct {
		off, n int
		to     string
	}
	edits := map[string][]edit{}
	editSeen := map[string]bool{}
	targets := map[types.Object]bool{}
	loadErrs := 0
	for _, p := range pkgs {
		loadErrs += len(p.Errors)
	}
	if loadErrs > 0 {
		fmt.Fprintf(os.Stderr, "credscan: %d package errors; results may be incomplete\n", loadErrs)
	}
	root, _ := filepath.Abs(*dir)
	rel := func(p string) string {
		if r, err := filepath.Rel(root, p); err == nil {
			return r
		}
		return p
	}
	inMod := func(p *packages.Package) bool { return p.Module != nil && p.Module.Main }

	// pass 1: names
	for _, p := range pkgs {
		if !inMod(p) || p.TypesInfo == nil {
			continue
		}
		for id, obj := range p.TypesInfo.Defs {
			if obj == nil || id.Name == "_" || !credName(id.Name) {
				continue
			}
			var kind string
			switch o := obj.(type) {
			case *types.Const:
				kind = "const"
			case *types.Var:
				kind = "var"
				if o.IsField() {
					kind = "field"
				}
			default:
				continue
			}
			if !stringish(obj.Type()) {
				continue
			}
			pos := p.Fset.Position(id.Pos())
			add(Finding{"CRED-NAME", rel(pos.Filename), pos.Line, id.Name, kind})
			targets[obj] = true
		}
	}
	// pass 2: literal values for credential-named things
	for _, p := range pkgs {
		if !inMod(p) || p.TypesInfo == nil {
			continue
		}
		isLit := func(e ast.Expr) bool {
			tv, ok := p.TypesInfo.Types[e]
			return ok && tv.Value != nil && tv.Value.Kind() == constant.String
		}
		name := func(e ast.Expr) string {
			switch x := e.(type) {
			case *ast.Ident:
				return x.Name
			case *ast.SelectorExpr:
				return x.Sel.Name
			}
			return ""
		}
		for _, f := range p.Syntax {
			ast.Inspect(f, func(n ast.Node) bool {
				report := func(at token.Pos, nm string) {
					pos := p.Fset.Position(at)
					add(Finding{"CRED-DEFAULT", rel(pos.Filename), pos.Line, nm, ""})
				}
				switch x := n.(type) {
				case *ast.ValueSpec:
					for i, id := range x.Names {
						if credName(id.Name) && i < len(x.Values) && isLit(x.Values[i]) {
							report(id.Pos(), id.Name)
						}
					}
				case *ast.AssignStmt:
					if len(x.Lhs) == len(x.Rhs) {
						for i := range x.Lhs {
							if nm := name(x.Lhs[i]); credName(nm) && isLit(x.Rhs[i]) {
								report(x.Lhs[i].Pos(), nm)
							}
						}
					}
				case *ast.KeyValueExpr:
					if nm := name(x.Key); credName(nm) && isLit(x.Value) {
						report(x.Key.Pos(), nm)
					}
				}
				return true
			})
		}
	}
	sort.Slice(finds, func(i, j int) bool {
		a, b := finds[i], finds[j]
		if a.File != b.File {
			return a.File < b.File
		}
		return a.Line < b.Line
	})
	if *rename {
		for _, p := range pkgs {
			if p.TypesInfo == nil {
				continue
			}
			for _, m := range []map[*ast.Ident]types.Object{p.TypesInfo.Defs, p.TypesInfo.Uses} {
				for id, obj := range m {
					if obj == nil || !targets[obj] {
						continue
					}
					pos := p.Fset.Position(id.Pos())
					k := fmt.Sprintf("%s:%d", pos.Filename, pos.Offset)
					if editSeen[k] {
						continue
					}
					editSeen[k] = true
					edits[pos.Filename] = append(edits[pos.Filename], edit{pos.Offset, len(id.Name), newName(id.Name)})
				}
			}
		}
		for file, es := range edits {
			b, err := os.ReadFile(file)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(2)
			}
			sort.Slice(es, func(i, j int) bool { return es[i].off > es[j].off })
			for _, e := range es {
				b = append(b[:e.off], append([]byte(e.to), b[e.off+e.n:]...)...)
			}
			if err := os.WriteFile(file, b, 0o644); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(2)
			}
		}
		fmt.Printf("credscan: renamed %d objects in %d files\n", len(targets), len(edits))
		return
	}
	for _, f := range finds {
		fmt.Printf("%s %s:%d %s %s\n", f.Rule, f.File, f.Line, f.Name, f.Kind)
	}
	if *out != "" {
		b, _ := json.MarshalIndent(finds, "", " ")
		_ = os.WriteFile(*out, b, 0o644)
	}
	fmt.Printf("credscan: %d findings (%d loaded packages)\n", len(finds), len(pkgs))
	if len(finds) > 0 {
		os.Exit(1)
	}
}

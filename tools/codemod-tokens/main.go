// Command codemod-tokens rewrites terraform-provider-aws so that idempotency
// tokens are no longer filled with per-process random IDs.
//
// Upstream sets tokens like `ClientToken: aws.String(create.UniqueId(ctx))`.
// A random token is regenerated on every apply, so it gives no protection
// across a killed-and-restarted apply. This tool replaces those random values
// with the sentinel `idempotency.AutoToken`. At request time the idempotency
// middleware swaps the sentinel for a deterministic, journaled token.
//
// The sentinel contains characters every AWS token pattern rejects, so if the
// middleware were ever missing the call fails loudly instead of silently
// deduplicating.
//
// Rewrite rules, all driven by the generated operation table (ops.json):
//
//	A. struct literal field:  <Field>: <random>          in pkg.<Op>Input{...}
//	B. field assignment:      x.<Field> = <random>       where <Field> is a token member
//	C. local variable:        v := <randomString>        used once, as <Field>: aws.String(v)
//
// <random> is aws.String(R) and R is one of create.UniqueId(ctx),
// create.UUID(ctx), sdkid.UniqueId(), sdkid.PrefixedUniqueId(...),
// id.UniqueId(), id.PrefixedUniqueId(...).
//
// Anything else that touches a token field is written to the report as
// MANUAL, and sites listed in the exclude file are left alone and reported
// as EXCLUDED.
//
// Usage:
//
//	go run ./tools/codemod-tokens -provider ../terraform-provider-aws \
//	    -ops ops.json -exclude tools/codemod-tokens/exclude.txt [-w] > codemod-report.tsv
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/tools/go/ast/astutil"
)

const (
	sdkPrefix       = "github.com/aws/aws-sdk-go-v2/service/"
	idempotencyPkg  = "github.com/hashicorp/terraform-provider-aws/internal/idempotency"
	sentinelExpr    = "idempotency.AutoToken"
	sentinelPtrExpr = "aws.String(idempotency.AutoToken)"
)

type opRow struct {
	GoPackage string `json:"go_package"`
	Operation string `json:"operation"`
	GoField   string `json:"go_field"`
}

type table struct {
	// byInput: "ec2.RunInstancesInput" (import path last element) -> field
	byInput map[string]map[string]bool
	// fields: every token field name that appears anywhere
	fields map[string]bool
}

func loadTable(path string) (*table, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var raw struct {
		Traited   []opRow `json:"traited"`
		Untraited []opRow `json:"untraited"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, err
	}
	t := &table{byInput: map[string]map[string]bool{}, fields: map[string]bool{}}
	for _, r := range append(raw.Traited, raw.Untraited...) {
		k := sdkPrefix + r.GoPackage + "." + r.Operation + "Input"
		if t.byInput[k] == nil {
			t.byInput[k] = map[string]bool{}
		}
		t.byInput[k][r.GoField] = true
		t.fields[r.GoField] = true
	}
	return t, nil
}

// exclude file lines: "<path relative to provider root>:<Field>" or "# comment"
func loadExcludes(path string) (map[string]bool, error) {
	ex := map[string]bool{}
	if path == "" {
		return ex, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		ex[line] = true
	}
	return ex, sc.Err()
}

type edit struct {
	start, end int
	text       string
}

type finding struct {
	file, kind, field, detail string
	line                      int
}

type fileCtx struct {
	fset    *token.FileSet
	file    *ast.File
	src     []byte
	rel     string
	imports map[string]string // local name -> import path
	tab     *table
	ex      map[string]bool
	edits   []edit
	report  []finding
}

func (c *fileCtx) text(n ast.Node) string {
	return string(c.src[c.fset.Position(n.Pos()).Offset:c.fset.Position(n.End()).Offset])
}

func (c *fileCtx) add(kind, field string, n ast.Node, detail string) {
	c.report = append(c.report, finding{c.rel, kind, field, detail, c.fset.Position(n.Pos()).Line})
}

func (c *fileCtx) replace(n ast.Node, text string) {
	c.edits = append(c.edits, edit{c.fset.Position(n.Pos()).Offset, c.fset.Position(n.End()).Offset, text})
}

// isRandomString reports whether e is a call producing a random string ID.
func (c *fileCtx) isRandomString(e ast.Expr) bool {
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok {
		return false
	}
	path := c.imports[pkg.Name]
	switch {
	case strings.HasSuffix(path, "/internal/create"):
		return sel.Sel.Name == "UniqueId" || sel.Sel.Name == "UUID"
	case strings.HasSuffix(path, "/helper/id"): // sdkid / id from terraform-plugin-sdk
		return sel.Sel.Name == "UniqueId" || sel.Sel.Name == "PrefixedUniqueId"
	}
	return false
}

// awsStringArg returns X for aws.String(X), else nil.
func (c *fileCtx) awsStringArg(e ast.Expr) ast.Expr {
	call, ok := e.(*ast.CallExpr)
	if !ok || len(call.Args) != 1 {
		return nil
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "String" {
		return nil
	}
	if pkg, ok := sel.X.(*ast.Ident); !ok || c.imports[pkg.Name] != "github.com/aws/aws-sdk-go-v2/aws" {
		return nil
	}
	return call.Args[0]
}

// inputTypeKey resolves `pkg.XInput` / `&pkg.XInput` to its table key.
func (c *fileCtx) inputTypeKey(t ast.Expr) string {
	sel, ok := t.(*ast.SelectorExpr)
	if !ok {
		return ""
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok {
		return ""
	}
	path := c.imports[pkg.Name]
	if !strings.HasPrefix(path, sdkPrefix) {
		return ""
	}
	return path + "." + sel.Sel.Name
}

func (c *fileCtx) excluded(field string) bool { return c.ex[c.rel+":"+field] }

func (c *fileCtx) run() {
	// Count identifier uses per object so rule C only fires on single-use vars.
	uses := map[*ast.Object]int{}
	ast.Inspect(c.file, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && id.Obj != nil {
			uses[id.Obj]++
		}
		return true
	})

	// handleValue decides what to do with a value assigned to a token field.
	handleValue := func(field string, val ast.Expr, site ast.Node) {
		if c.excluded(field) {
			c.add("EXCLUDED", field, site, c.text(site))
			return
		}
		arg := c.awsStringArg(val)
		if c.text(val) == sentinelPtrExpr {
			return // already rewritten by a previous run
		}
		if id, ok := arg.(*ast.Ident); ok && id.Obj != nil {
			if as, ok := id.Obj.Decl.(*ast.AssignStmt); ok && len(as.Rhs) == 1 && c.text(as.Rhs[0]) == sentinelExpr {
				return // rule C already applied
			}
		}
		switch {
		case arg != nil && c.isRandomString(arg): // rules A/B
			c.replace(val, sentinelPtrExpr)
			c.add("REWRITE", field, site, c.text(val))
		case arg != nil:
			if id, ok := arg.(*ast.Ident); ok && id.Obj != nil {
				if as, ok := id.Obj.Decl.(*ast.AssignStmt); ok && len(as.Lhs) == 1 && len(as.Rhs) == 1 && c.isRandomString(as.Rhs[0]) {
					// defining ident + this use == 2
					if uses[id.Obj] == 2 { // rule C
						c.replace(as.Rhs[0], sentinelExpr)
						c.add("REWRITE", field, site, id.Name+" := "+c.text(as.Rhs[0]))
						return
					}
					c.add("MANUAL", field, site, "random var "+id.Name+" reused elsewhere (e.g. waiter/event filter)")
					return
				}
			}
			c.add("MANUAL", field, site, "non-random value: "+c.text(val))
		default:
			c.add("MANUAL", field, site, "unrecognized value: "+c.text(val))
		}
	}

	ast.Inspect(c.file, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.CompositeLit: // rule A
			key := c.inputTypeKey(n.Type)
			fields := c.tab.byInput[key]
			if fields == nil {
				return true
			}
			for _, elt := range n.Elts {
				kv, ok := elt.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				k, ok := kv.Key.(*ast.Ident)
				if ok && fields[k.Name] {
					handleValue(k.Name, kv.Value, kv)
				}
			}
		case *ast.AssignStmt: // rule B (syntactic: field name must be a known token member)
			if len(n.Lhs) != 1 || len(n.Rhs) != 1 {
				return true
			}
			sel, ok := n.Lhs[0].(*ast.SelectorExpr)
			if !ok || !c.tab.fields[sel.Sel.Name] {
				return true
			}
			if arg := c.awsStringArg(n.Rhs[0]); arg != nil && c.isRandomString(arg) {
				handleValue(sel.Sel.Name, n.Rhs[0], n)
			}
		}
		return true
	})
}

func (c *fileCtx) apply() ([]byte, error) {
	if len(c.edits) == 0 {
		return c.src, nil
	}
	sort.Slice(c.edits, func(i, j int) bool { return c.edits[i].start > c.edits[j].start })
	out := append([]byte(nil), c.src...)
	last := -1
	for _, e := range c.edits {
		if last >= 0 && e.end > last {
			continue // overlapping edit (same node reached twice); keep first
		}
		out = append(out[:e.start], append([]byte(e.text), out[e.end:]...)...)
		last = e.start
	}
	// Fix imports: add idempotency, drop generators that are now unused.
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, c.rel, out, parser.ParseComments)
	if err != nil {
		return nil, fmt.Errorf("reparse %s: %w", c.rel, err)
	}
	astutil.AddImport(fset, f, idempotencyPkg)
	for _, imp := range f.Imports {
		p, _ := strconv.Unquote(imp.Path.Value)
		if strings.HasSuffix(p, "/internal/create") || strings.HasSuffix(p, "/helper/id") {
			if !astutil.UsesImport(f, p) {
				name := ""
				if imp.Name != nil {
					name = imp.Name.Name
				}
				astutil.DeleteNamedImport(fset, f, name, p)
			}
		}
	}
	var buf bytes.Buffer
	if err := format.Node(&buf, fset, f); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func main() {
	provider := flag.String("provider", "", "path to terraform-provider-aws checkout")
	opsPath := flag.String("ops", "", "ops.json from tools/gen-idempotent-ops")
	exPath := flag.String("exclude", "", "exclude list (path:Field per line)")
	write := flag.Bool("w", false, "write changes in place")
	flag.Parse()
	if *provider == "" || *opsPath == "" {
		flag.Usage()
		os.Exit(2)
	}
	tab, err := loadTable(*opsPath)
	must(err)
	ex, err := loadExcludes(*exPath)
	must(err)

	var all []finding
	root := filepath.Join(*provider, "internal", "service")
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, src, parser.ParseComments)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(*provider, path)
		c := &fileCtx{fset: fset, file: f, src: src, rel: filepath.ToSlash(rel), tab: tab, ex: ex, imports: map[string]string{}}
		for _, imp := range f.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			name := p[strings.LastIndex(p, "/")+1:]
			if imp.Name != nil {
				name = imp.Name.Name
			}
			c.imports[name] = p
		}
		c.run()
		all = append(all, c.report...)
		if *write && len(c.edits) > 0 {
			out, err := c.apply()
			if err != nil {
				return err
			}
			return os.WriteFile(path, out, d.Type().Perm()|0o644)
		}
		return nil
	})
	must(err)

	sort.Slice(all, func(i, j int) bool {
		if all[i].kind != all[j].kind {
			return all[i].kind < all[j].kind
		}
		if all[i].file != all[j].file {
			return all[i].file < all[j].file
		}
		return all[i].line < all[j].line
	})
	counts := map[string]int{}
	fmt.Println("kind\tfile:line\tfield\tdetail")
	for _, f := range all {
		counts[f.kind]++
		fmt.Printf("%s\t%s:%d\t%s\t%s\n", f.kind, f.file, f.line, f.field, strings.ReplaceAll(f.detail, "\n", " "))
	}
	fmt.Fprintf(os.Stderr, "REWRITE=%d MANUAL=%d EXCLUDED=%d\n", counts["REWRITE"], counts["MANUAL"], counts["EXCLUDED"])
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

package jsrt

import (
	"sort"
	"strings"

	"github.com/dop251/goja"
	"github.com/dop251/goja/ast"
	"github.com/dop251/goja/parser"
)

type astEdit struct {
	start int
	end   int
	text  string
}

// compileReplCode uses Goja's native ECMAScript AST parser to:
//  1. Persist all top-level bindings (let/const/var/destructuring/function/async function/class)
//     onto globalThis right after each declaration so subsequent turns can reuse them, even if
//     the cell returns early or re-declares the same const name in a later turn.
//  2. Auto-return a bare final ExpressionStatement (REPL ergonomics) while keeping trailing
//     declarations quiet in VM state.
func compileReplCode(code string) (*goja.Program, error) {
	wrapped := "(async () => {\n" + code + "\n})"
	prog, err := parser.ParseFile(nil, "cell.js", wrapped, 0)
	if err != nil {
		// Delegate to goja.Compile so callers get Goja's exact syntax error formatting.
		return goja.Compile("cell.js", wrapped, false)
	}

	stmts := topLevelBodyStatements(prog)
	if len(stmts) == 0 {
		return goja.Compile("cell.js", wrapped, false)
	}

	base := prog.File.Base()
	var edits []astEdit

	for i, stmt := range stmts {
		names := collectTopLevelNames(stmt)
		if len(names) > 0 {
			end := int(stmt.Idx1()) - base
			if end >= 0 && end <= len(wrapped) {
				var sb strings.Builder
				sb.WriteString(";\n")
				for _, name := range names {
					sb.WriteString("globalThis.")
					sb.WriteString(name)
					sb.WriteString(" = ")
					sb.WriteString(name)
					sb.WriteString(";\n")
				}
				edits = append(edits, astEdit{start: end, end: end, text: sb.String()})
			}
			continue
		}

		// Auto-return the final top-level statement when it is a pure expression.
		if i == len(stmts)-1 {
			if exprStmt, ok := stmt.(*ast.ExpressionStatement); ok {
				s0 := int(exprStmt.Expression.Idx0()) - base
				s1 := int(exprStmt.Expression.Idx1()) - base
				if s0 >= 0 && s1 > s0 && s1 <= len(wrapped) {
					exprSrc := strings.TrimSpace(wrapped[s0:s1])
					// AST already guarantees this is an ExpressionStatement (not a BlockStatement),
					// so object literals like `({ a: 1 })` safely wrap inside `return (...)`.
					if exprSrc != "" {
						edits = append(edits, astEdit{
							start: s0,
							end:   s1,
							text:  "return (\n" + exprSrc + "\n)",
						})
					}
				}
			}
		}
	}

	if len(edits) == 0 {
		return goja.Compile("cell.js", wrapped, false)
	}

	// Apply edits in descending source offset order so earlier indices remain valid.
	sort.Slice(edits, func(i, j int) bool {
		if edits[i].start != edits[j].start {
			return edits[i].start > edits[j].start
		}
		return edits[i].end > edits[j].end
	})
	rewritten := wrapped
	for _, e := range edits {
		rewritten = rewritten[:e.start] + e.text + rewritten[e.end:]
	}
	compiled, err := goja.Compile("cell.js", rewritten, false)
	if err != nil {
		// Fall back cleanly to unmodified wrapper if an exotic edge case failed rewrite.
		return goja.Compile("cell.js", wrapped, false)
	}
	return compiled, nil
}

func topLevelBodyStatements(prog *ast.Program) []ast.Statement {
	if prog == nil || len(prog.Body) != 1 {
		return nil
	}
	es, ok := prog.Body[0].(*ast.ExpressionStatement)
	if !ok {
		return nil
	}
	af, ok := es.Expression.(*ast.ArrowFunctionLiteral)
	if !ok {
		return nil
	}
	block, ok := af.Body.(*ast.BlockStatement)
	if !ok {
		return nil
	}
	return block.List
}

func collectTopLevelNames(stmt ast.Statement) []string {
	var names []string
	seen := map[string]bool{}
	add := func(s string) {
		if s != "" && !seen[s] {
			seen[s] = true
			names = append(names, s)
		}
	}

	var walkTarget func(expr ast.Expression)
	walkTarget = func(expr ast.Expression) {
		if expr == nil {
			return
		}
		switch t := expr.(type) {
		case *ast.Identifier:
			add(t.Name.String())
		case *ast.AssignExpression:
			walkTarget(t.Left)
		case *ast.SpreadElement:
			walkTarget(t.Expression)
		case *ast.ArrayPattern:
			for _, el := range t.Elements {
				walkTarget(el)
			}
			walkTarget(t.Rest)
		case *ast.ObjectPattern:
			for _, prop := range t.Properties {
				switch p := prop.(type) {
				case *ast.PropertyShort:
					add(p.Name.Name.String())
				case *ast.PropertyKeyed:
					walkTarget(p.Value)
				}
			}
			walkTarget(t.Rest)
		}
	}

	switch s := stmt.(type) {
	case *ast.LexicalDeclaration:
		for _, b := range s.List {
			if b != nil {
				walkTarget(b.Target)
			}
		}
	case *ast.VariableStatement:
		for _, b := range s.List {
			if b != nil {
				walkTarget(b.Target)
			}
		}
	case *ast.FunctionDeclaration:
		if s.Function != nil && s.Function.Name != nil {
			add(s.Function.Name.Name.String())
		}
	case *ast.ClassDeclaration:
		if s.Class != nil && s.Class.Name != nil {
			add(s.Class.Name.Name.String())
		}
	}
	return names
}

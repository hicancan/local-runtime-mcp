package jsrt

import (
	"sort"
	"strings"

	"github.com/dop251/goja"
	"github.com/dop251/goja/ast"
	"github.com/dop251/goja/file"
	"github.com/dop251/goja/parser"
)

type astEdit struct {
	start int
	end   int
	text  string
}

// compileReplCode uses Goja's native ECMAScript AST parser to:
//  1. Persist all top-level bindings (let/const/var/destructuring/function/async function/class)
//     and function-hoisted `var` / `function` declarations onto globalThis—AND keep live `let`/`var`
//     reassignments synchronized onto globalThis via a `finally` closure even on early `return` or
//     mid-cell `throw`.
//  2. Auto-return a bare final ExpressionStatement (REPL ergonomics) while keeping trailing
//     declarations quiet in VM state.
func compileReplCode(code string) (*goja.Program, error) {
	innerPrefix := "(async () => {\n"
	innerSuffix := "\n})"
	wrapped := innerPrefix + code + innerSuffix
	prog, err := parser.ParseFile(nil, "cell.js", wrapped, 0)
	if err != nil {
		// Delegate to goja.Compile so callers get Goja's exact syntax error formatting.
		return goja.Compile("cell.js", wrapped, false)
	}

	block := topLevelBodyBlock(prog)
	if block == nil || len(block.List) == 0 {
		return goja.Compile("cell.js", wrapped, false)
	}
	stmts := block.List

	base := prog.File.Base()
	blockStart := int(block.LeftBrace) - base + 1
	blockEnd := int(block.RightBrace) - base
	var edits []astEdit
	needSyncWrapper := false

	// Collect hoisted function declarations (persist even if declared after an early `return`)
	// and hoisted block-level `var` bindings (e.g. `if (ok) { var x = 1; }`).
	var hoistedFuncs []string
	var hoistedVars []string
	topVarSet := map[string]bool{}
	for _, stmt := range stmts {
		switch s := stmt.(type) {
		case *ast.FunctionDeclaration:
			if s.Function != nil && s.Function.Name != nil {
				hoistedFuncs = append(hoistedFuncs, s.Function.Name.Name.String())
			}
		case *ast.VariableStatement:
			for _, name := range collectTopLevelNames(stmt) {
				topVarSet[name] = true
			}
		default:
			for _, name := range collectNestedVars(stmt) {
				if !topVarSet[name] {
					topVarSet[name] = true
					hoistedVars = append(hoistedVars, name)
				}
			}
		}
	}

	if len(hoistedFuncs) > 0 || len(hoistedVars) > 0 {
		needSyncWrapper = true
		var sb strings.Builder
		sb.WriteString("\n")
		for _, name := range hoistedFuncs {
			sb.WriteString("globalThis.")
			sb.WriteString(name)
			sb.WriteString(" = ")
			sb.WriteString(name)
			sb.WriteString("; __lrmcp_sync.push(() => { globalThis.")
			sb.WriteString(name)
			sb.WriteString(" = ")
			sb.WriteString(name)
			sb.WriteString("; });\n")
		}
		for _, name := range hoistedVars {
			sb.WriteString("__lrmcp_sync.push(() => { if (typeof ")
			sb.WriteString(name)
			sb.WriteString(" !== 'undefined') globalThis.")
			sb.WriteString(name)
			sb.WriteString(" = ")
			sb.WriteString(name)
			sb.WriteString("; });\n")
		}
		edits = append(edits, astEdit{start: blockStart, end: blockStart, text: sb.String()})
	}

	for i, stmt := range stmts {
		names := collectTopLevelNames(stmt)
		if len(names) > 0 {
			// Top-level FunctionDeclaration is already persisted at blockStart via hoisting.
			if _, isFnDecl := stmt.(*ast.FunctionDeclaration); isFnDecl {
				continue
			}
			var end int
			switch stmt.(type) {
			case *ast.ClassDeclaration:
				end = int(trueIdx1(stmt)) - base
			default:
				_, end = statementEndAfter(wrapped, int(trueIdx1(stmt))-base, blockEnd)
			}
			// Mutable bindings (`let`, `var`, `class`) sync their live value on cell exit/return/throw
			// via __lrmcp_sync so same-turn reassignments (e.g. inside try/catch/if/for) persist cleanly.
			isMutable := true
			if lex, ok := stmt.(*ast.LexicalDeclaration); ok && lex.Token.String() == "const" {
				isMutable = false
			}
			if end >= 0 && end <= len(wrapped) {
				var sb strings.Builder
				sb.WriteString(";\n")
				for _, name := range names {
					sb.WriteString("globalThis.")
					sb.WriteString(name)
					sb.WriteString(" = ")
					sb.WriteString(name)
					sb.WriteString(";\n")
					if isMutable {
						needSyncWrapper = true
						sb.WriteString("__lrmcp_sync.push(() => { globalThis.")
						sb.WriteString(name)
						sb.WriteString(" = ")
						sb.WriteString(name)
						sb.WriteString("; });\n")
					}
				}
				edits = append(edits, astEdit{start: end, end: end, text: sb.String()})
			}
			continue
		}

		// Auto-return the final top-level statement when it is a pure expression.
		if i == len(stmts)-1 {
			if exprStmt, ok := stmt.(*ast.ExpressionStatement); ok {
				prevEnd := blockStart
				if i > 0 {
					prev := stmts[i-1]
					switch prev.(type) {
					case *ast.FunctionDeclaration, *ast.ClassDeclaration:
						prevEnd = int(trueIdx1(prev)) - base
					default:
						_, prevEnd = statementEndAfter(wrapped, int(trueIdx1(prev))-base, blockEnd)
					}
				}
				innerStart := int(exprStmt.Expression.Idx0()) - base
				innerEnd := int(trueIdx1(exprStmt.Expression)) - base
				if innerStart < prevEnd {
					prevEnd = innerStart
				}
				s0 := skipWSAndComments(wrapped, prevEnd, innerStart)
				s1, _ := statementEndAfter(wrapped, innerEnd, blockEnd)
				if s0 >= 0 && s1 > s0 && s1 <= blockEnd {
					exprSrc := strings.TrimSpace(wrapped[s0:s1])
					// AST already guarantees this is an ExpressionStatement (not a BlockStatement),
					// and [s0, s1] preserves any outer/side parentheses stripped by Goja's AST.
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
	if needSyncWrapper {
		rewritten = "(async () => {\nconst __lrmcp_sync = [];\ntry {\nreturn await " + rewritten + "();\n} finally {\nfor (let __i = 0; __i < __lrmcp_sync.length; __i++) { try { __lrmcp_sync[__i](); } catch (_) {} }\n}\n})"
	}
	compiled, err := goja.Compile("cell.js", rewritten, false)
	if err != nil {
		// Fall back cleanly to unmodified wrapper if an exotic edge case failed rewrite.
		return goja.Compile("cell.js", wrapped, false)
	}
	return compiled, nil
}

func topLevelBodyBlock(prog *ast.Program) *ast.BlockStatement {
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
	return block
}

// skipWSAndComments advances pos past ECMAScript whitespace and line/block comments up to limit.
func skipWSAndComments(src string, pos, limit int) int {
	for pos < limit {
		c := src[pos]
		if c == ' ' || c == '\t' || c == '\r' || c == '\n' || c == '\f' || c == '\v' {
			pos++
			continue
		}
		if c == '/' && pos+1 < limit {
			if src[pos+1] == '/' {
				pos += 2
				for pos < limit && src[pos] != '\n' && src[pos] != '\r' {
					pos++
				}
				continue
			}
			if src[pos+1] == '*' {
				pos += 2
				for pos+1 < limit && !(src[pos] == '*' && src[pos+1] == '/') {
					pos++
				}
				if pos+1 < limit {
					pos += 2
				} else {
					pos = limit
				}
				continue
			}
		}
		break
	}
	return pos
}

// statementEndAfter expands past closing ')' tokens stripped by Goja's AST from parenthesized
// expressions/initializers, returning both the expression end (before ';') and statement end (after optional ';').
func statementEndAfter(src string, idx1, limit int) (exprEnd, stmtEnd int) {
	if idx1 < 0 {
		idx1 = 0
	}
	if idx1 > limit {
		idx1 = limit
	}
	pos := idx1
	for {
		next := skipWSAndComments(src, pos, limit)
		if next < limit && src[next] == ')' {
			pos = next + 1
			continue
		}
		break
	}
	exprEnd = pos
	next := skipWSAndComments(src, exprEnd, limit)
	if next < limit && src[next] == ';' {
		return exprEnd, next + 1
	}
	return exprEnd, exprEnd
}

// trueIdx1 works around Goja's upstream NewExpression.Idx1() bug where zero-argument
// `new Foo()` leaves ArgumentList == nil and erroneously returns Callee.Idx1() before `()`,
// propagating that exact end offset through right-edge wrapper nodes.
func trueIdx1(n ast.Node) file.Idx {
	if n == nil {
		return 0
	}
	switch x := n.(type) {
	case *ast.NewExpression:
		if x.RightParenthesis > 0 {
			return x.RightParenthesis + 1
		}
		return trueIdx1(x.Callee)
	case *ast.ArrowFunctionLiteral:
		if eb, ok := x.Body.(*ast.ExpressionBody); ok {
			if end := trueIdx1(eb.Expression); end > x.End {
				return end
			}
		}
		return x.End
	case *ast.LexicalDeclaration:
		if len(x.List) > 0 {
			return trueIdx1(x.List[len(x.List)-1])
		}
	case *ast.VariableStatement:
		if len(x.List) > 0 {
			return trueIdx1(x.List[len(x.List)-1])
		}
	case *ast.Binding:
		if x.Initializer != nil {
			return trueIdx1(x.Initializer)
		}
		return trueIdx1(x.Target)
	case *ast.ExpressionStatement:
		return trueIdx1(x.Expression)
	case *ast.AssignExpression:
		return trueIdx1(x.Right)
	case *ast.AwaitExpression:
		return trueIdx1(x.Argument)
	case *ast.BinaryExpression:
		return trueIdx1(x.Right)
	case *ast.ConditionalExpression:
		return trueIdx1(x.Alternate)
	case *ast.SequenceExpression:
		if len(x.Sequence) > 0 {
			return trueIdx1(x.Sequence[len(x.Sequence)-1])
		}
	case *ast.UnaryExpression:
		if !x.Postfix {
			return trueIdx1(x.Operand)
		}
	case *ast.OptionalChain:
		return trueIdx1(x.Expression)
	case *ast.Optional:
		return trueIdx1(x.Expression)
	case *ast.ReturnStatement:
		if x.Argument != nil {
			return trueIdx1(x.Argument)
		}
	case *ast.ThrowStatement:
		return trueIdx1(x.Argument)
	case *ast.IfStatement:
		if x.Alternate != nil {
			return trueIdx1(x.Alternate)
		}
		return trueIdx1(x.Consequent)
	case *ast.ForStatement:
		return trueIdx1(x.Body)
	case *ast.ForInStatement:
		return trueIdx1(x.Body)
	case *ast.ForOfStatement:
		return trueIdx1(x.Body)
	case *ast.WhileStatement:
		return trueIdx1(x.Body)
	case *ast.WithStatement:
		return trueIdx1(x.Body)
	case *ast.LabelledStatement:
		return trueIdx1(x.Statement)
	}
	return n.Idx1()
}

func collectBindingTargetNames(expr ast.Expression, add func(string)) {
	if expr == nil {
		return
	}
	switch t := expr.(type) {
	case *ast.Identifier:
		add(t.Name.String())
	case *ast.AssignExpression:
		collectBindingTargetNames(t.Left, add)
	case *ast.SpreadElement:
		collectBindingTargetNames(t.Expression, add)
	case *ast.ArrayPattern:
		for _, el := range t.Elements {
			collectBindingTargetNames(el, add)
		}
		collectBindingTargetNames(t.Rest, add)
	case *ast.ObjectPattern:
		for _, prop := range t.Properties {
			switch p := prop.(type) {
			case *ast.PropertyShort:
				add(p.Name.Name.String())
			case *ast.PropertyKeyed:
				collectBindingTargetNames(p.Value, add)
			}
		}
		collectBindingTargetNames(t.Rest, add)
	}
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

	switch s := stmt.(type) {
	case *ast.LexicalDeclaration:
		for _, b := range s.List {
			if b != nil {
				collectBindingTargetNames(b.Target, add)
			}
		}
	case *ast.VariableStatement:
		for _, b := range s.List {
			if b != nil {
				collectBindingTargetNames(b.Target, add)
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

// collectNestedVars walks non-function control-flow statements to find function-hoisted `var` declarations.
func collectNestedVars(stmt ast.Statement) []string {
	var names []string
	seen := map[string]bool{}
	add := func(s string) {
		if s != "" && !seen[s] {
			seen[s] = true
			names = append(names, s)
		}
	}
	var walk func(s ast.Statement)
	walk = func(s ast.Statement) {
		if s == nil {
			return
		}
		switch x := s.(type) {
		case *ast.VariableStatement:
			if x == nil {
				return
			}
			for _, b := range x.List {
				if b != nil {
					collectBindingTargetNames(b.Target, add)
				}
			}
		case *ast.BlockStatement:
			if x == nil {
				return
			}
			for _, child := range x.List {
				walk(child)
			}
		case *ast.IfStatement:
			if x == nil {
				return
			}
			walk(x.Consequent)
			walk(x.Alternate)
		case *ast.ForStatement:
			if x == nil {
				return
			}
			if vdl, ok := x.Initializer.(*ast.ForLoopInitializerVarDeclList); ok && vdl != nil {
				for _, b := range vdl.List {
					if b != nil {
						collectBindingTargetNames(b.Target, add)
					}
				}
			}
			walk(x.Body)
		case *ast.ForInStatement:
			if x == nil {
				return
			}
			if fv, ok := x.Into.(*ast.ForIntoVar); ok && fv != nil && fv.Binding != nil {
				collectBindingTargetNames(fv.Binding.Target, add)
			}
			walk(x.Body)
		case *ast.ForOfStatement:
			if x == nil {
				return
			}
			if fv, ok := x.Into.(*ast.ForIntoVar); ok && fv != nil && fv.Binding != nil {
				collectBindingTargetNames(fv.Binding.Target, add)
			}
			walk(x.Body)
		case *ast.WhileStatement:
			if x != nil {
				walk(x.Body)
			}
		case *ast.DoWhileStatement:
			if x != nil {
				walk(x.Body)
			}
		case *ast.WithStatement:
			if x != nil {
				walk(x.Body)
			}
		case *ast.SwitchStatement:
			if x == nil {
				return
			}
			for _, c := range x.Body {
				if c != nil {
					for _, st := range c.Consequent {
						walk(st)
					}
				}
			}
		case *ast.TryStatement:
			if x == nil {
				return
			}
			if x.Body != nil {
				walk(x.Body)
			}
			if x.Catch != nil && x.Catch.Body != nil {
				walk(x.Catch.Body)
			}
			if x.Finally != nil {
				walk(x.Finally)
			}
		case *ast.LabelledStatement:
			if x != nil {
				walk(x.Statement)
			}
		}
	}
	walk(stmt)
	return names
}

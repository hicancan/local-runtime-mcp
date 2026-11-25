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

	block := topLevelBodyBlock(prog)
	if block == nil || len(block.List) == 0 {
		return goja.Compile("cell.js", wrapped, false)
	}
	stmts := block.List

	base := prog.File.Base()
	blockStart := int(block.LeftBrace) - base + 1
	blockEnd := int(block.RightBrace) - base
	var edits []astEdit

	for i, stmt := range stmts {
		names := collectTopLevelNames(stmt)
		if len(names) > 0 {
			var end int
			switch stmt.(type) {
			case *ast.FunctionDeclaration, *ast.ClassDeclaration:
				end = int(trueIdx1(stmt)) - base
			default:
				_, end = statementEndAfter(wrapped, int(trueIdx1(stmt))-base, blockEnd)
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

package jsrt

import (
	"strings"
	"unicode"
)

// wrapReplCode wraps arbitrary async JS code inside an async IIFE while hoisting
// top-level declarations (let/const/var/function/async function) onto globalThis
// so variables and helper functions persist cleanly across run turns.
func wrapReplCode(code string) string {
	rewritten := hoistTopLevelDeclarations(code)
	return "(async () => {\n" + rewritten + "\n})"
}

func hoistTopLevelDeclarations(src string) string {
	var out strings.Builder
	out.Grow(len(src) + 128)

	n := len(src)
	braceDepth := 0
	parenDepth := 0
	bracketDepth := 0
	stmtStart := true

	i := 0
	for i < n {
		ch := src[i]

		// Line comment
		if ch == '/' && i+1 < n && src[i+1] == '/' {
			start := i
			i += 2
			for i < n && src[i] != '\n' {
				i++
			}
			out.WriteString(src[start:i])
			continue
		}
		// Block comment
		if ch == '/' && i+1 < n && src[i+1] == '*' {
			start := i
			i += 2
			for i+1 < n && !(src[i] == '*' && src[i+1] == '/') {
				i++
			}
			if i+1 < n {
				i += 2
			}
			out.WriteString(src[start:i])
			continue
		}
		// Single or double quoted string
		if ch == '\'' || ch == '"' {
			quote := ch
			start := i
			i++
			for i < n {
				if src[i] == '\\' && i+1 < n {
					i += 2
					continue
				}
				if src[i] == quote {
					i++
					break
				}
				i++
			}
			out.WriteString(src[start:i])
			stmtStart = false
			continue
		}
		// Template literal (preserve nested ${...} cleanly)
		if ch == '`' {
			start := i
			i++
			exprDepth := 0
			for i < n {
				if src[i] == '\\' && i+1 < n {
					i += 2
					continue
				}
				if exprDepth == 0 {
					if src[i] == '`' {
						i++
						break
					}
					if src[i] == '$' && i+1 < n && src[i+1] == '{' {
						exprDepth = 1
						i += 2
						continue
					}
					i++
					continue
				}
				if src[i] == '{' {
					exprDepth++
				} else if src[i] == '}' {
					exprDepth--
				}
				i++
			}
			out.WriteString(src[start:i])
			stmtStart = false
			continue
		}

		// Track nesting depths
		switch ch {
		case '{':
			braceDepth++
			out.WriteByte(ch)
			i++
			stmtStart = false
			continue
		case '}':
			if braceDepth > 0 {
				braceDepth--
			}
			out.WriteByte(ch)
			i++
			if braceDepth == 0 && parenDepth == 0 && bracketDepth == 0 {
				stmtStart = true
			}
			continue
		case '(':
			parenDepth++
			out.WriteByte(ch)
			i++
			stmtStart = false
			continue
		case ')':
			if parenDepth > 0 {
				parenDepth--
			}
			out.WriteByte(ch)
			i++
			continue
		case '[':
			bracketDepth++
			out.WriteByte(ch)
			i++
			stmtStart = false
			continue
		case ']':
			if bracketDepth > 0 {
				bracketDepth--
			}
			out.WriteByte(ch)
			i++
			continue
		case ';':
			out.WriteByte(ch)
			i++
			if braceDepth == 0 && parenDepth == 0 && bracketDepth == 0 {
				stmtStart = true
			}
			continue
		case '\n', '\r':
			out.WriteByte(ch)
			i++
			if braceDepth == 0 && parenDepth == 0 && bracketDepth == 0 {
				stmtStart = true
			}
			continue
		case ' ', '\t':
			out.WriteByte(ch)
			i++
			continue
		}

		// Only consider hoisting at true top-level statement start.
		if stmtStart && braceDepth == 0 && parenDepth == 0 && bracketDepth == 0 && isIdentStart(ch) {
			wordEnd := i
			for wordEnd < n && isIdentPart(src[wordEnd]) {
				wordEnd++
			}
			word := src[i:wordEnd]

			if word == "let" || word == "const" || word == "var" {
				j := skipSpaces(src, wordEnd)
				if j < n && isIdentStart(src[j]) {
					nameEnd := j
					for nameEnd < n && isIdentPart(src[nameEnd]) {
						nameEnd++
					}
					name := src[j:nameEnd]
					k := skipSpaces(src, nameEnd)
					if k < n && src[k] == '=' {
						// Simple top-level binding: `const foo = ...` -> `globalThis.foo = ...`
						out.WriteString("globalThis.")
						out.WriteString(name)
						out.WriteByte(' ')
						i = k
						stmtStart = false
						continue
					}
				}
				// Destructuring `const {a, b} = ...` or `const [x, y] = ...`
				if j < n && (src[j] == '{' || src[j] == '[') {
					if endIdx, names, ok := parseDestructureDecl(src, j); ok {
						// Find the end of the initializer statement cleanly.
						stmtEnd := findStatementEnd(src, endIdx)
						if stmtEnd > endIdx {
							out.WriteString("var ")
							out.WriteString(src[j:stmtEnd])
							out.WriteString("; ")
							for _, name := range names {
								out.WriteString("globalThis.")
								out.WriteString(name)
								out.WriteString(" = ")
								out.WriteString(name)
								out.WriteString("; ")
							}
							i = stmtEnd
							stmtStart = true
							continue
						}
					}
				}
			}

			if word == "function" {
				j := skipSpaces(src, wordEnd)
				if j < n && isIdentStart(src[j]) {
					nameEnd := j
					for nameEnd < n && isIdentPart(src[nameEnd]) {
						nameEnd++
					}
					name := src[j:nameEnd]
					out.WriteString("globalThis.")
					out.WriteString(name)
					out.WriteString(" = function ")
					out.WriteString(name)
					i = nameEnd
					stmtStart = false
					continue
				}
			}

			if word == "async" {
				j := skipSpaces(src, wordEnd)
				if strings.HasPrefix(src[j:], "function") && (j+8 >= n || !isIdentPart(src[j+8])) {
					k := skipSpaces(src, j+8)
					if k < n && isIdentStart(src[k]) {
						nameEnd := k
						for nameEnd < n && isIdentPart(src[nameEnd]) {
							nameEnd++
						}
						name := src[k:nameEnd]
						out.WriteString("globalThis.")
						out.WriteString(name)
						out.WriteString(" = async function ")
						out.WriteString(name)
						i = nameEnd
						stmtStart = false
						continue
					}
				}
			}

			out.WriteString(word)
			i = wordEnd
			stmtStart = false
			continue
		}

		out.WriteByte(ch)
		i++
		stmtStart = false
	}
	return out.String()
}

func skipSpaces(s string, i int) int {
	for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
		i++
	}
	return i
}

func isIdentStart(b byte) bool {
	return b == '_' || b == '$' || unicode.IsLetter(rune(b))
}

func isIdentPart(b byte) bool {
	return isIdentStart(b) || unicode.IsDigit(rune(b))
}

// parseDestructureDecl scans `{ a, b: c }` or `[x, y]` starting at start,
// returning the index after `=`, bound identifier names, and ok.
func parseDestructureDecl(src string, start int) (int, []string, bool) {
	open := src[start]
	closeCh := byte('}')
	if open == '[' {
		closeCh = ']'
	}
	depth := 0
	i := start
	for i < len(src) {
		if src[i] == open {
			depth++
		} else if src[i] == closeCh {
			depth--
			if depth == 0 {
				i++
				break
			}
		}
		i++
	}
	if depth != 0 {
		return 0, nil, false
	}
	eq := skipSpaces(src, i)
	if eq >= len(src) || src[eq] != '=' {
		return 0, nil, false
	}
	pattern := src[start+1 : i-1]
	parts := strings.Split(pattern, ",")
	var names []string
	for _, part := range parts {
		part = strings.TrimSpace(part)
		part = strings.TrimPrefix(part, "...")
		if part == "" {
			continue
		}
		// Strip default value `x = 1`
		if eqIdx := strings.IndexByte(part, '='); eqIdx >= 0 {
			part = strings.TrimSpace(part[:eqIdx])
		}
		// For object rename `orig: alias`, bind `alias`
		if colIdx := strings.IndexByte(part, ':'); colIdx >= 0 {
			part = strings.TrimSpace(part[colIdx+1:])
		}
		if len(part) > 0 && isIdentStart(part[0]) {
			valid := true
			for k := 1; k < len(part); k++ {
				if !isIdentPart(part[k]) {
					valid = false
					break
				}
			}
			if valid {
				names = append(names, part)
			}
		}
	}
	if len(names) == 0 {
		return 0, nil, false
	}
	return eq + 1, names, true
}

func findStatementEnd(src string, start int) int {
	n := len(src)
	brace, paren, bracket := 0, 0, 0
	for i := start; i < n; i++ {
		ch := src[i]
		if ch == '\'' || ch == '"' {
			q := ch
			i++
			for i < n {
				if src[i] == '\\' && i+1 < n {
					i += 2
					continue
				}
				if src[i] == q {
					break
				}
				i++
			}
			continue
		}
		if ch == '`' {
			i++
			for i < n {
				if src[i] == '\\' && i+1 < n {
					i += 2
					continue
				}
				if src[i] == '`' {
					break
				}
				i++
			}
			continue
		}
		switch ch {
		case '{':
			brace++
		case '}':
			if brace > 0 {
				brace--
			}
		case '(':
			paren++
		case ')':
			if paren > 0 {
				paren--
			}
		case '[':
			bracket++
		case ']':
			if bracket > 0 {
				bracket--
			}
		case ';':
			if brace == 0 && paren == 0 && bracket == 0 {
				return i
			}
		case '\n':
			if brace == 0 && paren == 0 && bracket == 0 {
				return i
			}
		}
	}
	return n
}

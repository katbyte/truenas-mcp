package main

// python.go reads as much of Python as the secrets are found in, in the TrueNAS source: classes with their bases, fields and decorators, the
// functions a class holds and what each is decorated with, and the names given to types. Nothing is run. Everything else is stepped over, read
// only far enough to know where it ends and which block it is in.

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"
)

// tokenKind is which of the four kinds of word Python is written in a token is.
type tokenKind int

const (
	tokenName tokenKind = iota
	tokenNumber
	tokenString
	tokenOperator
)

// token is one word of Python: a name or keyword, a number, a string as written (quotes and all), or an operator or bracket.
type token struct {
	kind tokenKind
	text string
	line int
}

// logicalLine is one statement's line, however many lines of the file it was written across, with how far in it starts.
type logicalLine struct {
	indent int
	line   int
	tokens []token
}

// operators are Python's operators and brackets, longest first, so that "**=" is read as one and not as "**" and "=".
var operators = []string{"**=", "//=", ">>=", "<<=", "...", "!=", "%=", "&=", "**", "*=", "+=", "-=", "->", "//", "/=", ":=", "<<", "<=", "==", ">=", ">>", "@=", "^=", "|=", "(", ")", "[", "]", "{", "}", ":", ",", ";", ".", "+", "-", "*", "/", "%", "|", "&", "^", "~", "<", ">", "=", "@", "!"}

// keywords are the names Python keeps for itself. The soft ones (match, case, type, _) are left out, as they are names everywhere but at the
// start of their own statements.
var keywords = map[string]bool{"False": true, "None": true, "True": true, "and": true, "as": true, "assert": true, "async": true, "await": true, "break": true, "class": true, "continue": true, "def": true, "del": true, "elif": true, "else": true, "except": true, "finally": true, "for": true, "from": true, "global": true, "if": true, "import": true, "in": true, "is": true, "lambda": true, "nonlocal": true, "not": true, "or": true, "pass": true, "raise": true, "return": true, "try": true, "while": true, "with": true, "yield": true}

// stringPrefixes are the letters a string may be started with, lowercased: raw, bytes, formatted, template, and unicode, which means nothing.
var stringPrefixes = map[string]bool{"r": true, "u": true, "b": true, "br": true, "rb": true, "f": true, "fr": true, "rf": true, "t": true, "tr": true, "rt": true}

// tokenizer splits Python source into its statements' lines.
type tokenizer struct {
	src  string
	at   int
	line int
}

// tokenize splits Python source into the lines of its statements, each with the tokens it is written in.
func tokenize(src string) ([]logicalLine, error) {
	t := &tokenizer{src: src, line: 1}

	var lines []logicalLine
	var current []token
	indent, first, depth, starting := 0, 0, 0, true
	for t.at < len(src) {
		if starting {
			// how far in a statement starts says which block it is in; a line of nothing or only a comment says nothing
			column := 0
			for ; t.at < len(src) && strings.IndexByte(" \t\f", src[t.at]) >= 0; t.at++ {
				switch src[t.at] {
				case ' ':
					column++
				case '\t':
					column = column/8*8 + 8
				default:
					column = 0
				}
			}
			if t.at < len(src) && strings.IndexByte("\r\n#", src[t.at]) >= 0 {
				t.toLineEnd()
				t.at++
				t.line++

				continue
			}
			indent, first, starting = column, t.line, false
		}
		if t.at >= len(src) {
			break
		}

		c := src[t.at]
		switch {
		case c == ' ' || c == '\t' || c == '\f' || c == '\r':
			t.at++

		case c == '\n':
			t.at++
			t.line++
			if depth == 0 && len(current) > 0 {
				lines = append(lines, logicalLine{indent: indent, line: first, tokens: current})
				current, starting = nil, true
			}

		case c == '#':
			t.toLineEnd()

		case c == '\\':
			rest := strings.TrimPrefix(src[t.at+1:], "\r")
			if !strings.HasPrefix(rest, "\n") {
				return nil, fmt.Errorf("line %d: a backslash that does not end the line", t.line)
			}
			t.at = len(src) - len(rest) + 1
			t.line++

		case c == '"' || c == '\'':
			s, err := t.str()
			if err != nil {
				return nil, err
			}
			current = append(current, s)

		case isNameStart(src[t.at:]):
			start := t.at
			t.at = nameEnd(src, t.at)
			if t.at < len(src) && (src[t.at] == '"' || src[t.at] == '\'') && stringPrefixes[strings.ToLower(src[start:t.at])] {
				t.at = start
				s, err := t.str()
				if err != nil {
					return nil, err
				}
				current = append(current, s)

				continue
			}
			current = append(current, token{kind: tokenName, text: src[start:t.at], line: t.line})

		case isDigit(c) || c == '.' && t.at+1 < len(src) && isDigit(src[t.at+1]):
			current = append(current, t.number())

		default:
			i := slices.IndexFunc(operators, func(op string) bool { return strings.HasPrefix(src[t.at:], op) })
			if i < 0 {
				return nil, fmt.Errorf("line %d: %q is not Python", t.line, c)
			}
			op := operators[i]
			t.at += len(op)

			switch op {
			case "(", "[", "{":
				depth++
			case ")", "]", "}":
				depth--
				if depth < 0 {
					return nil, fmt.Errorf("line %d: a %s that closes nothing", t.line, op)
				}
			}
			current = append(current, token{kind: tokenOperator, text: op, line: t.line})
		}
	}

	if depth != 0 {
		return nil, fmt.Errorf("line %d: a bracket that is never closed", t.line)
	}
	if len(current) > 0 {
		lines = append(lines, logicalLine{indent: indent, line: first, tokens: current})
	}

	return lines, nil
}

// toLineEnd steps to the end of the line, and no further.
func (t *tokenizer) toLineEnd() {
	if end := strings.IndexByte(t.src[t.at:], '\n'); end >= 0 {
		t.at += end
	} else {
		t.at = len(t.src)
	}
}

// number reads a number as written, in any of the ways Python has of writing one.
func (t *tokenizer) number() token {
	start := t.at
	hex := strings.HasPrefix(strings.ToLower(t.src[t.at:]), "0x")
	for t.at < len(t.src) {
		c := t.src[t.at]
		exponent := !hex && (c == '+' || c == '-') && (t.src[t.at-1] == 'e' || t.src[t.at-1] == 'E')
		if !isDigit(c) && !isLetter(c) && c != '_' && c != '.' && !exponent {
			break
		}
		t.at++
	}

	return token{kind: tokenNumber, text: t.src[start:t.at], line: t.line}
}

// str reads a string, its prefix and quotes included, from where its prefix starts.
func (t *tokenizer) str() (token, error) {
	start, line := t.at, t.line
	for isLetter(t.src[t.at]) {
		t.at++
	}
	prefix := strings.ToLower(t.src[start:t.at])

	quote := t.src[t.at : t.at+1]
	if strings.HasPrefix(t.src[t.at:], strings.Repeat(quote, 3)) {
		quote = strings.Repeat(quote, 3)
	}
	t.at += len(quote)

	if err := t.through(quote, strings.Contains(prefix, "r"), strings.ContainsAny(prefix, "ft")); err != nil {
		return token{}, fmt.Errorf("line %d: %w", line, err)
	}

	return token{kind: tokenString, text: t.src[start:t.at], line: line}, nil
}

// through steps over the rest of a string, to and past the quote that ends it. A formatted string's values are stepped over as Python, so
// that a quote inside one does not end the string.
func (t *tokenizer) through(quote string, raw, formatted bool) error {
	for t.at < len(t.src) {
		c := t.src[t.at]
		switch {
		case strings.HasPrefix(t.src[t.at:], quote):
			t.at += len(quote)

			return nil

		case c == '\\':
			next := t.src[t.at+1:]
			switch {
			case formatted && !raw && strings.HasPrefix(next, "N{"):
				// a character by its name, whose braces hold no value
				end := strings.IndexByte(next, '}')
				if end < 0 {
					return errors.New("a character named and the name never ended")
				}
				t.at += end + 2
			case formatted && (strings.HasPrefix(next, "{") || strings.HasPrefix(next, "}")):
				t.at++
			default:
				if strings.HasPrefix(next, "\n") {
					t.line++
				}
				t.at += 2
			}

		case c == '\n':
			if len(quote) == 1 {
				return errors.New("a string that runs off the end of its line")
			}
			t.line++
			t.at++

		case formatted && c == '{':
			if strings.HasPrefix(t.src[t.at:], "{{") {
				t.at += 2

				continue
			}
			t.at++
			if err := t.value(); err != nil {
				return err
			}

		case formatted && strings.HasPrefix(t.src[t.at:], "}}"):
			t.at += 2

		default:
			t.at++
		}
	}

	return errors.New("a string that never ends")
}

// value steps over a value written in a formatted string, to and past the brace that ends it.
func (t *tokenizer) value() error {
	depth := 0
	for t.at < len(t.src) {
		c := t.src[t.at]
		switch {
		case c == '\n':
			t.line++
			t.at++

		case c == '#':
			t.toLineEnd()

		case c == '"' || c == '\'':
			if _, err := t.str(); err != nil {
				return err
			}

		case isNameStart(t.src[t.at:]):
			start := t.at
			t.at = nameEnd(t.src, t.at)
			if t.at < len(t.src) && (t.src[t.at] == '"' || t.src[t.at] == '\'') && stringPrefixes[strings.ToLower(t.src[start:t.at])] {
				t.at = start
				if _, err := t.str(); err != nil {
					return err
				}
			}

		case c == '(' || c == '[' || c == '{':
			depth++
			t.at++

		case c == ')' || c == ']':
			depth--
			t.at++

		case c == '}':
			t.at++
			if depth == 0 {
				return nil
			}
			depth--

		case c == ':' && depth == 0:
			// how the value is to be written, which may hold values of its own
			t.at++

			return t.spec()

		default:
			t.at++
		}
	}

	return errors.New("a value in a formatted string that never ends")
}

// spec steps over how a value in a formatted string is to be written, to and past the brace that ends the value.
func (t *tokenizer) spec() error {
	for t.at < len(t.src) {
		switch t.src[t.at] {
		case '{':
			t.at++
			if err := t.value(); err != nil {
				return err
			}
		case '}':
			t.at++

			return nil
		case '\n':
			t.line++
			t.at++
		default:
			t.at++
		}
	}

	return errors.New("a value in a formatted string that never ends")
}

func isDigit(c byte) bool {
	return c >= '0' && c <= '9'
}

func isLetter(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// isNameStart is whether a name starts here: a letter, an underscore, or a letter of another alphabet, which Python takes in names too.
func isNameStart(s string) bool {
	return isLetter(s[0]) || s[0] == '_' || s[0] >= utf8.RuneSelf
}

// nameEnd is where the name that starts at i ends.
func nameEnd(src string, i int) int {
	for i < len(src) && (isLetter(src[i]) || isDigit(src[i]) || src[i] == '_' || src[i] >= utf8.RuneSelf) {
		i++
	}

	return i
}

// stmtKind is which kind of statement a stmt is, as far as the secrets go.
type stmtKind int

const (
	stmtOther     stmtKind = iota // anything the secrets do not depend on: an import, a call, a return...
	stmtClass                     // class Name(Base): ...
	stmtDef                       // def name(...): ..., or async def
	stmtAssign                    // name = value, or a = b = value
	stmtAnnotated                 // name: annotation, or name: annotation = value
	stmtBlock                     // if, for, while, try, with, match and the clauses of each: only the statements inside matter
)

// stmt is one statement, with the statements it holds.
type stmt struct {
	kind       stmtKind
	line       int
	name       string  // a class's or function's name, the name annotated, or the first name assigned ("" when that is not a plain name)
	targets    int     // how many things a value is assigned to
	bases      []*expr // a class's bases, without its keyword arguments
	decorators []*expr
	annotation *expr
	value      *expr // what is assigned, nil when nothing is

	// what a class, function or block holds. They are apart because Python's own reading visits them in this order: a try's handlers come
	// after its body, and its else and finally after them
	body     []*stmt
	handlers []*stmt
	orelse   []*stmt
	final    []*stmt
	chain    *stmt // the last if of an if's elifs, which the next elif or else belongs to
}

// children are the statements a statement holds, in the order Python's own walk over them visits them.
func (s *stmt) children() []*stmt {
	return slices.Concat(s.body, s.handlers, s.orelse, s.final)
}

// classesOf is every class in a module, however deep, in the order Python's ast.walk meets them: all those of one depth before any deeper.
func classesOf(module []*stmt) []*stmt {
	var classes []*stmt
	for todo := slices.Clone(module); len(todo) > 0; {
		s := todo[0]
		todo = append(todo[1:], s.children()...)
		if s.kind == stmtClass {
			classes = append(classes, s)
		}
	}

	return classes
}

// parseModule reads a file of Python into its statements.
func parseModule(src string) ([]*stmt, error) {
	lines, err := tokenize(src)
	if err != nil {
		return nil, err
	}

	p := &statements{lines: lines}
	module, err := p.block(0, true)
	if err != nil {
		return nil, err
	}
	if p.at < len(lines) {
		return nil, fmt.Errorf("line %d: indented less than the block it ends", lines[p.at].line)
	}

	return module, nil
}

// statements reads the statements of a file from its lines.
type statements struct {
	lines []logicalLine
	at    int
}

// block reads the statements that start indent in. Their expressions are read only when read is set, which is for a module's own statements
// and a class's: the rest, inside functions and blocks, are only stepped over.
func (p *statements) block(indent int, read bool) ([]*stmt, error) {
	var out []*stmt
	var decorators []*expr
	for p.at < len(p.lines) {
		l := p.lines[p.at]
		if l.indent < indent {
			break
		}
		if l.indent > indent {
			return nil, fmt.Errorf("line %d: indented where no block was started", l.line)
		}
		p.at++

		words := l.tokens
		lead := ""
		if words[0].kind == tokenName || words[0].kind == tokenOperator {
			lead = words[0].text
		}
		if lead == "async" && len(words) > 1 {
			lead = words[1].text
		}
		if decorators != nil && lead != "@" && lead != "class" && lead != "def" {
			return nil, fmt.Errorf("line %d: decorators with no class or function after them", l.line)
		}

		switch {
		case lead == "@":
			d, err := parseExpr(words[1:], l.line)
			if err != nil {
				return nil, err
			}
			decorators = append(decorators, d)

		case lead == "class" || lead == "def":
			s, err := p.definition(l, lead, decorators)
			if err != nil {
				return nil, err
			}
			decorators = nil
			out = append(out, s)

		case lead == "elif" || lead == "else" || lead == "except" || lead == "finally":
			// a clause of the block before it
			if len(out) == 0 || out[len(out)-1].kind != stmtBlock {
				return nil, fmt.Errorf("line %d: %s with no block before it", l.line, lead)
			}
			last := out[len(out)-1]

			body, err := p.body(l, false)
			if err != nil {
				return nil, err
			}
			clause := &stmt{kind: stmtBlock, line: l.line, body: body}

			switch {
			case lead == "elif" && last.chain != nil:
				// an elif is an if inside the else of the one before it
				clause.chain = clause
				last.chain.orelse = []*stmt{clause}
				last.chain = clause
			case lead == "else" && last.chain != nil:
				last.chain.orelse = body
			case lead == "except":
				last.handlers = append(last.handlers, clause)
			case lead == "else":
				last.orelse = body
			case lead == "finally":
				last.final = body
			default:
				return nil, fmt.Errorf("line %d: elif with no if before it", l.line)
			}

		case blockKeywords[lead] || (lead == "match" || lead == "case") && softBlock(words):
			body, err := p.body(l, false)
			if err != nil {
				return nil, err
			}
			s := &stmt{kind: stmtBlock, line: l.line, body: body}
			if lead == "if" {
				s.chain = s
			}
			out = append(out, s)

		default:
			simple, err := simpleStatements(words, read)
			if err != nil {
				return nil, err
			}
			out = append(out, simple...)
		}
	}
	if decorators != nil {
		return nil, errors.New("decorators with no class or function after them, at the end of a block")
	}

	return out, nil
}

// blockKeywords start a statement that holds a block of others, besides class and def.
var blockKeywords = map[string]bool{"if": true, "for": true, "while": true, "try": true, "with": true}

// softBlock is whether a line that starts with match or case starts a block, and is not a statement that uses the word as a name.
func softBlock(words []token) bool {
	if len(words) < 2 || slices.Contains([]string{":", "=", ".", ",", ";", ")", "]", "}", "+=", "-=", "*=", "/=", "|=", "&=", "^=", "%=", "@=", "//=", "**=", ">>=", "<<="}, words[1].text) {
		return false
	}
	colon := topLevel(words, ":")
	if words[0].text == "match" {
		// a match's cases are always on the lines after it
		return colon == len(words)-1
	}

	return colon > 0
}

// topLevel is where the first of an operator is in a run of tokens outside any brackets, or -1 when it is not there.
func topLevel(words []token, op string) int {
	depth := 0
	for i, w := range words {
		if w.kind != tokenOperator {
			continue
		}
		switch w.text {
		case "(", "[", "{":
			depth++
		case ")", "]", "}":
			depth--
		case op:
			if depth == 0 {
				return i
			}
		}
	}

	return -1
}

// body reads what a class, function or block holds: the rest of its line after the colon, or the lines indented under it.
func (p *statements) body(l logicalLine, read bool) ([]*stmt, error) {
	colon := topLevel(l.tokens, ":")
	if colon < 0 {
		return nil, fmt.Errorf("line %d: a block with no colon to start it", l.line)
	}
	if rest := l.tokens[colon+1:]; len(rest) > 0 {
		return simpleStatements(rest, read)
	}

	if p.at >= len(p.lines) || p.lines[p.at].indent <= l.indent {
		return nil, fmt.Errorf("line %d: a block with nothing in it", l.line)
	}

	return p.block(p.lines[p.at].indent, read)
}

// definition reads a class or a function, with what it holds. A class's statements have their expressions read, a function's do not.
func (p *statements) definition(l logicalLine, lead string, decorators []*expr) (*stmt, error) {
	words := l.tokens
	if words[0].text == "async" {
		words = words[1:]
	}
	if len(words) < 2 || words[1].kind != tokenName {
		return nil, fmt.Errorf("line %d: %s with no name", l.line, lead)
	}
	s := &stmt{kind: stmtDef, line: l.line, name: words[1].text, decorators: decorators}

	if lead == "class" {
		s.kind = stmtClass

		// what follows the name: its type parameters, class Name[T], then what it is made from
		rest := words[2:]
		if len(rest) > 0 && rest[0].text == "[" {
			rest = rest[closing(rest)+1:]
		}
		if len(rest) > 0 && rest[0].text == "(" {
			end := closing(rest)
			for _, argument := range splitTopLevel(rest[1:end], ",") {
				// a keyword argument, as metaclass=, and an unpacked list of bases are not bases Python names
				if len(argument) == 0 || argument[0].text == "*" || argument[0].text == "**" || len(argument) > 1 && argument[0].kind == tokenName && argument[1].text == "=" {
					continue
				}
				base, err := parseExpr(argument, l.line)
				if err != nil {
					return nil, err
				}
				s.bases = append(s.bases, base)
			}
		}
	}

	body, err := p.body(l, lead == "class")
	if err != nil {
		return nil, err
	}
	s.body = body

	return s, nil
}

// closing is where the bracket that the first token opens is closed.
func closing(words []token) int {
	depth := 0
	for i, w := range words {
		if w.kind != tokenOperator {
			continue
		}
		switch w.text {
		case "(", "[", "{":
			depth++
		case ")", "]", "}":
			depth--
			if depth == 0 {
				return i
			}
		}
	}

	return len(words) - 1
}

// splitTopLevel splits a run of tokens at each of an operator outside any brackets.
func splitTopLevel(words []token, op string) [][]token {
	var parts [][]token
	for {
		at := topLevel(words, op)
		if at < 0 {
			return append(parts, words)
		}
		parts = append(parts, words[:at])
		words = words[at+1:]
	}
}

// simpleStatements reads the statements of one line, which semicolons part. Only assignments are told apart, and only when read is set.
func simpleStatements(words []token, read bool) ([]*stmt, error) {
	var out []*stmt
	for _, part := range splitTopLevel(words, ";") {
		if len(part) == 0 {
			continue
		}
		s := &stmt{kind: stmtOther, line: part[0].line}
		out = append(out, s)
		if !read {
			continue
		}

		named := part[0].kind == tokenName && !keywords[part[0].text]
		if named && len(part) > 1 && part[1].text == ":" {
			// name: annotation, and = value when it has one
			s.kind, s.name = stmtAnnotated, part[0].text
			annotation := part[2:]
			if equals := topLevel(annotation, "="); equals >= 0 {
				value, err := parseValue(annotation[equals+1:], s.line)
				if err != nil {
					return nil, err
				}
				s.value, annotation = value, annotation[:equals]
			}

			var err error
			if s.annotation, err = parseExpr(annotation, s.line); err != nil {
				return nil, err
			}

			continue
		}

		// a = b = value: the equals signs before any lambda, whose own defaults come after one
		var equals []int
		depth := 0
		for i, w := range part {
			if w.kind == tokenName && w.text == "lambda" && depth == 0 {
				break
			}
			if w.kind != tokenOperator {
				continue
			}
			switch w.text {
			case "(", "[", "{":
				depth++
			case ")", "]", "}":
				depth--
			case "=":
				if depth == 0 {
					equals = append(equals, i)
				}
			}
		}
		if len(equals) == 0 {
			continue
		}

		s.kind, s.targets = stmtAssign, len(equals)
		if named && equals[0] == 1 {
			s.name = part[0].text
		}
		value, err := parseValue(part[equals[len(equals)-1]+1:], s.line)
		if err != nil {
			return nil, err
		}
		s.value = value
	}

	return out, nil
}

// exprKind is which kind of expression an expr is, as far as the secrets go.
type exprKind int

const (
	exprOther     exprKind = iota // anything the secrets do not depend on: a list, a sum, a lambda...
	exprName                      // a name
	exprAttribute                 // something.name
	exprSubscript                 // something[slice]
	exprCall                      // something(arguments)
	exprOr                        // a | b
	exprTuple                     // a, b
	exprText                      // a string that is text, not bytes, and not formatted
	exprConstant                  // any other constant: a number, bytes, None, True, False or ...
)

// expr is one expression, with the expressions it is made of.
type expr struct {
	kind     exprKind
	name     string     // a name, or the last part of an attribute
	text     string     // what a string that is text holds
	truthy   bool       // whether a constant counts as true
	of       *expr      // what an attribute, subscript or call is of
	parts    []*expr    // a subscript's slice, a call's positional arguments, the two sides of a |, a tuple's elements
	keywords []argument // a call's keyword arguments
	inside   []*expr    // whatever else an expression holds, so that a walk over everything meets it
}

// argument is a keyword argument of a call: name=value, or **value with no name.
type argument struct {
	name  string
	value *expr
}

// nameOf is the plain name an expression refers to: a name, or the last part of a dotted one. It is "" for anything else.
func nameOf(e *expr) string {
	if e != nil && (e.kind == exprName || e.kind == exprAttribute) {
		return e.name
	}

	return ""
}

// walk is every expression inside one, itself first, in the order Python's ast.walk meets them: all those of one depth before any deeper.
func walk(e *expr) []*expr {
	var all []*expr
	for todo := []*expr{e}; len(todo) > 0; {
		e := todo[0]
		todo = todo[1:]
		if e == nil {
			continue
		}
		all = append(all, e)

		todo = append(todo, e.of)
		todo = append(todo, e.parts...)
		for _, k := range e.keywords {
			todo = append(todo, k.value)
		}
		todo = append(todo, e.inside...)
	}

	return all
}

// parseExpr reads an expression, which may be a tuple written without brackets, from the statement on line.
func parseExpr(words []token, line int) (*expr, error) {
	p := &exprParser{words: words, line: line}
	e, err := p.list()
	if err != nil {
		return nil, err
	}
	if !p.done() {
		return nil, p.wrong("where the expression should end")
	}

	return e, nil
}

// parseValue reads what is assigned to something, an expression or a yield, from the statement on line.
func parseValue(words []token, line int) (*expr, error) {
	if len(words) > 0 && words[0].kind == tokenName && words[0].text == "yield" {
		p := &exprParser{words: words, line: line}
		e, err := p.yield()
		if err == nil && !p.done() {
			err = p.wrong("where the yield should end")
		}

		return e, err
	}

	return parseExpr(words, line)
}

// parseText reads an expression written as text, as a type that is named before it is made is: "PoolEntry".
func parseText(text string) (*expr, error) {
	lines, err := tokenize(text)
	if err != nil {
		return nil, err
	}
	if len(lines) != 1 {
		return nil, errors.New("not one expression")
	}

	return parseExpr(lines[0].tokens, 1)
}

// exprParser reads an expression from its tokens.
type exprParser struct {
	words []token
	at    int
	line  int // the statement's, for an error where there are no tokens to say where
}

func (p *exprParser) done() bool {
	return p.at >= len(p.words)
}

// is says whether the next token is this operator or keyword.
func (p *exprParser) is(texts ...string) bool {
	return !p.done() && (p.words[p.at].kind == tokenOperator || p.words[p.at].kind == tokenName) && slices.Contains(texts, p.words[p.at].text)
}

// take steps over the next token when it is this operator or keyword, and says whether it was.
func (p *exprParser) take(text string) bool {
	if p.is(text) {
		p.at++

		return true
	}

	return false
}

func (p *exprParser) expect(text string) error {
	if !p.take(text) {
		return p.wrong("where " + text + " should be")
	}

	return nil
}

// wrong is an error at the next token.
func (p *exprParser) wrong(what string) error {
	if p.done() {
		if len(p.words) == 0 {
			return fmt.Errorf("line %d: nothing %s", p.line, what)
		}

		return fmt.Errorf("line %d: the end %s", p.words[len(p.words)-1].line, what)
	}

	return fmt.Errorf("line %d: %q %s", p.words[p.at].line, p.words[p.at].text, what)
}

// ends is whether the next token ends a list of expressions: the end, a closing bracket, or what an assignment or slice goes on with.
func (p *exprParser) ends() bool {
	return p.done() || p.is(")", "]", "}", "=", ":", "in")
}

// list reads expressions that commas part, as a tuple when there is more than one or a comma after the one.
func (p *exprParser) list() (*expr, error) {
	first, err := p.starred()
	if err != nil {
		return nil, err
	}
	if !p.is(",") {
		return first, nil
	}

	tuple := &expr{kind: exprTuple, parts: []*expr{first}}
	for p.take(",") && !p.ends() {
		e, err := p.starred()
		if err != nil {
			return nil, err
		}
		tuple.parts = append(tuple.parts, e)
	}

	return tuple, nil
}

// starred reads an expression that may be unpacked with *.
func (p *exprParser) starred() (*expr, error) {
	if p.take("*") {
		e, err := p.or()
		if err != nil {
			return nil, err
		}

		return &expr{inside: []*expr{e}}, nil
	}

	return p.named()
}

// named reads an expression that may be given a name as it is worked out: name := value.
func (p *exprParser) named() (*expr, error) {
	if p.at+1 < len(p.words) && p.words[p.at].kind == tokenName && p.words[p.at+1].text == ":=" {
		p.at += 2
		e, err := p.expression()
		if err != nil {
			return nil, err
		}

		return &expr{inside: []*expr{e}}, nil
	}

	return p.expression()
}

// expression reads one expression: a lambda, a conditional one, or anything less.
func (p *exprParser) expression() (*expr, error) {
	if p.take("lambda") {
		// its parameters are stepped over, to the colon its body follows
		for depth := 0; !p.done() && (depth > 0 || !p.is(":")); p.at++ {
			switch {
			case p.is("(", "[", "{"):
				depth++
			case p.is(")", "]", "}"):
				depth--
			}
		}
		if err := p.expect(":"); err != nil {
			return nil, err
		}
		body, err := p.expression()
		if err != nil {
			return nil, err
		}

		return &expr{inside: []*expr{body}}, nil
	}

	e, err := p.disjunction()
	if err != nil || !p.take("if") {
		return e, err
	}

	condition, err := p.disjunction()
	if err != nil {
		return nil, err
	}
	if err := p.expect("else"); err != nil {
		return nil, err
	}
	otherwise, err := p.expression()
	if err != nil {
		return nil, err
	}

	return &expr{inside: []*expr{e, condition, otherwise}}, nil
}

// disjunction reads a or b, and anything less.
func (p *exprParser) disjunction() (*expr, error) {
	return p.joined(p.conjunction, "or")
}

// conjunction reads a and b, and anything less.
func (p *exprParser) conjunction() (*expr, error) {
	return p.joined(p.inversion, "and")
}

// inversion reads not a, and anything less.
func (p *exprParser) inversion() (*expr, error) {
	if p.take("not") {
		e, err := p.inversion()
		if err != nil {
			return nil, err
		}

		return &expr{inside: []*expr{e}}, nil
	}

	return p.comparison()
}

// comparison reads a < b, a in b, a is not b and the like, and anything less.
func (p *exprParser) comparison() (*expr, error) {
	e, err := p.or()
	if err != nil {
		return nil, err
	}
	for p.is("<", ">", "==", ">=", "<=", "!=", "in", "not", "is") {
		switch {
		case p.take("not"):
			if err := p.expect("in"); err != nil {
				return nil, err
			}
		case p.take("is"):
			p.take("not")
		default:
			p.at++
		}

		right, err := p.or()
		if err != nil {
			return nil, err
		}
		e = &expr{inside: []*expr{e, right}}
	}

	return e, nil
}

// or reads a | b, which is how a type that is one of several is written, and anything less.
func (p *exprParser) or() (*expr, error) {
	e, err := p.xor()
	if err != nil {
		return nil, err
	}
	for p.take("|") {
		right, err := p.xor()
		if err != nil {
			return nil, err
		}
		e = &expr{kind: exprOr, parts: []*expr{e, right}}
	}

	return e, nil
}

func (p *exprParser) xor() (*expr, error) {
	return p.joined(p.and, "^")
}

func (p *exprParser) and() (*expr, error) {
	return p.joined(p.shift, "&")
}

func (p *exprParser) shift() (*expr, error) {
	return p.joined(p.sum, "<<", ">>")
}

func (p *exprParser) sum() (*expr, error) {
	return p.joined(p.term, "+", "-")
}

func (p *exprParser) term() (*expr, error) {
	return p.joined(p.factor, "*", "/", "//", "%", "@")
}

// joined reads expressions of one kind joined by any of these operators, which the secrets do not depend on.
func (p *exprParser) joined(next func() (*expr, error), ops ...string) (*expr, error) {
	e, err := next()
	if err != nil {
		return nil, err
	}
	for p.is(ops...) {
		p.at++
		right, err := next()
		if err != nil {
			return nil, err
		}
		e = &expr{inside: []*expr{e, right}}
	}

	return e, nil
}

// factor reads -a, +a, ~a, a ** b, await a, and anything less.
func (p *exprParser) factor() (*expr, error) {
	if p.is("-", "+", "~") {
		p.at++
		e, err := p.factor()
		if err != nil {
			return nil, err
		}

		return &expr{inside: []*expr{e}}, nil
	}

	waited := p.take("await")
	e, err := p.primary()
	if err != nil {
		return nil, err
	}
	if waited {
		e = &expr{inside: []*expr{e}}
	}
	if p.take("**") {
		power, err := p.factor()
		if err != nil {
			return nil, err
		}
		e = &expr{inside: []*expr{e, power}}
	}

	return e, nil
}

// primary reads a value with what follows it: .name, a call, a subscript.
func (p *exprParser) primary() (*expr, error) {
	e, err := p.atom()
	if err != nil {
		return nil, err
	}

	for {
		switch {
		case p.take("."):
			if p.done() || p.words[p.at].kind != tokenName {
				return nil, p.wrong("where a name should follow a dot")
			}
			e = &expr{kind: exprAttribute, name: p.words[p.at].text, of: e}
			p.at++

		case p.take("("):
			call := &expr{kind: exprCall, of: e}
			if err := p.arguments(call); err != nil {
				return nil, err
			}
			e = call

		case p.take("["):
			slice, err := p.slices()
			if err != nil {
				return nil, err
			}
			if err := p.expect("]"); err != nil {
				return nil, err
			}
			e = &expr{kind: exprSubscript, of: e, parts: []*expr{slice}}

		default:
			return e, nil
		}
	}
}

// arguments reads a call's arguments, and the bracket that ends them.
func (p *exprParser) arguments(call *expr) error {
	for !p.take(")") {
		switch {
		case p.take("**"):
			e, err := p.expression()
			if err != nil {
				return err
			}
			call.keywords = append(call.keywords, argument{value: e})

		case p.at+1 < len(p.words) && p.words[p.at].kind == tokenName && p.words[p.at+1].text == "=":
			name := p.words[p.at].text
			p.at += 2
			e, err := p.expression()
			if err != nil {
				return err
			}
			call.keywords = append(call.keywords, argument{name: name, value: e})

		default:
			e, err := p.starred()
			if err != nil {
				return err
			}
			if p.is("for", "async") {
				if e, err = p.comprehension(e); err != nil {
					return err
				}
			}
			call.parts = append(call.parts, e)
		}

		if !p.take(",") && !p.is(")") {
			return p.wrong("where a comma or ) should be")
		}
	}

	return nil
}

// slices reads what is between a subscript's brackets: one slice or expression, or several as a tuple.
func (p *exprParser) slices() (*expr, error) {
	var items []*expr
	trailing := false
	for !p.is("]") {
		item, err := p.slice()
		if err != nil {
			return nil, err
		}
		items = append(items, item)

		if trailing = p.take(","); !trailing {
			break
		}
	}

	switch {
	case len(items) == 0:
		return nil, p.wrong("where a subscript should be")
	case len(items) == 1 && !trailing:
		return items[0], nil
	default:
		return &expr{kind: exprTuple, parts: items}, nil
	}
}

// slice reads one slice, a:b:c with any part left out, or an expression.
func (p *exprParser) slice() (*expr, error) {
	var parts []*expr
	if !p.is(":") {
		e, err := p.starred()
		if err != nil {
			return nil, err
		}
		if !p.is(":") {
			return e, nil
		}
		parts = append(parts, e)
	}

	for range 2 {
		if !p.take(":") {
			break
		}
		if !p.is(":", ",", "]") {
			e, err := p.expression()
			if err != nil {
				return nil, err
			}
			parts = append(parts, e)
		}
	}

	return &expr{inside: parts}, nil
}

// comprehension reads the fors and ifs that follow the first expression of a comprehension.
func (p *exprParser) comprehension(first *expr) (*expr, error) {
	e := &expr{inside: []*expr{first}}
	for p.is("for", "async") {
		p.take("async")
		if err := p.expect("for"); err != nil {
			return nil, err
		}

		for {
			target, err := p.starredOr()
			if err != nil {
				return nil, err
			}
			e.inside = append(e.inside, target)
			if !p.take(",") || p.is("in") {
				break
			}
		}
		if err := p.expect("in"); err != nil {
			return nil, err
		}
		iterable, err := p.disjunction()
		if err != nil {
			return nil, err
		}
		e.inside = append(e.inside, iterable)

		for p.take("if") {
			condition, err := p.disjunction()
			if err != nil {
				return nil, err
			}
			e.inside = append(e.inside, condition)
		}
	}

	return e, nil
}

// starredOr reads what a comprehension's for names: a | b and anything less, which stops before its in, or that unpacked with *.
func (p *exprParser) starredOr() (*expr, error) {
	starred := p.take("*")
	e, err := p.or()
	if err != nil || !starred {
		return e, err
	}

	return &expr{inside: []*expr{e}}, nil
}

// yield reads yield, yield value and yield from value.
func (p *exprParser) yield() (*expr, error) {
	if err := p.expect("yield"); err != nil {
		return nil, err
	}
	if p.take("from") {
		e, err := p.expression()
		if err != nil {
			return nil, err
		}

		return &expr{inside: []*expr{e}}, nil
	}
	if p.ends() {
		return &expr{}, nil
	}
	e, err := p.list()
	if err != nil {
		return nil, err
	}

	return &expr{inside: []*expr{e}}, nil
}

// atom reads a value on its own: a name, a constant, or something in brackets.
func (p *exprParser) atom() (*expr, error) {
	if p.done() {
		return nil, p.wrong("where a value should be")
	}

	w := p.words[p.at]
	switch {
	case w.kind == tokenNumber:
		p.at++

		return &expr{kind: exprConstant, truthy: strings.ContainsAny(strings.TrimPrefix(strings.ToLower(w.text), "0x"), "123456789abcdef")}, nil

	case w.kind == tokenString:
		return p.strings()

	case p.take("None"), p.take("False"):
		return &expr{kind: exprConstant}, nil

	case p.take("True"):
		return &expr{kind: exprConstant, truthy: true}, nil

	case w.kind == tokenName && !keywords[w.text]:
		p.at++

		return &expr{kind: exprName, name: w.text}, nil

	case p.take("..."):
		return &expr{kind: exprConstant, truthy: true}, nil

	case p.take("("):
		if p.take(")") {
			return &expr{kind: exprTuple}, nil
		}
		if p.is("yield") {
			e, err := p.yield()
			if err != nil {
				return nil, err
			}

			return e, p.expect(")")
		}

		first, err := p.starred()
		if err != nil {
			return nil, err
		}
		e := first
		switch {
		case p.is("for", "async"):
			if e, err = p.comprehension(first); err != nil {
				return nil, err
			}
		case p.is(","):
			e = &expr{kind: exprTuple, parts: []*expr{first}}
			for p.take(",") && !p.is(")") {
				next, err := p.starred()
				if err != nil {
					return nil, err
				}
				e.parts = append(e.parts, next)
			}
		}

		return e, p.expect(")")

	case p.take("["):
		return p.collection("]")

	case p.take("{"):
		return p.collection("}")
	}

	return nil, p.wrong("where a value should be")
}

// collection reads a list, set or dictionary, or a comprehension of one, and the bracket that ends it.
func (p *exprParser) collection(end string) (*expr, error) {
	e := &expr{}
	for !p.take(end) {
		var item *expr
		var err error
		if p.take("**") {
			item, err = p.or()
		} else {
			item, err = p.starred()
		}
		if err != nil {
			return nil, err
		}

		if end == "}" && p.take(":") {
			var value *expr
			if value, err = p.expression(); err != nil {
				return nil, err
			}
			item = &expr{inside: []*expr{item, value}}
		}
		if p.is("for", "async") {
			if item, err = p.comprehension(item); err != nil {
				return nil, err
			}
		}
		e.inside = append(e.inside, item)

		if !p.take(",") && !p.is(end) {
			return nil, p.wrong("where a comma or " + end + " should be")
		}
	}

	return e, nil
}

// strings reads strings written one after another, which Python joins into one.
func (p *exprParser) strings() (*expr, error) {
	joined := &expr{kind: exprText}
	for !p.done() && p.words[p.at].kind == tokenString {
		text, kind := stringValue(p.words[p.at].text)
		p.at++

		switch {
		case kind == exprOther || joined.kind == exprOther:
			joined.kind = exprOther
		case kind == exprConstant:
			joined.kind, joined.truthy = exprConstant, joined.truthy || text != ""
		default:
			joined.text += text
		}
	}

	return joined, nil
}

// stringValue is what a string as written holds, and whether it is text (exprText), bytes (exprConstant) or formatted, so worked out only
// when it runs (exprOther).
func stringValue(written string) (string, exprKind) {
	prefixed := strings.IndexAny(written, `"'`)
	prefix := strings.ToLower(written[:prefixed])
	quote := 1
	if strings.HasPrefix(written[prefixed:], `"""`) || strings.HasPrefix(written[prefixed:], `'''`) {
		quote = 3
	}
	held := written[prefixed+quote : len(written)-quote]

	if strings.ContainsAny(prefix, "ft") {
		return "", exprOther
	}
	if !strings.Contains(prefix, "r") {
		held = unescape(held)
	}
	if strings.Contains(prefix, "b") {
		return held, exprConstant
	}

	return held, exprText
}

// unescape is what a string's backslashes stand for, for the ones a name or a type might be written with.
func unescape(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}

	var out strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 == len(s) {
			out.WriteByte(s[i])

			continue
		}

		i++
		switch c := s[i]; c {
		case '\n':
		case 'n':
			out.WriteByte('\n')
		case 't':
			out.WriteByte('\t')
		case 'r':
			out.WriteByte('\r')
		case '\\', '\'', '"':
			out.WriteByte(c)
		default:
			out.WriteByte('\\')
			out.WriteByte(c)
		}
	}

	return out.String()
}

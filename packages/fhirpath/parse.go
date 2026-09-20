package fhirpath

import (
	"fmt"
	"strconv"
	"strings"
)

// Node is one piece of a parsed expression.
//
// The tree is small on purpose: a path step, a call, a literal, a unary or
// binary operator. Everything the invariants are written in reduces to those.
type Node interface{ node() }

// Literal is a constant.
type Literal struct{ Value any }

// Variable is %resource, %context or $this.
type Variable struct{ Name string }

// Path is one navigation step. Left is what it navigates from, nil at the root
// of an expression, where the step reads from the context.
type Path struct {
	Left Node
	Name string
}

// Call is a function applied to what precedes it.
type Call struct {
	Left      Node
	Name      string
	Arguments []Node
}

// Index is a [n] subscript.
type Index struct {
	Left  Node
	Where Node
}

// Binary is an operator with two sides.
type Binary struct {
	Operator string
	Left     Node
	Right    Node
}

// Unary is a prefixed operator, which in this subset is only negation.
type Unary struct {
	Operator string
	Operand  Node
}

// TypeTest is `is` or `as` applied to a type name.
type TypeTest struct {
	Operator string
	Left     Node
	Type     string
}

func (Literal) node()  {}
func (Variable) node() {}
func (Path) node()     {}
func (Call) node()     {}
func (Index) node()    {}
func (Binary) node()   {}
func (Unary) node()    {}
func (TypeTest) node() {}

// precedence is how tightly each operator binds. FHIRPath's own order, which is
// why `implies` is lowest and `.` is handled apart from this table.
var precedence = map[string]int{
	"implies": 1,
	"or":      2, "xor": 2,
	"and": 3,
	"in":  4, "contains": 4,
	"=": 5, "!=": 5, "~": 5, "!~": 5, "<": 5, ">": 5, "<=": 5, ">=": 5,
	"is": 6, "as": 6,
	"|": 7,
	"+": 8, "-": 8, "&": 8,
	"*": 9, "/": 9, "div": 9, "mod": 9,
}

// parser walks the tokens once.
type parser struct {
	tokens     []token
	at         int
	expression string
}

// Parse reads an expression into a tree.
//
// An expression this subset does not cover is an error here rather than a
// surprise later: the caller has to be able to tell "this invariant does not
// hold" from "this invariant was never evaluated", and the only honest place to
// draw that line is before anything is evaluated.
func Parse(expression string) (Node, error) {
	tokens, err := lex(expression)
	if err != nil {
		return nil, err
	}

	held := &parser{tokens: tokens, expression: expression}

	parsed, err := held.expressionAt(0)
	if err != nil {
		return nil, err
	}

	if held.peek().kind != tokenEnd {
		return nil, held.unexpected("trailing input")
	}

	return parsed, nil
}

func (p *parser) peek() token { return p.tokens[p.at] }

func (p *parser) next() token {
	held := p.tokens[p.at]
	if held.kind != tokenEnd {
		p.at++
	}

	return held
}

func (p *parser) unexpected(because string) error {
	held := p.peek()

	return ErrSyntax{
		Expression: p.expression, At: held.at,
		Because: fmt.Sprintf("%s (%q)", because, held.text),
	}
}

// expressionAt parses operators binding at least as tightly as the level given.
func (p *parser) expressionAt(level int) (Node, error) {
	left, err := p.unary()
	if err != nil {
		return nil, err
	}

	for {
		held := p.peek()
		if held.kind != tokenSymbol && held.kind != tokenKeyword {
			return left, nil
		}

		binds, known := precedence[held.text]
		if !known || binds < level {
			return left, nil
		}

		p.next()

		// `is` and `as` take a type name rather than an expression.
		if held.text == "is" || held.text == "as" {
			named := p.next()
			if named.kind != tokenIdentifier {
				return nil, p.unexpected("a type name")
			}

			left = TypeTest{Operator: held.text, Left: left, Type: named.text}

			continue
		}

		right, err := p.expressionAt(binds + 1)
		if err != nil {
			return nil, err
		}

		left = Binary{Operator: held.text, Left: left, Right: right}
	}
}

// unary parses a prefixed sign and whatever it applies to.
func (p *parser) unary() (Node, error) {
	if held := p.peek(); held.kind == tokenSymbol && (held.text == "-" || held.text == "+") {
		p.next()

		operand, err := p.unary()
		if err != nil {
			return nil, err
		}

		if held.text == "+" {
			return operand, nil
		}

		return Unary{Operator: "-", Operand: operand}, nil
	}

	return p.postfix()
}

// postfix parses a primary and every step, call and subscript applied to it.
func (p *parser) postfix() (Node, error) {
	left, err := p.primary()
	if err != nil {
		return nil, err
	}

	for {
		held := p.peek()

		switch {
		case held.kind == tokenSymbol && held.text == ".":
			p.next()

			named := p.next()
			if named.kind != tokenIdentifier && named.kind != tokenKeyword {
				return nil, p.unexpected("a name after a dot")
			}

			if left, err = p.stepOrCall(left, named.text); err != nil {
				return nil, err
			}

		case held.kind == tokenSymbol && held.text == "[":
			p.next()

			where, err := p.expressionAt(0)
			if err != nil {
				return nil, err
			}

			if closing := p.next(); closing.text != "]" {
				return nil, p.unexpected("a closing bracket")
			}

			left = Index{Left: left, Where: where}

		default:
			return left, nil
		}
	}
}

// stepOrCall reads either a plain navigation step or a call's arguments.
func (p *parser) stepOrCall(left Node, name string) (Node, error) {
	if held := p.peek(); held.kind != tokenSymbol || held.text != "(" {
		return Path{Left: left, Name: name}, nil
	}

	arguments, err := p.arguments()
	if err != nil {
		return nil, err
	}

	return Call{Left: left, Name: name, Arguments: arguments}, nil
}

// arguments reads a parenthesised, comma-separated list.
func (p *parser) arguments() ([]Node, error) {
	p.next()

	var held []Node

	if closing := p.peek(); closing.kind == tokenSymbol && closing.text == ")" {
		p.next()

		return held, nil
	}

	for {
		argument, err := p.expressionAt(0)
		if err != nil {
			return nil, err
		}

		held = append(held, argument)

		switch separator := p.next(); separator.text {
		case ",":
		case ")":
			return held, nil
		default:
			return nil, p.unexpected("a comma or a closing parenthesis")
		}
	}
}

// primary parses a literal, a variable, a parenthesised expression, or the
// first step of a path.
func (p *parser) primary() (Node, error) {
	held := p.next()

	switch {
	case held.kind == tokenString:
		return Literal{Value: held.text}, nil

	case held.kind == tokenNumber:
		return numberLiteral(held, p.expression)

	case held.kind == tokenVariable:
		return Variable{Name: held.text}, nil

	case held.kind == tokenKeyword && held.text == "true":
		return Literal{Value: true}, nil

	case held.kind == tokenKeyword && held.text == "false":
		return Literal{Value: false}, nil

	case held.kind == tokenSymbol && held.text == "{":
		// The empty collection, written {}.
		if closing := p.next(); closing.text != "}" {
			return nil, p.unexpected("an empty collection")
		}

		return Literal{Value: nil}, nil

	case held.kind == tokenSymbol && held.text == "(":
		inner, err := p.expressionAt(0)
		if err != nil {
			return nil, err
		}

		if closing := p.next(); closing.text != ")" {
			return nil, p.unexpected("a closing parenthesis")
		}

		return inner, nil

	case held.kind == tokenIdentifier || held.kind == tokenKeyword:
		// A bare name at the start of an expression reads from the context,
		// and a bare call applies to it.
		p.at--

		named := p.next()

		return p.stepOrCall(nil, named.text)

	default:
		p.at--

		return nil, p.unexpected("an expression")
	}
}

// numberLiteral reads an integer or a decimal.
//
// The two are different types in FHIRPath — an integer compares and divides
// differently — so the spelling decides, which is what a client writing the
// expression would expect.
func numberLiteral(held token, expression string) (Node, error) {
	if !strings.Contains(held.text, ".") {
		value, err := strconv.ParseInt(held.text, 10, 64)
		if err != nil {
			return nil, ErrSyntax{Expression: expression, At: held.at, Because: "an unreadable integer"}
		}

		return Literal{Value: value}, nil
	}

	value, err := strconv.ParseFloat(held.text, 64)
	if err != nil {
		return nil, ErrSyntax{Expression: expression, At: held.at, Because: "an unreadable decimal"}
	}

	return Literal{Value: value}, nil
}

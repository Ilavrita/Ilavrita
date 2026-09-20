// Package fhirpath evaluates the subset of FHIRPath that R4's invariants are
// written in.
//
// It is a subset on purpose. FHIRPath is a large language and the invariants
// use a small, measurable part of it: thirty functions cover 197 of R4's 205
// error-severity invariants, and the rest need eight more. Building the whole
// language to reach the last few would be a great deal of code that nothing
// here evaluates.
//
// What matters more than the subset is what happens outside it. An expression
// this package cannot parse is an error, never an empty result and never true:
// an invariant reported as passing because nobody could evaluate it is worse
// than one nobody checked, because it looks checked.
package fhirpath

import (
	"fmt"
	"strings"
	"unicode"
)

// kind is what a token is.
type kind int

// The tokens this grammar is written in.
const (
	tokenEnd kind = iota
	tokenIdentifier
	tokenString
	tokenNumber
	tokenSymbol  // . ( ) [ ] , + - * / & | = != < > <= >=
	tokenKeyword // and or xor implies in contains is as div mod true false
	tokenVariable
)

// token is one lexeme and where it started, so an error can point at it.
type token struct {
	kind kind
	text string
	at   int
}

// keywords are the words this grammar reads as operators rather than as the
// start of a path. FHIRPath allows them as identifiers too, which is why the
// parser decides by position rather than the lexer by spelling.
var keywords = map[string]bool{
	"and": true, "or": true, "xor": true, "implies": true,
	"in": true, "contains": true, "is": true, "as": true,
	"div": true, "mod": true, "true": true, "false": true,
}

// ErrSyntax reports an expression this package cannot read.
type ErrSyntax struct {
	Expression string
	At         int
	Because    string
}

func (e ErrSyntax) Error() string {
	return fmt.Sprintf("fhirpath: %s at %d in %q", e.Because, e.At, e.Expression)
}

// lex breaks an expression into tokens.
func lex(expression string) ([]token, error) {
	var (
		held  []token
		runes = []rune(expression)
	)

	for i := 0; i < len(runes); {
		r := runes[i]

		switch {
		case unicode.IsSpace(r):
			i++

		case r == '\'':
			text, next, err := lexString(runes, i, expression)
			if err != nil {
				return nil, err
			}

			held = append(held, token{kind: tokenString, text: text, at: i})
			i = next

		case unicode.IsDigit(r):
			start := i
			for i < len(runes) && (unicode.IsDigit(runes[i]) || runes[i] == '.') {
				// A dot is part of a number only when a digit follows it, so
				// "1.first()" reads as a path and "1.5" as a decimal.
				if runes[i] == '.' && (i+1 >= len(runes) || !unicode.IsDigit(runes[i+1])) {
					break
				}

				i++
			}

			held = append(held, token{kind: tokenNumber, text: string(runes[start:i]), at: start})

		case r == '%' || r == '$':
			start := i
			i++

			for i < len(runes) && (unicode.IsLetter(runes[i]) || unicode.IsDigit(runes[i]) || runes[i] == '_') {
				i++
			}

			held = append(held, token{kind: tokenVariable, text: string(runes[start:i]), at: start})

		case unicode.IsLetter(r) || r == '_':
			start := i
			for i < len(runes) && (unicode.IsLetter(runes[i]) || unicode.IsDigit(runes[i]) || runes[i] == '_') {
				i++
			}

			word := string(runes[start:i])
			shape := tokenIdentifier

			if keywords[word] {
				shape = tokenKeyword
			}

			held = append(held, token{kind: shape, text: word, at: start})

		case r == '`':
			// A delimited identifier, which R4 uses for names that collide with
			// keywords.
			start := i
			i++

			for i < len(runes) && runes[i] != '`' {
				i++
			}

			if i >= len(runes) {
				return nil, ErrSyntax{Expression: expression, At: start, Because: "an unterminated identifier"}
			}

			held = append(held, token{kind: tokenIdentifier, text: string(runes[start+1 : i]), at: start})
			i++

		default:
			symbol, next, err := lexSymbol(runes, i, expression)
			if err != nil {
				return nil, err
			}

			held = append(held, token{kind: tokenSymbol, text: symbol, at: i})
			i = next
		}
	}

	return append(held, token{kind: tokenEnd, at: len(runes)}), nil
}

// lexString reads a quoted literal, with the escapes FHIRPath defines.
func lexString(runes []rune, start int, expression string) (string, int, error) {
	var built strings.Builder

	i := start + 1

	for i < len(runes) {
		switch runes[i] {
		case '\'':
			return built.String(), i + 1, nil
		case '\\':
			if i+1 >= len(runes) {
				return "", 0, ErrSyntax{Expression: expression, At: i, Because: "an unterminated escape"}
			}

			i++

			switch runes[i] {
			case 'n':
				built.WriteRune('\n')
			case 'r':
				built.WriteRune('\r')
			case 't':
				built.WriteRune('\t')
			case 'f':
				built.WriteRune('\f')
			case 'u':
				// \uXXXX. Anything shorter is malformed rather than literal.
				if i+4 >= len(runes) {
					return "", 0, ErrSyntax{Expression: expression, At: i, Because: "a short unicode escape"}
				}

				var value rune

				for _, digit := range runes[i+1 : i+5] {
					value = value*16 + hexValue(digit)
				}

				built.WriteRune(value)

				i += 4
			default:
				built.WriteRune(runes[i])
			}

			i++
		default:
			built.WriteRune(runes[i])
			i++
		}
	}

	return "", 0, ErrSyntax{Expression: expression, At: start, Because: "an unterminated string"}
}

// hexValue reads one hexadecimal digit.
func hexValue(r rune) rune {
	switch {
	case r >= '0' && r <= '9':
		return r - '0'
	case r >= 'a' && r <= 'f':
		return r - 'a' + 10
	case r >= 'A' && r <= 'F':
		return r - 'A' + 10
	default:
		return 0
	}
}

// twoRuneSymbols are the operators spelled with two characters, read before the
// one-character ones so "<=" is not a "<" followed by an "=".
var twoRuneSymbols = []string{"!=", "!~", "<=", ">=", "~"}

// oneRuneSymbols are the rest.
const oneRuneSymbols = ".()[],+-*/&|=<>{}"

// lexSymbol reads one operator or delimiter.
func lexSymbol(runes []rune, i int, expression string) (string, int, error) {
	if i+1 < len(runes) {
		pair := string(runes[i : i+2])
		for _, known := range twoRuneSymbols {
			if pair == known {
				return pair, i + 2, nil
			}
		}
	}

	if strings.ContainsRune(oneRuneSymbols, runes[i]) {
		return string(runes[i]), i + 1, nil
	}

	return "", 0, ErrSyntax{
		Expression: expression, At: i,
		Because: fmt.Sprintf("an unexpected %q", string(runes[i])),
	}
}

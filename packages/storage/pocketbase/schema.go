package pocketbase

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"strings"
)

//go:embed schema.sql
var schema string

// scanState is where the splitter stands in the script. A semicolon only ends a
// statement in scanSQL, so quoted text and comments cannot cut one in half.
type scanState int

const (
	scanSQL scanState = iota
	scanString
	scanLineComment
	scanBlockComment
)

// statementLabelWords is how many opening words name a statement in an error.
const statementLabelWords = 6

// Schema returns the data definition language for every table, index and
// trigger the backend needs.
func Schema() string {
	return schema
}

// ApplySchema creates everything Schema declares, inside one transaction. Every
// statement is guarded by IF NOT EXISTS, so applying a schema that is already
// current changes nothing and reports no error.
func ApplySchema(ctx context.Context, db *sql.DB) error {
	if err := AssertForeignKeysEnforced(ctx, db); err != nil {
		return err
	}

	statements, err := splitStatements(schema)
	if err != nil {
		return fmt.Errorf("pocketbase: read schema: %w", err)
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("pocketbase: begin schema transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("pocketbase: apply %q: %w", summarize(statement), err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("pocketbase: commit schema: %w", err)
	}

	return nil
}

// splitStatements cuts the script into separately executable statements and
// drops its comments, because most drivers execute one statement per call.
func splitStatements(script string) ([]string, error) {
	var scanner scriptScanner
	if err := scanner.scan(script); err != nil {
		return nil, err
	}

	return scanner.statements, nil
}

// scriptScanner walks a SQL script one byte at a time, tracking whether it
// stands in quoted text, in a comment, or in statement text.
type scriptScanner struct {
	state      scanState
	current    strings.Builder
	statements []string
}

func (s *scriptScanner) scan(script string) error {
	for index := 0; index < len(script); index++ {
		var next byte
		if index+1 < len(script) {
			next = script[index+1]
		}

		if s.consume(script[index], next) {
			index++
		}
	}

	return s.finish()
}

// consume takes one character and reports whether it also consumed the next.
func (s *scriptScanner) consume(character, next byte) bool {
	switch s.state {
	case scanLineComment:
		if character == '\n' {
			s.state = scanSQL
			s.current.WriteByte(character)
		}
	case scanBlockComment:
		if character == '*' && next == '/' {
			s.state = scanSQL

			return true
		}
	case scanString:
		s.current.WriteByte(character)

		if character == '\'' {
			s.state = scanSQL
		}
	case scanSQL:
		return s.consumeStatement(character, next)
	}

	return false
}

func (s *scriptScanner) consumeStatement(character, next byte) bool {
	switch {
	case character == '-' && next == '-':
		s.state = scanLineComment

		return true
	case character == '/' && next == '*':
		s.state = scanBlockComment

		return true
	case character == '\'':
		s.state = scanString
		s.current.WriteByte(character)
	case character == ';' && !endsInsideTrigger(s.current.String()):
		s.endStatement()
	default:
		s.current.WriteByte(character)
	}

	return false
}

func (s *scriptScanner) endStatement() {
	if statement := strings.TrimSpace(s.current.String()); statement != "" {
		s.statements = append(s.statements, statement)
	}

	s.current.Reset()
}

// finish rejects a script the scanner could not fully consume, so a truncated
// file fails loudly instead of applying a partial schema.
func (s *scriptScanner) finish() error {
	switch s.state {
	case scanString:
		return errors.New("unterminated string literal")
	case scanBlockComment:
		return errors.New("unterminated block comment")
	case scanSQL, scanLineComment:
	}

	if trailing := strings.TrimSpace(s.current.String()); trailing != "" {
		return fmt.Errorf("%q is missing a terminating semicolon", summarize(trailing))
	}

	return nil
}

// endsInsideTrigger reports whether a semicolon closing this text would land in
// a trigger body, whose own statements end in semicolons and which runs to END.
func endsInsideTrigger(statement string) bool {
	upper := strings.ToUpper(statement)
	if !strings.Contains(upper, "CREATE TRIGGER") {
		return false
	}

	return !strings.HasSuffix(strings.TrimRight(upper, " \t\r\n"), "END")
}

// summarize names a statement by its opening words, which is enough to identify
// the object it creates.
func summarize(statement string) string {
	words := strings.Fields(statement)
	if len(words) > statementLabelWords {
		words = words[:statementLabelWords]
	}

	return strings.Join(words, " ")
}

// AssertForeignKeysEnforced refuses a connection that does not enforce foreign
// keys. The composite keys guarding Super Admin and link grants are constraints
// only while this is on; with it off they are comments.
func AssertForeignKeysEnforced(ctx context.Context, db *sql.DB) error {
	var enabled int
	if err := db.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&enabled); err != nil {
		return fmt.Errorf("pocketbase: read foreign_keys pragma: %w", err)
	}

	if enabled != 1 {
		return errors.New("pocketbase: foreign keys are not enforced on this connection; " +
			"open the database with _pragma=foreign_keys(ON)")
	}

	return nil
}

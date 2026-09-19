package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"

	"github.com/Ilavrita/Ilavrita/packages/conformance"
)

// issueCodeValueSet and issueSeverityValueSet are what R4 binds an
// OperationOutcome's issue to. Both bindings are required, so a value outside
// them is not a FHIR issue.
const (
	issueCodeValueSet     = "http://hl7.org/fhir/ValueSet/issue-type"
	issueSeverityValueSet = "http://hl7.org/fhir/ValueSet/issue-severity"
)

// declaredConstants returns the string values of every constant of one type
// declared in a file.
//
// Read from the source rather than listed here, because a list is a thing that
// goes out of date silently: a code added next year would be covered by this
// the day it is written, and a list would not.
func declaredConstants(t *testing.T, file, named string) []string {
	t.Helper()

	parsed, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}

	var held []string

	ast.Inspect(parsed, func(node ast.Node) bool {
		spec, isValue := node.(*ast.ValueSpec)
		if !isValue || spec.Type == nil {
			return true
		}

		if declared, isName := spec.Type.(*ast.Ident); !isName || declared.Name != named {
			return true
		}

		for _, value := range spec.Values {
			literal, isLiteral := value.(*ast.BasicLit)
			if !isLiteral {
				continue
			}

			held = append(held, literal.Value[1:len(literal.Value)-1])
		}

		return true
	})

	return held
}

// TestEveryIssueCodeIsOneR4Defines.
//
// Every refusal this server answers with renders through one constructor, so
// what makes an OperationOutcome mean anything is the code inside it. A code
// outside R4's own set produces a body that validates as FHIR and says nothing
// a client can act on — which is worse than an error, because it looks handled.
//
// The codes are read from the source, so this covers every refusal this build
// can produce rather than the handful a test happened to provoke.
func TestEveryIssueCodeIsOneR4Defines(t *testing.T) {
	model, err := conformance.Terminologies()
	if err != nil {
		t.Fatalf("read the terminology: %v", err)
	}

	for _, held := range []struct{ named, valueSet, typed string }{
		{"issue code", issueCodeValueSet, "IssueCode"},
		{"issue severity", issueSeverityValueSet, "IssueSeverity"},
	} {
		t.Run(held.named, func(t *testing.T) {
			admitted, resolved := model.Admits(held.valueSet)
			if !resolved {
				t.Fatalf("this build cannot resolve %s, so it cannot check its own codes", held.valueSet)
			}

			declared := declaredConstants(t, "../../packages/fhir/outcome.go", held.typed)
			if len(declared) == 0 {
				t.Fatalf("no %s constants were found, so this rule guards nothing", held.typed)
			}

			admits := map[string]bool{}
			for _, one := range admitted.Codes() {
				admits[one.Code] = true
			}

			for _, code := range declared {
				if !admits[code] {
					t.Errorf("%q is not a %s R4 defines", code, held.named)
				}
			}

			t.Logf("checked %d %s(s) against %d admitted", len(declared), held.named, admitted.Size())
		})
	}
}

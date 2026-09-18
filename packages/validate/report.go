// Package validate checks a resource against the rules this build can state.
//
// It is structural validation and says so. There is no element model here: this
// build ships no StructureDefinitions, so it cannot tell an element R4 defines
// from one nobody has ever heard of, and it cannot check a cardinality or a
// terminology binding. What it does check is the set of rules that hold for
// every R4 resource whatever its definition — how JSON represents FHIR, what an
// id is, what a reference is — plus the syntax of the elements this build
// already asserts something about by indexing them.
//
// A resource that passes is well-formed. It may still be clinically
// nonsensical, and that is the gap this package does not close.
package validate

import (
	"slices"
	"strings"

	"github.com/Ilavrita/Ilavrita/packages/fhir"
)

// Severity is how badly one issue matters. Only two are used: something that
// makes the resource invalid, and something a client should know about that
// does not.
type Severity string

// The severities a report carries.
const (
	SeverityError   Severity = "error"
	SeverityWarning Severity = "warning"
)

// Issue is one thing wrong with a resource.
//
// Expression names where, in FHIRPath's own notation — "Observation.subject" —
// because an issue a client cannot locate is one they have to find by reading
// the whole body.
type Issue struct {
	Severity   Severity
	Expression string
	Detail     string
}

// Report is everything one resource was found to be.
//
// It is a value with unexported contents: a report something could append to
// after it was answered would describe a different resource from the one that
// was checked.
type Report struct {
	issues []Issue
}

// OK reports whether the resource may be stored. A warning does not stop a
// write: it is something the client should know, not something wrong.
func (r Report) OK() bool {
	for _, issue := range r.issues {
		if issue.Severity == SeverityError {
			return false
		}
	}

	return true
}

// Issues returns what was found, in the order it was found.
func (r Report) Issues() []Issue { return slices.Clone(r.issues) }

// Outcome renders the report as the OperationOutcome R4 answers $validate with.
//
// A clean report is one information issue saying so, because an outcome with no
// issue at all is not a valid OperationOutcome.
func (r Report) Outcome() fhir.OperationOutcome {
	if len(r.issues) == 0 {
		return fhir.OperationOutcome{
			ResourceType: "OperationOutcome",
			Issue: []fhir.Issue{{
				Severity:    fhir.SeverityInformation,
				Code:        fhir.CodeInformational,
				Diagnostics: "No issue detected during validation.",
			}},
		}
	}

	issues := make([]fhir.Issue, 0, len(r.issues))

	for _, issue := range r.issues {
		severity := fhir.SeverityError
		if issue.Severity == SeverityWarning {
			severity = fhir.SeverityWarning
		}

		issues = append(issues, fhir.Issue{
			Severity:    severity,
			Code:        fhir.CodeInvalid,
			Diagnostics: issue.Detail,
			Expression:  []string{issue.Expression},
		})
	}

	return fhir.OperationOutcome{ResourceType: "OperationOutcome", Issue: issues}
}

// Error renders the report as one line, for a refusal that carries no outcome
// of its own.
func (r Report) Error() string {
	stated := make([]string, 0, len(r.issues))

	for _, issue := range r.issues {
		if issue.Severity == SeverityError {
			stated = append(stated, issue.Expression+": "+issue.Detail)
		}
	}

	return strings.Join(stated, "; ")
}

// note records one finding.
func (r *Report) note(severity Severity, expression, detail string) {
	r.issues = append(r.issues, Issue{Severity: severity, Expression: expression, Detail: detail})
}

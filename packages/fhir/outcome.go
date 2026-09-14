package fhir

// IssueSeverity classifies how badly an operation failed.
type IssueSeverity string

const (
	SeverityFatal       IssueSeverity = "fatal"
	SeverityError       IssueSeverity = "error"
	SeverityWarning     IssueSeverity = "warning"
	SeverityInformation IssueSeverity = "information"
)

// IssueCode names the reason an operation failed, drawn from the FHIR
// issue-type value set.
type IssueCode string

const (
	CodeNotSupported IssueCode = "not-supported"
	CodeNotFound     IssueCode = "not-found"
	CodeInvalid      IssueCode = "invalid"
	CodeSecurity     IssueCode = "security"
	CodeConflict     IssueCode = "conflict"
	CodeException    IssueCode = "exception"
)

// OperationOutcome is the only error shape permitted on FHIR routes. Runtime,
// database and framework errors are translated into this type so that internal
// detail never reaches a client (FR-007).
type OperationOutcome struct {
	ResourceType string  `json:"resourceType"`
	Issue        []Issue `json:"issue"`
}

// Issue is a single problem reported within an OperationOutcome.
type Issue struct {
	Severity    IssueSeverity `json:"severity"`
	Code        IssueCode     `json:"code"`
	Diagnostics string        `json:"diagnostics,omitempty"`
}

// NewOperationOutcome builds a single-issue outcome.
//
// Diagnostics are written for the API consumer. They must never carry resource
// bodies, stack traces or internal identifiers.
func NewOperationOutcome(severity IssueSeverity, code IssueCode, diagnostics string) OperationOutcome {
	return OperationOutcome{
		ResourceType: "OperationOutcome",
		Issue: []Issue{{
			Severity:    severity,
			Code:        code,
			Diagnostics: diagnostics,
		}},
	}
}

package fhir

// IssueSeverity classifies how badly an operation failed.
type IssueSeverity string

// Severity levels an OperationOutcome issue can carry.
const (
	SeverityFatal       IssueSeverity = "fatal"
	SeverityError       IssueSeverity = "error"
	SeverityWarning     IssueSeverity = "warning"
	SeverityInformation IssueSeverity = "information"
)

// IssueCode names the reason an operation failed, drawn from the FHIR
// issue-type value set.
type IssueCode string

// Issue codes Ilavrita reports today.
const (
	CodeNotSupported IssueCode = "not-supported"
	CodeNotFound     IssueCode = "not-found"
	CodeInvalid      IssueCode = "invalid"
	CodeSecurity     IssueCode = "security"
	CodeConflict     IssueCode = "conflict"
	CodeException    IssueCode = "exception"

	// CodeLogin reports that nothing identified the caller, which a client can
	// tell from a refused one by the code alone, without reading the status.
	CodeLogin IssueCode = "login"

	// CodeForbidden reports a caller this server knows and refuses. Paired with
	// CodeLogin it separates "nobody is asking" from "you may not", which a
	// client can tell apart from the body alone.
	CodeForbidden IssueCode = "forbidden"

	// CodeDeleted reports a resource that existed and was deleted. Reusing
	// CodeNotFound here would make 404 and 410 indistinguishable to a client
	// that parses only the body.
	CodeDeleted IssueCode = "deleted"

	// CodeDuplicate reports a create naming a logical id another resource holds.
	CodeDuplicate IssueCode = "duplicate"

	// CodeThrottled reports a request refused for its rate rather than its
	// content. R4 lists it under "transient": the same request may succeed
	// later, which is exactly what a login limit is saying.
	CodeThrottled IssueCode = "throttled"

	// CodeInformational reports something that is not a problem. An
	// OperationOutcome with no issue in it is not one, so a validation that
	// found nothing says that rather than saying nothing.
	CodeInformational IssueCode = "informational"

	// CodeTooCostly reports a request this server refuses to spend resources on,
	// such as a body larger than it accepts. It is distinct from CodeInvalid:
	// the request is well formed, and only its size is refused.
	CodeTooCostly IssueCode = "too-costly"
)

// OperationOutcome is the only error shape permitted on FHIR routes. Runtime and
// database errors are translated here so internal detail never reaches a client.
type OperationOutcome struct {
	ResourceType string  `json:"resourceType"`
	Issue        []Issue `json:"issue"`
}

// Issue is a single problem reported within an OperationOutcome.
type Issue struct {
	Severity    IssueSeverity `json:"severity"`
	Code        IssueCode     `json:"code"`
	Diagnostics string        `json:"diagnostics,omitempty"`

	// Expression names where the issue is, in FHIRPath's own notation. It is
	// carried only by validation: an issue a client cannot locate is one they
	// have to find by reading the whole body back.
	Expression []string `json:"expression,omitempty"`
}

// NewOperationOutcome builds a single-issue outcome. Diagnostics must never carry
// resource bodies, stack traces or internal identifiers.
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

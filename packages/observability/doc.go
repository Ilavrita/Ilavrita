// Package observability provides structured logging, request correlation and
// health reporting.
//
// Logs carry a request identifier so an operator can connect an API failure to
// a server log. Resource bodies are never logged, because they can contain
// patient data (FR-034, R-008).
package observability

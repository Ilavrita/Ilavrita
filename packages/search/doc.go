// Package search turns a FHIR search query into a backend-independent plan.
//
// The pipeline is: query string -> parser -> search AST -> SearchParameter
// registry -> authorization and tenant predicates -> planner -> backend query
// compiler -> searchset Bundle.
//
// Nothing in this package may contain SQL. Keeping the plan abstract is what
// allows a second storage backend to be added without reimplementing FHIR
// search semantics (FR-011, FR-017, R-001).
package search

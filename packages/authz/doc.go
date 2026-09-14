// Package authz decides who may see or change a resource.
//
// Authorization runs before results are returned, and search predicates take
// part in query planning rather than filtering a broad result set in memory
// (FR-028).
package authz

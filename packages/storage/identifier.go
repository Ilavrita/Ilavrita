package storage

// TenantID scopes every stored record to one tenant.
type TenantID string

// ResourceType is a FHIR resource type name, such as "Patient".
type ResourceType string

// LogicalID is the FHIR logical id of a resource. It is the server's published
// identifier and is kept separate from any identifier the storage backend
// happens to use internally (FR-003).
type LogicalID string

// VersionID identifies one immutable version of a resource. Once issued it
// never changes or gets reused (FR-006).
type VersionID string

// ResourceKey addresses a single resource within a tenant.
type ResourceKey struct {
	Tenant TenantID
	Type   ResourceType
	ID     LogicalID
}

package storage

import "fmt"

// ProjectID scopes every stored record to one Project, which is Ilavrita's
// isolation boundary (FR-045).
type ProjectID string

// ResourceType is a FHIR resource type name, such as "Patient".
type ResourceType string

// LogicalID is the FHIR logical id: the server's published identifier, kept
// separate from whatever the storage backend uses internally.
type LogicalID string

// VersionID identifies one immutable version of a resource. Once issued it
// never changes or gets reused (FR-006).
type VersionID string

// ResourceKey addresses a single resource within a Project. Project is part of
// the key, not an optional argument, so no storage call can omit it (FR-045).
type ResourceKey struct {
	Project ProjectID
	Type    ResourceType
	ID      LogicalID
}

// NewResourceKey rejects a key missing any part, so an empty Project can never
// reach a query as a silently matching value.
func NewResourceKey(project ProjectID, resourceType ResourceType, id LogicalID) (ResourceKey, error) {
	switch {
	case project == "":
		return ResourceKey{}, fmt.Errorf("storage: resource key needs a project")
	case resourceType == "":
		return ResourceKey{}, fmt.Errorf("storage: resource key needs a resource type")
	case id == "":
		return ResourceKey{}, fmt.Errorf("storage: resource key needs a logical id")
	}

	return ResourceKey{Project: project, Type: resourceType, ID: id}, nil
}

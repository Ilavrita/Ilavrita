package search_test

import "github.com/Ilavrita/Ilavrita/packages/storage"

// storageType is a one-line convenience so the cases below read as FHIR type
// names rather than as conversions.
func storageType(name string) storage.ResourceType { return storage.ResourceType(name) }

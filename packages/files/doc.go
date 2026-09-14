// Package files abstracts payload storage for Binary and DocumentReference
// content.
//
// Large payloads stay out of the canonical database rows. Local disk serves
// development; an S3-compatible store serves deployments that need external
// object storage (FR-032).
package files

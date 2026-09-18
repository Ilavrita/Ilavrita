package pocketbase

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/Ilavrita/Ilavrita/packages/audit"
)

// AuditStore appends audit events. It writes through whatever transaction the
// context carries, which is what makes an event atomic with the thing it
// describes: a write that commits without its record, or a record without its
// write, is the pair an incident cannot reconcile (AUD-1).
type AuditStore struct {
	db *sql.DB
}

var _ audit.Recorder = (*AuditStore)(nil)

// NewAuditStore binds a recorder to an open database.
func NewAuditStore(db *sql.DB) *AuditStore {
	return &AuditStore{db: db}
}

// Record appends one event. There is no update and no delete here, and the
// table refuses both anyway.
func (s *AuditStore) Record(ctx context.Context, event audit.Event) error {
	const insert = "INSERT INTO audit_events" +
		" (project_id, id, at, principal_kind, principal_id, membership_id," +
		" action, res_type, res_id, outcome, detail)" +
		" VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)"

	resource := event.Resource()

	var resourceType, resourceID any
	if resource.Valid() {
		resourceType, resourceID = string(resource.Type), string(resource.ID)
	}

	var membership any
	if event.Membership() != "" {
		membership = string(event.Membership())
	}

	_, err := conn(ctx, s.db).ExecContext(ctx, insert,
		string(event.Project()), string(event.ID()), event.At().UnixMilli(),
		string(event.Principal().Kind), string(event.Principal().ID), membership,
		string(event.Action()), resourceType, resourceID,
		string(event.Outcome()), string(event.Reason()),
	)
	if err != nil {
		return fmt.Errorf("pocketbase: record an audit event: %w", err)
	}

	return nil
}

package pocketbase

import (
	"context"
	"crypto/rand"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Ilavrita/Ilavrita/packages/audit"
	"github.com/Ilavrita/Ilavrita/packages/project"
	"github.com/Ilavrita/Ilavrita/packages/storage"
)

func anEvent(t *testing.T, proj project.ID, outcome audit.Outcome) audit.Event {
	t.Helper()

	id, err := audit.MintEventID(rand.Reader)
	if err != nil {
		t.Fatalf("mint an event id: %v", err)
	}

	event, err := audit.NewEvent(audit.EventConfig{
		Project:   proj,
		ID:        id,
		At:        time.Date(2026, time.March, 1, 9, 0, 0, 0, time.UTC),
		Principal: project.PrincipalRef{Kind: project.PrincipalUser, ID: "usr_1"},
		Action:    audit.ActionRead,
		Resource:  project.ProfileRef{Type: "Observation", ID: "obs-1"},
		Outcome:   outcome,
	})
	if err != nil {
		t.Fatalf("build an event: %v", err)
	}

	return event
}

func countEvents(t *testing.T, store *ResourceStore) int {
	t.Helper()

	var count int
	if err := store.db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM audit_events").Scan(&count); err != nil {
		t.Fatalf("count audit events: %v", err)
	}

	return count
}

// TestAnAuditEventCommitsWithTheThingItDescribes. A record written separately
// is one that can be lost exactly when it matters: the process that wrote the
// resource and then died is the case an incident asks about (AUD-1).
func TestAnAuditEventCommitsWithTheThingItDescribes(t *testing.T) {
	store, db := newStore(t)
	recorder := NewAuditStore(db)

	key := observationKey("prj_a", "obs-1")
	record := storage.ResourceRecord{Key: key, Content: []byte(`{"resourceType":"Observation"}`)}

	undone := errors.New("the interaction failed after both writes")

	err := store.WithinTransaction(t.Context(), func(ctx context.Context) error {
		if err := store.Create(ctx, fullScope("prj_a", "Observation"), record); err != nil {
			return err
		}

		if err := recorder.Record(ctx, anEvent(t, "prj_a", audit.OutcomeAllowed)); err != nil {
			return err
		}

		return undone
	})

	if !errors.Is(err, undone) {
		t.Fatalf("the transaction ended with %v", err)
	}

	if count := countEvents(t, store); count != 0 {
		t.Errorf("the rolled-back transaction left %d audit events", count)
	}

	if _, err := store.Read(t.Context(), fullScope("prj_a", "Observation"), key); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("the rolled-back transaction left the resource: %v", err)
	}
}

// TestAnAuditEventIsAppendOnly. A record whoever acted can revise afterwards is
// not a record, so the table refuses rather than trusting no code tries (AUD-4).
func TestAnAuditEventIsAppendOnly(t *testing.T) {
	store, db := newStore(t)

	if err := NewAuditStore(db).Record(t.Context(), anEvent(t, "prj_a", audit.OutcomeRefused)); err != nil {
		t.Fatalf("record: %v", err)
	}

	refused := map[string]string{
		"an update":                  "UPDATE audit_events SET outcome = 'allowed'",
		"an update of who":           "UPDATE audit_events SET principal_id = 'someone-else'",
		"a delete":                   "DELETE FROM audit_events",
		"a delete by key":            "DELETE FROM audit_events WHERE project_id = 'prj_a'",
		"a free-text detail":         "INSERT INTO audit_events (project_id, id, at, principal_kind, principal_id, action, outcome, detail) VALUES ('prj_a', 'aud_x', 0, 'user', 'usr_1', 'read', 'refused', 'password was hunter2')",
		"an id belonging to no type": "INSERT INTO audit_events (project_id, id, at, principal_kind, principal_id, action, res_id, outcome) VALUES ('prj_a', 'aud_y', 0, 'user', 'usr_1', 'read', 'obs-1', 'allowed')",
	}

	for name, statement := range refused {
		if _, err := db.ExecContext(t.Context(), statement); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}

	if count := countEvents(t, store); count != 1 {
		t.Errorf("the trail holds %d events after the refusals, want the one it started with", count)
	}
}

// TestAPurgeableProjectCanShedItsTrail, because erasing a Project has to be
// possible and erasing what it did while it existed has to not be. This is the
// rule fhir_resource_history already carries.
func TestAPurgeableProjectCanShedItsTrail(t *testing.T) {
	store, db := newStore(t)

	if err := NewAuditStore(db).Record(t.Context(), anEvent(t, "prj_a", audit.OutcomeAllowed)); err != nil {
		t.Fatalf("record: %v", err)
	}

	if _, err := db.ExecContext(t.Context(), "DELETE FROM audit_events"); err == nil {
		t.Fatal("an active Project shed its trail")
	}

	if _, err := db.ExecContext(t.Context(),
		"UPDATE projects SET state = 'deleting' WHERE id = 'prj_a'"); err != nil {
		t.Fatalf("mark the project deleting: %v", err)
	}

	if _, err := db.ExecContext(t.Context(), "DELETE FROM audit_events"); err != nil {
		t.Fatalf("a deleting Project could not shed its trail: %v", err)
	}

	if count := countEvents(t, store); count != 0 {
		t.Errorf("the purge left %d events", count)
	}
}

// TestAnEventOutsideEveryProjectIsRecordable. A login naming a slug that
// resolves to nothing still happened, and the row must not record the caller's
// own spelling of that slug in a tenant column.
func TestAnEventOutsideEveryProjectIsRecordable(t *testing.T) {
	store, db := newStore(t)

	if err := NewAuditStore(db).Record(t.Context(),
		anEvent(t, project.SystemScope, audit.OutcomeRefused)); err != nil {
		t.Fatalf("record a system-scoped event: %v", err)
	}

	if count := countEvents(t, store); count != 1 {
		t.Errorf("the trail holds %d events, want the system-scoped one", count)
	}
}

// TestAnEventIsBuiltOnlyFromWhatAnIncidentAsks.
func TestAnEventIsBuiltOnlyFromWhatAnIncidentAsks(t *testing.T) {
	sound := audit.EventConfig{
		Project:   "prj_a",
		ID:        "aud_1",
		At:        time.Date(2026, time.March, 1, 9, 0, 0, 0, time.UTC),
		Principal: project.PrincipalRef{Kind: project.PrincipalUser, ID: "usr_1"},
		Action:    audit.ActionRead,
		Outcome:   audit.OutcomeAllowed,
	}

	if _, err := audit.NewEvent(sound); err != nil {
		t.Fatalf("a complete event was refused: %v", err)
	}

	refused := map[string]func(audit.EventConfig) audit.EventConfig{
		"no project": func(c audit.EventConfig) audit.EventConfig { c.Project = ""; return c },
		"no id":      func(c audit.EventConfig) audit.EventConfig { c.ID = ""; return c },
		"no instant": func(c audit.EventConfig) audit.EventConfig { c.At = time.Time{}; return c },
		"nobody":     func(c audit.EventConfig) audit.EventConfig { c.Principal = project.PrincipalRef{}; return c },
		"no action":  func(c audit.EventConfig) audit.EventConfig { c.Action = ""; return c },
		"no outcome": func(c audit.EventConfig) audit.EventConfig { c.Outcome = ""; return c },
		"a made-up outcome": func(c audit.EventConfig) audit.EventConfig {
			c.Outcome = audit.Outcome("probably-fine")

			return c
		},
		"a reason nobody declared": func(c audit.EventConfig) audit.EventConfig {
			c.Reason = audit.Reason("the password was hunter2")

			return c
		},
	}

	for name, break_ := range refused {
		if _, err := audit.NewEvent(break_(sound)); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

// TestAnEventIdentifierIsDrawnInItsOwnNamespace, so a row cannot be mistaken
// for one of another family and the table's own CHECK can say so.
func TestAnEventIdentifierIsDrawnInItsOwnNamespace(t *testing.T) {
	id, err := audit.MintEventID(rand.Reader)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	if !strings.HasPrefix(string(id), "aud_") {
		t.Errorf("minted %q, want an aud_ identifier", id)
	}

	second, err := audit.MintEventID(rand.Reader)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	if second == id {
		t.Error("two events were minted the same identifier")
	}
}

package main

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/Ilavrita/Ilavrita/packages/audit"
	"github.com/Ilavrita/Ilavrita/packages/fhir"
	"github.com/Ilavrita/Ilavrita/packages/project"
	"github.com/Ilavrita/Ilavrita/packages/storage"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/router"
)

// recordedEvent is one audit_events row as the tests read it back.
type recordedEvent struct {
	project       string
	principalKind string
	principalID   string
	membership    sql.NullString
	action        string
	resourceType  sql.NullString
	resourceID    sql.NullString
	outcome       string
	detail        string
}

// recorded reads every event, oldest first.
func recorded(t *testing.T, db *sql.DB) []recordedEvent {
	t.Helper()

	rows, err := db.QueryContext(context.Background(),
		"SELECT project_id, principal_kind, principal_id, membership_id, action,"+
			" res_type, res_id, outcome, detail FROM audit_events ORDER BY at, id")
	if err != nil {
		t.Fatalf("read the audit trail: %v", err)
	}
	defer func() { _ = rows.Close() }()

	var events []recordedEvent

	for rows.Next() {
		var event recordedEvent
		if err := rows.Scan(&event.project, &event.principalKind, &event.principalID,
			&event.membership, &event.action, &event.resourceType, &event.resourceID,
			&event.outcome, &event.detail); err != nil {
			t.Fatalf("scan an audit event: %v", err)
		}

		events = append(events, event)
	}

	if err := rows.Err(); err != nil {
		t.Fatalf("read the audit trail: %v", err)
	}

	return events
}

// onlyEvent reads back the single event a test expects, failing loudly when the
// trail holds none or several: an audit written twice is as wrong as one
// written never.
func onlyEvent(t *testing.T, db *sql.DB) recordedEvent {
	t.Helper()

	events := recorded(t, db)
	if len(events) != 1 {
		t.Fatalf("the audit trail holds %d events, want exactly one: %+v", len(events), events)
	}

	return events[0]
}

// auditingServer wires a whole server whose interactions are recorded.
func auditingServer(t *testing.T) (http.Handler, *sql.DB) {
	t.Helper()

	db := preparedDatabase(t)
	seedProject(t, db, homeProject)
	serveProject(t, db, homeProject, everyAction)

	return fhirRoutes(t), db
}

// TestEverySignificantInteractionIsRecorded. A log holding only some of what
// happened cannot answer the question an incident asks, and the interaction it
// is missing is the one that will be asked about.
func TestEverySignificantInteractionIsRecorded(t *testing.T) {
	// Both tables: an interaction about one resource type and one about the
	// server are served the same way and have to be recorded the same way.
	served := append(slices.Clone(servedInteractions), systemInteractions...)

	for _, held := range served {
		if _, audits := auditedAs[held.code]; !audits {
			t.Errorf("the %s interaction is served but never recorded", held.code)
		}
	}

	for interaction := range auditedAs {
		if !slices.ContainsFunc(served, func(candidate servedInteraction) bool {
			return candidate.code == interaction
		}) {
			t.Errorf("%s is recorded but this build does not serve it", interaction)
		}
	}
}

// TestEachInteractionWritesOneEvent drives every served interaction and checks
// the trail it leaves: one row, naming the resource and the action.
func TestEachInteractionWritesOneEvent(t *testing.T) {
	routes, db := auditingServer(t)

	id := assertCreate(t, routes, "Organization")

	created := onlyEvent(t, db)
	if created.action != string(storage.ActionWrite) || created.outcome != string(audit.OutcomeAllowed) {
		t.Fatalf("a create recorded %+v", created)
	}

	if created.resourceID.String != id {
		t.Errorf("a create recorded resource %q, want the id it minted (%q)", created.resourceID.String, id)
	}

	if created.resourceType.String != "Organization" {
		t.Errorf("a create recorded type %q", created.resourceType.String)
	}

	if created.principalKind == "" || created.principalID == "" {
		t.Error("a create recorded nobody")
	}

	if !created.membership.Valid || created.membership.String == "" {
		t.Error("a create recorded no membership, so nothing says what standing it acted under")
	}

	interactions := []struct {
		name   string
		call   call
		action storage.Action
	}{
		{"read", call{method: http.MethodGet, path: resourcePath("Organization", id)}, storage.ActionRead},
		{
			"update",
			call{
				method: http.MethodPut, path: resourcePath("Organization", id),
				body: replacement("Organization", id),
			},
			storage.ActionWrite,
		},
		{
			"history",
			call{method: http.MethodGet, path: resourcePath("Organization", id) + "/_history"},
			storage.ActionHistory,
		},
		{
			"vread",
			call{method: http.MethodGet, path: resourcePath("Organization", id) + "/_history/1"},
			storage.ActionHistory,
		},
		{"delete", call{method: http.MethodDelete, path: resourcePath("Organization", id)}, storage.ActionDelete},
	}

	// Two events written in the same millisecond are ordered by id rather than
	// by time, so what each interaction recorded is counted rather than read off
	// the end of the trail.
	want := map[storage.Action]int{storage.ActionWrite: 1}

	for _, interaction := range interactions {
		interaction.call.send(t, routes)
		want[interaction.action]++
	}

	got := map[storage.Action]int{}

	for _, event := range recorded(t, db) {
		got[storage.Action(event.action)]++

		if event.resourceID.String != id {
			t.Errorf("an event recorded resource %q, want %q", event.resourceID.String, id)
		}

		if event.outcome != string(audit.OutcomeAllowed) {
			t.Errorf("an allowed %s recorded outcome %q", event.action, event.outcome)
		}
	}

	for action, count := range want {
		if got[action] != count {
			t.Errorf("the trail holds %d %s events, want %d", got[action], action, count)
		}
	}
}

// TestARefusalIsRecordedAsLoudlyAsASuccess. A log holding only what succeeded
// cannot answer the question an incident asks (AUD-3).
func TestARefusalIsRecordedAsLoudlyAsASuccess(t *testing.T) {
	routes, db := auditingServer(t)

	assertStatus(t, call{
		method: http.MethodGet, path: resourcePath("Organization", "nothing-here"),
	}.send(t, routes), http.StatusNotFound)

	refused := onlyEvent(t, db)

	if refused.outcome != string(audit.OutcomeRefused) {
		t.Errorf("a refused read recorded outcome %q", refused.outcome)
	}

	if refused.detail != string(audit.ReasonNotFound) {
		t.Errorf("a refused read recorded detail %q, want %q", refused.detail, audit.ReasonNotFound)
	}

	if refused.resourceID.String != "nothing-here" {
		t.Errorf("a refused read recorded resource %q", refused.resourceID.String)
	}
}

// TestAnUnidentifiedRequestIsRecordedAgainstNobodyRatherThanNotAtAll. A request
// nothing identified still happened, and it is the one an intrusion looks like.
func TestAnUnidentifiedRequestIsRecordedAgainstNobodyRatherThanNotAtAll(t *testing.T) {
	routes, db := auditingServer(t)

	assertStatus(t, call{
		method: http.MethodGet, path: resourcePath("Organization", "anything"), anonymous: true,
	}.send(t, routes), http.StatusUnauthorized)

	anonymous := onlyEvent(t, db)

	if anonymous.outcome != string(audit.OutcomeRefused) || anonymous.detail != string(audit.ReasonUnidentified) {
		t.Errorf("an unidentified read recorded %+v", anonymous)
	}

	if anonymous.principalKind != string(audit.UnidentifiedPrincipal.Kind) {
		t.Errorf("an unidentified read recorded principal kind %q", anonymous.principalKind)
	}

	if anonymous.membership.Valid {
		t.Error("an unidentified read recorded a membership")
	}
}

// TestAnAuditRowCarriesNoResourceContent. The record exists to be trusted about
// the resources it watches, not to become a second copy of them (AUD-2).
func TestAnAuditRowCarriesNoResourceContent(t *testing.T) {
	routes, db := auditingServer(t)

	const distinctive = "a-name-no-audit-row-may-repeat"

	assertStatus(t, call{
		method: http.MethodPost, path: fhir.BasePath + "/Organization",
		body: `{"resourceType":"Organization","name":"` + distinctive + `"}`,
	}.send(t, routes), http.StatusCreated)

	rows, err := db.QueryContext(context.Background(), "SELECT * FROM audit_events")
	if err != nil {
		t.Fatalf("read the audit trail: %v", err)
	}
	defer func() { _ = rows.Close() }()

	columns, err := rows.Columns()
	if err != nil {
		t.Fatalf("read the audit columns: %v", err)
	}

	for rows.Next() {
		cells := make([]any, len(columns))
		for index := range cells {
			cells[index] = new(sql.NullString)
		}

		if err := rows.Scan(cells...); err != nil {
			t.Fatalf("scan an audit event: %v", err)
		}

		for index, cell := range cells {
			held, _ := cell.(*sql.NullString)
			if held.Valid && held.String == distinctive {
				t.Errorf("column %s carries the resource's own content", columns[index])
			}
		}
	}
}

// failingRecorder is a recorder that cannot write, which is the only way to ask
// what this server does when it cannot account for what it is about to do.
type failingRecorder struct{ err error }

func (f failingRecorder) Record(context.Context, audit.Event) error { return f.err }

// TestAWriteThatCannotBeRecordedIsNotPerformed. An action this server cannot
// account for afterwards is one it must not take, so the record and the write
// share a commit boundary (AUD-1).
func TestAWriteThatCannotBeRecordedIsNotPerformed(t *testing.T) {
	routes, db := auditingServer(t)

	serving.audits = failingRecorder{err: errors.New("the audit trail is unavailable")}

	assertStatus(t, call{
		method: http.MethodPost, path: fhir.BasePath + "/Organization",
		body: `{"resourceType":"Organization","name":"Clinic"}`,
	}.send(t, routes), http.StatusInternalServerError)

	assertStoredCount(t, db, "Organization", 0)

	if events := recorded(t, db); len(events) != 0 {
		t.Errorf("a failed recorder still wrote %d events", len(events))
	}
}

// TestAnAllowedInteractionIsRecordedBeforeItIsAnswered. The client is told a
// write succeeded only once the record of it has committed, so no answer this
// server gave is one it cannot account for.
func TestAnAllowedInteractionIsRecordedBeforeItIsAnswered(t *testing.T) {
	routes, db := auditingServer(t)

	id := assertCreate(t, routes, "Organization")

	// The row and its record are both there, and the response named the row.
	assertStoredCount(t, db, "Organization", 1)

	if event := onlyEvent(t, db); event.resourceID.String != id {
		t.Errorf("the record names %q and the answer named %q", event.resourceID.String, id)
	}
}

// TestASuccessfulLoginIsRecordedAgainstWhoProvedIt.
func TestASuccessfulLoginIsRecordedAgainstWhoProvedIt(t *testing.T) {
	routes, db := authenticatedServer(t)

	assertStatus(t, logInOver(t, routes, loginAddress, loginPassword), http.StatusOK)

	event := onlyEvent(t, db)

	if event.action != string(audit.ActionAuthenticate) || event.outcome != string(audit.OutcomeAllowed) {
		t.Fatalf("a successful login recorded %+v", event)
	}

	if event.project != "clinic-a" {
		t.Errorf("a successful login recorded project %q", event.project)
	}

	if event.principalKind != string(project.PrincipalUser) || event.principalID == "" {
		t.Errorf("a successful login recorded principal %q/%q", event.principalKind, event.principalID)
	}

	if !event.membership.Valid || event.membership.String == "" {
		t.Error("a successful login recorded no membership")
	}

	if event.resourceType.Valid || event.resourceID.Valid {
		t.Error("a login recorded a resource; it is about a credential, not a row")
	}
}

// TestARefusedLoginRecordsTheSameRowWhateverRefusedIt. The route answers a
// wrong password and an unknown address alike; a record naming the user it
// found would say which of the checks got that far and turn the trail into the
// address oracle the uniform answer exists to prevent (AUD-5).
func TestARefusedLoginRecordsTheSameRowWhateverRefusedIt(t *testing.T) {
	refusals := map[string]struct{ address, password string }{
		"a wrong password":                        {loginAddress, "not the password"},
		"an unknown address":                      {"nobody@example.test", loginPassword},
		"an unknown address and a wrong password": {"nobody@example.test", "not the password"},
	}

	var shapes []recordedEvent

	for name, attempt := range refusals {
		routes, db := authenticatedServer(t)

		assertStatus(t, logInOver(t, routes, attempt.address, attempt.password), http.StatusUnauthorized)

		event := onlyEvent(t, db)
		if event.outcome != string(audit.OutcomeRefused) {
			t.Errorf("%s recorded outcome %q", name, event.outcome)
		}

		if event.principalID != string(audit.UnidentifiedPrincipal.ID) {
			t.Errorf("%s recorded principal %q, which says the address was found", name, event.principalID)
		}

		if event.project != string(project.SystemScope) {
			t.Errorf("%s recorded project %q, which says the slug resolved", name, event.project)
		}

		shapes = append(shapes, event)
	}

	for index := 1; index < len(shapes); index++ {
		if shapes[index] != shapes[0] {
			t.Errorf("two refusals recorded different rows:\n%+v\n%+v", shapes[0], shapes[index])
		}
	}
}

// TestAThrottledLoginIsRecordedAsThrottled, because an attempt refused before
// anything was proved is a different fact from one that was proved wrong, and
// the client was already told so.
func TestAThrottledLoginIsRecordedAsThrottled(t *testing.T) {
	routes, db := authenticatedServer(t)

	for range attemptsPerIdentity {
		assertStatus(t, logInOver(t, routes, loginAddress, "not the password"), http.StatusUnauthorized)
	}

	assertStatus(t, logInOver(t, routes, loginAddress, "not the password"), http.StatusTooManyRequests)

	events := recorded(t, db)
	if len(events) != attemptsPerIdentity+1 {
		t.Fatalf("the trail holds %d events, want one per attempt", len(events))
	}

	throttled := 0

	for _, event := range events {
		if event.detail == string(audit.ReasonThrottled) {
			throttled++
		}
	}

	if throttled != 1 {
		t.Errorf("%d attempts recorded as throttled, want the one that was", throttled)
	}
}

// TestALoginThatCannotBeRecordedIssuesNoSession. A token handed out by a
// process that then failed to write down that it had is a credential nothing
// accounts for (AUD-1).
func TestALoginThatCannotBeRecordedIssuesNoSession(t *testing.T) {
	routes, db := authenticatedServer(t)

	serving.audits = failingRecorder{err: errors.New("the audit trail is unavailable")}

	// A credential this server cannot account for is a fault, not a wrong
	// password: answering 401 would both lie and count against the throttle of
	// somebody whose password was right.
	assertStatus(t, logInOver(t, routes, loginAddress, loginPassword), http.StatusInternalServerError)

	var sessions int
	if err := db.QueryRowContext(context.Background(),
		"SELECT COUNT(*) FROM sessions").Scan(&sessions); err != nil {
		t.Fatalf("count sessions: %v", err)
	}

	if sessions != 0 {
		t.Errorf("a login that could not be recorded issued %d sessions", sessions)
	}
}

// TestAnAuditOutageDoesNotLockOutAValidPassword. The throttle counts wrong
// passwords, and a fault in this server is not one: an outage that consumed a
// correct caller's attempts would turn a recording failure into a lockout.
func TestAnAuditOutageDoesNotLockOutAValidPassword(t *testing.T) {
	routes, _ := authenticatedServer(t)

	working := serving.audits
	serving.audits = failingRecorder{err: errors.New("the audit trail is unavailable")}

	for range attemptsPerIdentity + 2 {
		assertStatus(t, logInOver(t, routes, loginAddress, loginPassword), http.StatusInternalServerError)
	}

	serving.audits = working

	// The trail is writable again and the password was always right, so the
	// caller is let in rather than met with the limit.
	assertStatus(t, logInOver(t, routes, loginAddress, loginPassword), http.StatusOK)
}

// interactionUnder drives the decorator with a handler of the test's own, which
// is the only way to ask what it does when an interaction writes and then does
// not succeed. No route this build serves can both write and refuse — that is
// the point of the guards above it — so the contract is checked here rather
// than assumed.
func interactionUnder(t *testing.T, handler func(*core.RequestEvent) error) *httptest.ResponseRecorder {
	t.Helper()

	answer := httptest.NewRecorder()
	asked := httptest.NewRequest(http.MethodPost, fhir.BasePath+"/Organization", nil)
	asked.Header.Set(authorizationField, bearerPrefix+"conformance-token")
	asked.SetPathValue(resourceTypeParameter, "Organization")

	request := &core.RequestEvent{Event: router.Event{Response: answer, Request: asked}}

	if err := serving.recordInteraction(request, audit.ActionWrite, handler); err != nil {
		t.Fatalf("the interaction returned %v", err)
	}

	return answer
}

// writesThenAnswers stores a resource and then answers with the status the case
// is about, which is the shape "wrote, then did not succeed" takes.
func writesThenAnswers(t *testing.T, status int) func(*core.RequestEvent) error {
	t.Helper()

	return func(request *core.RequestEvent) error {
		key := storage.ResourceKey{Project: homeProject, Type: "Organization", ID: "written-anyway"}

		settle(request.Request.Context(), key)

		err := serving.resources.Create(request.Request.Context(),
			storage.NewScope(storage.Grant{
				Project: homeProject, Kind: storage.KindFHIR, Type: "Organization",
				Action: storage.ActionWrite, Source: storage.SourceMembership,
			}, storage.Grant{
				Project: homeProject, Kind: storage.KindFHIR, Type: "Organization",
				Action: storage.ActionRead, Source: storage.SourceMembership,
			}),
			storage.ResourceRecord{Key: key, Content: []byte(`{"resourceType":"Organization"}`)})
		if err != nil {
			t.Fatalf("the test handler could not write: %v", err)
		}

		request.Response.WriteHeader(status)

		return nil
	}
}

// TestAnInteractionThatWritesAndThenRefusesCommitsNothing. A handler may write
// and then refuse — a read-back the caller's own Scope cannot satisfy is the
// case — and the answer decides the transaction, because the handler's own
// return is nil once a refusal has been rendered.
func TestAnInteractionThatWritesAndThenRefusesCommitsNothing(t *testing.T) {
	_, db := auditingServer(t)

	answer := interactionUnder(t, writesThenAnswers(t, http.StatusForbidden))

	if answer.Code != http.StatusForbidden {
		t.Fatalf("the answer was %d, want the refusal the handler rendered", answer.Code)
	}

	assertStoredCount(t, db, "Organization", 0)

	event := onlyEvent(t, db)
	if event.outcome != string(audit.OutcomeRefused) {
		t.Errorf("a refused interaction recorded outcome %q", event.outcome)
	}
}

// TestAnInteractionThatFailsCommitsNothing. A status nobody classified is a
// fault rather than a success, so it undoes its transaction and is recorded as
// one: answering a client from an outcome nobody classified is how a write
// slips through unaccounted for.
func TestAnInteractionThatFailsCommitsNothing(t *testing.T) {
	_, db := auditingServer(t)

	answer := interactionUnder(t, writesThenAnswers(t, http.StatusBadGateway))

	if answer.Code != http.StatusBadGateway {
		t.Fatalf("the answer was %d, want the one the handler rendered", answer.Code)
	}

	assertStoredCount(t, db, "Organization", 0)

	event := onlyEvent(t, db)
	if event.outcome != string(audit.OutcomeFailed) {
		t.Errorf("an unclassified answer recorded outcome %q, want %q", event.outcome, audit.OutcomeFailed)
	}

	if event.detail != string(audit.ReasonUnavailable) {
		t.Errorf("an unclassified answer recorded detail %q", event.detail)
	}
}

// TestAnInteractionThatSucceedsCommitsBoth, so the two tests above fail for the
// answer they gave rather than because nothing was ever written.
func TestAnInteractionThatSucceedsCommitsBoth(t *testing.T) {
	_, db := auditingServer(t)

	answer := interactionUnder(t, writesThenAnswers(t, http.StatusCreated))

	if answer.Code != http.StatusCreated {
		t.Fatalf("the answer was %d", answer.Code)
	}

	assertStoredCount(t, db, "Organization", 1)

	if event := onlyEvent(t, db); event.outcome != string(audit.OutcomeAllowed) {
		t.Errorf("an allowed interaction recorded outcome %q", event.outcome)
	}
}

package main

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/Ilavrita/Ilavrita/packages/authz"
	"github.com/Ilavrita/Ilavrita/packages/fhir"
	"github.com/Ilavrita/Ilavrita/packages/storage"
	sqlite "github.com/Ilavrita/Ilavrita/packages/storage/pocketbase"
)

// Every failure a handler can reach has exactly one answer. A mapping that
// drifts is how a deleted resource starts reading as a missing one.
func TestEveryFailureHasOneAnswer(t *testing.T) {
	answers := map[string]struct {
		failure error
		status  int
		code    fhir.IssueCode
	}{
		"nothing identified the caller": {errNoPrincipal, http.StatusUnauthorized, fhir.CodeLogin},
		"the scope authorizes nothing":  {storage.ErrDenied, http.StatusForbidden, fhir.CodeForbidden},
		"no such resource":              {storage.ErrNotFound, http.StatusNotFound, fhir.CodeNotFound},
		"the resource was deleted":      {storage.ErrDeleted, http.StatusGone, fhir.CodeDeleted},
		"the logical id is taken":       {storage.ErrAlreadyExists, http.StatusConflict, fhir.CodeDuplicate},
		"another writer won the race":   {storage.ErrVersionConflict, http.StatusConflict, fhir.CodeConflict},
		"a port was never wired":        {authz.ErrMissingResolver, http.StatusInternalServerError, fhir.CodeException},
		"the server is not serving":     {errNotServing, http.StatusInternalServerError, fhir.CodeException},
		"a row escaped its scope":       {sqlite.ErrScopeEscape, http.StatusInternalServerError, fhir.CodeException},
		"nobody classified it":          {errors.New("something else"), http.StatusInternalServerError, fhir.CodeException},
	}

	for name, want := range answers {
		t.Run(name, func(t *testing.T) {
			// Wrapped, because that is how every one of these actually arrives.
			got := translate(fmt.Errorf("wrapped: %w", want.failure))

			if got.status != want.status || got.code != want.code {
				t.Fatalf("answered %d %q, want %d %q", got.status, got.code, want.status, want.code)
			}
		})
	}
}

// An internal error can name a resource type, a logical id and a Project. None
// of that may travel to a client, whatever the error itself says.
func TestAnInternalFailureNeverCarriesItsOwnText(t *testing.T) {
	request, recorder := readRequest(t)

	leaked := fmt.Errorf("%w: Organization/secret-id in clinic-a", sqlite.ErrScopeEscape)
	if err := refuse(request, leaked); err != nil {
		t.Fatalf("write the refusal: %v", err)
	}

	assertIssue(t, recorder, http.StatusInternalServerError, fhir.CodeException)

	for _, secret := range []string{"secret-id", "clinic-a", "Organization", "escaped"} {
		if strings.Contains(recorder.Body.String(), secret) {
			t.Fatalf("the response body carries %q: %s", secret, recorder.Body.String())
		}
	}
}

// If-Match is the one header a write's outcome turns on, so what it parses to
// is worth pinning down on its own.
func TestClaimedPreconditionReadsOneQuotedVersion(t *testing.T) {
	accepted := map[string]storage.VersionID{
		`W/"2"`:   "2",
		`"2"`:     "2",
		`  W/"2"`: "2",
		`W/"10"`:  "10",
	}

	for header, want := range accepted {
		claim := readClaim(t, header)
		if claim.kind != namedVersion || claim.version != want {
			t.Fatalf("%q read as %+v, want version %q", header, claim, want)
		}
	}

	// An absent header claims nothing; "*" claims existence, which is a claim
	// RFC 9110 13.1.1 gives its own answer to.
	for _, header := range []string{"", "  "} {
		if claim := readClaim(t, header); claim.stated() {
			t.Fatalf("%q was read as a claim: %+v", header, claim)
		}
	}

	if claim := readClaim(t, "*"); claim.kind != anyVersion || !claim.stated() {
		t.Fatalf(`"*" read as %+v, want a claim of existence`, claim)
	}

	for _, header := range []string{"2", `"1", "2"`, `""`, "W/2", `"un"quoted"`} {
		request, _ := readRequest(t)
		request.Request.Header.Set(ifMatchField, header)

		if _, err := claimedPrecondition(request); !errors.Is(err, malformedIfMatch) {
			t.Fatalf("%q was accepted: %v", header, err)
		}
	}
}

func readClaim(t *testing.T, header string) precondition {
	t.Helper()

	request, _ := readRequest(t)
	request.Request.Header.Set(ifMatchField, header)

	claim, err := claimedPrecondition(request)
	if err != nil {
		t.Fatalf("%q was refused: %v", header, err)
	}

	return claim
}

// A claim is refuted only by a version that contradicts it. "*" names no
// version, so nothing a live resource holds can disagree with it.
func TestOnlyANamedVersionCanDisagree(t *testing.T) {
	claims := map[string]precondition{
		"absent":   {kind: noPrecondition},
		"any":      {kind: anyVersion},
		"the same": {kind: namedVersion, version: "2"},
	}

	for name, claim := range claims {
		if claim.disagrees("2") {
			t.Errorf("%s claim disagrees with the version the resource holds", name)
		}
	}

	if !(precondition{kind: namedVersion, version: "1"}).disagrees("2") {
		t.Error("a claim naming another version was read as agreement")
	}
}

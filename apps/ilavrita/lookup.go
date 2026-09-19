package main

import (
	"errors"
	"net/http"

	"github.com/Ilavrita/Ilavrita/packages/fhir"
	"github.com/Ilavrita/Ilavrita/packages/storage"
	"github.com/pocketbase/pocketbase/core"
)

// The operations a code system answers, and the paths they are served on.
const (
	lookupOperationName   = "lookup"
	validateCodeOperation = "validate-code"

	lookupPath       = "/CodeSystem/$" + lookupOperationName
	validateCodePath = "/CodeSystem/$" + validateCodeOperation
)

// The parameters these operations take.
const (
	systemParameter  = "system"
	codeParameter    = "code"
	displayParameter = "display"
)

var (
	// errNoCodeNamed reports a lookup naming no system or no code.
	errNoCodeNamed = errors.New("ilavrita: a lookup names a system and a code")

	// errSystemNotHeld reports a code system this install never loaded.
	//
	// It is a different answer from a code that is not in one it did: telling a
	// client their code is wrong, when it is this install that is empty, sends
	// them to fix something that is not broken.
	errSystemNotHeld = errors.New("ilavrita: this install holds no such code system")

	// errNoSuchCode reports a code that is not in a system this install holds.
	errNoSuchCode = errors.New("ilavrita: that code system holds no such code")
)

// lookUpCode answers what one code means.
//
// It is served from what this install loaded and from nothing else. A system it
// does not hold is answered as such rather than by asking somebody: validation
// and lookup here are local and deterministic, and a server whose answers depend
// on a third party's uptime is one whose answers change without its code
// changing.
func lookUpCode(request *core.RequestEvent) error {
	if _, err := beginOn(request, codeSystemType, readActions); err != nil {
		return refuse(request, err)
	}

	system := request.Request.URL.Query().Get(systemParameter)
	code := request.Request.URL.Query().Get(codeParameter)

	if system == "" || code == "" {
		return refuse(request, errNoCodeNamed)
	}

	if serving == nil || serving.terminology == nil {
		return refuse(request, errSystemNotHeld)
	}

	ctx := request.Request.Context()

	if _, loaded, err := serving.terminology.Holds(ctx, system); err != nil {
		return refuse(request, err)
	} else if !loaded {
		return refuse(request, errSystemNotHeld)
	}

	concept, found, err := serving.terminology.Lookup(ctx, system, code)
	if err != nil {
		return refuse(request, err)
	}

	if !found {
		return refuse(request, errNoSuchCode)
	}

	return request.JSON(http.StatusOK, fhir.Parameters{
		ResourceType: "Parameters",
		Parameter: []fhir.Parameter{
			{Name: "name", ValueString: system},
			{Name: displayParameter, ValueString: concept.Display},
			{Name: "code", ValueString: concept.Code},
			{Name: "inactive", ValueBoolean: boolPtr(!concept.Active)},
		},
	})
}

// validateCode answers whether one code is in one system.
//
// R4 answers this with a Parameters holding `result`, whichever way it went: the
// operation was performed, and what it found is the answer. A system this
// install does not hold is the one case that is not a result at all — "no" would
// say the code is wrong, and what is true is that nothing here can say.
func validateCode(request *core.RequestEvent) error {
	if _, err := beginOn(request, codeSystemType, readActions); err != nil {
		return refuse(request, err)
	}

	system := request.Request.URL.Query().Get(systemParameter)
	code := request.Request.URL.Query().Get(codeParameter)

	if system == "" || code == "" {
		return refuse(request, errNoCodeNamed)
	}

	if serving == nil || serving.terminology == nil {
		return refuse(request, errSystemNotHeld)
	}

	ctx := request.Request.Context()

	if _, loaded, err := serving.terminology.Holds(ctx, system); err != nil {
		return refuse(request, err)
	} else if !loaded {
		return refuse(request, errSystemNotHeld)
	}

	concept, found, err := serving.terminology.Lookup(ctx, system, code)
	if err != nil {
		return refuse(request, err)
	}

	answer := fhir.Parameters{
		ResourceType: "Parameters",
		Parameter:    []fhir.Parameter{{Name: "result", ValueBoolean: boolPtr(found)}},
	}

	if found {
		answer.Parameter = append(answer.Parameter,
			fhir.Parameter{Name: displayParameter, ValueString: concept.Display})
	} else {
		answer.Parameter = append(answer.Parameter, fhir.Parameter{
			Name:        "message",
			ValueString: "That code system holds no such code.",
		})
	}

	return request.JSON(http.StatusOK, answer)
}

// boolPtr carries a boolean into a Parameters, where absent and false are
// different answers.
func boolPtr(held bool) *bool { return &held }

// terminologyOperations are the operations every install advertises on
// CodeSystem. They are advertised whether or not a system is loaded, because
// what they answer then is "this install holds no such system" — which is an
// answer, and a useful one.
var terminologyOperations = []servedInteraction{
	{method: http.MethodGet, path: lookupPath, handler: lookUpCode},
	{method: http.MethodGet, path: validateCodePath, handler: validateCode},
}

// codeSystemType is the one type these operations are served on.
const codeSystemType storage.ResourceType = "CodeSystem"

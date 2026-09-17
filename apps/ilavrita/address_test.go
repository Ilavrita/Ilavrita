package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func hostedRequest(host string) *http.Request {
	request := httptest.NewRequest(http.MethodGet, "/fhir/R4/metadata", nil)
	request.Host = host

	return request
}

// A configured origin is the whole answer, whatever the client claims its Host
// is. That is what a deployment behind a proxy must be able to say.
func TestAConfiguredOriginIgnoresTheHost(t *testing.T) {
	t.Setenv(baseURLVariable, "https://fhir.clinic.example")
	t.Setenv(allowedHostsVariable, "")

	address, err := configuredAddress()
	if err != nil {
		t.Fatalf("read the configured address: %v", err)
	}

	for _, host := range []string{"fhir.clinic.example", "evil.example", "127.0.0.1:8090"} {
		origin, err := address.origin(hostedRequest(host))
		if err != nil || origin != "https://fhir.clinic.example" {
			t.Fatalf("Host %q published %q: %v, want the configured origin", host, origin, err)
		}
	}
}

// Nothing configured is the deny-by-default state: a loopback deployment
// publishes itself, and every other Host is refused rather than reflected.
func TestAnUnconfiguredAddressPublishesLoopbackOnly(t *testing.T) {
	t.Setenv(baseURLVariable, "")
	t.Setenv(allowedHostsVariable, "")

	address, err := configuredAddress()
	if err != nil {
		t.Fatalf("read the unconfigured address: %v", err)
	}

	for _, host := range []string{"127.0.0.1:8090", "localhost:8090", "[::1]:8090"} {
		if _, err := address.origin(hostedRequest(host)); err != nil {
			t.Errorf("loopback Host %q was refused: %v", host, err)
		}
	}

	for _, host := range []string{"evil.example", "clinic.example:443", "127.0.0.1.evil.example"} {
		if _, err := address.origin(hostedRequest(host)); !errors.Is(err, unrecognisedHost) {
			t.Errorf("Host %q was published: %v", host, err)
		}
	}
}

// An allowlisted host is published as the client reached it, which is how one
// deployment serves several names without naming an origin per name.
func TestAnAllowedHostIsPublishedBack(t *testing.T) {
	t.Setenv(baseURLVariable, "")
	t.Setenv(allowedHostsVariable, " fhir.clinic.example , Records.Example:8443 ")

	address, err := configuredAddress()
	if err != nil {
		t.Fatalf("read the configured address: %v", err)
	}

	published := map[string]string{
		"fhir.clinic.example":      "http://fhir.clinic.example",
		"fhir.clinic.example:8090": "http://fhir.clinic.example:8090",
		"records.example:8443":     "http://records.example:8443",
	}

	for host, want := range published {
		origin, err := address.origin(hostedRequest(host))
		if err != nil || origin != want {
			t.Errorf("Host %q published %q: %v, want %q", host, origin, err, want)
		}
	}
}

// A value the parser cannot read whole stops the process. A base URL nobody
// meant is worse than none: every client is sent somewhere wrong.
func TestAMalformedBaseURLRefusesToStart(t *testing.T) {
	malformed := map[string]string{
		"no scheme":     "fhir.clinic.example",
		"no host":       "https://",
		"wrong scheme":  "ftp://fhir.clinic.example",
		"carries path":  "https://fhir.clinic.example/fhir/R4",
		"carries query": "https://fhir.clinic.example?tenant=a",
		"not a url":     "https://%zz",
	}

	for name, value := range malformed {
		t.Run(name, func(t *testing.T) {
			if _, err := parseBaseURL(value); !errors.Is(err, errMalformedBaseURL) {
				t.Fatalf("%q was accepted: %v", value, err)
			}
		})
	}
}

// A trailing slash is the one shape worth accepting: it names the same origin.
func TestABaseURLMayCarryATrailingSlash(t *testing.T) {
	origin, err := parseBaseURL("https://fhir.clinic.example/")
	if err != nil || origin != "https://fhir.clinic.example" {
		t.Fatalf("parsed %q: %v, want the bare origin", origin, err)
	}
}

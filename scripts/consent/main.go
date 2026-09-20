// Command consent is a consent page for testing, and only for testing.
//
// Ilavrita's authorization endpoint redirects a person to whatever
// ILAVRITA_CONSENT_URL names, and deliberately ships no such page: a consent
// screen has to be styled, translated and kept accessible by whoever deploys it,
// and none of that belongs in this repository.
//
// The cost of that decision is that nothing can drive the flow end to end — a
// conformance suite included. This closes that gap for a test run by doing what
// a real page would do, minus everything that makes it a page: it signs in as
// one configured identity, reads what would be granted, approves all of it, and
// sends the browser on.
//
// # It approves everything, without asking anybody
//
// That is the whole reason it must never be deployed. A real page shows a person
// what an app is asking for and lets them refuse some of it; this one exists so
// an automated suite can get past the step a person would occupy. It lives in
// scripts/ rather than apps/ for the same reason, and says so on every start.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// The configuration a run needs. There are no defaults for the credentials: a
// program that signed in as "admin/admin" when nobody configured it is one that
// would eventually be pointed at something real.
const (
	serverVariable   = "ILAVRITA_SERVER"
	projectVariable  = "ILAVRITA_CONSENT_PROJECT"
	emailVariable    = "ILAVRITA_CONSENT_EMAIL"
	passwordVariable = "ILAVRITA_CONSENT_PASSWORD" //nolint:gosec // the name of a variable, not one
	listenVariable   = "ILAVRITA_CONSENT_LISTEN"
)

// approver holds what one run needs to sign in and approve.
type approver struct {
	server   string
	project  string
	email    string
	password string
	client   *http.Client
}

func main() {
	held, err := configured()
	if err != nil {
		log.Fatal(err)
	}

	listen := os.Getenv(listenVariable)
	if listen == "" {
		listen = "127.0.0.1:8140"
	}

	log.Printf("WARNING: this consent page approves every scope without asking anybody.")
	log.Printf("It exists so a conformance suite can drive the flow. Never deploy it.")
	// The values are this operator's own configuration, printed once at start so
	// a run says what it is about to do as. Nothing here came from a request.
	log.Printf("approving for %s in %s, against %s, on %s", //nolint:gosec // operator configuration, not request input
		held.email, held.project, held.server, listen)

	http.HandleFunc("/approve", held.approve)

	server := &http.Server{
		Addr:              listen,
		ReadHeaderTimeout: 10 * time.Second,
	}

	log.Fatal(server.ListenAndServe())
}

// configured reads what this run needs, refusing to start without all of it.
func configured() (*approver, error) {
	held := &approver{
		server:   strings.TrimRight(os.Getenv(serverVariable), "/"),
		project:  os.Getenv(projectVariable),
		email:    os.Getenv(emailVariable),
		password: os.Getenv(passwordVariable),
		client: &http.Client{
			Timeout: 30 * time.Second,

			// A redirect is the answer this program produces, never one it
			// follows: the approval names where the browser goes, and following
			// it here would spend the code the browser needs.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}

	for name, value := range map[string]string{
		serverVariable:   held.server,
		projectVariable:  held.project,
		emailVariable:    held.email,
		passwordVariable: held.password,
	} {
		if value == "" {
			return nil, fmt.Errorf("%s is required", name)
		}
	}

	return held, nil
}

// approve is what the authorization endpoint redirected the browser to.
func (a *approver) approve(response http.ResponseWriter, request *http.Request) {
	ask := request.URL.Query()

	token, err := a.signIn()
	if err != nil {
		failed(response, "sign in", err)

		return
	}

	offered, err := a.offered(token, ask)
	if err != nil {
		failed(response, "read what would be granted", err)

		return
	}

	sent, err := a.approved(token, ask, offered)
	if err != nil {
		failed(response, "approve", err)

		return
	}

	// Not an open redirect: sent is the address the server itself built, and the
	// server only builds one after checking it against the addresses the client
	// registered. What arrives in the query is never redirected to.
	http.Redirect(response, request, sent, http.StatusFound) //nolint:gosec // the server validated this address against the registration
}

// signIn proves the configured credential and returns the session a person would
// be carrying.
func (a *approver) signIn() (string, error) {
	body, err := json.Marshal(map[string]string{
		"project": a.project, "email": a.email, "password": a.password,
	})
	if err != nil {
		return "", err
	}

	answer, err := a.client.Post(a.server+"/auth/login", "application/json", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	defer func() { _ = answer.Body.Close() }()

	if answer.StatusCode != http.StatusOK {
		return "", statusError("login", answer)
	}

	var held struct {
		Token string `json:"token"`
	}

	if err := json.NewDecoder(answer.Body).Decode(&held); err != nil {
		return "", err
	}

	if held.Token == "" {
		return "", errors.New("the login returned no token")
	}

	return held.Token, nil
}

// offered reads which scopes this server would grant, which is what a real page
// would show somebody.
func (a *approver) offered(token string, ask url.Values) ([]string, error) {
	// The host is this operator's own ILAVRITA_SERVER, fixed at start; only the
	// query travels from the request, and it reaches a path on that one host.
	request, err := http.NewRequest( //nolint:gosec // the host is operator configuration, fixed at start
		http.MethodGet, a.server+"/oauth2/consent?"+ask.Encode(), nil)
	if err != nil {
		return nil, err
	}

	request.Header.Set("Authorization", "Bearer "+token)

	answer, err := a.client.Do(request) //nolint:gosec // same host, see above
	if err != nil {
		return nil, err
	}
	defer func() { _ = answer.Body.Close() }()

	if answer.StatusCode != http.StatusOK {
		return nil, statusError("consent", answer)
	}

	var held struct {
		Grantable []string `json:"grantable"`
		Refused   []struct {
			Scope  string `json:"scope"`
			Reason string `json:"reason"`
		} `json:"refused"`
	}

	if err := json.NewDecoder(answer.Body).Decode(&held); err != nil {
		return nil, err
	}

	// Logged rather than shown, because there is nobody here to show it to — but
	// a scope this server refused is the thing a person most needs to know, so a
	// run that dropped it silently would hide the interesting half.
	for _, refused := range held.Refused {
		log.Printf("refused %s: %s", refused.Scope, refused.Reason)
	}

	return held.Grantable, nil
}

// approved records the approval and returns where the browser goes next.
func (a *approver) approved(token string, ask url.Values, scopes []string) (string, error) {
	body, err := json.Marshal(map[string]any{"approved": scopes})
	if err != nil {
		return "", err
	}

	request, err := http.NewRequest( //nolint:gosec // the host is operator configuration, fixed at start
		http.MethodPost, a.server+"/oauth2/consent?"+ask.Encode(), bytes.NewReader(body))
	if err != nil {
		return "", err
	}

	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")

	answer, err := a.client.Do(request) //nolint:gosec // same host, see above
	if err != nil {
		return "", err
	}
	defer func() { _ = answer.Body.Close() }()

	if answer.StatusCode != http.StatusOK {
		return "", statusError("approval", answer)
	}

	var held struct {
		Redirect string `json:"redirect"`
	}

	if err := json.NewDecoder(answer.Body).Decode(&held); err != nil {
		return "", err
	}

	if held.Redirect == "" {
		return "", errors.New("the approval named nowhere to go")
	}

	return held.Redirect, nil
}

// statusError carries what the server said, because a conformance run that fails
// here needs the reason and not the number.
func statusError(step string, answer *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(answer.Body, 2048))

	return fmt.Errorf("%s answered %d: %s", step, answer.StatusCode, strings.TrimSpace(string(body)))
}

// failed answers the browser with what went wrong, since a suite driving this
// sees only what the page renders.
func failed(response http.ResponseWriter, step string, err error) {
	log.Printf("could not %s: %v", step, err)

	response.Header().Set("Content-Type", "text/plain; charset=utf-8")
	response.WriteHeader(http.StatusBadGateway)

	_, _ = fmt.Fprintf(response, "could not %s: %v\n", step, err)
}

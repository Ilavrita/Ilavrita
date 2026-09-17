package main

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
)

// Where clients actually reach this server. The Host header is chosen by the
// client, so it is published back only when this deployment recognises it;
// a deployment behind a proxy names its external origin outright.
const (
	baseURLVariable      = "ILAVRITA_BASE_URL"
	allowedHostsVariable = "ILAVRITA_ALLOWED_HOSTS"
)

// errMalformedBaseURL refuses a value that is not one absolute origin, which
// stops the process rather than publishing a URL that resolves nowhere.
var errMalformedBaseURL = errors.New(
	"ilavrita: " + baseURLVariable + " must read <scheme>://<host>, carrying no path")

// loopbackHosts are what a deployment reaches itself on, and the only hosts
// reflected when nothing is configured.
var loopbackHosts = []string{"localhost", "127.0.0.1", "::1"}

// publishedAddress decides the origin every published URL is built from: one
// named by configuration, or the hosts whose own Host header may be reflected.
type publishedAddress struct {
	configured string
	allowed    []string
}

// publishing is read by every route that publishes a URL. The zero value
// reflects loopback alone, which is what an unconfigured process serves.
var publishing publishedAddress

// configuredAddress reads where this deployment is reachable. Nothing
// configured is no error: a loopback deployment publishes itself.
func configuredAddress() (publishedAddress, error) {
	configured, err := parseBaseURL(strings.TrimSpace(os.Getenv(baseURLVariable)))
	if err != nil {
		return publishedAddress{}, err
	}

	return publishedAddress{
		configured: configured,
		allowed:    parseAllowedHosts(os.Getenv(allowedHostsVariable)),
	}, nil
}

// parseBaseURL reads one absolute origin. A path, query or fragment is refused
// rather than silently dropped: what follows the origin is this server's own.
func parseBaseURL(value string) (string, error) {
	if value == "" {
		return "", nil
	}

	parsed, err := url.Parse(value)
	if err != nil {
		return "", fmt.Errorf("%w: %w", errMalformedBaseURL, err)
	}

	if !publishesScheme(parsed.Scheme) || parsed.Host == "" ||
		strings.Trim(parsed.Path, "/") != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("%w: read %q", errMalformedBaseURL, value)
	}

	return parsed.Scheme + "://" + parsed.Host, nil
}

func publishesScheme(scheme string) bool {
	return scheme == "http" || scheme == "https"
}

// parseAllowedHosts reads the hosts whose Host header may be published back.
func parseAllowedHosts(value string) []string {
	var allowed []string

	for _, entry := range strings.Split(value, ",") {
		if host := hostname(entry); host != "" {
			allowed = append(allowed, host)
		}
	}

	return allowed
}

// hostname is one host without its port, lowercased. The port is dropped
// because it names no authority a URL could be forged through.
func hostname(value string) string {
	trimmed := strings.TrimSpace(value)
	if host, _, err := net.SplitHostPort(trimmed); err == nil {
		trimmed = host
	}

	return strings.ToLower(strings.Trim(trimmed, "[]"))
}

// origin answers with the scheme and authority to publish. A Host this
// deployment does not recognise is refused rather than reflected, so a forged
// one can never reach a client through a URL this server minted.
func (p publishedAddress) origin(request *http.Request) (string, error) {
	if p.configured != "" {
		return p.configured, nil
	}

	host := hostname(request.Host)
	if !slices.Contains(p.allowed, host) && !slices.Contains(loopbackHosts, host) {
		return "", unrecognisedHost
	}

	return requestScheme(request) + "://" + request.Host, nil
}

// requestScheme is the only signal an unconfigured deployment has. A proxy that
// terminates TLS is invisible here, which is what baseURLVariable is for.
func requestScheme(request *http.Request) string {
	if request.TLS != nil {
		return "https"
	}

	return "http"
}

package project

import (
	"fmt"
	"net/url"
	"slices"
	"strings"
)

// maxRedirectURIs bounds how many addresses one registration may name. An app
// has a handful; a list longer than this is a registration being used as
// storage.
const maxRedirectURIs = 16

// maxRedirectURI bounds one address, for the same reason.
const maxRedirectURI = 2048

// RedirectURI is one address an authorization code may be handed back to.
//
// It is a distinct type because it is compared, never re-parsed, at the moment
// it matters: OAuth requires the string a client presents to equal the string it
// registered, character for character. Normalising at that comparison is how a
// server ends up accepting an address nobody registered, so normalising happens
// here, once, and what is stored is what must be presented.
type RedirectURI string

// ParseRedirectURI reads one registered address.
//
// It is the one way an outside value becomes a RedirectURI, so every stored
// address is absolute, carries no fragment, and is one an authorization response
// can actually be delivered to.
func ParseRedirectURI(stated string) (RedirectURI, error) {
	stated = strings.TrimSpace(stated)

	if stated == "" {
		return "", fmt.Errorf("%w: an address nobody stated", ErrInvalidRedirectURI)
	}

	if len(stated) > maxRedirectURI {
		return "", fmt.Errorf("%w: %d characters exceeds the %d one address carries",
			ErrInvalidRedirectURI, len(stated), maxRedirectURI)
	}

	parsed, err := url.Parse(stated)
	if err != nil {
		return "", fmt.Errorf("%w: %q: %w", ErrInvalidRedirectURI, stated, err)
	}

	if !parsed.IsAbs() {
		return "", fmt.Errorf("%w: %q names no scheme", ErrInvalidRedirectURI, stated)
	}

	// A fragment is never sent to a server, so an address carrying one states
	// something it cannot mean. RFC 6749 refuses it outright.
	if parsed.Fragment != "" || strings.Contains(stated, "#") {
		return "", fmt.Errorf("%w: %q carries a fragment", ErrInvalidRedirectURI, stated)
	}

	if err := redirectSchemeAllowed(parsed, stated); err != nil {
		return "", err
	}

	return RedirectURI(stated), nil
}

// redirectSchemeAllowed refuses the schemes an authorization code must not be
// handed back over.
//
// http is permitted only for loopback, which is how a native app receives a
// code: there is no network for anyone to read it from, and refusing loopback
// would push native apps onto a hosted redirect, which is worse. Every other
// http address is refused, because a code in a query string over cleartext is a
// code anybody on the path has.
func redirectSchemeAllowed(parsed *url.URL, stated string) error {
	switch parsed.Scheme {
	case "https":
		return nil

	case "http":
		if loopback(parsed.Hostname()) {
			return nil
		}

		return fmt.Errorf("%w: %q is cleartext and not loopback", ErrInvalidRedirectURI, stated)

	default:
		// A private-use scheme — com.example.app:/callback — is how a mobile app
		// is called back, and is permitted. What is refused is a scheme that
		// makes the address executable rather than navigable.
		if executableScheme(parsed.Scheme) {
			return fmt.Errorf("%w: %q is not an address a code is delivered to",
				ErrInvalidRedirectURI, stated)
		}

		return nil
	}
}

// loopback reports whether a host is this machine, which is the one place
// cleartext carries a code safely.
func loopback(host string) bool {
	return host == "127.0.0.1" || host == "::1" || host == "localhost"
}

// executableSchemes are the ones that run something rather than address
// something. A code delivered to one of these is a code delivered to whatever
// the string evaluates to.
var executableSchemes = []string{"javascript", "data", "vbscript", "file", "blob"}

// executableScheme reports whether a scheme runs its own content.
func executableScheme(scheme string) bool {
	return slices.Contains(executableSchemes, strings.ToLower(scheme))
}

// RedirectURIs are the addresses one registration named.
//
// The zero value is a registration that named none, which is what a client doing
// no authorization-code flow is. That is not an error: a backend service has no
// redirect to state.
type RedirectURIs struct {
	held []RedirectURI
}

// NewRedirectURIs reads the addresses a registration names, refusing a
// duplicate: two identical entries are one address stated twice, and a list that
// tolerated them would make "how many did this client register" unanswerable.
func NewRedirectURIs(stated ...string) (RedirectURIs, error) {
	if len(stated) > maxRedirectURIs {
		return RedirectURIs{}, fmt.Errorf("%w: %d addresses exceeds the %d a registration names",
			ErrInvalidRedirectURI, len(stated), maxRedirectURIs)
	}

	held := make([]RedirectURI, 0, len(stated))

	for _, one := range stated {
		parsed, err := ParseRedirectURI(one)
		if err != nil {
			return RedirectURIs{}, err
		}

		if slices.Contains(held, parsed) {
			return RedirectURIs{}, fmt.Errorf("%w: %q is stated twice", ErrInvalidRedirectURI, one)
		}

		held = append(held, parsed)
	}

	if len(held) == 0 {
		return RedirectURIs{}, nil
	}

	return RedirectURIs{held: held}, nil
}

// Allows reports whether an address is one this registration named.
//
// The comparison is exact, which is the whole point: a prefix or a host match
// would let a client that registered https://app.example.test/callback receive a
// code at any path an open redirect on that host reaches.
func (r RedirectURIs) Allows(stated string) bool {
	return slices.Contains(r.held, RedirectURI(strings.TrimSpace(stated)))
}

// Stated returns the addresses, for a registration being read back.
func (r RedirectURIs) Stated() []RedirectURI {
	return slices.Clone(r.held)
}

// Len returns how many addresses were named.
func (r RedirectURIs) Len() int { return len(r.held) }

// IsZero reports whether the registration named no address at all, which is what
// a client doing no authorization-code flow looks like.
func (r RedirectURIs) IsZero() bool { return len(r.held) == 0 }

package project

import (
	"strings"
	"testing"
	"unicode"
)

// FuzzNormaliseEmailIsInjectiveEnough pins the four properties the identity
// layer rests on. Each one is the difference between a duplicate account and a
// collapsed one, and none of them is visible from a table of worked cases.
func FuzzNormaliseEmailIsInjectiveEnough(f *testing.F) {
	for _, seed := range []string{
		"alice@corp.example", "Alice@Example.COM.", "alice@café.example",
		"alice@xn--caf-dma.example", "a.b+c@a.example。com", "alice@ex\u00adample.com",
		"alice@Kelvin.example", "alice@straße.de", "A!#$%&'*+-/=?^_`{|}~@a.bb",
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, raw string) {
		email, err := NormaliseEmail(raw)
		if err != nil {
			if !email.IsZero() {
				t.Fatalf("%q was rejected but returned %q", raw, email.Normalized())
			}

			return
		}

		assertKeyShape(t, raw, email.Normalized())
		assertDisplayIsAFixedPoint(t, raw, email)
		assertLocalPartOnlyCaseFolds(t, raw, email.Normalized())
	})
}

// assertKeyShape holds the normalised form to what a realm-scoped index key may
// be: one mailbox, spelled in printable lowercase ASCII.
func assertKeyShape(t *testing.T, raw, normalized string) {
	t.Helper()

	if strings.Count(normalized, "@") != 1 {
		t.Fatalf("%q normalised to %q, which is not one mailbox", raw, normalized)
	}

	for _, char := range normalized {
		if char > unicode.MaxASCII || unicode.IsUpper(char) || char <= ' ' || char == 0x7F {
			t.Fatalf("%q normalised to %q holding %q", raw, normalized, char)
		}
	}
}

// assertDisplayIsAFixedPoint covers the store's read path, which re-derives the
// normalised column from the display column. A spelling that drifts on the
// second pass would fail the crosscheck on a row nobody tampered with.
func assertDisplayIsAFixedPoint(t *testing.T, raw string, email Email) {
	t.Helper()

	again, err := NormaliseEmail(email.Display())
	if err != nil {
		t.Fatalf("%q: display %q no longer normalises: %v", raw, email.Display(), err)
	}

	if again.Normalized() != email.Normalized() {
		t.Fatalf("%q: display round trip moved %q to %q", raw, email.Normalized(), again.Normalized())
	}

	if again.Display() != email.Display() {
		t.Fatalf("%q: display is unstable, %q became %q", raw, email.Display(), again.Display())
	}
}

// assertLocalPartOnlyCaseFolds is the collapse guarantee. ASCII case is the one
// distinction this server folds away; any other rewrite of the local part would
// merge two mailboxes only their provider can tell apart.
func assertLocalPartOnlyCaseFolds(t *testing.T, raw, normalized string) {
	t.Helper()

	before := strings.TrimLeft(raw[:strings.IndexByte(raw, '@')], " \t\n\v\f\r")
	after := normalized[:strings.IndexByte(normalized, '@')]

	if !strings.EqualFold(before, after) {
		t.Fatalf("%q: local part %q became %q, which is more than case folding", raw, before, after)
	}
}

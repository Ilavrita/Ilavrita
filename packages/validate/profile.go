package validate

import (
	"os"
	"sync"

	"github.com/Ilavrita/Ilavrita/packages/conformance"
)

// profiles are the implementation guides this install was given. A build given
// none checks a resource against its base definition and no further, which is
// silence about the guide rather than a refusal.
var profiles = sync.OnceValue(func() conformance.Profiles {
	dir := os.Getenv(conformance.ProfileDirectory)
	if dir == "" {
		return conformance.Profiles{}
	}

	held, err := conformance.ProfilesFrom(dir)
	if err != nil {
		return conformance.Profiles{}
	}

	return held
})

// againstDeclaredProfiles holds a resource to every profile it claims in
// meta.profile.
//
// A profile this install does not hold is reported rather than passed: a
// resource claiming conformance to something nobody here can check is a
// narrowing nobody checked, and saying so is the difference between a server
// that validated and one that was silent.
func (r *Report) againstDeclaredProfiles(model conformance.Model, held map[string]any, where string) {
	resolved := profiles()

	for _, url := range declaredProfiles(held) {
		structure, found := resolved.Profile(url)
		if !found {
			r.note(SeverityWarning, where+".meta.profile",
				"This install does not hold "+url+", so the resource was not checked against it.")

			continue
		}

		r.checkObject(model, structure, "", held, where, 0)
	}
}

// declaredProfiles reads meta.profile, which is a list of canonical URLs.
func declaredProfiles(held map[string]any) []string {
	meta, carried := held["meta"].(map[string]any)
	if !carried {
		return nil
	}

	listed, stated := meta["profile"].([]any)
	if !stated {
		return nil
	}

	var urls []string

	for _, each := range listed {
		if url, isString := each.(string); isString && url != "" {
			urls = append(urls, url)
		}
	}

	return urls
}

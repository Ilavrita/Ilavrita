package validate_test

import (
	"sort"
	"strconv"
	"testing"

	"github.com/Ilavrita/Ilavrita/packages/conformance"
	"github.com/Ilavrita/Ilavrita/packages/storage"
	"github.com/Ilavrita/Ilavrita/packages/validate"
)

// TestTheSpecificationValidatesAgainstItself. Refusing a resource is the
// expensive kind of wrong: it is a write a client cannot make, and a client
// cannot argue with it. The strongest available evidence against that is the
// specification's own content — a few hundred real R4 resources of several
// types, deeply nested, full of backbone elements, choices, contained content
// and elements that point at their own children.
//
// Every one of them must pass. One that does not is this build refusing what HL7
// published, which is a bug here and never there.
func TestTheSpecificationValidatesAgainstItself(t *testing.T) {
	held, err := conformance.Bundled()
	if err != nil {
		t.Fatalf("read the specification: %v", err)
	}

	if len(held) < 250 {
		t.Fatalf("read %d resources, which is not the specification", len(held))
	}

	kinds := map[string]int{}
	faulted := 0

	for _, resource := range held {
		kinds[resource.Type]++

		report := validate.Resource(storage.ResourceType(resource.Type), resource.Content)
		if report.OK() {
			continue
		}

		faulted++

		if faulted > 5 {
			continue
		}

		for _, issue := range report.Issues() {
			if issue.Severity == validate.SeverityError {
				t.Errorf("%s/%s is refused at %s: %s",
					resource.Type, resource.ID, issue.Expression, issue.Detail)
			}
		}
	}

	if faulted != 0 {
		t.Errorf("%d of %d published resources were refused", faulted, len(held))
	}

	// And it covers more than one shape of resource, so passing is not one
	// type's good luck.
	var covered []string
	for name, count := range kinds {
		covered = append(covered, name+"="+strconv.Itoa(count))
	}

	sort.Strings(covered)
	t.Logf("checked %v", covered)

	if len(kinds) < 3 {
		t.Errorf("only %d resource types were covered: %v", len(kinds), covered)
	}
}
